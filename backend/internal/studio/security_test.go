package studio

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginAndSecurityHeaders(t *testing.T) {
	handler := securityHeadersAndOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		name, origin string
		want         int
	}{{"same_origin", "http://studio.example", 204}, {"cross_origin", "https://evil.example", 403}, {"opaque_origin", "null", 403}, {"userinfo", "http://evil@studio.example", 403}, {"cli_no_origin", "", 204}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://studio.example/api/campaigns", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d", w.Code, tc.want)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("nosniff missing")
			}
		})
	}
}
