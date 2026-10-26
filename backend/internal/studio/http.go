package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hibiken/asynq"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("Geçersiz istek gövdesi")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("Tek JSON nesnesi bekleniyor")
	}
	return nil
}
func fail(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	status := 500
	msg := "İşlem tamamlanamadı; tekrar deneyin"
	if isNotFound(err) {
		status = 404
		msg = "Kampanya bulunamadı"
	}
	if errors.Is(err, ErrConflict) {
		status = 409
		msg = err.Error()
	}
	writeJSON(w, status, map[string]string{"error": msg})
}
func (e *Engine) Handler(mode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		pg := e.Store.DB.Ping(ctx) == nil
		rd := e.Redis.Ping(ctx).Err() == nil
		status := 200
		if !pg || !rd {
			status = 503
		}
		writeJSON(w, status, map[string]any{"status": map[bool]string{true: "ok", false: "degraded"}[pg && rd], "postgres": pg, "redis": rd, "llm_mode": mode, "channels": e.Registry.Names()})
	})
	mux.HandleFunc("GET /api/errors", e.ErrorList)
	mux.HandleFunc("GET /api/deliveries/{id}/attempts", e.DeliveryHistory)
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"llm_mode": mode, "channels": e.Registry.Names(), "limits": map[string]int{"brief": 4000, "sms": 160, "email_subject": 80, "email_body": 10000}, "personalization_excluded": true})
	})
	mux.HandleFunc("POST /api/campaigns", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Brief string `json:"brief"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		in.Brief = strings.TrimSpace(in.Brief)
		if utf8.RuneCountInString(in.Brief) < 10 || utf8.RuneCountInString(in.Brief) > 4000 {
			writeJSON(w, 422, map[string]string{"error": "Brief 10–4000 karakter olmalı"})
			return
		}
		id, err := e.Store.Create(r.Context(), in.Brief)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 202, map[string]string{"id": id, "status": "generating"})
	})
	mux.HandleFunc("GET /api/campaigns", func(w http.ResponseWriter, r *http.Request) {
		rows, err := e.Store.DB.Query(r.Context(), "SELECT id FROM campaigns ORDER BY created_at DESC LIMIT 100")
		if err != nil {
			fail(w, err)
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				fail(w, err)
				return
			}
			ids = append(ids, id)
		}
		rows.Close()
		if rows.Err() != nil {
			fail(w, rows.Err())
			return
		}
		items := []Campaign{}
		for _, id := range ids {
			c, err := e.Store.Campaign(r.Context(), id)
			if err != nil {
				fail(w, err)
				return
			}
			items = append(items, c)
		}
		writeJSON(w, 200, items)
	})
	mux.HandleFunc("GET /api/campaigns/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := e.Store.Campaign(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("PATCH /api/campaigns/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int     `json:"version"`
			Content Content `json:"content"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		c, err := e.Store.Campaign(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		c.Proposal.Content = in.Content
		if err = e.Registry.Validate(c.Proposal); err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		if err = e.Store.Edit(r.Context(), c.ID, in.Version, in.Content); err != nil {
			fail(w, err)
			return
		}
		updated, err := e.Store.Campaign(r.Context(), c.ID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, updated)
	})
	mux.HandleFunc("POST /api/campaigns/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int `json:"version"`
		}
		if err := readJSON(w, r, &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		c, err := e.Store.Campaign(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if err = e.Registry.Validate(c.Proposal); err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		if err = e.Store.Approve(r.Context(), c.ID, in.Version, e.Registry.Names()); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 202, map[string]string{"status": "approved"})
	})
	mux.HandleFunc("GET /api/campaigns/{id}/events", e.SSE)
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		if channel != "" {
			if _, ok := e.Registry.Channels[channel]; !ok {
				writeJSON(w, 400, map[string]string{"error": "Geçersiz kanal"})
				return
			}
		}
		stats, err := e.Stats(r.Context(), channel)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, stats)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		stats, err := e.Stats(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for _, key := range []string{"accepted", "failed", "pending", "processing", "retrying", "suppressed", "retry_attempts", "outbox_pending", "provider_accepted"} {
			fmt.Fprintf(w, "# TYPE studio_%s gauge\nstudio_%s %d\n", key, key, stats[key])
		}
	})
	return mux
}
func (e *Engine) Stats(ctx context.Context, channels ...string) (map[string]int, error) {
	channel := ""
	if len(channels) > 0 {
		channel = channels[0]
	}
	stats := map[string]int{}
	rows, err := e.Store.DB.Query(ctx, "SELECT state,count(*) FROM deliveries WHERE ($1='' OR channel=$1) GROUP BY state", channel)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		if err = rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		stats[k] = n
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	for _, q := range []struct {
		k, sql   string
		filtered bool
	}{
		{"retry_attempts", "SELECT count(*) FROM attempts a JOIN deliveries d ON d.id=a.delivery_id WHERE a.kind IN ('transient','throttled') AND ($1='' OR d.channel=$1)", true},
		{"outbox_pending", "SELECT count(*) FROM outbox WHERE published_at IS NULL AND ($1='' OR (task_type='message:send' AND queue=$1))", true},
		{"provider_accepted", "SELECT count(*) FROM provider_requests WHERE ($1='' OR payload->>'channel'=$1)", true},
		{"campaigns", "SELECT count(*) FROM campaigns", false},
		{"users", "SELECT count(*) FROM users", false},
	} {
		var args []any
		if q.filtered {
			args = []any{channel}
		}
		var n int
		if err = e.Store.DB.QueryRow(ctx, q.sql, args...).Scan(&n); err != nil {
			return nil, err
		}
		stats[q.k] = n
	}
	return stats, nil
}
func (e *Engine) SSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := e.Store.Campaign(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	send := func(kind string, data any) error {
		b, _ := json.Marshal(data)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastSnapshot := ""
	for {
		rows, err := e.Store.DB.Query(r.Context(), "SELECT id,kind,data FROM events WHERE campaign_id=$1 AND id>$2 ORDER BY id LIMIT 200", id, cursor)
		if err != nil {
			return
		}
		for rows.Next() {
			var kind string
			var b []byte
			if rows.Scan(&cursor, &kind, &b) != nil {
				rows.Close()
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", cursor, kind, b); err != nil {
				rows.Close()
				return
			}
		}
		rows.Close()
		if rows.Err() != nil {
			return
		}
		c, err := e.Store.Campaign(r.Context(), id)
		if err != nil {
			return
		}
		b, _ := json.Marshal(c)
		if string(b) != lastSnapshot {
			if err = send("snapshot", c); err != nil {
				return
			}
			lastSnapshot = string(b)
		}
		if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// QueueSnapshot is separate from durable business counters.
func QueueSnapshot(inspector *asynq.Inspector, names []string) map[string]int {
	out := map[string]int{}
	for _, name := range names {
		if q, err := inspector.GetQueueInfo(name); err == nil {
			out[name+"_pending"] = q.Pending
		}
	}
	return out
}
