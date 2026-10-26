# Doğrulama · 26 Ekim 2026

Son kaynak üzerinde `make test` başarılı: Docker içinde `go test -race -count=1 -v ./...`.

PostgreSQL ve Redis ile entegrasyon kontrolleri:

- Aynı delivery'ye 20 eşzamanlı duplicate çağrı: yalnız bir provider Send; diğerleri DeferredError.
- Provider sonucu doğrulanmadan kullanıcı gönderim tarihleri boş.
- Başarıdan sonra task replay: yeni Send yok, tarih değişmiyor.
- Kalıcı DB retry deadline: erken gelen task provider'a ulaşmıyor.
- Geçici hata ve 429: kullanıcı timestamp'i yazılmıyor; 429 hata bütçesini tüketmiyor.
- Provider kabulünden sonra worker context iptali: sonuç commit olmuyor; yeni worker receipt sorgusuyla yeniden Send yapmadan accepted/timestamp kaydediyor.
- İptal edilmiş izin ve kalıcı provider hatası: gönderim/timestamp engeli.
- Aynı kullanıcının SMS ve email işlemleri eşzamanlı tamamlandığında iki tarih de korunuyor.
- 12 eşzamanlı onay: duplicate delivery oluşmuyor.
- Ortak rate limiter, onay öncesi gönderim engeli, invalid template, SSE, hata listesi ve deneme geçmişi API'leri.

Son izole pipeline: 95 mesaj; 92 accepted, 3 permanent failure. Bir alıcının izni onay öncesinde test için kaldırıldığı için snapshot'taki 96 mesajdan biri oluşturulmadı. Test şeması ve Redis DB 15 temizlendi; demo verisi korunuyor.

Canlı demo verisinde migration sonrası 80 kullanıcının SMS, 12 kullanıcının email tarihi yalnız eşleşen provider key/receipt/payload kanıtıyla dolduruldu. Başarısız dört mesaj için gönderim tarihi yazılmadığı ayrıca SQL ile kontrol edildi.

UI kontrolü: Hata kaydı açılarak HTTP 503 açıklaması, idempotency key, payload hash, alıcı ID ve iki kanalın doğrulanan tarihleri görüldü. `error-history.png` bu görünümün ekran görüntüsüdür.

Bu kontroller test edilen senaryolarda yarış/duplicate bulgusu olmadığını gösterir; tüm olası dağıtık sistem arızaları için mutlak garanti iddiası değildir. Gerçek alıcı teslimatı ve gerçek Claude API çağrısı bu mock testin kapsamında değildir.

Kanal ayrımı kontrolü: SMS/email için ayrı /api/stats sorgularının durum, retry, provider kabul ve message outbox değerleri kapalı pipeline toplamına eşit bulundu; bilinmeyen kanal 400 ile reddedildi. Vue production build başarılı.
