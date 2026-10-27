# Kuyruk görünümü düzeltmesi · 27 Ekim 2026

## Neden sıfır görünüyordu?

Önceki kart yalnız PostgreSQL pending + processing delivery kayıtlarını topluyordu. Rate limit/provider hatası nedeniyle retry durumuna geçen mesajlar bu kartın dışında kalıyordu. Outbox sayısı ise Redis'teki hazır iş sayısı değildi; henüz Redis'e aktarılmamış kalıcı kayıtları gösteriyordu. Dört saniyelik polling, hızlı tüketilen hazır işleri de kaçırabiliyordu.

## Değişiklik

- Yeni kampanya eylemi yalnız Kampanya stüdyosunda.
- `GET /api/queues`: Asynq Inspector ile gerçek Redis pending/active/scheduled/retry/aggregating/archived/completed sayıları, paused bilgisi ve sampled_at.
- Kuyruktaki işler = pending + active + scheduled + retry + aggregating. Completed/archived dahil değil.
- Toplam ve retry kartı aynı Redis anlık görüntüsünden; takip sayfasında 1 saniyelik yenileme.
- Gönderime hazır, worker işliyor, zamanı bekleniyor ve retry bekliyor ayrı. Outbox ayrıca etiketlendi.
- Boş/hiç oluşturulmamış kuyruk 0; Redis erişim hatası — ve uyarı.
- Worker hızı, 100/s üst sınırı, retry/idempotency ve mevcut LLM ayarları değiştirilmedi. Yalnız API/frontend servisleri yenilendi.

## Kanıt

- `make test` PASS; Go -race paketi 52.520 saniye, frontend 15/15, production build başarılı.
- Gerçek Redis testi: worker'ın hata verip retry durumuna aldığı bir iş, bir hazır iş ve bir gelecek zaman işi birlikte toplam 3 gösterildi. DB delivery sayısından bağımsız.
- Frontend regresyonu: pending=0/active=0/scheduled=2/retry=95 olduğunda toplam 97 ve retry 95 görünür.
- Redis hatasının 0 olarak gösterilmediği ve Yeni kampanya düğmesinin takip sayfasında bulunmadığı test edildi.
- Canlı smoke: yapay bekletme/duraklatma olmadan mock kampanya onaylandı; bir anlık görüntüde email pending=96, SMS pending=4 ölçüldü. Daha sonra tarayıcıda SMS retry=482 ve toplam=482, email retry=80 ve toplam=80 görüldü; sayılar canlı değişir.
- Son frontend tutarlılık düzeltmesinden sonra Docker frontend testleri (15/15) ve production build yeniden geçti.

Hazır iş sayısının 0 olması tek başına hata değildir: worker işleri hızlı tüketebilir veya kalan işler retry zamanını bekliyor olabilir. Bütün işler sonuçlandığında toplamın 0 olması beklenir. Süreçleri görünür kılmak için sahte kuyruk sayısı veya yapay provider gecikmesi eklenmedi.

Ekler: queue-visibility-tests-2026-10-27.log ve queue-visibility-live-2026-10-27.json.

Değişiklikler lokalde uygulanmıştır; bu adımda GitHub'a push yapılmamıştır.
