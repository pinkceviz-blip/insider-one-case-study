package studio

import (
	"net/http"
	"net/url"
	"regexp"
)

var campaignIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var deliveryIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

func requireID(w http.ResponseWriter, id string, pattern *regexp.Regexp) bool {
	if !pattern.MatchString(id) {
		writeJSON(w, 400, map[string]string{"error": "Geçersiz kayıt kimliği"})
		return false
	}
	return true
}
func securityHeadersAndOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					writeJSON(w, 403, map[string]string{"error": "İstek kaynağına izin verilmiyor"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
