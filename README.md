# Insider One · Campaign Studio

Vue 3 + Go + PostgreSQL + Redis/Asynq ile insan onaylı SMS/email kampanyaları. Arayüz, verilen ekran görüntülerinden yeniden oluşturulmuştur; hazır frontend kaynak kodu sağlanmamıştır.

## Başlatma

Gereksinim: Docker Engine/Desktop ve Docker Compose v2. Host üzerinde Go veya Node kurulması gerekmez.

```sh
cp .env.example .env  # isteğe bağlı; varsayılan mod mock
docker compose up --build -d
```

**Arayüz:** http://localhost:18080 · **Go API:** http://localhost:18081

İlk kurulumda şema ve 5.000 sahte kullanıcı otomatik hazırlanır. Redis/PostgreSQL dışarıya açılmaz; HTTP portları yalnızca loopback'e bağlıdır. Veriler named volume'larda kalır. Gönderim, mock provider'ın PostgreSQL'e yazdığı bir kayıt oluşturur; gerçek SMS veya email servisi çağrılmaz. Adresler `example.invalid` alan adını kullanır.

1. Brief'i yaz; canlı üretilen içeriği ve kitleyi incele.
2. SMS/email metnini düzenle; backend doğrulamasından geçir.
3. Önizlemeyi kontrol et ve onay kutusunu işaretle.
4. “Onayla & gönder” ile kuyruğa aktar. Gönderim takibinde SSE üzerinden ilerlemeyi izle.

Mock AI yalnızca şehir, gün ve indirim yüzdesi çıkarır. Genel amaçlı doğal dil yorumlama ve ton analizi yapmaz; UI bunu belirtir. Desteklenen segment: İstanbul/Ankara/İzmir, son 1–365 gün, satın alınmamış sepet. Case veri modeli kullanıcı başına tek güncel sepet özeti tutar; tüm sipariş/sepet olaylarını modellemez.

## Claude

`.env`:

```dotenv
LLM_MODE=claude
ANTHROPIC_API_KEY=your-key
ANTHROPIC_MODEL=claude-sonnet-4-5
```

```sh
docker compose up -d --force-recreate api generation-worker
```

Model, hesabında structured outputs desteği olan bir model olmalı. Anahtar tarayıcıya veya loglara yazılmaz. Claude structured JSON çıktısı stream edilir; tamamlanınca backend şema, segment ve içerik kurallarını yeniden doğrular. `LLM_MODE=mock` ile hiçbir API key gerekmez. Bu teslimde gerçek Claude çağrısı yapılmamıştır.

## Test

```sh
make test
```

Docker içinde `go test -race -count=1 -v ./...` çalışır. PostgreSQL'de rastgele, izole bir test şeması ve **Redis DB 15** kullanılır; test başında/sonunda yalnızca bu DB temizlenir. Uygulama DB 0 kullanır. Testler gerçek API key istemez.

Kapsam: yapılandırılmış çıktı reddi, SQL benzeri segment girdisi, isim hariç Unicode karakter bütçesi, bilinmeyen değişkenler, email header enjeksiyonu, mock LLM streaming, retry sınıfları; gerçek PostgreSQL/Redis ile paylaşılan limiter, kampanya oluşturma/onay, onay öncesi gönderim engeli, sürüm çakışması, çift onay, iptal edilmiş izin, task replay, SSE, accepted-then-timeout provider idempotency.

```sh
docker compose logs -f --tail=80
docker compose down        # veriyi korur
```

Verileri sıfırlamak istersen `docker compose down -v` named volume'ları siler.

## Mimari

[Servis mimarisi ve kampanya akışı diyagramları](ARCHITECTURE.md)

- `api`: REST, input validation, sürüm kontrollü edit/onay ve SSE.
- `generation-worker`: `campaign` kuyruğunda LLM adapter'ı; son JSON doğrulaması ve kitle snapshot'ı.
- `dispatcher`: PostgreSQL transactional outbox → Asynq; `SKIP LOCKED` ile birden fazla instance çalışabilir.
- `sms-worker`, `email-worker`: ayrı kuyruklar, process başına 32 concurrent task; Asynq goroutine'leri yönetir.
- `provider`: kalıcı idempotency destekli HTTP mock. Deterministik olarak 422, ilk denemede 503/429 veya kabulden sonra istemci timeout'undan uzun gecikme üretir.
- Redis: Asynq ve kanal başına tüm replica'ları kapsayan sliding-window limiter (100 deneme/s). 429 ortak cooldown uygular.
- PostgreSQL: kullanıcı/izinler, kampanya sürümü, alıcı snapshot'ı, delivery, deneme, outbox, event ve provider receipt kaydı.

İçerik onay sonrası değiştirilemez. Onay ve delivery/outbox kayıtları aynı transaction içindedir. Dispatcher yeniden enqueue edebilir; DB uniqueness + bağlantıya bağlı PostgreSQL advisory lock + fencing token + sabit provider idempotency key tekrar gönderimi engeller. Worker son render edilmiş payload'ı saklar; retry boyunca aynı kalır. İzin gönderimden önce `FOR SHARE` ile kilit altında tekrar okunur; devam eden gönderimler tamamlanmadan izin iptali commit olamaz.

Redis kaybında, dispatcher her dakika 10 dakikadan uzun süredir ilerlemeyen delivery'leri PostgreSQL'den yeniden kuyruğa alır. Sadece Asynq arşivine güvenilmez. Kampanya üretim işinin Redis kaybından kurtarılması için mevcut sürümde kampanyayı yeniden oluşturmak gerekir; bu sınırlama DECISIONS.md'de açıklanır.

SMS 160, email konusu 80 **literal Unicode code point** ile sınırlıdır. `{{first_name}}` placeholder'ı ve değeri bütçeye dahil değildir (istenen özel kural). İsim en fazla 100 code point; eksik isim `arkadaşım`. Email gövdesi düz metindir, 10.000 literal karakter. Gerçek SMS segment/encoding hesabı yapılmaz.

SSE, PostgreSQL `events` tablosundaki ID'lerle replay yapar; tarayıcı `Last-Event-ID` ile devam edebilir. Canlı delivery sayaçları snapshot olarak gelir. Dashboard global özeti `/api/dashboard/events` üzerinden SSE ile akar; tarayıcıda periyodik REST yenilemesi yoktur. Her yeniden bağlantıda tam güncel snapshot gönderilir. `accepted`, mock provider'ın isteği kabul etmesidir; gerçek alıcı teslimatı anlamına gelmez.

## API

| Endpoint | İşlev |
|---|---|
| `GET /api/config` | LLM modu, kanallar ve input limitleri |
| `POST /api/campaigns` | `{brief}` ile üretimi başlat (202) |
| `GET /api/campaigns` | En son 100 kampanya |
| `GET /api/campaigns/{id}` | İçerik, kitle ve delivery sayaçları |
| `PATCH /api/campaigns/{id}` | `{version, content:{sms,email_subject,email_body}}` |
| `POST /api/campaigns/{id}/approve` | `{version}` ile açık onay (202) |
| `GET /api/campaigns/{id}/events` | SSE: content.delta, draft.ready, snapshot |
| `GET /api/stats` | Global sayaçlar |
| `GET /health/live` | HTTP process canlılığı |
| `GET /health/ready` | PostgreSQL ve Redis bağlantıları |
| `GET /metrics` | Prometheus biçiminde business gauge'lar |

## Kanal genişletme

`Channel` ve `Provider` arayüzleri `backend/internal/studio/domain.go` içinde. Yeni kanalın validate/render adapter'ını ve provider adapter'ını registry'ye ekle, kanal queue/worker konfigürasyonunu Compose'a ekle. Rate limiter/retry/outbox/lease handler'ı ortaktır. Yeni kanalın recipient adres çözümü ve LLM/content DTO'su, kendine özgü form/önizlemesi ayrıca eklenir; bu case'te yalnız SMS/email UI'si vardır. Tamamen metadata tabanlı içerik/form sistemi yapılmamıştır.

## Kapsam

Bu uygulama lokal case/demodur; milyonlarca alıcılık production deployment iddiası taşımaz. Auth/RBAC, tenant izolasyonu, gerçek provider webhook'ları, HA, üretim migration yönetimi, tracing ve büyük kitlelerde batch fan-out tamamlanmadan internete açılmamalıdır. Production adımları ve trade-off'lar DECISIONS.md'de.

## Retry güvenliği ve hata inceleme

Gönderim takibi ekranındaki **Hatalar ve retry nedenleri** bölümünde kanal, hata sınıfı ve kampanya filtresi bulunur. Bir kayda tıklayınca delivery'nin bütün denemeleri, HTTP kodu, güvenli hata açıklaması, request ID, DB retry zamanı, idempotency key ve sabit payload SHA-256 bilgisi açılır. Başarıyla toparlanmış mesajların eski hataları silinmez. Eski sürüm kayıtlarında attempt numarası/request ID bulunmayabilir; UI bunları ayrıca belirtir. API alıcı adresini veya ham provider yanıtını bu görünümde döndürmez.

- `GET /api/errors?campaign_id=...&channel=sms&kind=transient&cursor=...`: 50 kayıtlık cursor pagination.
- `GET /api/deliveries/{id}/attempts`: Son 100 deneme, son durum ve kullanıcıdaki gönderim tarihleri.
- `GET /receipts/{idempotency_key}`: Yalnız dahili mock provider kabul kaydı sorgusu.

Her delivery'nin `next_attempt_at` ve `failure_count` alanı DB'dedir. Erken gelen duplicate veya recovery task bu tarihi atlayamaz. Retry bütçesi Asynq task ID'sine bağlı değildir; task yeniden oluşturulsa da kalır. Geçici gönderim hataları dokuz hatalık bütçeyi tüketir; 429/rate limit/kilit beklemesi tüketmez. Asynq'a `DeferredError` dönülür; business retry planı DB'den belirlenir. Provider receipt sorgusu yapılamıyorsa yeni mesaj gönderilmez; sonuç doğrulanana kadar reconciliation ertelenir.

Payload ve `inflight` deneme provider çağrısından önce kalıcılaştırılır. Session advisory lock aynı delivery'yi işleyen worker'ları serileştirir ve DB bağlantısı kopunca serbest kalır. Token kontrollü DB update, eski worker'ın yeni sonucu ezmesini engeller. Kilitli iş başarılı sayılmaz; ertelenir. Kilit cleanup'ı ayrı timeout kullanır; unlock doğrulanamazsa bağlantı havuza dönmeden kapatılır.

Provider kabulünden sonra worker çökerse `inflight` kayıt `unknown` olur. Sonraki worker önce aynı key için receipt sorgular: kabul varsa yeniden Send çağırmadan accepted durumunu kaydeder. Bu sorgu hata bütçesi ve kampanya son kullanma kontrolünden önce yapılır; bilinen kabulün yanlışlıkla failed olması önlenir.

## Kullanıcıdaki gönderim tarihleri

`users.last_sms_sent_at` ve `users.last_email_sent_at` yalnız ilgili kanal için, doğrulanmış provider receipt'i ile delivery `accepted` olduğunda aynı transaction içinde güncellenir. Bir DB hatası ikisini birlikte rollback eder. Duplicate task timestamp'i değiştirmez; `GREATEST` farklı kampanyalar eşzamanlı sonuçlandığında tarihi geriye götürmez. Başarısız, 429, iptal edilmiş izin veya henüz doğrulanmamış timeout tarihi doldurmaz. Receipt recovery başarılıysa tarih o doğrulama anında yazılır.

Bu tarihler **provider kabulünün doğrulandığı zamanı** ifade eder; gerçek alıcıya teslimat zamanı değildir. Gerçek provider'da delivery webhook'ları için ayrı `delivered_at` alanları gerekir. Önceki demo kayıtları yalnız eşleşen kalıcı provider key/receipt/payload kanıtı varsa migration sırasında provider kabul zamanı ile doldurulur.

Gönderim takibinin özet ve kuyruk alanları SMS/email için ayrı gösterilir. `GET /api/stats?channel=sms` ve `GET /api/stats?channel=email` kanal bazlı sayaçları döndürür. Kanal belirtilmezse mevcut global özet korunur. Kanal filtreli outbox sayısı yalnız ilgili message:send işlerini kapsar; campaign generation işleri bu sayıya dahil değildir.

## Frontend ve SQL injection güvenlik testleri

`make test` artık Go entegrasyon/race testlerinden sonra Docker'da 12 Vue/Vitest/jsdom güvenlik testini ve frontend production build'ini de çalıştırır. Yalnız frontend için `cd frontend && npm ci && npm test` kullanılabilir (Docker testi host Node sürümünden bağımsızdır).

Kapsam: kalıcı/stream edilmiş HTML ve hata mesajlarının metin olarak render edilmesi, localStorage ID müdahalesi, URL yol enjeksiyonu, değişken allowlist'i, SQL görünümlü brief/metnin JSON veri olarak kalması; gerçek PostgreSQL üzerinde boolean/UNION/DDL/pg_sleep payload'larıyla ID/filtre/tip doğrulaması, veri bütünlüğü ve cross-origin yazma engeli.

SQL injection savunması backend'deki parametreli sorgular ve sabit sorgu yapısıdır; frontend doğrulaması tek başına güvenlik sınırı değildir. SQL anahtar kelimeleri ve apostrof metinden silinmez. ID'ler biçim allowlist'i ile doğrulanır ve URL bileşeni olarak kodlanır. Vue kullanıcı içeriğini HTML olarak çalıştırmaz. Nginx CSP, frame-ancestors/X-Frame-Options ve Permissions-Policy başlıkları gönderir. API, Origin varsa yabancı origin'den yazmayı reddeder; proxy asıl Host değerini korur.

Bu testler tam kapsamlı pentest değildir. Lokal demo auth/RBAC/tenant izolasyonu içermez; bu ekler onları tamamlamaz. İnceleme bulguları `reports/security-2026-10-27.md` dosyasındadır.

## Gerçek kuyruk görünümü

“Yeni kampanya” yalnız Kampanya stüdyosundadır. Gönderim takibindeki “Kuyruktaki işler” DB pending/processing toplamı yerine `GET /api/queues` üzerinden gerçek Asynq/Redis değerini gösterir: pending + active + scheduled + retry + aggregating. Completed ve archived toplamın dışında tutulur. Retry mesajları da kuyrukta beklediğinden toplamda yer alır.

Kuyruklar ve DB sayaçları sunucuda 500 ms aralıkla ölçülür; yalnız değişen snapshot SSE ile iletilir. Asynq public queue-change subscription sunmadığı için bu sunucu ölçümü kullanılır. Tarayıcıda setInterval/polling yoktur; 15 saniyelik SSE heartbeat bağlantıyı korur. Hazır işler hızlı tüketilebilir, bu yüzden yalnız pending sayısına bakarak kuyruk boş denemez. Redis’te bağlantı hatası sıfır yerine “—” ve uyarı olarak gösterilir. “Redis’e aktarılacak (outbox)” ayrı bir kalıcı kayıt sayacıdır. Asynq teknik arşivi ile provider kaynaklı kalıcı mesaj hataları farklı metriklerdir.


### Güvenilirlik politikaları (27 Ekim 2026)

- Brief'te açıkça yazılmış İstanbul/Ankara/İzmir ve sayısal gün aralığı, LLM segmentiyle üretimde ve son onayda karşılaştırılır. Uyuşmazlık gönderime geçmez; birden fazla şehir veya farklı gün aralıkları açıklığa kavuşturulmalıdır. Bu, serbest doğal dil anlamının tamamını doğrulayan bir mekanizma değildir.
- Provider adaptörünün `SupportsIdempotency()` sözleşmesi, aynı anahtarın retry süresi boyunca aynı dış yan etkiyi temsil ettiğini garanti etmelidir. Dahili HTTP mock bu garantiyi kalıcı provider kaydıyla sağlar. Yalnız receipt sorgusu yapabilmek, negatif sonucun kesin olduğu anlamına gelmez.
- Bu garantiyi vermeyen adaptörde ilk belirsiz hata veya yarım kalmış deneme `unknown` durumuna alınır. Yeni Send yapılmaz, sent tarihi yazılmaz, campaign `needs_review` olur. SSE/dashboard ve hata geçmişi manuel incelemeyi gösterir. Otomatik veya körlemesine manuel resend endpoint'i yoktur; dış kabul doğrulanmadan tekrar göndermek güvenli değildir. Bu politika duplicate riskini önlerken teslimatın tamamlanmasını garanti etmez. Adaptör eklenirken sözleşme gerçek provider'ın retention süresine göre değerlendirilmelidir.
- Varsayılan `CAMPAIGN_TTL=0s`: eski sabit 24 saatlik kesme kaldırılmıştır. İsteğe bağlı `CAMPAIGN_TTL=72h` gibi bir işletme süresi tanımlanabilir; ilgili süre aşılırsa yeni Send durur. Önceki kabulü doğrulayan receipt sorgusu süreden önce çalışır. Bu ayar bütün backend worker instance'larında aynı olmalıdır.
- Kanal başına 100/s değişmedi. 10 milyon/kanal için ideal alt sınır 27 saat 46 dakika 40 saniyedir; retry, diğer kampanyalar ve gerçek throughput süreyi uzatır. Süresiz kuyruk kapasite garantisi değildir; backlog ve oldest-message-age izlenmelidir.
