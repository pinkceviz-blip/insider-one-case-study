# Insider One — senaryo bazlı güvenilirlik denetimi

Tarih: 27 Ekim 2026. Ürün kodu ve mimari değiştirilmedi. Yalnız test eklendi. Gerçek Claude/SMS/email servisi kullanılmadı. Çalışan uygulamanın verisi yerine ayrı `insider-audit` Compose projesi, PostgreSQL şemaları ve Redis DB15 kullanıldı. Test edilen backend ürün dosyalarının mevcut çalışma kopyasıyla byte düzeyinde eşleştiği doğrulandı; SHA256 listesi ektedir.

**Sonuç:** mevcut temel testler geçiyor; bu, istenen bütün kabul kriterlerinin karşılandığı anlamına gelmiyor. İki karakterizasyon testi özellikle mevcut eksiklikleri yeniden üretiyor ve bu davranışı doğruladığı için test çalıştırıcısında PASS görünüyor: brief/segment anlam uyuşmazlığı ve idempotency sağlamayan provider’da duplicate kabul.

Son toplu `make test`: **başarılı**, Go race/entegrasyon paketi 90,174 saniye; frontend 16/16 ve üretim build başarılı. 11 yeni üst seviye audit testi (9 JSON alt senaryosu ve 2 concurrency alt senaryosu dahil); subprocess helper üst süreçte bilinçli SKIP, SIGKILL testi tarafından ayrı süreçte çalıştırılır. Docker ortamı: 8 CPU, yaklaşık 3,83 GiB RAM. Ürün kodunda race detector hatası gözlenmedi; bu bütün olası yarışların yokluğunu kanıtlamaz.

## İncelenen yapı

- Vue 3/Vite; Go 1.24; pgx v5.7.2; Redis client v9.7.0; Asynq v0.25.1; PostgreSQL 16; Redis 7.4. BullMQ yoktur.
- `POST /api/campaigns` üretim işini outbox’a yazar. Ayrı generation worker LLM’yi çağırır, JSON/domain doğrulaması yapar, parametreli sorguyla recipients snapshot’ı oluşturur.
- `PATCH /api/campaigns/{id}` taslak içeriğini optimistic version kontrolüyle düzenler. `POST .../approve` campaign satır kilidi altında deliveries + outbox üretir.
- Dispatcher `FOR UPDATE SKIP LOCKED` ile 100 kayıt alır; sabit task ID ile Asynq’ya yayınlar. SMS ve email ayrı queue/worker’larda çalışır.
- `users`, `consents`, `campaigns`, `recipients`, `deliveries`, `outbox`, `attempts`, `events`, `provider_requests`, `provider_attempts` temel tablolardır.
- Delivery başına PostgreSQL session advisory lock, fencing token, transaction’lı sonuç/kanal tarihi; provider’a delivery ID üzerinden `Idempotency-Key` ve receipt sorgusu.
- `/api/stats`, `/api/queues`, `/api/errors`, `/api/deliveries/{id}/attempts`, health ve metrics endpoint’leri. Kampanya SSE’si event replay; global dashboard SSE’si güncel snapshot gönderir.

## Test matrisi: beklenen / gerçekleşen / sonuç

| Senaryo | Beklenen | Gerçekleşen | Sonuç ve kanıt |
|---|---|---|---|
| Bozuk LLM JSON | Reddedilsin, alıcı/gönderim oluşmasın | Decode hatası, recipients=0, deliveries=0 | GEÇTİ — `TestAuditLLMInvalidMatrix/malformed` |
| Whitelist dışı alan | Reddedilsin | `unknown field sql` | GEÇTİ — `unknown_field` |
| Yanlış tip | Reddedilsin | string days alanında decode hatası | GEÇTİ — `wrong_type` |
| Eksik alan | Reddedilsin | Eksik city domain doğrulamasında reddedildi | GEÇTİ — `missing_city` |
| Null alan/nesne | Reddedilsin | null days/content/root doğrulamadan geçmedi | GEÇTİ — üç null alt testi |
| İzin verilmeyen operatör/filtre | SQL’e dönüşmesin | operator ve desteklenmeyen şehir reddedildi; recipient=0 | GEÇTİ — `operator`, `unsupported_filter` |
| Hatalı LLM retry | Sınırlı tekrar ve kontrollü hata | İlk çağrı + 2 retry; campaign failed ve `generation.failed` event’i | GEÇTİ — `TestAuditLLMRetryTerminalError`; bu test zamanlamayı hızlandırır, ürün retry hesabı ayrı testtedir |
| İstanbul brief, Ankara segment | Anlam uyuşmazlığı yakalansın | Ankara geçerli sayıldı, draft oluştu | **EKSİK** — `TestAuditSemanticMismatchCharacterization` |
| Onay öncesi dispatch | Mesaj oluşmasın | Taslakta delivery=0; temel uçtan uca testte provider=0 | GEÇTİ |
| Onaylanan kitle tutarlılığı | Dispatch aynı snapshot’ı kullansın | Yanlış Ankara önerisi insan tarafından onaylanınca yalnız Ankara delivery’leri oluştu | GEÇTİ; anlam doğruluğu eksikliğini gidermez |
| Onay sonrası düzenleme | Reddedilsin | Store ErrConflict, HTTP 409 | GEÇTİ — semantic test ve temel pipeline |
| Kişiselleştirme/limit | İsim hariç literal bütçe ve güvenli isim | 160 literal + 90 karakter isim kabul; 161 literal ve kontrol karakteri reddedildi | GEÇTİ — domain testleri; isim toplam limite dahil değil, talep edilen politika |
| Çift onay | Tek delivery kombinasyonu | 12 paralel onayda duplicate yok | GEÇTİ — mevcut race testi |
| Aynı delivery, farklı queue job’ları | Tek provider çağrısı | Farklı task ID’leri + iki Asynq instance: 1 delivery, 1 Send | GEÇTİ — `TestAuditDifferentQueueJobsSameDelivery` |
| Aynı tamamlanmış job’ı tekrar işle | Yeniden göndermesin | Send ve kullanıcı tarihi değişmedi | GEÇTİ — temel race + yeni queue testi |
| Provider kabul + timeout | Retry ikinci mesaj üretmesin | Mock provider’da stable key/receipt ile tek kabul | GEÇTİ — mevcut deterministik timeout testi |
| Provider idempotency/receipt desteklemiyor | Belirsiz sonuç duplicate’e yol açmasın | İlk kabul timeout oldu; retry ikinci kabulü oluşturdu | **EKSİK** — `TestAuditNoProviderIdempotencyCharacterization` |
| Provider kabulünden sonra SIGKILL | DB sonucu ve lock kurtarılsın | Alt Go süreci gerçekten öldürüldü; retry receipt ile accepted yaptı; Send=1 | GEÇTİ — `TestAuditSIGKILLAfterProviderAcceptance`; handler subprocess testi, native stalled-job testi değil |
| Provider kabul sonrası DB commit iptali | Tekrar göndermeden toparlansın | Context iptali sonrası receipt kurtarma; sent tarihi yalnız başarılı commit’te | GEÇTİ — `accepted_then_worker_cancel_receipt_recovery` |
| DB commit sonrası ack öncesi tekrar | Duplicate oluşmasın | Accepted delivery replay Send çağırmadı | GEÇTİ — terminal handler replay; o sınırda ayrıca OS süreci öldürülmedi |
| 429 / 503 / timeout | Doğru retry, 429 bütçeyi tüketmesin | Deterministik senaryolar toparlandı; deadline DB’de; erken tekrar Send yapmadı | GEÇTİ — temel acceptance/race testleri |
| Retry bütçesi dolu | Kontrolsüz Send olmasın | failure_count=9, failed/attempts_exhausted, Send=0 | GEÇTİ — `TestAuditFailuresExhaustedAndRedisUnavailable` |
| Limiter Redis bağlantısı yok | Kontrolsüz gönderim olmasın | Delivery ertelendi, Send=0 | GEÇTİ — aynı test; Redis tamamen silinmesi/partition farklı senaryodur |
| Çoklu worker rate limit | Kanal toplamı <=100/s | Her kanalda 2 instance; concurrency 1 ve 8; ölçülen peak her koşulda 100 | GEÇTİ — `TestAuditMultipleWorkersThroughput`; 480 delivery toplam, her koşul 120 SMS + 120 email |
| Gönderimden önce izin iptali | Bekleyen iş suppress edilsin | Provider çağrılmadı, sent tarihi yazılmadı | GEÇTİ — mevcut race testi |
| Consent kontrolü sırasında yarış | Revocation commit’inden sonra yeni gönderim olmasın | Gönderim FOR SHARE tutarken UPDATE timeout oldu; Send tamamlandıktan sonra iptal commit edildi; sonraki iş suppressed | GEÇTİ — `TestAuditConsentTransactionRace`; başlamış gönderimi geri alma garantisi yok |
| Redis publish sonrası DB ack çökmesi | Outbox’tan güvenli tekrar | DB trigger ile ack hatası; gerçek Redis task kaldı; yeniden dispatch duplicate üretmedi | GEÇTİ — mevcut outbox fault-injection testi |
| SSE reconnect | Ekran kaçırılan durumla toparlansın | Kampanya event cursor replay + global fresh snapshot testleri geçti | GEÇTİ — temel acceptance ve Vue testleri |
| SQL injection/XSS | Veri SQL/HTML olarak çalışmasın | ID/filter/type reddi, SQL benzeri metin literal kaldı, Vue HTML kaçırdı | GEÇTİ — mevcut backend/frontend security testleri |
| 24 saatten eski bekleyen kampanya | Süre politikası uygulansın | 25 saatlik kampanya `campaign_expired`, Send=0 | GEÇTİ — `TestAuditCampaignExpiry`; son toplu testte doğrulandı |

## Kontrollü yük ölçümleri

Aşağıdaki ölçümler `audit.log` içindeki ilk kontrollü denemeye aittir. Son toplu çalıştırma normal zamanlama farkları gösterebilir. Süreler SLO/production garantisi değildir.

| Ölçüm | 5.000 kullanıcı | 50.000 kullanıcı |
|---|---:|---:|
| Segment SELECT EXPLAIN ANALYZE | 0,208 ms | 12,496 ms |
| Sorgu planı | Bitmap index/heap, users_segment_idx | Seq Scan; yaklaşık %91’i eşleştiği için makul |
| Snapshot + audience kaydı | 26,47 ms | 1.167 ms |
| Onay transaction’ı | 25,63 ms | 5.028 ms |
| Oluşturulan delivery | 598 | 90.598 |
| Go heap önce/sonra | 4,34 / 4,36 MB | 4,42 / 4,43 MB |

50 bin dataset’i homojen İstanbul ağırlıklı sentetik yük olarak büyütüldü; 5 bin seed ile selectivity aynı değildir. Heap değerleri anlık örnektir, **peak RSS veya PostgreSQL belleği değildir**. Test pool’u bilinçli olarak 32 bağlantıdır; ürün OpenStore varsayılan pgxpool ayarlarını kullanır. Pool doygunluk testi yapılmadı.

Dispatcher’a arka arkaya 10 kez çağrı: yaklaşık 248 ms’de 1.000 outbox işi işlendi; 2 generation ve **998 delivery** SMS/email kuyruğuna geldi. Bu mikro ölçüm yaklaşık 4 bin iş/saniye gösterir ama gerçek dispatcher’ın 200 ms ticker + 100 batch sınırını bypass eder. Tek ürün dispatcher’ın teorik cadence tavanı 500 iş/saniyedir; bunun sürekli sağlandığı test edilmedi.

Redis used_memory 1.969.608 → 2.623.696 byte; 998 delivery + 2 generation işinin yaklaşık artışı 654 KB. Bu küçük örnekte yaklaşık 654 byte/iş; retention, attempt history, allocator, fragmentation ve farklı payload maliyetlerini içermeyen kaba kapasite girdisidir. 1 milyon benzer bekleyen iş yaklaşık 0,65 GB, 10 milyon yaklaşık 6,5 GB ek Redis belleği **tahmini** gerektirir. Gerçekte bu hacimler oluşturulmadı.

Worker ölçümleri: iki instance/kanal, concurrency=1 iken 240 delivery toplam 5,43 s; concurrency=8 iken 5,66 s. Her kanalda rolling 1 saniye peak 100. Küçük iş grubundaki ortalama yaklaşık 21–22 istek/s/kanal; startup/retry scheduler ve DB işlemleri dahil. Bu sonuç 100/s sürdürülebilir throughput kanıtı değildir; concurrency artırmanın bu küçük iş grubunda hız kazandırmadığını gösterir.

### 1 / 10 milyon için hesap

| Kanal başına mesaj | 100/s ile en iyi süre | İki kanala aynı sayıda gönderim |
|---|---|---|
| 1.000.000 | 10.000 s = 2 sa 46 dk 40 sn | Limitler bağımsız ve ideal paralel ise aynı süre; toplam 2 milyon delivery |
| 10.000.000 | 100.000 s = 27 sa 46 dk 40 sn | Aynı ideal süre; toplam 20 milyon delivery |

Retry/latency yokken bile 10 milyon/kanal **24 saatlik expiry ile çelişiyor**. 24 saatte teorik 8,64 milyon/kanal; diğer kampanyalar kapasiteyi paylaşır ve hata/retry bu tavanı düşürür. Bu bir hesap ve expiry testi birlikte yorumudur, 10 milyonluk gerçek yük testi değildir.

## Bulgular ve mimari öneriler (uygulanmadı)

1. **Yüksek — brief/segment anlam kontrolü yok.** `backend/internal/studio/worker.go:81–88`, `domain.go:194`. Validate yalnız üretilen Proposal’ı alır; brief ile karşılaştırmaz. Yanlış Ankara üretimi otomatik testle doğrulandı. Öneri: desteklenen niyetleri açık biçimde çıkarıp tutarlılık uyarısı üretmek; önerilen segmenti onay ekranında göstermeye devam etmek. Serbest doğal dil eşleşmesini kesin doğrulanmış saymamak.
2. **Yüksek — idempotency provider sözleşmesine bağlı.** `domain.go:69–75`, `delivery.go:153–168`, `provider.go:26`. ReceiptLookup opsiyonel; provider sözleşmesi stable key’e saygıyı garanti etmiyor. PostgreSQL kilidi iki farklı zamandaki belirsiz dış yan etkiyi tek başına çözemez. Öneri: kanal adaptörünün idempotency/receipt kabiliyetini zorunlu belirtmesi; desteklemeyen provider’da timeout sonucunu explicit unknown/manual-reconcile durumuna almak ve duplicate risk politikasını tanımlamak.
3. **Yüksek, kapasite politikası — 24 saat expiry.** `delivery.go:169`. 10 milyon/kanal için matematiksel olarak yetersiz. Öneri: onay öncesi tahmini tamamlanma zamanı ve kapasite kontrolü; kampanya süresi politikasını provider kapasitesiyle uyumlu tasarlamak.
4. **Orta — onay HTTP isteğinde büyük transaction.** `http.go:141–163`, `store.go:133–170`. LLM üretimi async olmasına rağmen delivery/outbox fan-out HTTP lifecycle içinde. 90.598 delivery 5,03 s sürdü. Öneri: onayı kısa atomic state transition olarak tutup recipient fan-out’u batch/keyset işlerine ayırmak; mevcut mimari değiştirilmedi.
5. **Orta — outbox backpressure/ingestion sınırı yok.** `worker.go:106–170`, `store.go:156`. Provider hızı sınırlı; onay bir kerede bütün outbox’ı dolduruyor. Dispatcher pending queue depth’e göre frenlemiyor. Bu bir kod inceleme bulgusudur; kontrollü test 90.598 delivery + yalnız ilk 1.000 publish ile sınırlandı. Öneri: high/low watermark, kabul bütçesi, tenant adaleti, queue age ve oldest outbox metrikleri.
6. **Orta — dashboard SSE her istemcide tekrar DB tarıyor.** `dashboard.go:52–75`, `http.go:196–235`, `store.go:33–60`. Değişen JSON dışında ağ trafiği gönderilmese de DB sorguları her 500 ms çalışıyor. 100 kampanyada liste N+1 ile 200 sorgu, stats 18 sorgu ve ek kontroller: yaklaşık 220 DB roundtrip/snapshot; hızlı döngüde yaklaşık 440/s/istemci. Bu hesap/kod incelemesidir; çoklu SSE kullanıcı yük testi yapılmadı. Öneri: ortak snapshot üreticisi, artımlı sayaçlar ve paylaşılan broadcast. Snapshot sayıları tek transaction’da alınmadığından hareket sırasında geçici çapraz sayaç farkları mümkündür; burada yarış testiyle gösterilmedi.
7. **Orta — bağlantı/pool bütçesi süreç sayısıyla büyür.** `store.go:15–22`, `locks.go:14`, `delivery.go:18–25,235–253`, `main.go:67`. Her send bir PG bağlantısını provider tamamlanana kadar tutar; consent row lock da bu süre boyunca tutulur. Test revocation’ın beklediğini doğruladı. Ürün pool_max_conns açık değil. Öneri: toplam worker instance × pool bütçesi, provider timeout ve DB kapasitesini birlikte sınırlamak; pool wait ve provider latency histogramı eklemek.
8. **Orta — limit 100 Lua içinde sabit.** `worker.go:192–200`. Worker concurrency env’den ayarlanabiliyor; rate limiti ayırarak farklı değerlerle karşılaştırmak ürün kodunu değiştirmeden mümkün değil. Bu nedenle yalnız concurrency 1/8 karşılaştırıldı. Öneri: doğrulanan kanal rate config’i ve aynı shared key üzerinde atomic limit.
9. **Orta — operasyonel recovery görünürlüğü sınırlı.** `worker.go:171–189`, `audit.go`, `http.go:183–191`. Asynq arşivi, business failed state ve attempts ayrıdır. Hata geçmişi okunabiliyor; explicit failed-job replay API’si yok. Reconcile 10 dakika eski nonterminal delivery’leri dakikada en fazla 1.000 tarar. Native stalled-job süresi bu denetimde ölçülmedi. Öneri: unknown/archived alarmı, oldest retry age, backlog, provider p95, outbox lag, kontrollü manual replay prosedürü.

## Tam kanıtlanmayan noktalar

- Asynq’nun native lease/stalled-job kurtarması, worker job’ı claim eder etmez `SIGKILL`, Redis lease/lock kaybı ve Redis veri kaybı/partition altında uçtan uca davranış ölçülmedi. Gerçek öldürme testi provider kabulünden sonra **HandleDelivery subprocess’i** üzerindedir; retry test tarafından çağrılır. Bunu stalled-job testinin yerine saymıyoruz.
- Provider çağrısından önceki bütün ara talimatlarda OS crash uygulanmadı. DB commit sonrası terminal replay kanıtlandı; o sınırda gerçek süreç öldürülmedi. Ürün koduna test hook’u eklemeden her instruction boundary deterministic hale getirilmedi.
- Hata dönen bütün provider tipleri ve günler süren 429/receipt-lookup kesintileri test edilmedi. Retry burst testi yukarıdaki kısa çoklu worker pencereleri ve deterministik hata testleriyle sınırlıdır.
- Gerçek Claude API, gerçek mesaj teslimatı, provider idempotency retention süresi ve internet/network davranışı bu mock testlerinden çıkarılamaz.
- Frontend component/security testleri çalıştırıldı; bu denetimde ayrıca tarayıcı üzerinden tüm form akışı veya çoklu tarayıcı disconnect testi yapılmadı. Backend SSE reconnect entegrasyon testleri çalıştı.
- 1 ve 10 milyon kullanıcı/job, peak backend RSS, DB peak bellek, connection starvation, disk/WAL/IO doygunluğu ve uzun süreli soak testi çalıştırılmadı. Kaynakları sınırlı yerel ortamda 50 bin kullanıcı ve 1.000 publish ile duruldu; kapasite tahminleri ölçüm diye sunulmadı.

## Tekrar çalıştırma ve dosyalar

Yeni test: `case_study_audit_test.go`. Repository’de `backend/internal/studio/` altına eklendiğinde normal `make test` tarafından bulunur. Ürün dosyaları değiştirilmedi.

Bu çalıştırmada macOS bazı repo dosyalarını dataless/offloaded tuttuğu için test imajındaki kaynaklar ayrı klasöre alındı; ürün Go dosyaları çalışma kopyasıyla bire bir karşılaştırıldı. `COMPOSE_FILE=/private/tmp/insider-audit/compose.yaml make test` ile aynı Makefile hedefleri, ayrı PG/Redis ve mount edilmiş denetim kaynağı üzerinde çalıştırıldı. Kalıcı raporla birlikte kaynaklar ve kendi test Compose tanımı teslim edilir; yeniden çalıştırmak için denetim klasöründe `docker compose run --rm integration` kullanılabilir. Mevcut Docker imajlarına ihtiyaç duyar.

Loglar: `baseline.log`, `audit.log`, `additional.log`, `current-frontend.log`, `final-make-test.log`. `GAP CONFIRMED` satırları kabul kriteri eksikliğinin kanıtıdır; PASS etiketi bu eksikliği çözülmüş yapmaz. İlk crash testindeki fake sunucu cleanup sırası düzeltildi; uygulama kodu değiştirilmedi ve temiz çalıştırmanın logu teslim edildi.

Son toplu ölçüm: 50 bin kullanıcıda snapshot 1,182 s; onay 4,460 s; delivery 90.598. Yukarıdaki tablo ilk koşuyu, final-make-test.log son koşuyu gösterir.
