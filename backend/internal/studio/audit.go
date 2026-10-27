package studio

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
	"time"
)

type AttemptView struct {
	ID         int64      `json:"id"`
	DeliveryID string     `json:"delivery_id"`
	CampaignID string     `json:"campaign_id"`
	Channel    string     `json:"channel"`
	State      string     `json:"state"`
	AttemptNo  int        `json:"attempt_no"`
	Outcome    string     `json:"outcome"`
	Kind       string     `json:"kind"`
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	HTTPStatus int        `json:"http_status"`
	RequestID  string     `json:"request_id"`
	RetryAt    *time.Time `json:"retry_at"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const attemptColumns = `a.id,a.delivery_id,d.campaign_id,d.channel,d.state,a.attempt_no,a.outcome,a.kind,a.code,a.message,a.http_status,a.request_id,a.retry_at,a.created_at,a.finished_at`

func scanAttempt(rows pgx.Rows) (AttemptView, error) {
	var a AttemptView
	err := rows.Scan(&a.ID, &a.DeliveryID, &a.CampaignID, &a.Channel, &a.State, &a.AttemptNo, &a.Outcome, &a.Kind, &a.Code, &a.Message, &a.HTTPStatus, &a.RequestID, &a.RetryAt, &a.CreatedAt, &a.FinishedAt)
	if a.Message == "" {
		a.Message = ExplainCode(a.Code)
	}
	if a.HTTPStatus == 0 {
		code := a.Code
		for i := len(code) - 1; i >= 0; i-- {
			if code[i] == ':' {
				code = code[i+1:]
				break
			}
		}
		n, _ := strconv.Atoi(code)
		if n >= 400 && n <= 599 {
			a.HTTPStatus = n
		}
	}
	return a, err
}
func (e *Engine) ErrorList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	campaign := q.Get("campaign_id")
	if campaign != "" && !requireID(w, campaign, campaignIDPattern) {
		return
	}
	channel := q.Get("channel")
	kind := q.Get("kind")
	cursor := int64(0)
	if raw := q.Get("cursor"); raw != "" {
		var err error
		cursor, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 1 {
			writeJSON(w, 400, map[string]string{"error": "Geçersiz sayfa imleci"})
			return
		}
	}
	if channel != "" {
		if _, ok := e.Registry.Channels[channel]; !ok {
			writeJSON(w, 400, map[string]string{"error": "Geçersiz kanal"})
			return
		}
	}
	if kind != "" && kind != "transient" && kind != "throttled" && kind != "permanent" {
		writeJSON(w, 400, map[string]string{"error": "Geçersiz hata sınıfı"})
		return
	}
	rows, err := e.Store.DB.Query(r.Context(), `SELECT `+attemptColumns+` FROM attempts a JOIN deliveries d ON d.id=a.delivery_id
 WHERE a.outcome IN ('error','unknown') AND ($1='' OR d.campaign_id=$1) AND ($2='' OR d.channel=$2)
 AND ($3='' OR a.kind=$3) AND ($4::bigint=0 OR a.id<$4) ORDER BY a.id DESC LIMIT 51`, campaign, channel, kind, cursor)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	items := []AttemptView{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			fail(w, err)
			return
		}
		items = append(items, a)
	}
	if rows.Err() != nil {
		fail(w, rows.Err())
		return
	}
	var next int64
	if len(items) > 50 {
		items = items[:50]
		next = items[49].ID
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
func (e *Engine) DeliveryHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireID(w, id, deliveryIDPattern) {
		return
	}
	var campaign, ch, state, code string
	var attempts, failures int
	var next *time.Time
	var receipt *string
	var payload []byte
	var userID int64
	var smsAt, emailAt *time.Time
	err := e.Store.DB.QueryRow(r.Context(), "SELECT campaign_id,channel,state,error,attempts,failure_count,next_attempt_at,provider_id,payload FROM deliveries WHERE id=$1", id).Scan(&campaign, &ch, &state, &code, &attempts, &failures, &next, &receipt, &payload)
	if err != nil {
		fail(w, err)
		return
	}
	if err = e.Store.DB.QueryRow(r.Context(), "SELECT u.id,u.last_sms_sent_at,u.last_email_sent_at FROM users u JOIN deliveries d ON d.user_id=u.id WHERE d.id=$1", id).Scan(&userID, &smsAt, &emailAt); err != nil {
		fail(w, err)
		return
	}
	hash := ""
	if len(payload) > 0 {
		h := sha256.Sum256(payload)
		hash = hex.EncodeToString(h[:])
	}
	rows, err := e.Store.DB.Query(r.Context(), `SELECT `+attemptColumns+` FROM attempts a JOIN deliveries d ON d.id=a.delivery_id WHERE d.id=$1 ORDER BY a.id DESC LIMIT 100`, id)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	history := []AttemptView{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			fail(w, err)
			return
		}
		history = append(history, a)
	}
	if rows.Err() != nil {
		fail(w, rows.Err())
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": userID, "last_sms_sent_at": smsAt, "last_email_sent_at": emailAt, "delivery_id": id, "idempotency_key": id, "campaign_id": campaign, "channel": ch, "state": state, "error": code, "attempts": attempts, "failure_count": failures, "next_attempt_at": next, "provider_id": receipt, "payload_sha256": hash, "history": history})
}
