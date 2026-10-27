# AI geliştirme kaydı

Bu proje Codex ile bu konuşmadaki gereksinimler ve kullanıcının verdiği referans ekran görüntüleri üzerinden oluşturuldu.

Gerçek Claude API çağrısı veya Claude geliştirme oturumu bu teslimde yapılmadı. Dolayısıyla bu klasörde Claude session export'u bulunduğu iddia edilmez. Mock LLM çağrıları test ve demo içindir; Claude export'u değildir.

Case'in Claude session export şartını tamamlamak için kendi Claude oturumundan alınan gerçek export'ları, API key ve kişisel verileri temizleyerek buraya ekle. Uygulama prompt'u backend/internal/studio/llm.go içinde `campaign-v1` olarak sürümlenmiştir.

## Codex konuşma export’u

Hazırlanan görünür konuşma kaydı, görselleriyle birlikte [codex-conversation-2026-10-27](codex-conversation-2026-10-27/) klasöründedir:

- [Markdown konuşma](codex-conversation-2026-10-27/conversation.md)
- [JSON konuşma](codex-conversation-2026-10-27/conversation.json)
- [Ekler](codex-conversation-2026-10-27/attachments/)

Bu anlık görüntü ilk “selam” mesajından konuşma export’u isteğine kadardır; daha sonraki güvenlik testi/push mesajlarını kapsamaz. 12 kullanıcı mesajı, 37 görünür asistan mesajı ve 6 kullanıcı görseli içerir. Sistem/geliştirici talimatları, özel muhakeme ve araç kayıtları dahil edilmemiştir. Bu dosyalar Claude session export şartının yerine geçtiği iddiasını taşımaz.
