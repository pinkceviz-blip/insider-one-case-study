package studio

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func auditFixture(t *testing.T) (context.Context, *Engine, asynq.RedisClientOpt) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("integration environment required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := "audit_" + NewID()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	cfg.MaxConns = 32
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); admin.Close() })
	s := &Store{DB: db}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	addr := os.Getenv("TEST_REDIS_ADDR")
	opt := asynq.RedisClientOpt{Addr: addr, DB: 15}
	rd := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	if err = rd.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	q := asynq.NewClient(opt)
	t.Cleanup(func() { q.Close(); rd.FlushDB(context.Background()); rd.Close() })
	reg := &Registry{Channels: map[string]Channel{"sms": SMSChannel{}, "email": EmailChannel{}}, Providers: map[string]Provider{}}
	return ctx, &Engine{Store: s, Redis: rd, Queue: q, Registry: reg, LLM: MockLLM{}}, opt
}
func auditCampaign(t *testing.T, ctx context.Context, e *Engine, city string) string {
	t.Helper()
	id := NewID()
	p, _ := json.Marshal(Proposal{Segment: Segment{city, 30, true}, Content: Content{"Hi {{first_name}}", "Subject", "Body"}})
	if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief,status,proposal,approved_at) VALUES($1,'İstanbul hedefle','approved',$2,now())", id, p); err != nil {
		t.Fatal(err)
	}
	return id
}
func auditDelivery(t *testing.T, ctx context.Context, e *Engine, campaign, ch string) (string, int64, *asynq.Task) {
	t.Helper()
	var user int64
	if err := e.Store.DB.QueryRow(ctx, "SELECT u.id FROM users u JOIN consents c ON c.user_id=u.id AND c.channel=$1 AND c.allowed WHERE NOT EXISTS(SELECT 1 FROM deliveries d WHERE d.campaign_id=$2 AND d.user_id=u.id AND d.channel=$1) LIMIT 1", ch, campaign).Scan(&user); err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if _, err := e.Store.DB.Exec(ctx, "INSERT INTO deliveries(id,campaign_id,version,user_id,channel) VALUES($1,$2,1,$3,$4)", id, campaign, user, ch); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"delivery_id": id})
	return id, user, asynq.NewTask("message:send", b)
}
func TestAuditLLMInvalidMatrix(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	valid := `{"segment":{"city":"İstanbul","days":30,"exclude_purchased":true},"content":{"sms":"Hi","email_subject":"Subject","email_body":"Body"}}`
	cases := map[string]string{"malformed": "{", "unknown_field": strings.Replace(valid, `"days":30`, `"sql":"DROP TABLE users","days":30`, 1), "wrong_type": strings.Replace(valid, `"days":30`, `"days":"30"`, 1), "missing_city": strings.Replace(valid, `"city":"İstanbul",`, "", 1), "null_days": strings.Replace(valid, `"days":30`, `"days":null`, 1), "null_content": `{"segment":{"city":"İstanbul","days":30,"exclude_purchased":true},"content":null}`, "operator": strings.Replace(valid, `"days":30`, `"operator":"OR","days":30`, 1), "unsupported_filter": strings.Replace(valid, `"city":"İstanbul"`, `"city":"Bursa"`, 1), "null_root": "null"}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			id := NewID()
			if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief) VALUES($1,'bad output')", id); err != nil {
				t.Fatal(err)
			}
			copy := *e
			copy.LLM = responseLLM{raw: raw}
			b, _ := json.Marshal(map[string]string{"campaign_id": id})
			err := copy.HandleGeneration(ctx, asynq.NewTask("campaign:generate", b))
			if err == nil {
				t.Fatal("invalid LLM accepted")
			}
			var recipients, deliveries int
			e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM recipients WHERE campaign_id=$1", id).Scan(&recipients)
			e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE campaign_id=$1", id).Scan(&deliveries)
			if recipients != 0 || deliveries != 0 {
				t.Fatal("invalid rule reached audience/delivery")
			}
			t.Logf("rejected=%v recipients=%d deliveries=%d", err, recipients, deliveries)
		})
	}
}
func TestAuditSemanticMismatchBlocked(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	id := NewID()
	if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief) VALUES($1,'Sadece İstanbul kullanıcıları')", id); err != nil {
		t.Fatal(err)
	}
	p := Proposal{Segment: Segment{"Ankara", 30, true}, Content: Content{"Hi", "Subject", "Body"}}
	e.LLM = responseLLM{proposal: p}
	b, _ := json.Marshal(map[string]string{"campaign_id": id})
	if err := e.HandleGeneration(ctx, asynq.NewTask("campaign:generate", b)); err == nil {
		t.Fatal("semantic mismatch accepted")
	}
	var recipients int
	if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM recipients WHERE campaign_id=$1", id).Scan(&recipients); err != nil || recipients != 0 {
		t.Fatal(recipients, err)
	}
	// A legacy invalid draft must also be blocked at the HTTP approval boundary.
	raw, _ := json.Marshal(p)
	if _, err := e.Store.DB.Exec(ctx, "UPDATE campaigns SET status='draft',proposal=$2 WHERE id=$1", id, raw); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/campaigns/"+id+"/approve", strings.NewReader(`{"version":1}`))
	w := httptest.NewRecorder()
	e.Handler("mock").ServeHTTP(w, req)
	if w.Code != 422 {
		t.Fatal("legacy draft approved", w.Code, w.Body.String())
	}
}

// Intentionally offers neither idempotency nor ReceiptLookup.
type auditUnsafeProvider struct{ calls int }

func (p *auditUnsafeProvider) Send(ctx context.Context, key string, msg Message) (string, error) {
	p.calls++
	if p.calls == 1 {
		return "", &ProviderError{Kind: Transient, Code: "timeout"}
	}
	return "second-receipt", nil
}
func TestAuditNoProviderIdempotencyStopsAmbiguousRetry(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	id, user, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &auditUnsafeProvider{}
	e.Registry.Providers["sms"] = p
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var stamp *time.Time
	if err := e.Store.DB.QueryRow(ctx, "SELECT state FROM deliveries WHERE id=$1", id).Scan(&state); err != nil || state != "unknown" || p.calls != 1 {
		t.Fatal(state, p.calls, err)
	}
	if err := e.Store.DB.QueryRow(ctx, "SELECT last_sms_sent_at FROM users WHERE id=$1", user).Scan(&stamp); err != nil || stamp != nil {
		t.Fatal("unconfirmed sent timestamp", err)
	}
	c, err := e.Store.Campaign(ctx, cid)
	if err != nil || c.Status != "needs_review" {
		t.Fatal(c.Status, err)
	}
	var n int
	if err = e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM attempts WHERE delivery_id=$1 AND outcome='unknown'", id).Scan(&n); err != nil || n != 1 {
		t.Fatal("unknown audit missing", n, err)
	}
}

func TestAuditCrashChild(t *testing.T) {
	if os.Getenv("AUDIT_CRASH_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("AUDIT_DB"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = os.Getenv("AUDIT_SCHEMA")
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{DB: pool}
	defer s.DB.Close()
	rd := redis.NewClient(&redis.Options{Addr: os.Getenv("TEST_REDIS_ADDR"), DB: 15})
	defer rd.Close()
	p := &HTTPProvider{URL: os.Getenv("AUDIT_PROVIDER"), Client: &http.Client{Timeout: 12 * time.Second}}
	e := &Engine{Store: s, Redis: rd, Registry: &Registry{Channels: map[string]Channel{"sms": SMSChannel{}}, Providers: map[string]Provider{"sms": p}}}
	b, _ := json.Marshal(map[string]string{"delivery_id": os.Getenv("AUDIT_DELIVERY")})
	if err = e.HandleDelivery(ctx, asynq.NewTask("message:send", b)); err != nil {
		t.Fatal(err)
	}
}
func TestAuditSIGKILLAfterProviderAcceptance(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	id, _, task := auditDelivery(t, ctx, e, cid, "sms")
	accepted := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	receipt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == "GET" {
			found := receipt
			mu.Unlock()
			if !found {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, map[string]string{"id": "confirmed"})
			return
		}
		calls++
		receipt = true
		mu.Unlock()
		accepted <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeJSON(w, 200, map[string]string{"id": "confirmed"})
	}))
	defer func() { close(release); server.Close() }()
	cfg := e.Store.DB.Config().ConnConfig.Copy()
	dburl := cfg.ConnString()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), "AUDIT_CRASH_CHILD=1", "AUDIT_DB="+dburl, "AUDIT_SCHEMA="+e.Store.DB.Config().ConnConfig.RuntimeParams["search_path"], "AUDIT_PROVIDER="+server.URL, "AUDIT_DELIVERY="+id)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	select {
	case <-accepted:
	case <-time.After(15 * time.Second):
		t.Fatal("child never reached provider")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	e.Registry.Providers["sms"] = &HTTPProvider{URL: server.URL, Client: &http.Client{Timeout: time.Second}}
	var err error
	for i := 0; i < 30; i++ {
		err = e.HandleDelivery(ctx, task)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err = e.Store.DB.QueryRow(ctx, "SELECT state FROM deliveries WHERE id=$1", id).Scan(&state); err != nil || state != "accepted" {
		t.Fatal(state, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatal("duplicate external send", calls)
	}
	t.Log("SIGKILL accepted-before-response: receipt recovered, one provider send, state accepted")
}

type auditTimedProvider struct {
	mu    sync.Mutex
	times map[string][]time.Time
	keys  map[string]bool
}

func (p *auditTimedProvider) SupportsIdempotency() bool { return true }
func (p *auditTimedProvider) Send(ctx context.Context, key string, msg Message) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.times[msg.Channel] = append(p.times[msg.Channel], time.Now())
	p.keys[key] = true
	return "r-" + key, nil
}
func TestAuditMultipleWorkersThroughput(t *testing.T) {
	ctx, e, opt := auditFixture(t)
	for _, concurrency := range []int{1, 8} {
		t.Run(fmt.Sprintf("two_instances_per_channel_concurrency_%d", concurrency), func(t *testing.T) {
			if err := e.Redis.FlushDB(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			cid := auditCampaign(t, ctx, e, "İstanbul")
			p := &auditTimedProvider{times: map[string][]time.Time{}, keys: map[string]bool{}}
			e.Registry.Providers = map[string]Provider{"sms": p, "email": p}
			const perChannel = 120
			for _, ch := range []string{"sms", "email"} {
				for i := 0; i < perChannel; i++ {
					_, _, task := auditDelivery(t, ctx, e, cid, ch)
					if _, err := e.Queue.EnqueueContext(ctx, task, asynq.Queue("audit-"+ch), asynq.TaskID(fmt.Sprintf("%s-%s-%d", cid, ch, i)), asynq.MaxRetry(8)); err != nil {
						t.Fatal(err)
					}
				}
			}
			var servers []*asynq.Server
			defer func() {
				for _, s := range servers {
					s.Shutdown()
				}
			}()
			start := time.Now()
			for _, ch := range []string{"sms", "email"} {
				for i := 0; i < 2; i++ {
					s := asynq.NewServer(opt, asynq.Config{Concurrency: concurrency, Queues: map[string]int{"audit-" + ch: 1}, RetryDelayFunc: RetryDelay, IsFailure: IsFailure, ShutdownTimeout: time.Second})
					mux := asynq.NewServeMux()
					mux.HandleFunc("message:send", e.HandleDelivery)
					if err := s.Start(mux); err != nil {
						t.Fatal(err)
					}
					servers = append(servers, s)
				}
			}
			deadline := time.Now().Add(40 * time.Second)
			for {
				var n int
				if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE campaign_id=$1 AND state='accepted'", cid).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n == 2*perChannel {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("throughput timed out", n)
				}
				time.Sleep(50 * time.Millisecond)
			}
			elapsed := time.Since(start)
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, ch := range []string{"sms", "email"} {
				times := p.times[ch]
				sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
				max := 0
				left := 0
				for right := range times {
					for times[right].Sub(times[left]) >= time.Second {
						left++
					}
					if n := right - left + 1; n > max {
						max = n
					}
				}
				t.Logf("channel=%s instances=2 concurrency=%d requests=%d max_rolling_second=%d elapsed=%s", ch, concurrency, len(times), max, elapsed)
				if len(times) != perChannel {
					t.Error("duplicate/missing sends", ch, len(times))
				}
				if max > 100 {
					t.Errorf("provider rolling window exceeded: %s=%d", ch, max)
				}
			}
		})
	}
}
func TestAuditBoundedCapacity(t *testing.T) {
	ctx, e, opt := auditFixture(t)
	for _, size := range []int{5000, 50000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			if size > 5000 {
				if _, err := e.Store.DB.Exec(ctx, `INSERT INTO users(first_name,email,phone,city,cart_at,purchased) SELECT 'Load','load'||i||'@example.invalid','load-'||i,'İstanbul',now()-interval '1 day',false FROM generate_series(5001,50000)i; INSERT INTO consents SELECT id,'sms',true FROM users ON CONFLICT DO NOTHING; INSERT INTO consents SELECT id,'email',true FROM users ON CONFLICT DO NOTHING; ANALYZE users;`); err != nil {
					t.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var plan string
			if err := e.Store.DB.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM users WHERE city=$1 AND cart_at>=now()-interval '30 days' AND NOT purchased`, "İstanbul").Scan(&plan); err != nil {
				t.Fatal(err)
			}
			id, err := e.Store.Create(ctx, "İstanbul load test")
			if err != nil {
				t.Fatal(err)
			}
			p := Proposal{Segment: Segment{"İstanbul", 30, true}, Content: Content{"Hello", "Subject", "Body"}}
			start := time.Now()
			if err = e.Store.SaveProposal(ctx, id, p); err != nil {
				t.Fatal(err)
			}
			segmentTime := time.Since(start)
			start = time.Now()
			if err = e.Store.Approve(ctx, id, 1, []string{"sms", "email"}); err != nil {
				t.Fatal(err)
			}
			approveTime := time.Since(start)
			var deliveries int
			e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE campaign_id=$1", id).Scan(&deliveries)
			runtime.ReadMemStats(&after)
			t.Logf("users=%d segment_snapshot=%s approval=%s deliveries=%d go_heap_before=%d go_heap_after=%d pool_max=%d acquired=%d explain=%s", size, segmentTime, approveTime, deliveries, before.HeapAlloc, after.HeapAlloc, e.Store.DB.Config().MaxConns, e.Store.DB.Stat().AcquiredConns(), plan)
		})
	}
	// Bounded dispatch sample, no worker sends. Remaining work stays in isolated outbox.
	before, _ := e.Redis.Info(ctx, "memory").Result()
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := e.Dispatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	var depth int
	for _, ch := range []string{"sms", "email"} {
		q, err := inspector.GetQueueInfo(ch)
		if err == nil {
			depth += q.Pending
		}
	}
	after, _ := e.Redis.Info(ctx, "memory").Result()
	t.Logf("bounded_dispatch_elapsed=%s queue_depth=%d redis_before=%s redis_after=%s", elapsed, depth, before, after)
}

func TestAuditConsentTransactionRace(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	_, user, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &controlledProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	e.Registry.Providers["sms"] = p
	done := make(chan error, 1)
	go func() { done <- e.HandleDelivery(ctx, task) }()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider not started")
	}
	revokeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	_, err := e.Store.DB.Exec(revokeCtx, "UPDATE consents SET allowed=false WHERE user_id=$1 AND channel='sms'", user)
	cancel()
	if err == nil {
		t.Fatal("consent revocation committed during authorized send")
	}
	close(p.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.DB.Exec(ctx, "UPDATE consents SET allowed=false WHERE user_id=$1 AND channel='sms'", user); err != nil {
		t.Fatal(err)
	}
	second := auditCampaign(t, ctx, e, "İstanbul")
	id := NewID()
	if _, err = e.Store.DB.Exec(ctx, "INSERT INTO deliveries(id,campaign_id,version,user_id,channel) VALUES($1,$2,1,$3,'sms')", id, second, user); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"delivery_id": id})
	if err = e.HandleDelivery(ctx, asynq.NewTask("message:send", b)); err != nil {
		t.Fatal(err)
	}
	var state string
	e.Store.DB.QueryRow(ctx, "SELECT state FROM deliveries WHERE id=$1", id).Scan(&state)
	if state != "suppressed" || p.count() != 1 {
		t.Fatal(state, p.count())
	}
	t.Log("revocation blocked while send held FOR SHARE; committed revocation suppresses next queued delivery")
}
func TestAuditFailuresExhaustedAndRedisUnavailable(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	id, _, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &controlledProvider{}
	e.Registry.Providers["sms"] = p
	if _, err := e.Store.DB.Exec(ctx, "UPDATE deliveries SET failure_count=9 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	var state, code string
	e.Store.DB.QueryRow(ctx, "SELECT state,error FROM deliveries WHERE id=$1", id).Scan(&state, &code)
	if state != "failed" || code != "attempts_exhausted" || p.count() != 0 {
		t.Fatal(state, code, p.count())
	}
	_, _, task = auditDelivery(t, ctx, e, cid, "sms")
	bad := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	defer bad.Close()
	copy := *e
	copy.Redis = bad
	if copy.HandleDelivery(ctx, task) == nil {
		t.Fatal("missing defer on Redis failure")
	}
	if p.count() != 0 {
		t.Fatal("limiter outage sent message")
	}
	t.Log("failure_count=9 becomes failed without provider call; unavailable Redis limiter fails closed")
}

type auditCountLLM struct {
	mu    sync.Mutex
	calls int
}

func (p *auditCountLLM) Generate(ctx context.Context, brief string, emit func(string, string) error) (Proposal, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Proposal{}, fmt.Errorf("controlled malformed LLM response")
}
func TestAuditLLMRetryTerminalError(t *testing.T) {
	ctx, e, opt := auditFixture(t)
	id := NewID()
	if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief) VALUES($1,'LLM retry test')", id); err != nil {
		t.Fatal(err)
	}
	llm := &auditCountLLM{}
	e.LLM = llm
	s := asynq.NewServer(opt, asynq.Config{Concurrency: 1, Queues: map[string]int{"llm-audit": 1}, RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return 10 * time.Millisecond }, ShutdownTimeout: time.Second})
	mux := asynq.NewServeMux()
	mux.HandleFunc("campaign:generate", e.HandleGeneration)
	if err := s.Start(mux); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()
	b, _ := json.Marshal(map[string]string{"campaign_id": id})
	if _, err := e.Queue.EnqueueContext(ctx, asynq.NewTask("campaign:generate", b), asynq.Queue("llm-audit"), asynq.MaxRetry(2)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(35 * time.Second)
	for {
		c, err := e.Store.Campaign(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if c.Status == "failed" {
			if !strings.Contains(c.Error, "İçerik üretilemedi") {
				t.Fatal("missing controlled error")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal failure never persisted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	llm.mu.Lock()
	calls := llm.calls
	llm.mu.Unlock()
	if calls != 3 {
		t.Fatal("want initial + 2 retries", calls)
	}
	var n int
	e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM events WHERE campaign_id=$1 AND kind='generation.failed'", id).Scan(&n)
	if n != 1 {
		t.Fatal("SSE failure event missing", n)
	}
	t.Log("3 LLM invocations, failed campaign and persisted generation.failed event; production backoff tested separately (this test accelerates scheduling only)")
}
func TestAuditDifferentQueueJobsSameDelivery(t *testing.T) {
	ctx, e, opt := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	id, _, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &controlledProvider{}
	e.Registry.Providers["sms"] = p
	for _, taskID := range []string{"original", "duplicate"} {
		if _, err := e.Queue.EnqueueContext(ctx, task, asynq.Queue("duplicate-audit"), asynq.TaskID(taskID), asynq.MaxRetry(8)); err != nil {
			t.Fatal(err)
		}
	}
	var servers []*asynq.Server
	defer func() {
		for _, s := range servers {
			s.Shutdown()
		}
	}()
	for i := 0; i < 2; i++ {
		s := asynq.NewServer(opt, asynq.Config{Concurrency: 4, Queues: map[string]int{"duplicate-audit": 1}, RetryDelayFunc: RetryDelay, IsFailure: IsFailure, ShutdownTimeout: time.Second})
		mux := asynq.NewServeMux()
		mux.HandleFunc("message:send", e.HandleDelivery)
		if err := s.Start(mux); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, s)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		q, err := inspector.GetQueueInfo("duplicate-audit")
		if err != nil {
			t.Fatal(err)
		}
		if q.Pending+q.Active+q.Retry+q.Scheduled == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("duplicate job not settled")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if p.count() != 1 {
		t.Fatal("duplicate external send", p.count())
	}
	var n int
	e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE id=$1", id).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	if p.count() != 1 {
		t.Fatal("replay resent")
	}
	t.Log("two independent queue task IDs, two Asynq worker instances, one delivery and one provider call; terminal replay safe")
}

func TestAuditCampaignExpiry(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	e.CampaignTTL = 24 * time.Hour
	cid := auditCampaign(t, ctx, e, "İstanbul")
	_, _, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &controlledProvider{}
	e.Registry.Providers["sms"] = p
	if _, err := e.Store.DB.Exec(ctx, "UPDATE campaigns SET approved_at=now()-interval '25 hours' WHERE id=$1", cid); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	var code string
	e.Store.DB.QueryRow(ctx, "SELECT error FROM deliveries WHERE campaign_id=$1", cid).Scan(&code)
	if code != "campaign_expired" || p.count() != 0 {
		t.Fatal(code, p.count())
	}
	t.Log("25-hour-old unsent campaign becomes campaign_expired without provider send")
}

func TestDefaultLifetimeDoesNotExpireLargeBacklog(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	_, _, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &controlledProvider{}
	e.Registry.Providers["sms"] = p
	if _, err := e.Store.DB.Exec(ctx, "UPDATE campaigns SET approved_at=now()-interval '48 hours' WHERE id=$1", cid); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	if p.count() != 1 {
		t.Fatal("default policy expired backlog")
	}
}
func TestUnsafeProviderInterruptedAttemptNeedsReview(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	id, _, task := auditDelivery(t, ctx, e, cid, "sms")
	p := &auditUnsafeProvider{}
	e.Registry.Providers["sms"] = p
	payload, _ := json.Marshal(Message{Channel: "sms", Address: "fake", Body: "persisted"})
	if _, err := e.Store.DB.Exec(ctx, "UPDATE deliveries SET state='processing',payload=$2,attempts=1 WHERE id=$1", id, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.DB.Exec(ctx, "INSERT INTO attempts(delivery_id,attempt_no,outcome,kind,code) VALUES($1,1,'inflight','','')", id); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleDelivery(ctx, task); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := e.Store.DB.QueryRow(ctx, "SELECT state FROM deliveries WHERE id=$1", id).Scan(&state); err != nil || state != "unknown" || p.calls != 0 {
		t.Fatal(state, p.calls, err)
	}
}
