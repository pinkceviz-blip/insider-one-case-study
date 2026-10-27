# Teknik kararlar ve nedenleri

Bu belge, projenin neden bu şekilde tasarlandığını mülakatta anlatabilmek için hazırlanmıştır. Mevcut uygulamayla, daha büyük bir sistem için önerilen geliştirmeler ayrı belirtilmiştir.

## Projeyi bir dakikada nasıl anlatırım?

> Pazarlamacı bir kampanya brief’i yazıyor. Go API bu isteği kaydedip arka plandaki üretim kuyruğuna bırakıyor. Generation worker, mock LLM veya Claude’dan yapılandırılmış bir öneri alıyor. Backend öneriyi doğruluyor, kullanıcı kitlesini belirliyor ve taslak oluşturuyor. İnsan içeriği inceleyip onayladıktan sonra SMS ve email işleri ayrı kuyruklara gidiyor. Worker’lar kanal iznini kontrol edip mock provider’a gönderiyor. Sonuçlar PostgreSQL’de saklanıyor, arayüz SSE üzerinden güncelleniyor.
>
> Temel tercihim, bir işi tekrar çalıştırmanın güvenli olmasını sağlamak. Kuyruğun bir işi yalnızca bir kere çalıştıracağını varsaymıyorum.

Görsel anlatım için: [Servis diyagramı](docs/architecture.mmd) ve [kampanya akışı](docs/campaign-flow.mmd).

## 1. Neden tek Go kod tabanı, birden fazla servis rolü?

API, dispatcher, generation worker, SMS worker ve email worker aynı Go kodunu farklı rollerle çalıştırıyor. Her rol Docker Compose içinde ayrı bir süreç.

Böylece SMS yükü artarsa SMS worker sayısını artırabilirim. Bunun için ayrı mikroservis repoları, servis keşfi veya Kubernetes kurmam gerekmiyor. Case için işletmesi ve test etmesi daha kolay bir yapı elde ediyorum.

Concurrency’yi Asynq yönetiyor; işler Go goroutine’leriyle eşzamanlı çalışıyor. Handler içinde ayrıca kontrolsüz goroutine başlatmıyorum. Handler, provider çağrısını ve sonucunun kaydedilmesini bekliyor.

**Sınırı:** Aynı veritabanını paylaşan bu yapı, birbirinden tamamen bağımsız mikroservisler değildir.

## 2. Neden PostgreSQL ve Redis birlikte kullanılıyor?

PostgreSQL kalıcı iş kayıtlarını tutuyor: kullanıcılar, izinler, kampanyalar, alıcı listeleri, gönderimler ve deneme geçmişi. Redis ise Asynq kuyruklarını ve ortak hız sınırını tutuyor.

Bir mesajın kabul edilip edilmediğine karar verirken yalnızca Redis kuyruğuna bakmıyorum. Asıl kayıt PostgreSQL’deki delivery durumudur.

**Mülakatta:** “Redis iş dağıtımı için, PostgreSQL ise işin kalıcı durumunu takip etmek için kullanılıyor.”

## 3. Neden transactional outbox var?

Bir kampanyayı veritabanına kaydettikten hemen sonra uygulama çökerse, Redis’e gönderilmemiş bir iş kalabilir. Sadece önce DB’ye, sonra Redis’e yazmak bu boşluğu kapatmaz.

Bu nedenle kampanya veya delivery kaydıyla birlikte bir **outbox**, yani “kuyruğa aktarılacak işler listesi” kaydı oluşturuyorum. İkisi aynı PostgreSQL transaction’ında kaydediliyor: ya ikisi birlikte kalıyor ya da ikisi de geri alınıyor.

Dispatcher bu listeyi okuyup Redis’e yayınlıyor. Birden fazla dispatcher aynı kayıtları almamak için `FOR UPDATE SKIP LOCKED` kullanıyor; kilitli kayıtları atlayıp başka kayıtlarla ilerleyebiliyor.

**Sınırı:** Redis’e yazdıktan sonra outbox kaydını güncellemeden çökme hâlâ mümkün. Bu yüzden tekrar yayınlama güvenli olmak zorunda. Outbox tek başına duplicate mesajı engellemez.

## 4. Aynı mesajın iki kez gitmesini nasıl önlüyorum?

Tek bir korumaya güvenmiyorum:

| Katman | Görevi |
|---|---|
| PostgreSQL unique constraint | Aynı kampanya sürümü, kullanıcı ve kanal için ikinci delivery oluşmasını engeller. |
| Sabit Asynq task ID | Aynı işi yeniden kuyruğa eklerken tekrarları azaltır. Tek başına gönderim garantisi değildir. |
| Delivery başına PostgreSQL kilidi | İki worker’ın aynı gönderimi aynı anda işlemesini engeller. |
| Fencing token | Sonuç yazan worker’ın hâlâ geçerli işlem sahibi olduğunu kontrol eder. |
| Provider idempotency key | Tekrar dış çağrı yapılsa bile provider’ın aynı mesajı ikinci kez kabul etmesini önler. |

Provider’a gönderilen idempotency key, delivery ID’dir. Retry boyunca anahtar ve kaydedilmiş mesaj içeriği değişmez.

**Örnek:** Provider mesajı kabul etti ama yanıt ulaşmadan timeout oldu. Worker bunu doğrudan başarısız kabul edip yeni mesaj üretmez. Destekleniyorsa önce receipt, yani provider kabul kaydı sorgulanır. Kabul doğrulanırsa DB sonucu tamamlanır.

**Önemli sınır:** Kuyruk işlemesi “en az bir kez” yaklaşımındadır; aynı job tekrar çalışabilir. Sistemin tamamı için koşulsuz “exactly once” garantisi vermiyorum. Dış yan etkinin tekrarsız olması provider’ın idempotency sözleşmesine de bağlıdır.

### Provider bu garantiyi vermiyorsa?

Adaptör `SupportsIdempotency()` ile bu kabiliyeti açıkça belirtir. Garanti yoksa belirsiz hata veya yarım kalmış deneme `unknown` durumuna alınır. Otomatik gönderim durur, kampanya `needs_review` olarak görünür.

Bu tercih duplicate riskini azaltır; mesajın mutlaka gönderileceğini garanti etmez. Manuel provider doğrulaması gerekir. Uygulamada bu kayıtları körlemesine yeniden gönderen bir endpoint yoktur. Receipt sorgusunda “bulunamadı” dönmesi de tek başına güvenli resend izni sayılmaz.

## 5. Retry ile rate limit arasındaki fark ne?

**Concurrency**, aynı anda kaç işin çalışabileceğidir. **Rate limit**, bir saniyede en fazla kaç provider isteği yapılabileceğidir. Worker concurrency’sini 32 yapmak, saniyede 32 veya 100 gönderim garantisi vermez.

SMS ve email ayrı kuyruklardır. Her kanalın bütün worker instance’ları aynı Redis limiter’ını kullanır. Hedef, kanal başına provider’a saniyede en fazla 100 istek göndermektir. Güncel kodda atomik Lua işlemi 1,2 saniyelik kabul penceresinde en fazla 100 işe izin verir. Ek 200 ms, limiter kontrolü ile provider çağrısı arasındaki DB işlemleri için bırakılan güvenlik payıdır. SMS ve email limitleri birbirinden bağımsızdır.

Redis erişilemiyorsa limiter kontrolünü atlayıp mesaj göndermiyorum; işi erteliyorum. Hız sınırı nedeniyle bekleyen iş, arayüzde provider hatası olarak değil kuyrukta bekleyen iş olarak gösterilir.

| Durum | Uygulamanın yaklaşımı |
|---|---|
| İdempotency garantili provider’da geçici hata / timeout | Artan bekleme ve rastgele küçük gecikmeyle retry; gerektiğinde önce receipt kontrolü. |
| Provider 429 | `Retry-After` dikkate alınır, aynı kanalın diğer worker’larına da ortak bekleme uygulanır. |
| Kalıcı ret | Delivery `failed` olur; yeniden gönderilmez. |
| Kanal izni yok | Delivery `suppressed` olur; provider çağrılmaz. |
| İdempotency garantisi olmayan provider’da belirsiz sonuç | `unknown`; otomatik retry durur. |

Artan bekleme **backoff**, worker’ların aynı anda tekrar yüklenmesini azaltan rastgele gecikme **jitter** olarak adlandırılır. Retry zamanı ve hata sayacı PostgreSQL’de tutulur; yeni bir recovery job’ı bunları atlayamaz. Kalıcı sayaçta dokuz geçici hata sınırı vardır; 429 ve normal hız sınırı beklemeleri bu bütçeyi tüketmez.

## 6. LLM çıktısına neden doğrudan güvenmiyorum?

LLM yalnızca öneri üretir. SQL üretip çalıştırma yetkisi yoktur. Claude çağrısını tarayıcı değil generation worker yapar; API anahtarı backend’de kalır.

Kontroller üç aşamalıdır:

1. JSON doğru mu? Bilinmeyen alanlar ve yanlış veri tipleri reddedilir.
2. Segment ve içerik desteklenen kurallara uyuyor mu? Şehir, gün aralığı, şablon değişkenleri ve karakter limitleri kontrol edilir.
3. Brief’te açıkça yazılmış şehir ve sayısal gün aralığı, öneriyle aynı mı? İstanbul istenirken Ankara dönmesi üretimde ve son onayda engellenir.

Kitle, backend’in sabit ve parametreli SQL sorgusuyla seçilir. LLM metni SQL olarak birleştirilmez.

Üretim en fazla üç task denemesiyle sınırlandırılmıştır. Özel bir “bozuk JSON’u düzelt” prompt döngüsü yoktur; üretim yeniden denenir. Testlerde API key gerektirmeyen mock LLM kullanılır.

**Sınırı:** Şehir/gün karşılaştırması genel bir doğal dil anlama garantisi değildir. Çok şehirli veya çelişen gün aralıkları desteklenen tek segment modeline sessizce indirgenmez. Mock LLM de genel amaçlı ton ve niyet analizi yapmaz.

## 7. İnsan onayında ve kanal izinlerinde tutarlılığı nasıl sağlıyorum?

Taslak düzenlendiğinde `version` artırılır. Kullanıcı onay verirken gördüğü sürümü gönderir. Başka bir değişiklik yapılmışsa eski sürüm onaylanmaz. Onay sırasında kampanya satırı kilitlenir; çift onay ikinci delivery seti oluşturmaz. Onay sonrası düzenleme kapalıdır.

Alıcı listesi, doğrulanmış öneri hazırlanırken sabitlenir. Sonradan sorgunun farklı kullanıcıları seçmesi önlenir. Göreli tarihler kampanyanın oluşturulma zamanına göre hesaplanır.

Ancak kanal izni sabitlenmez. `consents` tablosunda SMS ve email izinleri ayrıdır; hem onayda hem gönderimden hemen önce yeniden kontrol edilir. Kullanıcı email’e izin verip SMS’i reddedebilir. Mevcut arayüzde bu tercihleri değiştiren bir kullanıcı ayar ekranı/API’si yoktur; başlangıç izinleri seed’den gelir.

Gönderim sırasında izin satırı `FOR SHARE` ile kilitli tutulur. İzin iptali, başlamış yetkili gönderimin tamamlanmasını bekler. İptal commit olduktan sonraki işler yeni izni görür. Zaten provider’a başlamış bir isteği geri çekebileceğimi iddia etmiyorum.

## 8. Kişiselleştirme ve gönderim tarihi nasıl ele alınıyor?

İçerik `{{first_name}}` ile kişiselleştirilir. Talep edilen özel kurala göre SMS’te 160, email konusunda 80 **isim hariç metin karakteri** sınırı uygulanır. Render sırasında da doğrulama yapılır. İsim için ayrıca 100 karakter sınırı vardır; geçersiz kontrol karakterleri reddedilir, eksik isimde “arkadaşım” kullanılır.

Bu nedenle isim eklendikten sonra SMS’in toplamı 160’ı aşabilir. Bu bilinçli bir case tercihidir. Gerçek SMS encoding ve parça maliyeti hesabı uygulanmadı.

Provider kabulü doğrulanınca delivery sonucu ile `last_sms_sent_at` veya `last_email_sent_at` aynı transaction’da yazılır. Hata, timeout veya rollback tarih yazmaz; tamamlanmış job’ı tekrar çalıştırmak tarihi ilerletmez.

**Provider kabulü, gerçek alıcıya teslim edildiği anlamına gelmez.** Bu alanlar delivered-at bilgisi değildir.

## 9. Canlı ekran için neden SSE?

İhtiyaç sunucudan tarayıcıya durum aktarmak; çift yönlü sürekli iletişim gerekmiyor. Bu yüzden WebSocket yerine SSE kullandım.

- Kampanya içeriği ve üretim event’leri PostgreSQL’de saklanır. Yeniden bağlantıda event ID üzerinden devam edilebilir.
- Dashboard bağlantısı her açıldığında güncel tam snapshot alır.
- SMS ve email ilerlemesi seçili kampanyanın ayrı kanal sayaçlarından hesaplanır.

Tarayıcıda periyodik REST polling yoktur. Sunucu DB/Asynq durumunu 500 ms aralıkla ölçüp değişen snapshot’ı SSE üzerinden gönderir.

**Sınırı:** Bu, Asynq’nun native event aboneliği değildir. Çok sayıda dashboard bağlantısında aynı DB hesapları tekrar yapılır. Daha büyük ölçekte ortak sayaç/snapshot üretimi ve bağlantılara paylaşımlı yayın gerekir.

## 10. Kampanyayı neden 24 saat sonra otomatik bitirmiyorum?

100 istek/saniye sınırında 10 milyon mesajın tek kanal için ideal minimum süresi 27 saat 46 dakika 40 saniyedir. Sabit 24 saatlik kesme, hata olmasa bile bazı mesajları iptal ederdi.

Bu yüzden varsayılan `CAMPAIGN_TTL=0s`: otomatik süre aşımı kapalıdır. İşletme gereksinimi varsa `72h` gibi açık bir süre verilebilir. Retry ve diğer kampanyalar tamamlanma süresini uzatır; süresiz kuyruk tek başına kapasite garantisi değildir.

Asynq’nun tamamlanan işleri 24 saat tutması ayrı bir konudur; kampanyanın gönderim süresiyle aynı değildir.

## 11. Neler test edildi, neler henüz garanti değil?

Otomatik testlerde bozuk LLM çıktısı, yanlış segment, çift onay, farklı job’larla aynı delivery, kanal izni yarışı, provider kabulü sonrası timeout, gerçek süreç öldürme sonrası receipt kurtarma, çoklu worker hız sınırı, outbox hatası ve SSE yeniden bağlantısı kontrol edildi. Go race detector da çalıştırıldı. Son arayüz değişikliğinde bağımsız SMS/email ilerlemesi test edildi.

Gerçek süreç öldürme testi, provider kabulünden sonraki delivery handler sürecini kapsar; bütün crash noktaları ve Asynq’nun native stalled-job kurtarması tamamlanmış testler gibi sunulmaz. Gerçek Claude veya gerçek SMS/email teslimatı bu testlerin kapsamı değildir.

Kontrollü kapasite denemesi 50 bin kullanıcıya kadar yapıldı. **1 veya 10 milyon kullanıcıyla gerçek yük testi yapılmadı.** Milyonluk süreler kapasite hesabıdır.

İlgili sonuçlar: [Senaryo denetimi](reports/case-study-audit-2026-10-27.md), [güvenilirlik düzeltmeleri](reports/reliability-fixes-2026-10-27.md), [kanal ilerleme testi](reports/channel-progress-2026-10-27.md).

## 12. Production’a geçerken önce neyi geliştiririm?

| Mevcut sınır | Sonraki adım ve nedeni |
|---|---|
| Onay, bütün delivery/outbox kayıtlarını HTTP isteği içinde tek transaction’da oluşturuyor. | Büyük kampanyaları parçalara bölüp ilerleme noktası kaydeden hazırlık job’ları: uzun transaction ve request süresini azaltmak için. |
| Kuyruk derinliğine göre iş üretimini frenleyen mekanizma yok. | Backpressure: kuyruk doldukça üretimi yavaşlatmak, DB/Redis belleğinin kontrolsüz büyümesini önlemek için. |
| Redis kaybında eski nonterminal delivery’ler DB’den yeniden oluşturulabilir; generation işleri için aynı otomatik recovery yok. | Generation recovery eklemek. Redis AOF ve noeviction tek başına kayıpsızlık garantisi değildir. |
| Readiness yalnız PostgreSQL/Redis bağlantısını kontrol ediyor. | Worker heartbeat, en eski işin yaşı, provider gecikmesi ve outbox bekleme alarmı: sistemin iş üretip tükettiğini görmek için. |
| Kimlik doğrulama ve tenant izolasyonu yok; portlar localhost’a bağlı. | İnternete açmadan önce auth, rol kontrolü, tenant sınırları ve secret yönetimi. |
| Kanallar interface/registry ile ayrılmış olsa da alıcı adresi ve bazı modeller SMS/email’e özel. | Yeni kanal eklerken bu alanları da genelleştirmek; “sadece bir interface eklemek yeterli” iddiasında bulunmamak. |

Mülakatta temel ayrımı şöyle anlatabilirim: **“Case’i çalışır ve test edilebilir tuttum. Milyonluk sistem için gerekli geliştirmeleri belirledim; henüz uygulamadığım özellikleri varmış gibi sunmadım.”**
