package studio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type HTTPProvider struct {
	URL    string
	Client *http.Client
}

func (p *HTTPProvider) Send(ctx context.Context, key string, m Message) (string, error) {
	b, _ := json.Marshal(m)
	req, e := http.NewRequestWithContext(ctx, "POST", p.URL+"/send", bytes.NewReader(b))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	res, e := p.Client.Do(req)
	if e != nil {
		code := "network_error"
		if errors.Is(e, context.DeadlineExceeded) {
			code = "timeout"
		}
		if errors.Is(e, context.Canceled) {
			code = "request_cancelled"
		}
		return "", &ProviderError{Kind: Transient, Code: code, Message: ExplainCode(code)}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		kind := Permanent
		if res.StatusCode == 429 {
			kind = Throttled
		} else if res.StatusCode >= 500 || res.StatusCode == 408 {
			kind = Transient
		}
		delay := time.Duration(0)
		if kind == Throttled {
			delay = 2 * time.Second
			if n, e := strconv.Atoi(res.Header.Get("Retry-After")); e == nil && n > 0 {
				delay = time.Duration(n) * time.Second
			} else if at, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil && time.Until(at) > 0 {
				delay = time.Until(at)
			}
			if delay > time.Hour {
				delay = time.Hour
			}
		}
		code := strconv.Itoa(res.StatusCode)
		return "", &ProviderError{Kind: kind, RetryAfter: delay, Code: code, HTTPStatus: res.StatusCode, Message: ExplainCode(code), RequestID: safeRequestID(res.Header.Get("X-Request-ID"))}
	}
	var result struct {
		ID string `json:"id"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&result); e != nil || result.ID == "" {
		return "", &ProviderError{Kind: Transient, Code: "invalid_response"}
	}
	return result.ID, nil
}
func (s *Store) ProviderHandler(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/receipts/") {
		key = strings.TrimPrefix(r.URL.Path, "/receipts/")
		var receipt string
		err := s.DB.QueryRow(r.Context(), "SELECT provider_id FROM provider_requests WHERE idempotency_key=$1", key).Scan(&receipt)
		if isNotFound(err) {
			http.Error(w, "not found", 404)
			return
		}
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		writeJSON(w, 200, map[string]string{"id": receipt})
		return
	}
	if key == "" {
		http.Error(w, "idempotency key required", 400)
		return
	}
	var m Message
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 50000)).Decode(&m); e != nil {
		http.Error(w, "bad message", 400)
		return
	}
	b, _ := json.Marshal(m)
	w.Header().Set("X-Request-ID", NewID())
	// A persistent unique key serializes concurrent requests, including accepted-but-timeout retries.
	tx, e := s.DB.Begin(r.Context())
	if e != nil {
		http.Error(w, "db", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", key); e != nil {
		http.Error(w, "db", 503)
		return
	}
	var old []byte
	var providerID string
	e = tx.QueryRow(r.Context(), "SELECT payload,provider_id FROM provider_requests WHERE idempotency_key=$1", key).Scan(&old, &providerID)
	if e == nil {
		var previous Message
		_ = json.Unmarshal(old, &previous)
		if previous != m {
			http.Error(w, "key payload mismatch", 409)
			return
		}
		writeJSON(w, 200, map[string]string{"id": providerID})
		return
	}
	if !isNotFound(e) {
		http.Error(w, "db", 503)
		return
	}
	var attempt int
	e = tx.QueryRow(r.Context(), "INSERT INTO provider_attempts(idempotency_key) VALUES($1) ON CONFLICT(idempotency_key) DO UPDATE SET count=provider_attempts.count+1 RETURNING count", key).Scan(&attempt)
	if e != nil {
		http.Error(w, "db", 503)
		return
	}
	h := sha256.Sum256([]byte(key))
	n := int(h[0])
	if n%37 == 0 {
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "db", 503)
			return
		}
		http.Error(w, "invalid recipient", 422)
		return
	}
	if attempt == 1 && n%11 == 0 {
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "db", 503)
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "rate limited", 429)
		return
	}
	if attempt == 1 && n%7 == 0 {
		if tx.Commit(r.Context()) != nil {
			http.Error(w, "db", 503)
			return
		}
		http.Error(w, "temporary error", 503)
		return
	}
	providerID = "mock-" + key
	_, e = tx.Exec(r.Context(), "INSERT INTO provider_requests(idempotency_key,payload,provider_id) VALUES($1,$2,$3)", key, b, providerID)
	if e != nil {
		http.Error(w, "db", 503)
		return
	}
	if tx.Commit(r.Context()) != nil {
		http.Error(w, "db", 503)
		return
	}
	// Some accepted messages deliberately outlive the HTTP client timeout.
	delay := time.Duration(20+n%100) * time.Millisecond
	if attempt == 1 && n%13 == 0 {
		delay = 1500 * time.Millisecond
	}
	time.Sleep(delay)
	writeJSON(w, 200, map[string]string{"id": providerID})
}
func providerKind(e error) ErrorKind {
	var pe *ProviderError
	if errors.As(e, &pe) {
		return pe.Kind
	}
	return Transient
}
func safeProviderError(e error) string {
	var pe *ProviderError
	if errors.As(e, &pe) {
		return fmt.Sprintf("%s:%s", pe.Kind, pe.Code)
	}
	return "internal_error"
}

func (p *HTTPProvider) Lookup(ctx context.Context, key string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/receipts/"+url.PathEscape(key), nil)
	if err != nil {
		return "", false, err
	}
	res, err := p.Client.Do(req)
	if err != nil {
		return "", false, &ProviderError{Kind: Transient, Code: "lookup_unavailable", Message: ExplainCode("lookup_unavailable")}
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return "", false, nil
	}
	if res.StatusCode != 200 {
		return "", false, &ProviderError{Kind: Transient, Code: "lookup_unavailable", HTTPStatus: res.StatusCode, Message: ExplainCode("lookup_unavailable")}
	}
	var result struct {
		ID string `json:"id"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&result); err != nil || result.ID == "" {
		return "", false, &ProviderError{Kind: Transient, Code: "lookup_invalid_response", Message: ExplainCode("lookup_invalid_response")}
	}
	return result.ID, true, nil
}
func safeRequestID(s string) string {
	if len(s) > 128 {
		return ""
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return ""
		}
	}
	return s
}
func ExplainCode(code string) string {
	if i := strings.LastIndex(code, ":"); i >= 0 {
		code = code[i+1:]
	}
	descriptions := map[string]string{
		"429": "Provider hız sınırı aşıldı; Retry-After süresi dolunca yeniden denenecek.",
		"503": "Provider geçici olarak kullanılamıyor.", "500": "Provider sunucu hatası döndürdü.",
		"422": "Provider alıcıyı veya mesajı geçersiz buldu; tekrar denenmez.",
		"409": "Aynı idempotency key farklı bir mesajla kullanıldı; gönderim durduruldu.",
		"408": "Provider isteği zaman aşımına uğradı.", "timeout": "Provider yanıtı zaman aşımına uğradı; mesaj kabul edilmiş olabilir. Sonraki işlemde receipt sorgulanır.",
		"network_error": "Provider bağlantısı kurulamadı veya yanıt bağlantısı kesildi.", "network_or_timeout": "Provider bağlantı hatası veya zaman aşımı (önceki sürüm kaydı).",
		"request_cancelled": "Worker isteği iptal edildi; provider sonucu sonraki işlemde sorgulanır.",
		"invalid_response":  "Provider geçerli bir kabul kimliği döndürmedi.", "lookup_unavailable": "Provider kabul kaydı sorgulanamadı; yeni mesaj gönderilmedi.",
		"lookup_invalid_response": "Provider kabul kaydı yanıtı geçersiz; yeni mesaj gönderilmedi.",
		"worker_interrupted":      "Önceki deneme tamamlanmadan worker durdu veya DB bağlantısı kesildi; kabul kaydı kontrol edilir.",
		"attempts_exhausted":      "Kalıcı retry hata bütçesi tükendi; son kabul kaydı sorgusunda mesaj bulunamadı.",
		"campaign_expired":        "Kampanyanın 24 saatlik gönderim süresi doldu.", "consent_revoked": "Alıcının kanal izni kaldırıldı.",
		"render_validation": "Kişiselleştirilmiş içerik doğrulanamadı.", "limiter_unavailable": "Redis hız sınırlayıcıya ulaşılamadı; güvenli biçimde gönderim ertelendi.",
		"rate_wait": "Kanalın ortak hız sınırı nedeniyle bekleniyor.", "unknown_channel": "Kanal kayıtlı değil.", "provider_missing": "Kanalın provider adapter'ı kayıtlı değil.",
	}
	if s, ok := descriptions[code]; ok {
		return s
	}
	return "İşlem hatası: " + code
}
