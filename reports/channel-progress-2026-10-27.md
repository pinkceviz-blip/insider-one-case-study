# Kanal bazında kampanya ilerlemesi

- Seçili kampanyadaki birleşik yedi durum satırı kaldırıldı.
- SMS ve email için ayrı işlenme çubukları var; hemen altlarında mevcut SMS/email özetleri devam eder.
- API ve SSE campaign snapshot'ı `channel_counts` döndürür. Eski `counts` toplamı korunur. Kanal dağılımı aynı GROUP BY sorgusundan üretilir; ek DB sorgusu yoktur.
- İlerleme = (accepted + failed + suppressed + unknown) / o kanalın toplam delivery sayısı. Bu gönderim/teslimat başarısı yüzdesi değildir. Provider kabulü, kalıcı hata, izin iptali ve manuel inceleme ayrıca belirtilir. Alıcısız kanal “Alıcı yok”, yüzde 0 gösterir.
- Vue regresyonunda SMS %80 / email %20 snapshot'ı ardından SMS %100 / email %20 doğrulandı. Eski detail-grid'in kaldırıldığı ve boş kanalın doğru gösterildiği test edildi.
- Frontend: 19 test ve production build başarılı.

Backend: race + uçtan uca entegrasyon paketi başarılı (41,087 s). Kanal sayaçlarının toplamı, kampanya toplamıyla ve gerçek channel/state SQL sayımlarıyla karşılaştırıldı.
