package studio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hibiken/asynq"
	"net/http"
	"strings"
	"testing"
	"time"
)

type responseLLM struct {
	raw      string
	proposal Proposal
}

func (l responseLLM) Generate(ctx context.Context, brief string, emit func(string, string) error) (Proposal, error) {
	if l.raw != "" {
		return DecodeProposal([]byte(l.raw))
	}
	return l.proposal, nil
}

// Run after background dispatch/consumers stop: every fault boundary is explicit,
// and the only Redis database touched is this suite's isolated DB 15.
func exerciseAcceptance(t *testing.T, ctx context.Context, e *Engine, campaignID string, opt asynq.RedisClientOpt, apiURL string) {
	t.Run("dashboard SSE reconnect sends current complete state", func(t *testing.T) {
		read := func() map[string]any {
			t.Helper()
			streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(streamCtx, "GET", apiURL+"/api/dashboard/events", nil)
			req.Header.Set("Last-Event-ID", "999999999")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatal("not SSE")
			}
			scan := bufio.NewScanner(res.Body)
			scan.Buffer(make([]byte, 4096), 4*1024*1024)
			for scan.Scan() {
				if strings.HasPrefix(scan.Text(), "data: ") {
					var v map[string]any
					if err = json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &v); err != nil {
						t.Fatal(err)
					}
					return v
				}
			}
			t.Fatalf("missing dashboard snapshot: %v", scan.Err())
			return nil
		}
		before := read()
		if before["queues"] == nil || before["channel_stats"] == nil || before["campaigns"] == nil {
			t.Fatal("incomplete snapshot")
		}
		if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief,status) VALUES($1,$2,'generating')", strings.Repeat("d", 32), "SSE reconnect test"); err != nil {
			t.Fatal(err)
		}
		after := read()
		if after["stats"].(map[string]any)["campaigns"].(float64) != before["stats"].(map[string]any)["campaigns"].(float64)+1 {
			t.Fatal("reconnect did not restore current state")
		}
	})

	t.Run("invalid_llm_never_reaches_send", func(t *testing.T) {
		valid := Proposal{Segment: Segment{"İstanbul", 4, true}, Content: Content{"hello", "subject", "body"}}
		unsupported := valid
		unsupported.Segment.City = "unsupported"
		overlong := valid
		overlong.Content.SMS = strings.Repeat("x", 161)
		for name, llm := range map[string]LLM{"malformed_json": responseLLM{raw: `{broken`}, "unsupported_segment": responseLLM{proposal: unsupported}, "overlong_content": responseLLM{proposal: overlong}} {
			t.Run(name, func(t *testing.T) {
				id := NewID()
				if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief) VALUES($1,'invalid llm test')", id); err != nil {
					t.Fatal(err)
				}
				testEngine := *e
				testEngine.LLM = llm
				b, _ := json.Marshal(map[string]string{"campaign_id": id})
				if err := testEngine.HandleGeneration(ctx, asynq.NewTask("campaign:generate", b)); err == nil {
					t.Fatal("invalid LLM output accepted")
				}
				if err := e.Store.Approve(ctx, id, 1, []string{"sms"}); !errors.Is(err, ErrConflict) {
					t.Fatalf("invalid draft could be approved: %v", err)
				}
				var n int
				if err := e.Store.DB.QueryRow(ctx, "SELECT count(*) FROM deliveries WHERE campaign_id=$1", id).Scan(&n); err != nil || n != 0 {
					t.Fatal("invalid output created deliveries", n, err)
				}
			})
		}
	})
	t.Run("outbox_recovers_redis_failure_and_crash_after_enqueue", func(t *testing.T) {
		queueName := "test-outbox"
		key := "outbox-" + NewID()
		payload := []byte(`{"delivery_id":"fault-test"}`)
		if _, err := e.Store.DB.Exec(ctx, "INSERT INTO outbox(task_type,task_id,queue,payload) VALUES('message:send',$1,$2,$3)", key, queueName, payload); err != nil {
			t.Fatal(err)
		}
		badQueue := asynq.NewClient(asynq.RedisClientOpt{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
		defer badQueue.Close()
		broken := *e
		broken.Queue = badQueue
		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := broken.Dispatch(short)
		cancel()
		if err == nil {
			t.Fatal("Redis outage was not observed")
		}
		var unpublished bool
		if err = e.Store.DB.QueryRow(ctx, "SELECT published_at IS NULL FROM outbox WHERE task_id=$1", key).Scan(&unpublished); err != nil || !unpublished {
			t.Fatal("outbox lost during Redis outage", err)
		}
		// Fail the DB acknowledgement after the real Redis enqueue succeeds. This
		// leaves exactly the same durable boundary as a dispatcher dying at this point.
		fn := `CREATE FUNCTION fail_outbox_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.task_id='` + key + `' THEN RAISE EXCEPTION 'injected crash after Redis enqueue'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER fail_outbox_ack BEFORE UPDATE ON outbox FOR EACH ROW EXECUTE FUNCTION fail_outbox_ack();`
		if _, err = e.Store.DB.Exec(ctx, fn); err != nil {
			t.Fatal(err)
		}
		defer e.Store.DB.Exec(context.Background(), "DROP TRIGGER IF EXISTS fail_outbox_ack ON outbox; DROP FUNCTION IF EXISTS fail_outbox_ack()")
		if err = e.Dispatch(ctx); err == nil {
			t.Fatal("injected post-enqueue crash was not observed")
		}
		inspector := asynq.NewInspector(opt)
		defer inspector.Close()
		if _, err = inspector.GetTaskInfo(queueName, key); err != nil {
			t.Fatal("task did not reach real Redis before injected crash", err)
		}
		if err = e.Store.DB.QueryRow(ctx, "SELECT published_at IS NULL FROM outbox WHERE task_id=$1", key).Scan(&unpublished); err != nil || !unpublished {
			t.Fatal("failed acknowledgement committed", err)
		}
		if _, err = e.Store.DB.Exec(ctx, "DROP TRIGGER fail_outbox_ack ON outbox; DROP FUNCTION fail_outbox_ack()"); err != nil {
			t.Fatal(err)
		}
		if err = e.Dispatch(ctx); err != nil {
			t.Fatal("outbox recovery failed", err)
		}
		if err = e.Dispatch(ctx); err != nil {
			t.Fatal("repeated dispatcher failed", err)
		}
		if err = e.Store.DB.QueryRow(ctx, "SELECT published_at IS NULL FROM outbox WHERE task_id=$1", key).Scan(&unpublished); err != nil || unpublished {
			t.Fatal("recovered outbox not acknowledged", err)
		}
		info, err := inspector.GetQueueInfo(queueName)
		if err != nil || info.Pending != 1 {
			t.Fatalf("double enqueue produced duplicate task: %+v %v", info, err)
		}
	})
	t.Run("sse_reconnect_replays_only_missed_events_and_fresh_snapshot", func(t *testing.T) {
		id := NewID()
		proposal := Proposal{Segment: Segment{"İstanbul", 4, true}, Content: Content{"before reconnect", "subject", "body"}}
		pb, _ := json.Marshal(proposal)
		if _, err := e.Store.DB.Exec(ctx, "INSERT INTO campaigns(id,brief,status,proposal) VALUES($1,'sse reconnect','draft',$2)", id, pb); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Event(ctx, id, "draft.ready", map[string]string{"stage": "before"}); err != nil {
			t.Fatal(err)
		}
		readSnapshot := func(cursor int64) (Campaign, []int64) {
			t.Helper()
			streamCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(streamCtx, "GET", apiURL+"/api/campaigns/"+id+"/events", nil)
			if cursor > 0 {
				req.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
			scan := bufio.NewScanner(res.Body)
			kind := ""
			ids := []int64{}
			for scan.Scan() {
				line := scan.Text()
				if strings.HasPrefix(line, "id: ") {
					var eventID int64
					if _, err = fmt.Sscan(strings.TrimPrefix(line, "id: "), &eventID); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, eventID)
				}
				if strings.HasPrefix(line, "event: ") {
					kind = strings.TrimPrefix(line, "event: ")
				}
				if kind == "snapshot" && strings.HasPrefix(line, "data: ") {
					var c Campaign
					if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
						t.Fatal(err)
					}
					return c, ids
				}
			}
			t.Fatalf("SSE snapshot missing: %v", scan.Err())
			return Campaign{}, nil
		}
		before, ids := readSnapshot(0)
		if before.Version != 1 || len(ids) != 1 {
			t.Fatal("initial stream invalid", before.Version, ids)
		}
		cursor := ids[0]
		// The browser is disconnected while content and its version change.
		proposal.Content.SMS = "after reconnect"
		if err := e.Store.Edit(ctx, id, 1, proposal.Content); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Event(ctx, id, "draft.updated", map[string]string{"stage": "while disconnected"}); err != nil {
			t.Fatal(err)
		}
		after, replayed := readSnapshot(cursor)
		if after.Version != 2 || after.Proposal.Content.SMS != "after reconnect" || len(replayed) != 1 || replayed[0] <= cursor {
			t.Fatalf("reconnect did not restore dashboard state: version=%d events=%v cursor=%d", after.Version, replayed, cursor)
		}
	})
	t.Run("deterministic_429_503_and_timeout_recover", func(t *testing.T) {
		for _, scenario := range []string{"429", "503", "timeout"} {
			t.Run(scenario, func(t *testing.T) {
				key := ""
				for i := 0; i < 10000; i++ {
					candidate := fmt.Sprintf("deterministic-%s-%d", scenario, i)
					n := providerHash(candidate)
					match := n%37 != 0
					switch scenario {
					case "429":
						match = match && n%11 == 0
					case "503":
						match = match && n%11 != 0 && n%7 == 0
					case "timeout":
						match = match && n%11 != 0 && n%7 != 0 && n%13 == 0
					}
					if match {
						key = candidate
						break
					}
				}
				if key == "" {
					t.Fatal("no deterministic key")
				}
				var user int64
				if err := e.Store.DB.QueryRow(ctx, "INSERT INTO users(first_name,email,phone,city,cart_at,purchased) VALUES('Deterministic',$1,$2,'İstanbul',now(),false) RETURNING id", key+"@example.invalid", key).Scan(&user); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Store.DB.Exec(ctx, "INSERT INTO consents(user_id,channel,allowed) VALUES($1,'sms',true)", user); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Store.DB.Exec(ctx, "INSERT INTO deliveries(id,campaign_id,version,user_id,channel) VALUES($1,$2,1,$3,'sms')", key, campaignID, user); err != nil {
					t.Fatal(err)
				}
				// No consumers are running; clearing only isolated test DB limiter keys is safe.
				if err := e.Redis.Del(ctx, "limit:{sms}:window", "limit:{sms}:cooldown").Err(); err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(map[string]string{"delivery_id": key})
				task := asynq.NewTask("message:send", b)
				var deferred *DeferredError
				if err := e.HandleDelivery(ctx, task); !errors.As(err, &deferred) {
					t.Fatalf("expected retry: %v", err)
				}
				var state, code string
				var failures int
				var next *time.Time
				if err := e.Store.DB.QueryRow(ctx, "SELECT state,error,failure_count,next_attempt_at FROM deliveries WHERE id=$1", key).Scan(&state, &code, &failures, &next); err != nil {
					t.Fatal(err)
				}
				if state != "retrying" || !strings.Contains(code, scenario) || next == nil {
					t.Fatal("wrong first failure", state, code, next)
				}
				if scenario == "429" && failures != 0 {
					t.Fatal("429 consumed retry budget")
				}
				if scenario != "429" && failures != 1 {
					t.Fatal("temporary failure not counted")
				}
				// Simulate time passing without a wall-clock sleep; the early-retry guard is
				// covered separately in persisted_retry_deadline_and_no_failure_timestamp.
				if _, err := e.Store.DB.Exec(ctx, "UPDATE deliveries SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1", key); err != nil {
					t.Fatal(err)
				}
				if err := e.Redis.Del(ctx, "limit:{sms}:window", "limit:{sms}:cooldown").Err(); err != nil {
					t.Fatal(err)
				}
				if err := e.HandleDelivery(ctx, task); err != nil {
					t.Fatal("retry failed", err)
				}
				var providerRecords int
				if err := e.Store.DB.QueryRow(ctx, "SELECT state,(SELECT count(*) FROM provider_requests WHERE idempotency_key=$1) FROM deliveries WHERE id=$1", key).Scan(&state, &providerRecords); err != nil {
					t.Fatal(err)
				}
				if state != "accepted" || providerRecords != 1 {
					t.Fatal("retry did not produce exactly one acceptance", state, providerRecords)
				}
			})
		}
	})
}
