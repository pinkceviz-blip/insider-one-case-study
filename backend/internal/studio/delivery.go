package studio

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"math/rand/v2"
	"time"
)

func (e *Engine) HandleDelivery(parent context.Context, t *asynq.Task) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	var payload struct {
		ID string `json:"delivery_id"`
	}
	if json.Unmarshal(t.Payload(), &payload) != nil || payload.ID == "" {
		return asynq.SkipRetry
	}
	id := payload.ID
	conn, release, err := e.Store.lockOperation(ctx, "delivery:"+id)
	if err != nil {
		return err
	}
	defer release()
	var ch, state string
	var contentBytes, stored []byte
	var recipient Recipient
	var attempts, failures int
	var approved, dbNow time.Time
	var next *time.Time
	var userID int64
	err = conn.QueryRow(ctx, `SELECT d.channel,d.state,d.attempts,d.failure_count,d.next_attempt_at,c.approved_at,d.payload,c.proposal->'content',u.first_name,
 CASE WHEN d.channel='sms' THEN u.phone ELSE u.email END,d.user_id,clock_timestamp()
 FROM deliveries d JOIN campaigns c ON c.id=d.campaign_id JOIN users u ON u.id=d.user_id
 WHERE d.id=$1 AND c.status='approved'`, id).Scan(&ch, &state, &attempts, &failures, &next, &approved, &stored, &contentBytes, &recipient.FirstName, &recipient.Address, &userID, &dbNow)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "accepted" || state == "failed" || state == "suppressed" || state == "unknown" {
		return nil
	}
	// The DB schedule is authoritative even if a duplicate/recovery task arrives early.
	if next != nil && next.Sub(dbNow) > 0 {
		return &DeferredError{After: next.Sub(dbNow)}
	}
	token := NewID()
	if _, err = conn.Exec(ctx, "UPDATE deliveries SET state='processing',lease_token=$2,lease_until=now()+interval '20 seconds',updated_at=now() WHERE id=$1", id, token); err != nil {
		return err
	}
	var sendTx pgx.Tx
	var attemptID int64
	var currentProviderError error
	// State, failure budget, durable retry deadline and attempt result commit together.
	finish := func(target, code, providerID string, kind ErrorKind, delay time.Duration, failureDelta int) error {
		tx := sendTx
		if tx == nil {
			var err error
			tx, err = conn.Begin(ctx)
			if err != nil {
				return err
			}
		}
		defer rollbackTx(tx)
		var retryAt *time.Time
		if delay > 0 {
			var at time.Time
			if err := tx.QueryRow(ctx, "SELECT clock_timestamp()+make_interval(secs => $1)", delay.Seconds()).Scan(&at); err != nil {
				return err
			}
			retryAt = &at
		}
		message := ExplainCode(code)
		outcome := "error"
		if target == "accepted" {
			outcome = "accepted"
			message = "Provider kabul kaydı doğrulandı."
		}
		if target == "unknown" {
			outcome = "unknown"
			message = "Provider sonucu belirsiz; otomatik gönderim durduruldu. Provider kabulünü manuel doğrulayın."
		}
		if target == "suppressed" {
			outcome = "suppressed"
		}
		var pe *ProviderError
		status := 0
		requestID := ""
		if errors.As(currentProviderError, &pe) {
			status = pe.HTTPStatus
			requestID = pe.RequestID
			if pe.Message != "" {
				message = pe.Message
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE deliveries SET state=$3,error=$4,provider_id=COALESCE(NULLIF($5,''),provider_id),next_attempt_at=$6,
 failure_count=failure_count+$7,lease_until=NULL,lease_token=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2`, id, token, target, code, providerID, retryAt, failureDelta)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("delivery fencing token lost")
		}

		if target == "accepted" {
			if providerID == "" {
				return errors.New("provider receipt required before marking sent")
			}
			// Same transaction as delivery acceptance: no timestamp on timeout, throttle,
			// failure, rollback or task replay. Preserve monotonicity across campaigns.
			if _, err = tx.Exec(ctx, `UPDATE users SET
    last_sms_sent_at=CASE WHEN $2='sms' THEN GREATEST(last_sms_sent_at,statement_timestamp()) ELSE last_sms_sent_at END,
    last_email_sent_at=CASE WHEN $2='email' THEN GREATEST(last_email_sent_at,statement_timestamp()) ELSE last_email_sent_at END
    WHERE id=$1`, userID, ch); err != nil {
				return err
			}
		}
		if attemptID != 0 {
			_, err = tx.Exec(ctx, `UPDATE attempts SET outcome=$2,kind=$3,code=$4,message=$5,http_status=$6,request_id=$7,retry_at=$8,finished_at=now() WHERE id=$1 AND outcome='inflight'`, attemptID, outcome, string(kind), code, message, status, requestID, retryAt)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO attempts(delivery_id,attempt_no,outcome,kind,code,message,http_status,request_id,retry_at,finished_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now())`, id, attempts, outcome, string(kind), code, message, status, requestID, retryAt)
		}
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Any in-flight ledger entry belongs to a dead handler: we own its DB session lock.

	recoveryTx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackTx(recoveryTx)
	tag, err := recoveryTx.Exec(ctx, `UPDATE attempts SET outcome='unknown',kind='transient',code='worker_interrupted',message=$2,finished_at=now() WHERE delivery_id=$1 AND outcome='inflight'`, id, ExplainCode("worker_interrupted"))
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		failures += int(tag.RowsAffected())
		if _, err = recoveryTx.Exec(ctx, "UPDATE deliveries SET failure_count=$2 WHERE id=$1 AND lease_token=$3", id, failures, token); err != nil {
			return err
		}
	}
	if err = recoveryTx.Commit(ctx); err != nil {
		return err
	}
	provider, ok := e.Registry.Providers[ch]
	if !ok {
		return finish("failed", "provider_missing", "", Permanent, 0, 0)
	}
	if len(stored) > 0 && attempts > 0 {
		if lookup, ok := provider.(ReceiptLookup); ok {
			receipt, found, lookupErr := lookup.Lookup(ctx, id)
			if lookupErr != nil {
				currentProviderError = lookupErr
				delay := RetryDelay(failures, lookupErr, t)
				if err = finish("retrying", "lookup_unavailable", "", Transient, delay, 0); err != nil {
					return err
				}
				return &DeferredError{After: delay}
			}
			if found {
				return finish("accepted", "receipt_reconciled", receipt, "", 0, 0)
			}
		}
	}
	if len(stored) > 0 && attempts > 0 && !safeToResend(provider) {
		return finish("unknown", "provider_outcome_unknown", "", Transient, 0, 0)
	}
	if e.CampaignTTL > 0 && dbNow.Sub(approved) > e.CampaignTTL {
		return finish("failed", "campaign_expired", "", Permanent, 0, 0)
	}
	if failures >= 9 {
		return finish("failed", "attempts_exhausted", "", Permanent, 0, 0)
	}
	channel, ok := e.Registry.Channels[ch]
	if !ok {
		return finish("failed", "unknown_channel", "", Permanent, 0, 0)
	}
	var msg Message
	if len(stored) > 0 {
		err = json.Unmarshal(stored, &msg)
	} else {
		var c Content
		err = json.Unmarshal(contentBytes, &c)
		if err == nil {
			msg, err = channel.Render(c, recipient)
		}
	}
	if err != nil {
		return finish("failed", "render_validation", "", Permanent, 0, 0)
	}
	wait, err := limitScript.Run(ctx, e.Redis, []string{"limit:{" + ch + "}:window", "limit:{" + ch + "}:cooldown"}, NewID()).Int64()
	if err != nil {
		delay := 2 * time.Second
		if dbErr := finish("retrying", "limiter_unavailable", "", Transient, delay, 0); dbErr != nil {
			return dbErr
		}
		return &DeferredError{After: delay}
	}
	if wait > 0 {
		// Normal throttling is a scheduling event, not an error log or failed attempt.
		delay := time.Duration(wait)*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond
		_, err = conn.Exec(ctx, "UPDATE deliveries SET state='retrying',error='rate_wait',next_attempt_at=clock_timestamp()+make_interval(secs => $3),lease_until=NULL,lease_token=NULL,updated_at=now() WHERE id=$1 AND lease_token=$2", id, token, delay.Seconds())
		if err != nil {
			return err
		}
		return &DeferredError{After: delay}
	}
	// Persist the exact request and start ledger before making the external call.
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackTx(tx)
	b, _ := json.Marshal(msg)
	err = tx.QueryRow(ctx, "UPDATE deliveries SET payload=COALESCE(payload,$3),attempts=attempts+1 WHERE id=$1 AND lease_token=$2 RETURNING attempts", id, token, b).Scan(&attempts)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, "INSERT INTO attempts(delivery_id,attempt_no,outcome,kind,code) VALUES($1,$2,'inflight','','') RETURNING id", id, attempts).Scan(&attemptID)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// Consent revocation obtains an exclusive row lock. Once it commits, subsequent
	// sends see false. Already-authorized sends finish before that commit.
	sendTx, err = conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackTx(sendTx)
	var allowed bool
	err = sendTx.QueryRow(ctx, "SELECT allowed FROM consents WHERE user_id=$1 AND channel=$2 FOR SHARE", userID, ch).Scan(&allowed)
	if isNotFound(err) || err == nil && !allowed {
		return finish("suppressed", "consent_revoked", "", Permanent, 0, 0)
	}
	if err != nil {
		return err
	}
	providerID, sendErr := provider.Send(ctx, id, msg)
	if sendErr == nil {
		return finish("accepted", "", providerID, "", 0, 0)
	}
	currentProviderError = sendErr
	if !safeToResend(provider) && providerKind(sendErr) != Permanent {
		return finish("unknown", "provider_outcome_unknown", "", Transient, 0, 0)
	}
	code := safeProviderError(sendErr)
	kind := providerKind(sendErr)
	if kind == Permanent {
		return finish("failed", code, "", kind, 0, 0)
	}
	delay := RetryDelay(failures, sendErr, t)
	if kind == Throttled {
		var pe *ProviderError
		errors.As(sendErr, &pe)
		delay = pe.RetryAfter
		if delay <= 0 {
			delay = 2 * time.Second
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		// Redis cooldown is an optimization. DB retry deadline must commit even if Redis fails.
		_, _ = cooldownScript.Run(ctx, e.Redis, []string{"limit:{" + ch + "}:cooldown"}, delay.Milliseconds()).Result()
		delay += time.Duration(rand.IntN(250)) * time.Millisecond
		if err = finish("retrying", code, "", kind, delay, 0); err != nil {
			return err
		}
		return &DeferredError{After: delay}
	}
	// On the final ambiguous failure, schedule a receipt check before terminalizing.
	if err = finish("retrying", code, "", kind, delay, 1); err != nil {
		return err
	}
	return &DeferredError{After: delay}
}
