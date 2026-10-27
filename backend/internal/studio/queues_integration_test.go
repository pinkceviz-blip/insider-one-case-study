package studio

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hibiken/asynq"
	"net/http/httptest"
	"testing"
	"time"
)

func exerciseQueueVisibility(t *testing.T, ctx context.Context, e *Engine, opt asynq.RedisClientOpt) {
	t.Run("real_redis_queue_counts_ready_scheduled_and_retry", func(t *testing.T) {
		name := "queue-visibility-" + NewID()
		inspector := asynq.NewInspector(opt)
		defer inspector.Close()
		empty := readQueueMetrics(inspector, []string{name})[name]
		if !empty.Available || empty.Outstanding != 0 {
			t.Fatal("new queue not recognized as empty", empty)
		}
		server := asynq.NewServer(opt, asynq.Config{Concurrency: 1, Queues: map[string]int{name: 1}, RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return time.Hour }, ShutdownTimeout: time.Second})
		mux := asynq.NewServeMux()
		mux.HandleFunc("queue:test", func(context.Context, *asynq.Task) error { return errors.New("deterministic delayed retry") })
		if err := server.Start(mux); err != nil {
			t.Fatal(err)
		}
		running := true
		defer func() {
			if running {
				server.Shutdown()
			}
		}()
		if _, err := e.Queue.EnqueueContext(ctx, asynq.NewTask("queue:test", nil), asynq.Queue(name)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(8 * time.Second)
		for {
			q, err := inspector.GetQueueInfo(name)
			if err != nil {
				t.Fatal(err)
			}
			if q.Retry == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("retry was not scheduled")
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		server.Shutdown()
		running = false
		if _, err := e.Queue.EnqueueContext(ctx, asynq.NewTask("queue:test", nil), asynq.Queue(name)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Queue.EnqueueContext(ctx, asynq.NewTask("queue:test", nil), asynq.Queue(name), asynq.ProcessIn(time.Hour)); err != nil {
			t.Fatal(err)
		}
		testEngine := *e
		testEngine.Registry = &Registry{Channels: map[string]Channel{name: SMSChannel{}}}
		w := httptest.NewRecorder()
		testEngine.Queues(w, httptest.NewRequest("GET", "/api/queues", nil).WithContext(ctx))
		var response struct {
			Channels map[string]QueueMetrics `json:"channels"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		q := response.Channels[name]
		if w.Code != 200 || !q.Available || q.Pending != 1 || q.Active != 0 || q.Scheduled != 1 || q.Retry != 1 || q.Outstanding != 3 {
			t.Fatalf("real queue jobs hidden: status=%d %+v", w.Code, q)
		}
	})
}
