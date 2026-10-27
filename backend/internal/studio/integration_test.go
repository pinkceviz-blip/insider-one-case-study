package studio

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationPipeline(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL and TEST_REDIS_ADDR for integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schemaName := "test_" + NewID()
	quoted := pgx.Identifier{schemaName}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schemaName
	cfg.MaxConns = 64
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &Store{DB: pool}
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rd := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	defer rd.Close()
	if err = rd.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	defer rd.FlushDB(context.Background())
	opt := asynq.RedisClientOpt{Addr: addr, DB: 15}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	provider := httptest.NewServer(http.HandlerFunc(store.ProviderHandler))
	defer provider.Close()
	hp := &HTTPProvider{URL: provider.URL, Client: &http.Client{Timeout: 250 * time.Millisecond}}
	registry := &Registry{Channels: map[string]Channel{"sms": SMSChannel{}, "email": EmailChannel{}}, Providers: map[string]Provider{"sms": hp, "email": hp}}
	engine := &Engine{Store: store, Redis: rd, Queue: queue, Registry: registry, LLM: MockLLM{}}
	// More than one goroutine competes for a shared limiter.
	var wg sync.WaitGroup
	allowed := make(chan bool, 150)
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, e := limitScript.Run(ctx, rd, []string{"test:limit", "test:cooldown"}, NewID()).Int64()
			allowed <- e == nil && n == 0
		}()
	}
	wg.Wait()
	close(allowed)
	successes := 0
	for ok := range allowed {
		if ok {
			successes++
		}
	}
	if successes != 100 {
		t.Fatalf("global limiter allowed %d, want 100", successes)
	}
	servers := []*asynq.Server{}
	for _, q := range []string{"campaign", "sms", "email"} {
		srv := asynq.NewServer(opt, asynq.Config{Concurrency: 8, Queues: map[string]int{q: 1}, RetryDelayFunc: RetryDelay, IsFailure: IsFailure, ShutdownTimeout: time.Second})
		mux := asynq.NewServeMux()
		mux.HandleFunc("campaign:generate", engine.HandleGeneration)
		mux.HandleFunc("message:send", engine.HandleDelivery)
		if err = srv.Start(mux); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, srv)
	}
	defer func() {
		for _, srv := range servers {
			srv.Shutdown()
		}
	}()
	dispatchCtx, stopDispatch := context.WithCancel(ctx)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); engine.RunDispatcher(dispatchCtx) }()
	defer func() { stopDispatch(); <-dispatchDone }()
	api := httptest.NewServer(engine.Handler("mock"))
	defer api.Close()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, api.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		r, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		out, _ := io.ReadAll(r.Body)
		if r.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, r.StatusCode, out)
		}
		return out
	}
	request("POST", "/api/campaigns", map[string]string{"brief": "x"}, 422)
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(request("POST", "/api/campaigns", map[string]string{"brief": "İstanbul son 4 gün %30 indirim"}, 202), &created)
	var campaign Campaign
	for {
		campaign, err = store.Campaign(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if campaign.Status == "draft" {
			break
		}
		if campaign.Status == "failed" {
			t.Fatal(campaign.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatal("generation timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if campaign.Audience.Total == 0 {
		t.Fatal("empty audience")
	}
	// No messages may leave before explicit approval.
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM provider_requests").Scan(&n)
	if n != 0 {
		t.Fatal("sent before approval")
	}
	content := campaign.Proposal.Content
	content.SMS = strings.Repeat("a", 161)
	request("PATCH", "/api/campaigns/"+created.ID, map[string]any{"version": campaign.Version, "content": content}, 422)
	request("PATCH", "/api/campaigns/"+created.ID, map[string]any{"version": campaign.Version + 1, "content": campaign.Proposal.Content}, 409)
	// Revoke one snapshotted recipient before approval.
	_, err = pool.Exec(ctx, "UPDATE consents SET allowed=false WHERE user_id=(SELECT user_id FROM recipients WHERE campaign_id=$1 LIMIT 1)", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]int{"version": campaign.Version}
	request("POST", "/api/campaigns/"+created.ID+"/approve", body, 202)
	request("POST", "/api/campaigns/"+created.ID+"/approve", body, 202)
	request("PATCH", "/api/campaigns/"+created.ID, map[string]any{"version": campaign.Version, "content": campaign.Proposal.Content}, 409)
	for {
		campaign, err = store.Campaign(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if campaign.Status == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("delivery timed out: %+v", campaign.Counts)
		case <-time.After(200 * time.Millisecond):
		}
	}
	var accepted, unique int
	if err = pool.QueryRow(ctx, "SELECT count(*),count(DISTINCT idempotency_key) FROM provider_requests").Scan(&accepted, &unique); err != nil {
		t.Fatal(err)
	}
	if accepted != unique || accepted != campaign.Counts["accepted"] || accepted == 0 {
		t.Fatalf("provider dedup mismatch accepted=%d unique=%d counts=%v", accepted, unique, campaign.Counts)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM deliveries d JOIN consents c ON c.user_id=d.user_id AND c.channel=d.channel WHERE NOT c.allowed").Scan(&n)
	if n != 0 {
		t.Fatal("delivery created for revoked consent")
	}
	// Replaying a completed task is harmless.
	var id string
	pool.QueryRow(ctx, "SELECT id FROM deliveries WHERE state='accepted' LIMIT 1").Scan(&id)
	b, _ := json.Marshal(map[string]string{"delivery_id": id})
	if err = engine.HandleDelivery(ctx, asynq.NewTask("message:send", b)); err != nil {
		t.Fatal(err)
	}
	request("GET", "/health/ready", nil, 200)
	// Channel summaries must partition the business totals, including retries.
	var all, sms, email map[string]int
	json.Unmarshal(request("GET", "/api/stats", nil, 200), &all)
	json.Unmarshal(request("GET", "/api/stats?channel=sms", nil, 200), &sms)
	json.Unmarshal(request("GET", "/api/stats?channel=email", nil, 200), &email)
	for _, key := range []string{"accepted", "failed", "pending", "processing", "retrying", "suppressed", "retry_attempts", "provider_accepted", "outbox_pending"} {
		if sms[key]+email[key] != all[key] {
			t.Fatalf("channel totals mismatch for %s: sms=%d email=%d all=%d", key, sms[key], email[key], all[key])
		}
	}
	request("GET", "/api/stats?channel=invalid", nil, 400)
	// Persistent SSE cursor and snapshot.
	req, _ := http.NewRequestWithContext(ctx, "GET", api.URL+"/api/campaigns/"+created.ID+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 4096)
	read, _ := res.Body.Read(chunk)
	res.Body.Close()
	if !strings.Contains(string(chunk[:read]), "event:") {
		t.Fatal("SSE missing")
	}
	var audit struct {
		Items []AttemptView `json:"items"`
	}
	if err = json.Unmarshal(request("GET", "/api/errors?campaign_id="+created.ID, nil, 200), &audit); err != nil || len(audit.Items) == 0 {
		t.Fatal("error history missing", err)
	}
	request("GET", "/api/deliveries/"+id+"/attempts", nil, 200)
	exerciseRaceSafety(t, ctx, engine, created.ID)
	t.Logf("pipeline: %d accepted, %d failed; duplicate approval and task replay safe", accepted, campaign.Counts["failed"])
	// Direct provider retry after an ambiguous timeout: find a deterministic key.
	key := ""
	for i := 0; i < 10000; i++ {
		candidate := fmt.Sprintf("timeout-%d", i)
		h := providerHash(candidate)
		if h%13 == 0 && h%7 != 0 && h%11 != 0 && h%37 != 0 {
			key = candidate
			break
		}
	}
	msg := Message{Channel: "sms", Address: "fake", Body: "hello"}
	_, err = hp.Send(ctx, key, msg)
	if err == nil {
		t.Fatal("expected ambiguous timeout")
	}
	a, err := hp.Send(ctx, key, msg)
	if err != nil || a == "" {
		t.Fatal("retry should return prior receipt", err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM provider_requests WHERE idempotency_key=$1", key).Scan(&n)
	if n != 1 {
		t.Fatal("provider accepted twice")
	}
	stopDispatch()
	<-dispatchDone
	for _, server := range servers {
		server.Shutdown()
	}
	servers = nil
	exerciseAcceptance(t, ctx, engine, created.ID, opt, api.URL)
}

func providerHash(key string) int { h := sha256.Sum256([]byte(key)); return int(h[0]) }
