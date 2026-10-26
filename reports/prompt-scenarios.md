# Farklı brief, kitle ve içerik denemeleri

Ortam: çalışan lokal uygulama, Mock LLM, PostgreSQL/Redis/Asynq ve mock provider. Gerçek alıcılara mesaj gönderilmedi.

| Kampanya | Kitle (kişi) | SMS + email işi | Kabul | Kalıcı hata |
|---|---:|---:|---:|---:|
| İstanbul · samimi · %10 | 168 | 192 | 187 | 5 |
| Ankara · resmi · %20 | 84 | 168 | 164 | 4 |
| İzmir · enerjik · %40 | 84 | 168 | 162 | 6 |

Toplam 336 farklı kişi için 528 kanal mesajı: 513 provider kabulü, 15 mock kalıcı hata. Kampanyalar arasında kişi çakışması yok.

## Doğrulamalar

- Brief’teki şehir/gün/indirim, üretilen segment ve içerikle eşleşti.
- Yanlış şehirden veya kanal izni olmayan alıcıya gönderim yok.
- Provider’a giden içerikte çözümlenmemiş kişiselleştirme değişkeni yok.
- Her accepted mesaj için tek kalıcı provider key ve ilgili kullanıcıda doğrulanmış kanal gönderim tarihi var.
- 161 literal karakterlik SMS düzenlemesi üç kampanyada da backend tarafından 422 ile reddedildi.
- Geçici hatalar ve 429 retry ile işlendi; bütün gönderimler terminal duruma ulaştı.

## Gerçek mock provider kayıtlarından örnekler

### İstanbul · samimi · %10

**Brief:** İstanbul'da son 7 günde sepetine ürün ekleyip satın almayan kullanıcılara %10 indirim hatırlatması gönder. Samimi ve kısa olsun.

**Kampanya ID:** `97ae870d03638143dc3cc6e1c5c9ae9c`

- EMAIL · Can · kullanıcı 126: Merhaba Can, seçtiğin ürünleri %10 indirimle tamamlayabilirsin. Sepetine dön, fırsatı değerlendir!
  Konu: Sepetinde %10 indirim seni bekliyor
- EMAIL · Zeynep · kullanıcı 63: Merhaba Zeynep, seçtiğin ürünleri %10 indirimle tamamlayabilirsin. Sepetine dön, fırsatı değerlendir!
  Konu: Sepetinde %10 indirim seni bekliyor
- SMS · Can · kullanıcı 6: Merhaba Can, sepetin seni bekliyor! %10 indirim fırsatını kaçırma.
- SMS · Zeynep · kullanıcı 3: Merhaba Zeynep, sepetin seni bekliyor! %10 indirim fırsatını kaçırma.

**Kanal sonuçları:** `{"email": {"accepted": 23, "failed": 1}, "sms": {"accepted": 164, "failed": 4}}`

**Hata sınıfları:** `{"transient": 33, "throttled": 13, "permanent": 5}`

### Ankara · resmi · %20

**Brief:** Ankara'da son 2 günde sepetine ürün ekleyip satın almayan kullanıcılara %20 indirim hatırlatması gönder. Resmi bir dil kullan.

**Kampanya ID:** `ccaad15c670df3b91ba0a0e0d71360b7`

- EMAIL · Ece · kullanıcı 1: Merhaba Ece, seçtiğin ürünleri %20 indirimle tamamlayabilirsin. Sepetine dön, fırsatı değerlendir!
  Konu: Sepetinde %20 indirim seni bekliyor
- SMS · Ece · kullanıcı 1: Merhaba Ece, sepetin seni bekliyor! %20 indirim fırsatını kaçırma.

**Kanal sonuçları:** `{"email": {"accepted": 82, "failed": 2}, "sms": {"accepted": 82, "failed": 2}}`

**Hata sınıfları:** `{"transient": 35, "throttled": 12, "permanent": 4}`

### İzmir · enerjik · %40

**Brief:** İzmir'de son 3 günde sepetine ürün ekleyip satın almayan kullanıcılara %40 indirim hatırlatması gönder. Enerjik bir dil kullan.

**Kampanya ID:** `ef838af6ef254a0eb987da41bd4862ea`

- EMAIL · Mehmet · kullanıcı 2: Merhaba Mehmet, seçtiğin ürünleri %40 indirimle tamamlayabilirsin. Sepetine dön, fırsatı değerlendir!
  Konu: Sepetinde %40 indirim seni bekliyor
- SMS · Mehmet · kullanıcı 2: Merhaba Mehmet, sepetin seni bekliyor! %40 indirim fırsatını kaçırma.

**Kanal sonuçları:** `{"email": {"accepted": 82, "failed": 2}, "sms": {"accepted": 80, "failed": 4}}`

**Hata sınıfları:** `{"transient": 30, "throttled": 12, "permanent": 6}`

## Sınırlama: mock AI semantik yorumlama yapmıyor

Samimi/resmi/enerjik istekleri metnin tonunu değiştirmedi; yalnız şehir/gün/indirim çıkarımı destekleniyor. Bu denemeler gerçek Claude davranışını doğrulamaz.

**Kontrol brief:** İzmir'de son 3 günde sepetine ürün ekleyip satın almayanlara indirimsiz bir hatırlatma gönder. İndirim oranı veya kampanya teklifi belirtme.

**Üretilen SMS:** Merhaba {{first_name}}, sepetin seni bekliyor! %30 indirim fırsatını kaçırma.

Bu kontrol brief’inde istenmeyen %30 teklif üretildi. Şema ve karakter kontrolü geçtiği halde brief ile anlam uyumu yok. Bu yüzden kampanya taslak bırakıldı; onay verilmedi ve provider gönderim sayısı 0. Mock LLM’in sabit varsayımının sınırı görünür hale geldi.

Ham kanıtlar: `prompt-scenarios.json`.
