package studio

import (
	"context"
	"errors"
	"github.com/hibiken/asynq"
	"net/http"
	"time"
)

type QueueMetrics struct {
	Available   bool   `json:"available"`
	Pending     int    `json:"pending"`
	Active      int    `json:"active"`
	Scheduled   int    `json:"scheduled"`
	Retry       int    `json:"retry"`
	Aggregating int    `json:"aggregating"`
	Archived    int    `json:"archived"`
	Completed   int    `json:"completed"`
	Outstanding int    `json:"outstanding"`
	Paused      bool   `json:"paused"`
	LatencyMS   int64  `json:"latency_ms"`
	Error       string `json:"error,omitempty"`
}
type queueInspector interface {
	Queues() ([]string, error)
	GetQueueInfo(string) (*asynq.QueueInfo, error)
}

func readQueueMetrics(inspector queueInspector, names []string) map[string]QueueMetrics {
	out := make(map[string]QueueMetrics, len(names))
	registered, err := inspector.Queues()
	if err != nil {
		for _, name := range names {
			out[name] = QueueMetrics{Error: "Redis kuyruk bilgisi alınamadı"}
		}
		return out
	}
	exists := map[string]bool{}
	for _, name := range registered {
		exists[name] = true
	}
	for _, name := range names {
		if !exists[name] {
			out[name] = QueueMetrics{Available: true}
			continue
		}
		q, err := inspector.GetQueueInfo(name)
		if errors.Is(err, asynq.ErrQueueNotFound) {
			out[name] = QueueMetrics{Available: true}
			continue
		}
		if err != nil {
			out[name] = QueueMetrics{Error: "Redis kuyruk bilgisi alınamadı"}
			continue
		}
		out[name] = QueueMetrics{Available: true, Pending: q.Pending, Active: q.Active, Scheduled: q.Scheduled, Retry: q.Retry, Aggregating: q.Aggregating, Archived: q.Archived, Completed: q.Completed, Outstanding: q.Pending + q.Active + q.Scheduled + q.Retry + q.Aggregating, Paused: q.Paused, LatencyMS: q.Latency.Milliseconds()}
	}
	return out
}
func (e *Engine) Queues(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"channels": e.QueueMetrics(r.Context()), "sampled_at": time.Now().UTC()})
}
func (e *Engine) QueueMetrics(parent context.Context) map[string]QueueMetrics {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if err := e.Redis.Ping(ctx).Err(); err != nil {
		out := map[string]QueueMetrics{}
		for _, ch := range e.Registry.Names() {
			out[ch] = QueueMetrics{Error: "Redis kuyruk bilgisi alınamadı"}
		}
		return out
	}
	o := e.Redis.Options()
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: o.Addr, Username: o.Username, Password: o.Password, DB: o.DB, TLSConfig: o.TLSConfig, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	defer inspector.Close()
	return readQueueMetrics(inspector, e.Registry.Names())
}
