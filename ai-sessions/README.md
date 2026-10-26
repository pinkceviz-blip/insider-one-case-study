# AI geliştirme kaydı

Bu proje Codex ile bu konuşmadaki gereksinimler ve kullanıcının verdiği beş ekran görüntüsü üzerinden oluşturuldu.

Gerçek Claude API çağrısı veya Claude geliştirme oturumu bu teslimde yapılmadı. Dolayısıyla bu klasörde Claude session export'u bulunduğu iddia edilmez. Mock LLM çağrıları test ve demo içindir; Claude export'u değildir.

Case'in Claude session export şartını tamamlamak için kendi Claude oturumundan alınan gerçek export'ları, API key ve kişisel verileri temizleyerek buraya ekle. Uygulama prompt'u backend/internal/studio/llm.go içinde `campaign-v1` olarak sürümlenmiştir.
