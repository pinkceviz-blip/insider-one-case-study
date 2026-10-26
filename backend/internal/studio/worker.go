package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"
)

type Engine struct {
	Store    *Store
	Redis    *redis.Client
	Queue    *asynq.Client
	Registry *Registry
	LLM      LLM
}

func RetryDelay(n int, e error, _ *asynq.Task) time.Duration {
	var d *DeferredError
	if errors.As(e, &d) {
		return d.After
	}
	var p *ProviderError
	if errors.As(e, &p) && p.RetryAfter > 0 {
		return p.RetryAfter
	}
	if n > 7 {
		n = 7
	}
	base := time.Second * time.Duration(1<<n)
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}
func IsFailure(e error) bool {
	var d *DeferredError
	if errors.As(e, &d) {
		return false
	}
	var p *ProviderError
	return !(errors.As(e, &p) && p.Kind == Throttled)
}
func (e *Engine) RunWorkers(ctx context.Context, redisOpt asynq.RedisClientOpt, queue string, concurrency int) error {
	server := asynq.NewServer(redisOpt, asynq.Config{Concurrency: concurrency, Queues: map[string]int{queue: 1}, RetryDelayFunc: RetryDelay, IsFailure: IsFailure, ShutdownTimeout: 15 * time.Second})
	mux := asynq.NewServeMux()
	mux.HandleFunc("message:send", e.HandleDelivery)
	mux.HandleFunc("campaign:generate", e.HandleGeneration)
	if err := server.Start(mux); err != nil {
		return err
	}
	<-ctx.Done()
	server.Shutdown()
	return nil
}
func (e *Engine) HandleGeneration(ctx context.Context, t *asynq.Task) error {
	var payload struct {
		ID string `json:"campaign_id"`
	}
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return asynq.SkipRetry
	}
	_, release, lockErr := e.Store.lockOperation(ctx, "generation:"+payload.ID)
	if lockErr != nil {
		return lockErr
	}
	defer release()
	c, err := e.Store.Campaign(ctx, payload.ID)
	if err != nil {
		return err
	}
	if c.Status != "generating" {
		return nil
	}
	if err = e.Store.Event(ctx, c.ID, "generation.started", map[string]string{"status": "generating"}); err != nil {
		return err
	}
	p, err := e.LLM.Generate(ctx, c.Brief, func(field, delta string) error {
		return e.Store.Event(ctx, c.ID, "content.delta", map[string]string{"field": field, "delta": delta})
	})
	if err == nil {
		err = e.Registry.Validate(p)
	}
	if err == nil {
		err = e.Store.SaveProposal(ctx, c.ID, p)
	}
	if err != nil {
		count, _ := asynq.GetRetryCount(ctx)
		if count >= 2 {
			_, dbErr := e.Store.DB.Exec(ctx, "UPDATE campaigns SET status='failed',error=$2 WHERE id=$1 AND status='generating'", c.ID, "İçerik üretilemedi: "+err.Error())
			if dbErr != nil {
				return dbErr
			}
			if dbErr = e.Store.Event(ctx, c.ID, "generation.failed", map[string]string{"error": "İçerik üretilemedi; yeniden kampanya oluşturun."}); dbErr != nil {
				return dbErr
			}
			return nil
		}
		return err
	}
	return e.Store.Event(ctx, c.ID, "draft.ready", map[string]string{"status": "draft"})
}
func (e *Engine) Dispatch(ctx context.Context) error {
	tx, err := e.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT id,task_type,task_id,queue,payload FROM outbox WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 100")
	if err != nil {
		return err
	}
	type item struct {
		id              int64
		typ, key, queue string
		payload         []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err = rows.Scan(&it.id, &it.typ, &it.key, &it.queue, &it.payload); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, it := range items {
		retries := 8
		timeout := 20 * time.Second
		if it.typ == "campaign:generate" {
			retries = 2
			timeout = 90 * time.Second
		}
		_, err = e.Queue.EnqueueContext(ctx, asynq.NewTask(it.typ, it.payload), asynq.Queue(it.queue), asynq.TaskID(it.key), asynq.MaxRetry(retries), asynq.Timeout(timeout), asynq.Retention(24*time.Hour))
		if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE outbox SET published_at=now() WHERE id=$1", it.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (e *Engine) RunDispatcher(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	reconcile := time.NewTicker(time.Minute)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Dispatch(ctx); err != nil && ctx.Err() == nil {
				slog.Error("dispatch", "error", err)
			}
		case <-reconcile.C:
			if err := e.Reconcile(ctx); err != nil {
				slog.Error("reconcile", "error", err)
			}
		}
	}
}
func (e *Engine) Reconcile(ctx context.Context) error {
	// Durable delivery state allows reconstruction after Redis loss. The handler and provider remain idempotent.
	rows, err := e.Store.DB.Query(ctx, "SELECT id,channel FROM deliveries WHERE state IN ('pending','retrying','processing') AND updated_at<now()-interval '10 minutes' AND (next_attempt_at IS NULL OR next_attempt_at<=now()) AND (lease_until IS NULL OR lease_until<now()) LIMIT 1000")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ch string
		if err = rows.Scan(&id, &ch); err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]string{"delivery_id": id})
		_, err = e.Queue.EnqueueContext(ctx, asynq.NewTask("message:send", b), asynq.Queue(ch), asynq.TaskID("recover-"+id+"-"+time.Now().UTC().Format("200601021504")), asynq.MaxRetry(8), asynq.Timeout(20*time.Second))
		if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
			return err
		}
	}
	return rows.Err()
}

var limitScript = redis.NewScript(`
local t=redis.call('TIME');local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local cooldown=tonumber(redis.call('GET',KEYS[2]) or '0')
if cooldown>now then return cooldown-now end
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now-1000)
if redis.call('ZCARD',KEYS[1])>=100 then
 local first=redis.call('ZRANGE',KEYS[1],0,0,'WITHSCORES');return math.max(1,tonumber(first[2])+1000-now)
end
redis.call('ZADD',KEYS[1],now,ARGV[1]);redis.call('PEXPIRE',KEYS[1],2000);return 0
`)
var cooldownScript = redis.NewScript(`
local t=redis.call('TIME');local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local untilAt=now+tonumber(ARGV[1]);local old=tonumber(redis.call('GET',KEYS[1]) or '0')
if untilAt>old then redis.call('SET',KEYS[1],untilAt,'PX',ARGV[1]) end
return 1
`)

func (r *Registry) Names() []string {
	var names []string
	for n := range r.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
func (e *Engine) String() string { return fmt.Sprintf("channels: %v", e.Registry.Names()) }
