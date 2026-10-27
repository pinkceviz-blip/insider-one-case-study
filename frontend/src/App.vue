<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { isCampaignId, campaignPath, deliveryHistoryPath, literalCount, templateError } from './security.js'
const channelViews=[{id:'sms',label:'SMS',icon:'▤'},{id:'email',label:'Email',icon:'✉'}]
const channelStats=ref({sms:{},email:{}}), channelStatsError=ref('')
const queueStats=ref({sms:{},email:{}}), queueStatsError=ref(''), queueSampledAt=ref(null)
const queueNumber=(ch,key)=>queueStats.value[ch]?.available?fmt(queueStats.value[ch][key]):'—'
const dashboardState=ref('connecting')
const page=ref('studio'), step=ref(1), brief=ref("Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun.")
const campaign=ref(null), content=ref({sms:'',email_subject:'',email_body:''}), busy=ref(false), error=ref(''), consent=ref(false), streamState=ref('connecting')
const config=ref({llm_mode:'mock',limits:{brief:4000,sms:160,email_subject:80,email_body:10000}}), campaigns=ref([]), stats=ref({}), health=ref(null), healthError=ref(''), lastRefresh=ref(null)
const errorItems=ref([]), auditCursor=ref(0), auditChannel=ref(''), auditKind=ref(''), auditScope=ref('all'), auditError=ref(''), auditBusy=ref(false), deliveryDetail=ref(null)
let auditEpoch=0, detailEpoch=0
const dateTime=value=>value?new Date(value).toLocaleString('tr-TR'):'—'
const kindLabels={transient:'Geçici hata',throttled:'Provider 429',permanent:'Kalıcı hata'}
async function refreshErrors(append=false){
 const epoch=++auditEpoch;auditBusy.value=true;auditError.value=''
 const params=new URLSearchParams();if(auditChannel.value)params.set('channel',auditChannel.value);if(auditKind.value)params.set('kind',auditKind.value)
 if(auditScope.value==='selected'&&campaign.value)params.set('campaign_id',campaign.value.id)
 if(append&&auditCursor.value)params.set('cursor',auditCursor.value)
 try{const data=await api('/api/errors?'+params);if(epoch!==auditEpoch)return;errorItems.value=append?[...errorItems.value,...data.items]:data.items;auditCursor.value=data.next_cursor}catch(e){if(epoch===auditEpoch)auditError.value=e.message}finally{if(epoch===auditEpoch)auditBusy.value=false}
}
async function inspectDelivery(id){
 const epoch=++detailEpoch;auditError.value='';try{const data=await api(deliveryHistoryPath(id));if(epoch===detailEpoch)deliveryDetail.value=data}catch(e){if(epoch===detailEpoch)auditError.value=e.message}
}
function closeDetail(){detailEpoch++;deliveryDetail.value=null}
watch([auditChannel,auditKind,auditScope],()=>refreshErrors())
let source, dashboardSource
const count=s=>Array.from(s||'').length
const budget=literalCount
const preview=s=>(s||'').replace(/\{\{\s*first_name\s*\}\}/g,'Deniz')
const fmt=n=>new Intl.NumberFormat('tr-TR').format(n||0)
const generating=computed(()=>campaign.value?.status==='generating')
const editable=computed(()=>campaign.value?.status==='draft')
const invalid=computed(()=>!content.value.sms.trim()||!content.value.email_subject.trim()||!content.value.email_body.trim()||budget(content.value.sms)>160||budget(content.value.email_subject)>80||budget(content.value.email_body)>10000||/\r|\n/.test(content.value.email_subject)||Object.values(content.value).some(v=>templateError(v)))
const statusNames={generating:'Hazırlanıyor',draft:'Taslak',approved:'Onaylandı',dispatching:'Gönderiliyor',completed:'Tamamlandı',failed:'Üretim hatası',needs_review:'İnceleme gerekiyor'}
const deliveryLabels={accepted:'Provider kabul etti',failed:'Kalıcı hata',pending:'Kuyrukta',processing:'İşleniyor',retrying:'Yeniden denenecek',suppressed:'İzin nedeniyle iptal',unknown:'Sonucu belirsiz · manuel inceleme'}
const campaignProgress=computed(()=>channelViews.map(channel=>{
 const counts=campaign.value?.channel_counts?.[channel.id]||{}
 const total=Object.values(counts).reduce((a,b)=>a+b,0)
 const finished=(counts.accepted||0)+(counts.failed||0)+(counts.suppressed||0)+(counts.unknown||0)
 return {...channel,counts,total,finished,percent:total?Math.min(100,Math.round(finished/total*100)):0}
}))
async function api(path, options={}){
 const r=await fetch(path,{...options,headers:{'Content-Type':'application/json',...options.headers}})
 let data;try{data=await r.json()}catch{throw new Error('Sunucudan geçerli yanıt alınamadı')}
 if(!r.ok)throw new Error(data.error||'İşlem tamamlanamadı');return data
}
function connect(id){
 source?.close();streamState.value='connecting';source=new EventSource(campaignPath(id,'/events'))
 source.onopen=()=>streamState.value='live';source.onerror=()=>streamState.value='reconnecting'
 source.addEventListener('generation.started',()=>{if(campaign.value?.id!==id)return;content.value={sms:'',email_subject:'',email_body:''}})
 source.addEventListener('content.delta',event=>{if(campaign.value?.id!==id||campaign.value?.status!=='generating')return;const d=JSON.parse(event.data);if(Object.hasOwn(content.value,d.field))content.value[d.field]+=d.delta})
 source.addEventListener('snapshot',event=>{const c=JSON.parse(event.data);if(c.id!==campaign.value?.id||c.version<campaign.value.version)return;const rank={generating:0,draft:1,approved:2,dispatching:3,completed:4,failed:4,needs_review:4};if(c.version===campaign.value.version&&rank[c.status]<rank[campaign.value.status])return;const wasGenerating=generating.value;campaign.value=c;if(wasGenerating&&c.status==='draft'){content.value={...c.proposal.content};step.value=2}if(c.status==='failed')error.value=c.error})
}
async function generate(){
 if(busy.value)return;busy.value=true;error.value='';consent.value=false
 try{const c=await api('/api/campaigns',{method:'POST',body:JSON.stringify({brief:brief.value})});content.value={sms:'',email_subject:'',email_body:''};campaign.value={...c,proposal:{},audience:{},counts:{}};step.value=2;localStorage.setItem('studio.campaign',c.id);connect(c.id)}catch(e){error.value=e.message}finally{busy.value=false}
}
async function save(){
 busy.value=true;error.value='';try{campaign.value=await api(campaignPath(campaign.value.id),{method:'PATCH',body:JSON.stringify({version:campaign.value.version,content:content.value})});step.value=3;consent.value=false}catch(e){error.value=e.message}finally{busy.value=false}
}
async function approve(){
 if(!consent.value||busy.value)return;busy.value=true;error.value=''
 try{await api(campaignPath(campaign.value.id,'/approve'),{method:'POST',body:JSON.stringify({version:campaign.value.version})});campaign.value=await api(campaignPath(campaign.value.id));page.value='monitor';await refresh()}catch(e){error.value=e.message}finally{busy.value=false}
}
async function load(id){
 busy.value=true;error.value='';try{const c=await api(campaignPath(id));campaign.value=c;brief.value=c.brief;content.value={...c.proposal.content};localStorage.setItem('studio.campaign',id);step.value=c.status==='generating'||c.status==='draft'?2:3;page.value=c.status==='draft'||c.status==='generating'?'studio':'monitor';connect(id)}catch(e){error.value=e.message}finally{busy.value=false}
}
function reset(){source?.close();campaign.value=null;step.value=1;page.value='studio';content.value={sms:'',email_subject:'',email_body:''};error.value='';consent.value=false;localStorage.removeItem('studio.campaign')}
function refresh(){
 dashboardSource?.close();dashboardState.value='connecting'
 const stream=new EventSource('/api/dashboard/events');dashboardSource=stream
 stream.onopen=()=>{if(dashboardSource===stream)dashboardState.value='live'}
 stream.onerror=()=>{if(dashboardSource===stream)dashboardState.value='reconnecting'}
 stream.addEventListener('dashboard',event=>{
  if(dashboardSource!==stream)return
  const data=JSON.parse(event.data)
  stats.value=data.stats;campaigns.value=data.campaigns;channelStats.value=data.channel_stats
  channelStatsError.value='';queueStats.value=data.queues;queueSampledAt.value=new Date()
  queueStatsError.value=Object.values(data.queues).some(q=>!q.available)?'Redis kuyruk bilgisi alınamadı; eksik değerler — ile gösteriliyor.':''
  health.value=data.health;healthError.value=data.health.postgres&&data.health.redis?'':'Bağımlılıklardan birine erişilemiyor.'
  lastRefresh.value=new Date();dashboardState.value='live'
  if(page.value==='monitor'&&!auditBusy.value&&errorItems.value.length<=50)refreshErrors()
 })
}
watch(page,()=>{error.value='';if(page.value==='monitor')refreshErrors()})
onMounted(async()=>{refresh();try{config.value=await api('/api/config')}catch(e){error.value=e.message}const id=localStorage.getItem('studio.campaign');if(isCampaignId(id))await load(id);else if(id)localStorage.removeItem('studio.campaign')})
onUnmounted(()=>{source?.close();dashboardSource?.close()})
</script>

<template>
 <div class="app-shell">
  <aside class="sidebar">
   <a class="brand" href="#" @click.prevent="page='studio'"><span class="brandmark">◖</span> INSIDER ONE</a>
   <div class="brand-sub">CAMPAIGN STUDIO</div>
   <nav aria-label="Ana menü">
    <button :class="{active:page==='studio'}" @click="page='studio'"><span class="navicon">◇</span><span>Kampanya <br>stüdyosu</span><small>01</small></button>
    <button :class="{active:page==='monitor'}" @click="page='monitor'"><span class="navicon">▤</span>Gönderim takibi</button>
    <button :class="{active:page==='health'}" @click="page='health'"><span class="navicon">⊙</span>Sistem sağlığı</button>
   </nav>
   <div class="sidebar-story"><div class="eyebrow">AI-POWERED<br>CAMPAIGNS</div><p>Daha akıllı kitleler.<br>Daha güçlü etkileşim.<br><em>Tek bir platform.</em></p></div>
   <div class="sidebar-footer"><i :class="{offline:!health}"></i> {{health?'Mock provider ortamı':'Bağlantı kontrol ediliyor'}}</div>
  </aside>
  <main>
   <header class="topbar"><div><span>PAZARLAMA</span><b class="slash">/</b><strong>{{page==='studio'?'Kampanya stüdyosu':page==='monitor'?'Gönderim takibi':'Sistem sağlığı'}}</strong></div><div class="top-right"><span class="env-badge"><i :class="{offline:!health}"></i>{{config.llm_mode==='mock'?'MOCK AI':'CLAUDE AI'}} · DENEME ORTAMI</span><span class="avatar">M</span></div></header>
   <div class="page-content">
    <section v-if="page==='studio'" class="hero">
     <div class="hero-copy"><div class="eyebrow">◆ AI-POWERED CUSTOMER ENGAGEMENT</div><h1>Doğru kitle.<br><em>Daha güçlü etkileşim.</em></h1><p>SMS ve email kampanyalarını tek bir yerden oluştur.<br>Yapay zekâ ile hazırla, son kararı sen ver.</p><div class="hero-note"><span></span> YAPAY ZEKÂ ÖNERİR. SEN KARAR VERİRSİN.</div></div>
     <div class="hero-art" aria-hidden="true"><div class="orbit orbit-1"></div><div class="orbit orbit-2"></div><span class="orbit-text">BİR FİKİR. ÇOK ETKİ.</span><span class="spark">✳</span><div class="floating-card email-card"><span class="art-icon">✉</span><div><small>EMAIL</small><b>Sepetinde bir sürpriz var.</b><p>Bir hatırlatma, yeni bir başlangıç.</p></div><span>✦</span></div><div class="floating-card sms-card"><span class="art-icon">▤</span><div><small>SMS</small><b>Merhaba Deniz, sepetin seni bekliyor!</b><p>Kısa. Samimi. Tam zamanında.</p></div></div><span class="sample-label">ÖRNEK İÇERİK</span></div>
    </section>
    <div v-if="page==='studio'" class="studio-toolbar"><button class="primary" :disabled="busy" @click="reset">Yeni kampanya ＋</button></div>
    <div v-if="error" class="error-banner" role="alert"><span>!</span>{{error}}<button aria-label="Uyarıyı kapat" @click="error=''">×</button></div>
    <div v-if="page==='studio'" class="studio-grid">
     <div>
      <div class="steps" aria-label="Kampanya adımları"><button v-for="(label,i) in ['Brief & kitle','İçeriği incele','Onayla & gönder']" :key="label" :class="{selected:step===i+1,done:step>i+1}" :disabled="generating||!editable||(i+1>step)" @click="step=i+1"><b>0{{i+1}}</b> {{label}}</button></div>
      <section v-if="step===1" class="panel compose-panel"><div class="panel-heading"><div><div class="eyebrow muted">YENİ KAMPANYA</div><h2>Kampanyanı oluştur</h2></div><span class="small-spark">✳</span></div><p class="intro">Kime ulaşmak istiyorsun, onlara ne söyleyelim?</p><label for="brief" class="field-label"><b>KAMPANYA BRİEFİ</b><span>Bir cümle yeter.</span></label><textarea id="brief" v-model="brief" class="brief-input" :maxlength="config.limits.brief" placeholder="Şehir, davranış, süre ve teklifini yaz…"></textarea><div class="field-foot"><span>✧ Şehir, davranış, süre ve ton ekleyebilirsin.</span><span>{{count(brief)}} / {{config.limits.brief}}</span></div><button class="primary wide" :disabled="busy||count(brief.trim())<10||count(brief)>4000" @click="generate">{{busy?'Hazırlanıyor…':'Kitle ve mesajları oluştur'}}<span>→</span></button><p class="fineprint">{{config.llm_mode==='mock'?'Demo AI: şehir, gün aralığı ve indirim oranını brief’ten okur. Ton ve serbest niyet analizi için Claude modunu kullan.':'Claude, brief’inden doğrulanmış bir segment ve içerik önerir.'}}</p></section>
      <section v-else-if="step===2" class="panel compose-panel"><div class="panel-heading"><div><div class="eyebrow muted">{{generating?'YAPAY ZEKÂ HAZIRLIYOR':'MESAJLARIN HAZIR'}}</div><h2>{{generating?'Fikrin kelimelere dönüşüyor…':'İçeriği senin dokunuşunla tamamla'}}</h2></div><span class="small-spark" :class="{spinning:generating}">✳</span></div><p class="intro">{{generating?'İçerik canlı olarak oluşturuluyor. Doğrulama tamamlanınca düzenleyebilirsin.':'İçeriği düzenle, ardından gönderimi onayla.'}}</p>
       <div v-if="!generating && campaign?.audience?.total!==undefined" class="audience-box"><b>{{campaign.proposal.segment.city}} · {{fmt(campaign.audience.total)}} kişi</b><span>Son {{campaign.proposal.segment.days}} gün · Satın alınmamış sepetler</span><span>SMS izni: {{fmt(campaign.audience.sms)}} · Email izni: {{fmt(campaign.audience.email)}}</span></div>
       <label for="sms" class="field-label"><span>SMS</span><span :class="{over:budget(content.sms)>160}">{{budget(content.sms)}} / 160</span></label><textarea id="sms" v-model="content.sms" :disabled="!editable" rows="4"></textarea><div class="preview-box">{{preview(content.sms)||'SMS içeriği burada görünecek…'}}</div>
       <label for="subject" class="field-label"><span>EMAIL KONUSU</span><span :class="{over:budget(content.email_subject)>80}">{{budget(content.email_subject)}} / 80</span></label><input id="subject" v-model="content.email_subject" :disabled="!editable"><label for="body" class="field-label"><span>EMAIL İÇERİĞİ</span><span :class="{over:budget(content.email_body)>10000}">{{budget(content.email_body)}} / 10000</span></label><textarea id="body" v-model="content.email_body" :disabled="!editable" rows="5"></textarea><p class="fineprint">Kişiselleştirme: <code v-pre>{{first_name}}</code> · Önizleme: Deniz<br>İsimler karakter bütçesine dahil edilmez. İçerik backend’de yeniden doğrulanır.</p><div class="actions"><button class="secondary" :disabled="generating||busy" @click="step=1">← Brief’e dön</button><button class="primary" :disabled="!editable||busy||invalid" @click="save">{{busy?'Doğrulanıyor…':'Kaydet & önizle'}} →</button></div><button v-if="campaign?.status==='failed'" class="secondary wide" @click="reset">Yeni kampanya oluştur</button>
      </section>
      <section v-else class="panel compose-panel"><div class="panel-heading"><div><div class="eyebrow muted">SON KONTROL · SÜRÜM {{campaign?.version}}</div><h2>Kampanyanı onayla</h2></div><span class="small-spark">✓</span></div><p class="intro">Kişiselleştirilmiş örnekleri ve alıcı sayılarını kontrol et. Gönderim yalnızca son onay düğmesiyle başlar.</p><div class="audience-box"><b>{{campaign?.proposal.segment.city}} · {{fmt(campaign?.audience.total)}} kişi</b><span>Son {{campaign?.proposal.segment.days}} gün · Satın alınmamış sepetler</span><span>SMS izni: {{fmt(campaign?.audience.sms)}} · Email izni: {{fmt(campaign?.audience.email)}}</span></div><h3>SMS · {{budget(content.sms)}} / 160 <small>İsim hariç</small></h3><div class="message-preview">{{preview(content.sms)}}</div><h3>Email</h3><div class="message-preview"><b>{{preview(content.email_subject)}}</b><p>{{preview(content.email_body)}}</p></div><p class="fineprint">Gönderim sırasında kanal izinleri yeniden kontrol edilir. Yalnızca sahte alıcılara, mock provider üzerinden gönderilir.</p><label v-if="editable" class="approval-check"><input type="checkbox" v-model="consent"> Kitleyi ve bu sürümdeki mesajları kontrol ettim; mock gönderimi onaylıyorum.</label><div class="actions"><button class="secondary" :disabled="busy||!editable" @click="step=2">← İçeriği düzenle</button><button class="primary" :disabled="!editable||!consent||busy" @click="approve">{{busy?'Kuyruğa alınıyor…':'Onayla & gönder'}} →</button></div></section>
     </div>
     <aside class="panel tips"><div class="eyebrow muted">KÜÇÜK BİR İPUCU</div><h2>Brief’ten<br>gönderime,<br><em>kontrol sende.</em></h2><p>Kampanyanı üç adımda hazırla.<br>Her aşamada kontrol sende.</p><div class="tip-row" v-for="(tip,i) in [['Brief & kitle','Brief’ini yaz, önerilen kitleyi kontrol et.'],['İçeriği incele','SMS ve email metinlerini önizle, gerekirse düzenle.'],['Onayla & gönder','Son kontrolünü yap, gönderimi onayla ve sonuçları izle.']]" :key="i"><span>0{{i+1}}</span><div><b>{{tip[0]}}</b><p>{{tip[1]}}</p></div></div><div class="channel-tips"><div class="eyebrow muted">İKİ KANAL, TEK HİKÂYE</div><div class="channel-tiles"><div>▤ &nbsp; SMS<small>160 karakter</small></div><div>✉ &nbsp; EMAIL<small>80 karakter konu</small></div></div><p class="fineprint"><i></i> İnsan onayı olmadan gönderilmez.</p></div></aside>
    </div>

    <template v-if="page==='monitor'">
     <div class="page-title"><div><div class="eyebrow">HER MESAJIN İZİNİ SÜR</div><h1>Gönderim takibi<span class="orange">.</span></h1><p>Kuyruktan provider’a, tüm süreç tek ekranda.</p></div></div>
     <section v-if="campaign && !['draft','generating','failed'].includes(campaign.status)" class="panel delivery-panel"><div class="panel-heading"><div><div class="eyebrow muted">SEÇİLİ KAMPANYA · {{campaign.id.slice(0,8)}}</div><h2>{{campaign.proposal.segment.city}} · {{statusNames[campaign.status]}}</h2></div><span class="live-badge"><i :class="{offline:streamState!=='live'}"></i>{{streamState==='live'?'CANLI SSE':'YENİDEN BAĞLANIYOR'}}</span></div><p class="fineprint">{{campaign.brief}}</p><div class="campaign-channel-progress" v-for="channel in campaignProgress" :key="channel.id" :data-channel="channel.id">
       <div class="progress-heading"><b>{{channel.label}} <span class="progress-count">{{fmt(channel.finished)}} / {{fmt(channel.total)}} işlendi</span></b><span>{{channel.total?'%'+channel.percent:'Alıcı yok'}}</span></div>
       <div class="progress-track" :class="'progress-'+channel.id" role="progressbar" :aria-label="channel.label+' işlenme ilerlemesi'" :aria-valuenow="channel.percent" aria-valuemin="0" aria-valuemax="100"><div :style="{width:channel.percent+'%'}"></div></div>
       <p class="fineprint progress-outcomes">{{fmt(channel.counts.accepted)}} provider kabulü · {{fmt(channel.counts.failed)}} kalıcı hata · {{fmt(channel.counts.suppressed)}} izin nedeniyle iptal<span v-if="channel.counts.unknown"> · {{fmt(channel.counts.unknown)}} manuel inceleme</span></p>
      </div></section>
     <p v-if="queueStatsError" class="audit-warning" role="alert">{{queueStatsError}}</p>
     <p v-if="stats.unknown" class="audit-warning" role="alert">{{fmt(stats.unknown)}} mesajın provider sonucu belirsiz. Otomatik retry durduruldu; hata geçmişinden inceleyin.</p>
     <p v-if="channelStatsError" class="audit-warning" role="alert">{{channelStatsError}}</p>
     <section v-for="channel in channelViews" :key="channel.id" class="channel-summary" :aria-label="channel.label+' gönderim özeti'">
      <div class="channel-summary-heading"><h2><span>{{channel.icon}}</span> {{channel.label}}</h2><span>Tüm kampanyalar · Kanal limiti 100 istek / saniye</span></div>
      <div class="stat-grid"><div class="panel stat" v-for="(item,i) in [['Provider kabul etti',channelStats[channel.id].accepted,'↗'],['Kuyruktaki işler',queueStats[channel.id]?.available?queueStats[channel.id].outstanding:null,'◷'],['Retry bekleyen işler',queueStats[channel.id]?.available?queueStats[channel.id].retry:null,'↻'],['Kalıcı hata',channelStats[channel.id].failed,'!']]" :key="i"><div>{{item[0]}}<span>{{item[2]}}</span></div><strong>{{item[1]===null?'—':fmt(item[1])}}</strong><small>{{i===0?'Alıcıya teslim edildi anlamına gelmez':i===1?'Hazır, aktif, zamanlanmış ve retry işleri':i===2?'Geçici hatalar ve rate limit beklemeleri':channel.label+' kanalı'}}</small></div></div>

     </section>


     <section class="panel error-panel">
      <div class="panel-heading"><div><div class="eyebrow muted">İNCELEME & DENEME GEÇMİŞİ</div><h2>Hatalar ve retry nedenleri</h2></div><button class="secondary" :disabled="auditBusy" @click="refreshErrors()">↻ Yenile</button></div>
      <p class="fineprint">Geçmişte hata alıp sonra başarılı olan mesajlar da burada görünür. Geçici bir hata, mesajın son durumunun başarısız olduğu anlamına gelmez.</p>
      <div class="audit-filters"><label>Kanal<select v-model="auditChannel"><option value="">Tüm kanallar</option><option value="sms">SMS</option><option value="email">Email</option></select></label><label>Hata sınıfı<select v-model="auditKind"><option value="">Tüm hatalar</option><option value="transient">Geçici hata</option><option value="throttled">Provider 429</option><option value="permanent">Kalıcı hata</option></select></label><label>Kampanya<select v-model="auditScope"><option value="all">Tüm kampanyalar</option><option v-if="campaign" value="selected">Seçili kampanya</option></select></label></div>
      <p v-if="auditError" class="audit-warning" role="alert">{{auditError}}</p>
      <p v-if="!errorItems.length" class="fineprint">{{auditBusy?'Hata kayıtları yükleniyor…':'Bu filtrelerle eşleşen hata kaydı yok.'}}</p>
      <div class="audit-rows"><button v-for="a in errorItems" :key="a.id" class="audit-row" @click="inspectDelivery(a.delivery_id)"><div class="audit-row-top"><b>{{a.channel.toUpperCase()}} · {{kindLabels[a.kind]||a.kind}}</b><span class="status-badge" :class="a.state==='accepted'?'completed':a.state==='failed'?'failed':'draft'">{{deliveryLabels[a.state]||a.state}}</span></div><p>{{a.message}}</p><div class="audit-meta"><span>{{dateTime(a.created_at)}}</span><span>{{a.attempt_no?'Deneme '+a.attempt_no:'Önceki sürüm kaydı'}} · {{a.http_status?'HTTP '+a.http_status:a.code}}</span><span>İncele ↗</span></div></button></div>
      <button v-if="auditCursor" class="secondary wide" :disabled="auditBusy" @click="refreshErrors(true)">Daha eski kayıtları yükle</button>
     </section>
     <section class="panel campaigns-panel"><div class="list-heading"><span class="eyebrow muted">KAMPANYALARIN</span><span>{{campaigns.length}} kayıt</span></div><div v-if="!campaigns.length" class="empty"><span>◇</span><h3>İlk kampanyanı oluştur</h3><p>Brief’ini yaz, kitleyi keşfet, son kararı sen ver.</p><button class="secondary" @click="reset">Kampanya stüdyosuna git →</button></div><button v-for="c in campaigns" :key="c.id" class="campaign-row" @click="load(c.id)"><div><b>{{c.proposal?.segment?.city||'Kitle hazırlanıyor'}} <small>· {{c.id.slice(0,8)}}</small></b><p>{{c.brief}}</p></div><span class="status-badge" :class="c.status">{{statusNames[c.status]}}</span><span class="row-arrow">↗</span></button></section>
    </template>

    <template v-if="page==='health'"><div class="page-title"><div><div class="eyebrow">HER ŞEY KONTROL ALTINDA</div><h1>Sistem sağlığı<span class="orange">.</span></h1><p>Bağımlılıklar, kanal kuralları ve çalışma ortamı.</p></div><button class="secondary" @click="refresh">↻ Yenile</button></div><div v-if="healthError" class="error-banner" role="alert">{{healthError}}</div><div class="health-grid"><section class="panel health-card"><div class="health-icon">⊙</div><h2>Servis durumu</h2><div v-for="(label,key) in {postgres:'PostgreSQL',redis:'Redis / Asynq'}" :key="key" class="health-row"><span>{{label}}</span><span class="status-badge" :class="health?.[key]?'completed':'failed'">{{health?.[key]?'Bağlantı sağlıklı':'Erişilemiyor'}}</span></div><div class="health-row"><span>Yapay zekâ</span><b>{{config.llm_mode==='mock'?'Mock LLM':'Claude API'}}</b></div><div class="health-row"><span>Sahte kullanıcı</span><b>{{fmt(stats.users)}}</b></div></section><section class="panel health-card"><div class="health-icon">⇄</div><h2>Gönderim politikası</h2><div class="health-row"><span>SMS</span><b>100 istek / saniye</b></div><div class="health-row"><span>Email</span><b>100 istek / saniye</b></div><div class="health-row"><span>Tekrarlama koruması</span><b>Provider idempotency</b></div><div class="health-row"><span>Ortam</span><span class="status-badge draft">Yalnızca mock gönderim</span></div></section></div><section class="panel health-card"><h2>Gözlemlenebilirlik</h2><p>Endpoint’ler canlı servis verisini gösterir. Provider kabul sayısı, gerçek teslimat metriği değildir.</p><div class="endpoint-links"><a href="/health/live" target="_blank">GET /health/live ↗</a><a href="/health/ready" target="_blank">GET /health/ready ↗</a><a href="/metrics" target="_blank">GET /metrics ↗</a></div><p class="fineprint">Son kontrol: {{lastRefresh?.toLocaleTimeString('tr-TR')}}</p></section></template>
    <div v-if="deliveryDetail" class="audit-overlay" @click.self="closeDetail"><section class="panel audit-detail" role="dialog" aria-modal="true" aria-labelledby="audit-title"><div class="panel-heading"><div><div class="eyebrow muted">MESAJ DENEMELERİ</div><h2 id="audit-title">{{deliveryDetail.channel.toUpperCase()}} · {{deliveryLabels[deliveryDetail.state]}}</h2></div><button class="secondary" aria-label="Deneme geçmişini kapat" @click="closeDetail">×</button></div><p class="fineprint">Aynı idempotency key bütün retry ve recovery işlemlerinde korunur.</p><dl class="audit-identifiers"><dt>Idempotency key / delivery ID</dt><dd>{{deliveryDetail.idempotency_key}}</dd><dt>Payload SHA-256</dt><dd>{{deliveryDetail.payload_sha256||'Henüz render edilmedi'}}</dd><dt>Provider kabul kimliği</dt><dd>{{deliveryDetail.provider_id||'Henüz kabul kaydı yok'}}</dd><dt>Alıcı ID</dt><dd>{{deliveryDetail.user_id}}</dd><dt>Son doğrulanan SMS gönderimi</dt><dd>{{dateTime(deliveryDetail.last_sms_sent_at)}}</dd><dt>Son doğrulanan email gönderimi</dt><dd>{{dateTime(deliveryDetail.last_email_sent_at)}}</dd><dt>Hata bütçesi</dt><dd>{{deliveryDetail.failure_count}} / 9 · 429 bu bütçeyi tüketmez</dd><dt>Sonraki deneme</dt><dd>{{dateTime(deliveryDetail.next_attempt_at)}}</dd></dl><button class="secondary" @click="inspectDelivery(deliveryDetail.delivery_id)">↻ Güncel durumu al</button><article v-for="a in deliveryDetail.history" :key="a.id" class="attempt-card"><div class="audit-row-top"><b>{{a.attempt_no?'Deneme '+a.attempt_no:'İşlem kaydı'}} · {{a.outcome==='accepted'?'Kabul doğrulandı':a.outcome==='unknown'?'Sonucu belirsiz':a.outcome==='inflight'?'İşleniyor':kindLabels[a.kind]||a.outcome}}</b><span>{{a.http_status?'HTTP '+a.http_status:''}}</span></div><p>{{a.message}}</p><dl><dt>Kod</dt><dd>{{a.code||'—'}}</dd><dt>Zaman</dt><dd>{{dateTime(a.created_at)}}</dd><dt>Yeniden deneme</dt><dd>{{dateTime(a.retry_at)}}</dd><dt>Request ID</dt><dd>{{a.request_id||'Provider döndürmedi'}}</dd></dl></article><p class="fineprint">En son 100 işlem gösterilir. API kimlik ve hash döndürür; alıcı adresi ve mesaj içeriği bu görünümde bulunmaz.</p></section></div>
    <footer class="main-footer"><span>INSIDER ONE <b>CAMPAIGN STUDIO</b></span><span>Bir fikir. Çok etki. <span class="orange">✳</span></span></footer>
   </div>
  </main>
 </div>
</template>
