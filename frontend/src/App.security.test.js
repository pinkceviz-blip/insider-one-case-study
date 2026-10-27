import { describe,it,expect,vi,afterEach,beforeEach } from 'vitest'
import { mount,flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import App from './App.vue'
const id='a'.repeat(32)
const xss='<img src=x onerror="window.__xss=1"><script>window.__xss=1</script>'
const campaign={id,brief:xss,status:'draft',version:1,proposal:{segment:{city:'İstanbul',days:4,exclude_purchased:true},content:{sms:'Merhaba {{first_name}}',email_subject:'Konu',email_body:'Mesaj'}},audience:{total:1,sms:1,email:1},counts:{}}
let wrapper,fetchMock,postError=false,streams=[]
class FakeEventSource{constructor(url){this.url=url;this.listeners={};streams.push(this)}addEventListener(kind,fn){this.listeners[kind]=fn}close(){}}
const result=(data,ok=true)=>({ok,status:ok?200:422,json:async()=>structuredClone(data)})
beforeEach(()=>{
 localStorage.clear();streams=[];postError=false
 fetchMock=vi.fn(async(path,options={})=>{
  if(path==='/api/config')return result({llm_mode:'mock',limits:{brief:4000,sms:160,email_subject:80,email_body:10000}})
  if(path==='/api/campaigns'&&options.method==='POST')return postError?result({error:xss},false):result({id,status:'generating'})
  if(path==='/api/campaigns')return result([campaign])
  if(path.startsWith('/api/stats'))return result({})
  if(path.startsWith('/api/errors'))return result({items:[],next_cursor:0})
  if(path==='/health/ready')return result({postgres:true,redis:true,status:'ok'})
  throw new Error('Unexpected request '+path)
 });vi.stubGlobal('fetch',fetchMock);vi.stubGlobal('EventSource',FakeEventSource)
})
afterEach(()=>{wrapper?.unmount();wrapper=null;document.body.innerHTML='';vi.unstubAllGlobals();localStorage.clear()})
async function start(){wrapper=mount(App,{attachTo:document.body});await flushPromises()}
async function clickText(text){const b=wrapper.findAll('button').find(b=>b.text().includes(text));expect(b).toBeTruthy();await b.trigger('click');await flushPromises()}
describe('Vue rendered security',()=>{
 it('renders stored campaign HTML as literal text',async()=>{await start();await clickText('Gönderim takibi');expect(wrapper.text()).toContain(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false)})
 it('submits SQL-like brief as unchanged JSON data',async()=>{await start();const payload="İstanbul son 4 gün O'Reilly'; DROP TABLE users; -- %10";await wrapper.get('#brief').setValue(payload);await clickText('Kitle ve mesajları oluştur');const call=fetchMock.mock.calls.find(([path,o])=>path==='/api/campaigns'&&o.method==='POST');expect(JSON.parse(call[1].body)).toEqual({brief:payload});expect(streams[0].url).toBe(`/api/campaigns/${id}/events`)})
 it('escapes streamed content and blocks unknown variables',async()=>{await start();await clickText('Kitle ve mesajları oluştur');const c=structuredClone(campaign);c.proposal.content.sms=xss;streams[0].listeners.snapshot({data:JSON.stringify(c)});await nextTick();expect(wrapper.get('.preview-box').text()).toBe(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false);await wrapper.get('#sms').setValue('{{email}}');const save=wrapper.findAll('button').find(b=>b.text().includes('Kaydet & önizle'));expect(save.attributes('disabled')).toBeDefined()})
 it('does not request a tampered localStorage route',async()=>{localStorage.setItem('studio.campaign','../../health/ready?x=1');await start();expect(localStorage.getItem('studio.campaign')).toBeNull();expect(fetchMock.mock.calls.some(([url])=>url.includes('..'))).toBe(false);expect(streams).toHaveLength(0)})
 it('renders API error HTML as text',async()=>{postError=true;await start();await clickText('Kitle ve mesajları oluştur');expect(wrapper.get('[role="alert"]').text()).toContain(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false)})
})
