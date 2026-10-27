import { describe,it,expect,vi,afterEach,beforeEach } from 'vitest'
import { mount,flushPromises } from '@vue/test-utils'
import { nextTick } from 'vue'
import App from './App.vue'
const id='a'.repeat(32)
const xss='<img src=x onerror="window.__xss=1"><script>window.__xss=1</script>'
const campaign={id,brief:xss,status:'draft',version:1,proposal:{segment:{city:'İstanbul',days:4,exclude_purchased:true},content:{sms:'Merhaba {{first_name}}',email_subject:'Konu',email_body:'Mesaj'}},audience:{total:1,sms:1,email:1},counts:{}}
let wrapper,fetchMock,postError=false,queueUnavailable=false,streams=[]
class FakeEventSource{constructor(url){this.url=url;this.listeners={};streams.push(this)}addEventListener(kind,fn){this.listeners[kind]=fn}close(){}}
const result=(data,ok=true)=>({ok,status:ok?200:422,json:async()=>structuredClone(data)})
beforeEach(()=>{
 localStorage.clear();streams=[];postError=false;queueUnavailable=false
 vi.spyOn(window,'scrollTo').mockImplementation(()=>{})
 fetchMock=vi.fn(async(path,options={})=>{
  if(path==='/api/config')return result({llm_mode:'mock',limits:{brief:4000,sms:160,email_subject:80,email_body:10000}})
  if(path==='/api/campaigns'&&options.method==='POST')return postError?result({error:xss},false):result({id,status:'generating'})
  if(path==='/api/campaigns')return result([campaign])
  if(path.startsWith('/api/stats'))return result({})
  if(path==='/api/queues')return result({channels:{sms:queueUnavailable?{available:false,error:'unavailable'}:{available:true,pending:0,active:0,scheduled:2,retry:95,outstanding:97},email:{available:true,pending:3,active:1,scheduled:0,retry:0,outstanding:4}},sampled_at:'2026-10-27T10:00:00Z'})
  if(path.startsWith('/api/errors'))return result({items:[],next_cursor:0})
  if(path==='/health/ready')return result({postgres:true,redis:true,status:'ok'})
  throw new Error('Unexpected request '+path)
 });vi.stubGlobal('fetch',fetchMock);vi.stubGlobal('EventSource',FakeEventSource)
})
afterEach(()=>{wrapper?.unmount();wrapper=null;document.body.innerHTML='';vi.unstubAllGlobals();vi.restoreAllMocks();localStorage.clear()})
function dashboard(data={}){streams.find(s=>s.url==='/api/dashboard/events').listeners.dashboard({data:JSON.stringify({stats:{},channel_stats:{sms:{},email:{}},campaigns:[campaign],health:{postgres:true,redis:true},queues:{sms:queueUnavailable?{available:false}:{available:true,pending:0,active:0,scheduled:2,retry:95,outstanding:97},email:{available:true,pending:3,active:1,scheduled:0,retry:0,outstanding:4}},...data})})}
async function start(){wrapper=mount(App,{attachTo:document.body});await flushPromises();dashboard();await nextTick()}
async function clickText(text){const b=wrapper.findAll('button').find(b=>b.text().includes(text));expect(b).toBeTruthy();await b.trigger('click');await flushPromises()}
describe('Vue rendered security',()=>{
 it('renders stored campaign HTML as literal text',async()=>{await start();await clickText('Gönderim takibi');expect(wrapper.text()).toContain(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false)})
 it('submits SQL-like brief as unchanged JSON data',async()=>{await start();const payload="İstanbul son 4 gün O'Reilly'; DROP TABLE users; -- %10";await wrapper.get('#brief').setValue(payload);await clickText('Kitle ve mesajları oluştur');const call=fetchMock.mock.calls.find(([path,o])=>path==='/api/campaigns'&&o.method==='POST');expect(JSON.parse(call[1].body)).toEqual({brief:payload});expect(streams.find(s=>s.url.includes(id)).url).toBe(`/api/campaigns/${id}/events`)})
 it('escapes streamed content and blocks unknown variables',async()=>{await start();await clickText('Kitle ve mesajları oluştur');const c=structuredClone(campaign);c.proposal.content.sms=xss;streams.find(s=>s.url.includes(id)).listeners.snapshot({data:JSON.stringify(c)});await nextTick();expect(wrapper.get('.preview-box').text()).toBe(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false);await wrapper.get('#sms').setValue('{{email}}');const save=wrapper.findAll('button').find(b=>b.text().includes('Kaydet & önizle'));expect(save.attributes('disabled')).toBeDefined()})
 it('does not request a tampered localStorage route',async()=>{localStorage.setItem('studio.campaign','../../health/ready?x=1');await start();expect(localStorage.getItem('studio.campaign')).toBeNull();expect(fetchMock.mock.calls.some(([url])=>url.includes('..'))).toBe(false);expect(streams.map(s=>s.url)).toEqual(['/api/dashboard/events'])})
 it('renders API error HTML as text',async()=>{postError=true;await start();await clickText('Kitle ve mesajları oluştur');expect(wrapper.get('[role="alert"]').text()).toContain(xss);expect(wrapper.find('img').exists()).toBe(false);expect(wrapper.find('script').exists()).toBe(false)})
})

describe('monitor queue visibility and Studio action',()=>{
 it('opens campaign creation at the first step without a separate new campaign button',async()=>{localStorage.setItem('studio.campaign',id);await start();expect(wrapper.find('#brief').exists()).toBe(true);expect(wrapper.find('.studio-toolbar').exists()).toBe(false);expect(fetchMock.mock.calls.some(([url])=>url===`/api/campaigns/${id}`)).toBe(false);await clickText('Kitle ve mesajları oluştur');expect(wrapper.find('#sms').exists()).toBe(true);await clickText('Gönderim takibi');await clickText('Kampanya oluştur');expect(wrapper.find('#brief').exists()).toBe(true);expect(wrapper.find('#sms').exists()).toBe(false)})
 it('shows separate campaign channel counts instead of global totals',async()=>{await start();await clickText('Gönderim takibi');dashboard({campaigns:[{...campaign,status:'completed',channel_counts:{sms:{accepted:12,retrying:3},email:{accepted:7,failed:2}}}]});await nextTick();expect(wrapper.get('[aria-label="SMS kampanya istatistikleri"]').text()).toContain('12');expect(wrapper.get('[aria-label="Email kampanya istatistikleri"]').text()).toContain('7');expect(wrapper.find('.channel-summary').exists()).toBe(false)})
})


it('shows campaigns requiring manual review',async()=>{await start();await clickText('Gönderim takibi');dashboard({campaigns:[{...campaign,status:'needs_review',channel_counts:{sms:{unknown:2},email:{}}}]});await nextTick();expect(wrapper.text()).toContain('İnceleme gerekiyor');expect(wrapper.get('[aria-label="SMS kampanya istatistikleri"]').text()).toContain('Sonucu belirsiz')})

it('streams independent campaign channel progress and restores it on reconnect without polling',async()=>{
 await start();await clickText('Gönderim takibi')
 const c={...campaign,status:'dispatching',channel_counts:{sms:{accepted:8,pending:2},email:{accepted:2,retrying:8}}}
 dashboard({campaigns:[c]});await nextTick()
 expect(wrapper.get('[aria-label="SMS işlenme ilerlemesi"]').attributes('aria-valuenow')).toBe('80')
 expect(wrapper.get('[aria-label="Email işlenme ilerlemesi"]').attributes('aria-valuenow')).toBe('20')
 await flushPromises();const before=fetchMock.mock.calls.length
 vi.useFakeTimers();try{await vi.advanceTimersByTimeAsync(12000);expect(fetchMock.mock.calls.length).toBe(before)}finally{vi.useRealTimers()}
 const stream=streams.find(s=>s.url==='/api/dashboard/events');stream.onerror();stream.onopen()
 c.channel_counts.sms={accepted:9,failed:1};dashboard({campaigns:[c]});await nextTick()
 expect(wrapper.get('[aria-label="SMS işlenme ilerlemesi"]').attributes('aria-valuenow')).toBe('100')
 expect(wrapper.get('[aria-label="Email işlenme ilerlemesi"]').attributes('aria-valuenow')).toBe('20')
 expect(fetchMock.mock.calls.some(([url])=>url==='/api/stats'||url==='/api/queues')).toBe(false)
})
it('shows channels without recipients with zero progress',async()=>{
 await start();await clickText('Gönderim takibi')
 dashboard({campaigns:[{...campaign,status:'approved',channel_counts:{sms:{pending:3},email:{}}}]});await nextTick()
 expect(wrapper.get('[aria-label="Email kampanya istatistikleri"]').text()).toContain('Alıcı yok')
 expect(wrapper.get('[aria-label="Email işlenme ilerlemesi"]').attributes('aria-valuenow')).toBe('0')
})
