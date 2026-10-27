# Üç güvenilirlik bulgusunun düzeltmesi — 27 Ekim 2026

1. **Brief/segment tutarlılığı:** `policy.go` açık desteklenen şehir ve sayısal gün aralığını karşılaştırır. Hem generation hem HTTP approve uygular; eski hatalı draft da 422 alır. Çok şehir veya çelişen günler sessizce daraltılmaz. Bu kontrol genel doğal dil anlama garantisi değildir; mevcut İstanbul/Ankara/İzmir ve tek gün aralığı modelini kapsar.
2. **Provider belirsizliği:** `SupportsIdempotency()` açık bir sözleşmedir. Kalıcı key dedup sağlayan HTTP mock opt-in yapar. Bu sözleşmeyi sağlamayan provider’da timeout ve yarım kalmış deneme `unknown` olur; yeni Send yapılmaz, sent tarihi yazılmaz. Campaign `needs_review`; metrik, SSE, hata geçmişi ve Vue uyarısı bu durumu gösterir. ReceiptLookup tek başına negatif lookup sonrası resend garantisi sayılmaz. Kabulü manuel doğrulamadan kör resend yapılmaz; manuel çözüm/replay endpoint’i bu değişikliğe dahil değildir. Gerçek provider’ın idempotency retention süresi adaptör sözleşmesinde ayrıca değerlendirilmelidir.
3. **Kuyruk ömrü:** örtük 24 saat expiry kaldırıldı. `CAMPAIGN_TTL=0s` varsayılanı otomatik kesme yapmaz. İstenirse `72h` gibi açık işletme süresi tanımlanır; bütün worker’larda aynı ayar olmalıdır. Receipt doğrulaması expiry kontrolünden önce kalır. 10 milyon/kanal ideal 27sa46dk40sn alt sınırı değişmedi; retry/diğer kampanyalar süreyi uzatır. Bu, milyonluk kapasitenin test edildiği veya teslimatın garanti edildiği anlamına gelmez.

## Doğrulama

- Tüm backend Go race/entegrasyon testleri: PASS, 82,830 saniye.
- 17 frontend testi ve production build: PASS.
- Yeni regresyonlar: `TestAuditSemanticMismatchBlocked`, `TestAuditNoProviderIdempotencyStopsAmbiguousRetry`, `TestUnsafeProviderInterruptedAttemptNeedsReview`, `TestDefaultLifetimeDoesNotExpireLargeBacklog`, `TestBriefSegmentExplicitConstraints`.
- Açık 24h TTL verildiğinde expiry davranışı da test edildi; varsayılan 0s altında 48 saatlik iş kabul edildi.
- Eski karakterizasyon testleri artık açıkları beklemek yerine korumaları doğruluyor. Mock provider idempotency, gerçek SIGKILL receipt recovery, duplicate queue jobs, rate limit, consent, outbox ve SSE testleri de geçmeye devam ediyor.
- İzole PostgreSQL/Redis kullanıldı. macOS dataless bağımlılık dosyaları nedeniyle tam yerel kaynak kopyası üzerinde çalıştırıldı; üretim servislerinin DB/Redis’ine test uygulanmadı.

Kaynak değişiklikleri source/ altında, çalıştırma kanıtları backend-tests.log ve frontend-tests.log içindedir. Henüz GitHub’a pushlanmadı.

Yerel dağıtım doğrulaması: API, provider, dispatcher ve tüm worker instance’ları güncellendi; eski worker’lar önce drain edildi. /health/ready PostgreSQL ve Redis için true döndü; LLM_MODE mock korundu; metrics içinde studio_unknown yayınlanıyor. localhost:18080 güncel sürümü çalıştırıyor.
