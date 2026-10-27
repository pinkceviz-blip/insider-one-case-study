package studio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func exerciseSecurity(t *testing.T, ctx context.Context, e *Engine, apiURL string) {
	request := func(method, path string, body any, origin string, want int) []byte {
		t.Helper()
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, apiURL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, res.StatusCode, want, raw)
		}
		return raw
	}
	t.Run("sql_injection_http_ids_filters_and_types", func(t *testing.T) {
		var usersBefore int
		if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&usersBefore); err != nil {
			t.Fatal(err)
		}
		attacks := []string{"' OR '1'='1", "'; DROP TABLE users; --", "' UNION SELECT current_user --", "' OR EXISTS(SELECT pg_sleep(2)) --"}
		for _, attack := range attacks {
			encoded := url.PathEscape(attack)
			for _, path := range []string{"/api/campaigns/" + encoded, "/api/campaigns/" + encoded + "/events", "/api/deliveries/" + encoded + "/attempts", "/api/errors?campaign_id=" + url.QueryEscape(attack), "/api/errors?channel=" + url.QueryEscape(attack), "/api/errors?kind=" + url.QueryEscape(attack), "/api/errors?cursor=" + url.QueryEscape(attack), "/api/stats?channel=" + url.QueryEscape(attack)} {
				raw := request("GET", path, nil, "", 400)
				for _, secret := range []string{"SQLSTATE", "postgres://", "syntax error", "pgx."} {
					if strings.Contains(string(raw), secret) {
						t.Fatal("database details leaked", path)
					}
				}
			}
			request("POST", "/api/campaigns/"+encoded+"/approve", map[string]any{"version": 1}, "", 400)
			request("PATCH", "/api/campaigns/"+encoded, map[string]any{"version": 1}, "", 400)
		}
		request("POST", "/api/campaigns", map[string]any{"brief": "İstanbul son 4 gün", "sql": "DROP TABLE users"}, "", 400)
		var usersAfter int
		if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&usersAfter); err != nil || usersAfter != usersBefore {
			t.Fatal("injection changed user table", err, usersBefore, usersAfter)
		}
	})
	t.Run("sql_like_brief_and_copy_remain_literal_data", func(t *testing.T) {
		brief := "İstanbul son 4 gün O'Reilly'; DROP TABLE users; -- %10"
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(request("POST", "/api/campaigns", map[string]string{"brief": brief}, apiURL, 202), &created); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := e.Store.DB.QueryRow(ctx, "SELECT brief FROM campaigns WHERE id=$1", created.ID).Scan(&stored); err != nil || stored != brief {
			t.Fatal("brief not stored as literal data", err)
		}
		proposal := Proposal{Segment: Segment{"İstanbul", 4, true}, Content: Content{"original", "subject", "body"}}
		pb, _ := json.Marshal(proposal)
		victim, bystander := NewID(), NewID()
		for _, id := range []string{victim, bystander} {
			if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief,status,proposal) VALUES($1,'security fixture','draft',$2)", id, pb); err != nil {
				t.Fatal(err)
			}
		}
		content := Content{SMS: "O'Reilly'; UPDATE users SET city='Pwned'; --", EmailSubject: "SELECT fırsatları", EmailBody: "<img src=x onerror=alert(1)>; DROP TABLE users; --"}
		request("PATCH", "/api/campaigns/"+victim, map[string]any{"version": 1, "content": content}, apiURL, 200)
		c, err := e.Store.Campaign(ctx, victim)
		if err != nil || c.Proposal.Content != content {
			t.Fatal("copy changed or SQL executed", err)
		}
		other, err := e.Store.Campaign(ctx, bystander)
		if err != nil || other.Proposal.Content.SMS != "original" || other.Version != 1 {
			t.Fatal("injection modified another campaign", err)
		}
		var changed int
		if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM users WHERE city='Pwned'").Scan(&changed); err != nil || changed != 0 {
			t.Fatal("SQL-like copy executed", err)
		}
		request("PATCH", "/api/campaigns/"+victim, map[string]any{"version": "1 OR 1=1", "content": content}, apiURL, 400)
		if err := e.Registry.Validate(Proposal{Segment: Segment{"İstanbul' OR 1=1 --", 4, true}, Content: proposal.Content}); err == nil {
			t.Fatal("injected LLM city accepted")
		}
	})
	t.Run("cross_origin_write_cannot_create_campaign", func(t *testing.T) {
		var before, after int
		e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM campaigns").Scan(&before)
		request("POST", "/api/campaigns", map[string]string{"brief": "İstanbul son 4 gün %10"}, "https://evil.example", 403)
		e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM campaigns").Scan(&after)
		if before != after {
			t.Fatal("cross-origin write reached DB")
		}
	})
}
