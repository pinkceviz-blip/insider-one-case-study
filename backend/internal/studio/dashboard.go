package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (e *Engine) CampaignList(ctx context.Context) ([]Campaign, error) {
	rows, err := e.Store.DB.Query(ctx, "SELECT id FROM campaigns ORDER BY created_at DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	items := []Campaign{}
	for _, id := range ids {
		c, err := e.Store.Campaign(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, nil
}

// DashboardSSE sends current state on every connection, including reconnects.
// Asynq has no public queue change subscription: sample on the server and only
// transmit changed snapshots. Browsers never poll REST endpoints.
func (e *Engine) DashboardSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	last := ""
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		stats, err := e.Stats(ctx)
		channels := map[string]map[string]int{}
		for _, name := range e.Registry.Names() {
			if err != nil {
				break
			}
			channels[name], err = e.Stats(ctx, name)
		}
		campaigns, listErr := e.CampaignList(ctx)
		queues := e.QueueMetrics(ctx)
		pg := e.Store.DB.Ping(ctx) == nil
		rd := e.Redis.Ping(ctx).Err() == nil
		cancel()
		if err != nil || listErr != nil {
			return
		} // EventSource reconnect gets a fresh full snapshot.
		payload := map[string]any{"stats": stats, "channel_stats": channels, "campaigns": campaigns, "queues": queues, "health": map[string]bool{"postgres": pg, "redis": rd}}
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if string(b) != last {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = fmt.Fprintf(w, "event: dashboard\ndata: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
			last = string(b)
		}
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
		}
	}
}
