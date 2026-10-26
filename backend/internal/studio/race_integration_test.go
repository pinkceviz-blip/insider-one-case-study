package studio

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hibiken/asynq"
	"sync"
	"testing"
	"time"
)

type controlledProvider struct {
	mu       sync.Mutex
	calls    int
	receipts map[string]Message
	started  chan struct{}
	release  chan struct{}
	fail     error
	cancel   context.CancelFunc
}

func (p *controlledProvider) Send(ctx context.Context, key string, msg Message) (string, error) {
	p.mu.Lock()
	p.calls++
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	p.mu.Unlock()
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return "", p.fail
	}
	if p.receipts == nil {
		p.receipts = map[string]Message{}
	}
	if old, ok := p.receipts[key]; ok && old != msg {
		return "", errors.New("payload changed across retries")
	}
	p.receipts[key] = msg
	if p.cancel != nil {
		p.cancel()
		return "", &ProviderError{Kind: Transient, Code: "timeout"}
	}
	return "receipt-" + key, nil
}
func (p *controlledProvider) Lookup(ctx context.Context, key string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, found := p.receipts[key]
	return "receipt-" + key, found, nil
}
func (p *controlledProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func exerciseRaceSafety(t *testing.T, ctx context.Context, base *Engine, campaignID string) {
	fixture := func(channel string, p *controlledProvider) (*Engine, string, int64, *asynq.Task) {
		t.Helper()
		id := NewID()
		var user int64
		err := base.Store.DB.QueryRow(ctx, "INSERT INTO users(first_name,email,phone,city,cart_at,purchased) VALUES('Test',$1,$2,'İstanbul',now(),false) RETURNING id", id+"@example.invalid", id).Scan(&user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = base.Store.DB.Exec(ctx, "INSERT INTO consents(user_id,channel,allowed) VALUES($1,$2,true)", user, channel); err != nil {
			t.Fatal(err)
		}
		if _, err = base.Store.DB.Exec(ctx, "INSERT INTO deliveries(id,campaign_id,version,user_id,channel) VALUES($1,$2,1,$3,$4)", id, campaignID, user, channel); err != nil {
			t.Fatal(err)
		}
		registry := &Registry{Channels: base.Registry.Channels, Providers: map[string]Provider{channel: p}}
		e := &Engine{Store: base.Store, Redis: base.Redis, Registry: registry}
		b, _ := json.Marshal(map[string]string{"delivery_id": id})
		return e, id, user, asynq.NewTask("message:send", b)
	}
	timestamps := func(user int64) (*time.Time, *time.Time) {
		t.Helper()
		var sms, email *time.Time
		if err := base.Store.DB.QueryRow(ctx, "SELECT last_sms_sent_at,last_email_sent_at FROM users WHERE id=$1", user).Scan(&sms, &email); err != nil {
			t.Fatal(err)
		}
		return sms, email
	}
	t.Run("concurrent_duplicate_and_success_timestamp", func(t *testing.T) {
		p := &controlledProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
		e, id, user, task := fixture("sms", p)
		result := make(chan error, 1)
		go func() { result <- e.HandleDelivery(ctx, task) }()
		select {
		case <-p.started:
		case <-time.After(5 * time.Second):
			t.Fatal("send did not start")
		}
		sms, email := timestamps(user)
		if sms != nil || email != nil {
			t.Fatal("timestamp written before provider validated")
		}
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- e.HandleDelivery(ctx, task) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			var d *DeferredError
			if !errors.As(err, &d) {
				t.Fatalf("busy delivery must defer, got %v", err)
			}
		}
		if p.count() != 1 {
			t.Fatal("duplicate provider calls while lock held")
		}
		close(p.release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		sms, email = timestamps(user)
		if sms == nil || email != nil {
			t.Fatal("wrong channel timestamp")
		}
		before := *sms
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
		sms, _ = timestamps(user)
		if !sms.Equal(before) || p.count() != 1 {
			t.Fatal("task replay changed timestamp or resent")
		}
		var n int
		if err := base.Store.DB.QueryRow(ctx, "SELECT count(*) FROM attempts WHERE delivery_id=$1 AND outcome='accepted'", id).Scan(&n); err != nil || n != 1 {
			t.Fatal("acceptance ledger duplicated", err, n)
		}
	})
	t.Run("persisted_retry_deadline_and_no_failure_timestamp", func(t *testing.T) {
		p := &controlledProvider{fail: &ProviderError{Kind: Transient, Code: "503", HTTPStatus: 503}}
		e, id, user, task := fixture("email", p)
		if err := e.HandleDelivery(ctx, task); err == nil {
			t.Fatal("retry missing")
		}
		if err := e.HandleDelivery(ctx, task); err == nil {
			t.Fatal("early duplicate must defer")
		}
		if p.count() != 1 {
			t.Fatal("duplicate bypassed durable retry deadline")
		}
		sms, email := timestamps(user)
		if sms != nil || email != nil {
			t.Fatal("failed attempt wrote sent timestamp")
		}
		var failures int
		var next *time.Time
		if err := base.Store.DB.QueryRow(ctx, "SELECT failure_count,next_attempt_at FROM deliveries WHERE id=$1", id).Scan(&failures, &next); err != nil || failures != 1 || next == nil {
			t.Fatal("failure and deadline not persisted", err)
		}
	})
	t.Run("throttle_does_not_consume_failure_budget", func(t *testing.T) {
		p := &controlledProvider{fail: &ProviderError{Kind: Throttled, Code: "429", HTTPStatus: 429, RetryAfter: time.Millisecond}}
		e, id, user, task := fixture("email", p)
		if err := e.HandleDelivery(ctx, task); err == nil {
			t.Fatal("429 not deferred")
		}
		var failures int
		if err := base.Store.DB.QueryRow(ctx, "SELECT failure_count FROM deliveries WHERE id=$1", id).Scan(&failures); err != nil || failures != 0 {
			t.Fatal("429 consumed budget", err)
		}
		sms, email := timestamps(user)
		if sms != nil || email != nil {
			t.Fatal("429 wrote timestamp")
		}
	})
	t.Run("accepted_then_worker_cancel_receipt_recovery", func(t *testing.T) {
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		p := &controlledProvider{cancel: cancel}
		e, id, user, task := fixture("email", p)
		if err := e.HandleDelivery(child, task); err == nil {
			t.Fatal("cancelled final commit should fail")
		}
		sms, email := timestamps(user)
		if sms != nil || email != nil {
			t.Fatal("uncommitted outcome wrote timestamp")
		}
		// A different task/recovery worker must reconcile receipt, even at retry limit.
		if _, err := base.Store.DB.Exec(ctx, "UPDATE deliveries SET failure_count=9 WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
		sms, email = timestamps(user)
		if sms != nil || email == nil || p.count() != 1 {
			t.Fatal("receipt recovery resent or did not stamp email")
		}
		var state string
		if err := base.Store.DB.QueryRow(ctx, "SELECT state FROM deliveries WHERE id=$1", id).Scan(&state); err != nil || state != "accepted" {
			t.Fatal("known receipt was marked failed", state, err)
		}
	})
	t.Run("revoked_consent_and_permanent_failure", func(t *testing.T) {
		p := &controlledProvider{}
		e, _, user, task := fixture("sms", p)
		if _, err := base.Store.DB.Exec(ctx, "UPDATE consents SET allowed=false WHERE user_id=$1", user); err != nil {
			t.Fatal(err)
		}
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
		sms, email := timestamps(user)
		if p.count() != 0 || sms != nil || email != nil {
			t.Fatal("revoked user was sent or stamped")
		}
		p = &controlledProvider{fail: &ProviderError{Kind: Permanent, Code: "422", HTTPStatus: 422}}
		e, _, user, task = fixture("sms", p)
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := e.HandleDelivery(ctx, task); err != nil {
			t.Fatal(err)
		}
		sms, email = timestamps(user)
		if p.count() != 1 || sms != nil || email != nil {
			t.Fatal("permanent failure retried or stamped")
		}
	})

	t.Run("same_user_parallel_sms_email_preserves_both_dates", func(t *testing.T) {
		p := &controlledProvider{}
		e, _, user, smsTask := fixture("sms", p)
		emailID := NewID()
		if _, err := base.Store.DB.Exec(ctx, "INSERT INTO consents(user_id,channel,allowed) VALUES($1,'email',true)", user); err != nil {
			t.Fatal(err)
		}
		if _, err := base.Store.DB.Exec(ctx, "INSERT INTO deliveries(id,campaign_id,version,user_id,channel) VALUES($1,$2,1,$3,'email')", emailID, campaignID, user); err != nil {
			t.Fatal(err)
		}
		e.Registry = &Registry{Channels: base.Registry.Channels, Providers: map[string]Provider{"sms": p, "email": p}}
		b, _ := json.Marshal(map[string]string{"delivery_id": emailID})
		emailTask := asynq.NewTask("message:send", b)
		errs := make(chan error, 2)
		go func() { errs <- e.HandleDelivery(ctx, smsTask) }()
		go func() { errs <- e.HandleDelivery(ctx, emailTask) }()
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		sms, email := timestamps(user)
		if sms == nil || email == nil || p.count() != 2 {
			t.Fatal("concurrent channels lost a timestamp")
		}
	})
	t.Run("parallel_approval_does_not_duplicate_deliveries", func(t *testing.T) {
		proposal, _ := json.Marshal(Proposal{Segment: Segment{"İstanbul", 4, true}, Content: Content{"hello", "subject", "body"}})
		id := NewID()
		if _, err := base.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief,status,proposal) VALUES($1,'race test','draft',$2)", id, proposal); err != nil {
			t.Fatal(err)
		}
		if _, err := base.Store.DB.Exec(ctx, "INSERT INTO recipients(campaign_id,user_id) SELECT $1,id FROM users ORDER BY id LIMIT 20", id); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- base.Store.Approve(ctx, id, 1, []string{"sms", "email"}) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var n, unique int
		if err := base.Store.DB.QueryRow(ctx, "SELECT count(*),count(DISTINCT(user_id,channel)) FROM deliveries WHERE campaign_id=$1", id).Scan(&n, &unique); err != nil || n != unique || n == 0 {
			t.Fatal("parallel approval duplicated", n, unique, err)
		}
	})
}
