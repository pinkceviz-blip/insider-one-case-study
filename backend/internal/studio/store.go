package studio

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

//go:embed schema.sql
var schema string

type Store struct{ DB *pgxpool.Pool }

func OpenStore(ctx context.Context, url string) (*Store, error) {
	p, e := pgxpool.New(ctx, url)
	if e != nil {
		return nil, e
	}
	return &Store{p}, nil
}
func (s *Store) Migrate(ctx context.Context) error { _, e := s.DB.Exec(ctx, schema); return e }
func (s *Store) Event(ctx context.Context, id, kind string, data any) error {
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec(ctx, "INSERT INTO events(campaign_id,kind,data) VALUES($1,$2,$3)", id, kind, b)
	return e
}
func (s *Store) Campaign(ctx context.Context, id string) (Campaign, error) {
	var c Campaign
	var p, a []byte
	e := s.DB.QueryRow(ctx, "SELECT id,brief,status,version,proposal,audience,error,created_at FROM campaigns WHERE id=$1", id).Scan(&c.ID, &c.Brief, &c.Status, &c.Version, &p, &a, &c.Error, &c.CreatedAt)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(p, &c.Proposal); e != nil {
		return c, e
	}
	if e = json.Unmarshal(a, &c.Audience); e != nil {
		return c, e
	}
	c.Counts = map[string]int{}
	rows, e := s.DB.Query(ctx, "SELECT state,count(*) FROM deliveries WHERE campaign_id=$1 GROUP BY state", id)
	if e != nil {
		return c, e
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if e = rows.Scan(&k, &n); e != nil {
			return c, e
		}
		c.Counts[k] = n
	}
	if rows.Err() != nil {
		return c, rows.Err()
	}
	if c.Status == "approved" || c.Status == "dispatching" {
		active := c.Counts["pending"] + c.Counts["processing"] + c.Counts["retrying"]
		if active == 0 {
			if c.Counts["unknown"] > 0 {
				c.Status = "needs_review"
			} else {
				c.Status = "completed"
			}
		} else {
			c.Status = "dispatching"
		}
	}
	return c, nil
}
func (s *Store) Create(ctx context.Context, brief string) (string, error) {
	id := NewID()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO campaigns(id,brief) VALUES($1,$2)", id, brief); e != nil {
		return "", e
	}
	payload, _ := json.Marshal(map[string]string{"campaign_id": id})
	_, e = tx.Exec(ctx, "INSERT INTO outbox(task_type,task_id,queue,payload) VALUES('campaign:generate',$1,'campaign',$2)", "generate-"+id, payload)
	if e != nil {
		return "", e
	}
	return id, tx.Commit(ctx)
}
func (s *Store) SaveProposal(ctx context.Context, id string, p Proposal) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var status string
	var at time.Time
	if e = tx.QueryRow(ctx, "SELECT status,created_at FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&status, &at); e != nil {
		return e
	}
	if status != "generating" {
		return nil
	}
	// Relative dates are anchored to campaign creation; all queries are parameterized.
	_, e = tx.Exec(ctx, `INSERT INTO recipients(campaign_id,user_id) SELECT $1,id FROM users WHERE city=$2 AND cart_at >= $3::timestamptz - make_interval(days => $4) AND cart_at <= $3 AND NOT purchased ON CONFLICT DO NOTHING`, id, p.Segment.City, at, p.Segment.Days)
	if e != nil {
		return e
	}
	var a Audience
	e = tx.QueryRow(ctx, `SELECT count(DISTINCT r.user_id),count(*) FILTER(WHERE c.channel='sms' AND c.allowed),count(*) FILTER(WHERE c.channel='email' AND c.allowed) FROM recipients r LEFT JOIN consents c ON c.user_id=r.user_id WHERE r.campaign_id=$1`, id).Scan(&a.Total, &a.SMS, &a.Email)
	if e != nil {
		return e
	}
	pb, _ := json.Marshal(p)
	ab, _ := json.Marshal(a)
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET proposal=$2,audience=$3,status='draft',error='' WHERE id=$1", id, pb, ab); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

var ErrConflict = errors.New("Kampanya sürümü değişti veya artık düzenlenemez; sayfayı yenileyin")

func (s *Store) Edit(ctx context.Context, id string, version int, c Content) error {
	b, _ := json.Marshal(c)
	tag, e := s.DB.Exec(ctx, "UPDATE campaigns SET proposal=jsonb_set(proposal,'{content}',$3),version=version+1 WHERE id=$1 AND version=$2 AND status='draft'", id, version, b)
	if e == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return e
}
func (s *Store) Approve(ctx context.Context, id string, version int, channels []string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var status string
	var v int
	e = tx.QueryRow(ctx, "SELECT status,version FROM campaigns WHERE id=$1 FOR UPDATE", id).Scan(&status, &v)
	if e != nil {
		return e
	}
	if v != version {
		return ErrConflict
	}
	if status == "approved" {
		return nil
	}
	if status != "draft" {
		return ErrConflict
	}
	for _, ch := range channels {
		// Delivery IDs are stable and unique for this campaign version, recipient and channel.
		_, e = tx.Exec(ctx, `WITH added AS (
   INSERT INTO deliveries(id,campaign_id,version,user_id,channel)
   SELECT $1||'-'||$2::integer::text||'-'||r.user_id::text||'-'||$3,$1,$2::integer,r.user_id,$3
   FROM recipients r JOIN consents co ON co.user_id=r.user_id AND co.channel=$3 AND co.allowed
   WHERE r.campaign_id=$1 ON CONFLICT DO NOTHING RETURNING id
  ) INSERT INTO outbox(task_type,task_id,queue,payload)
  SELECT 'message:send',id,$3,jsonb_build_object('delivery_id',id) FROM added`, id, version, ch)
		if e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE campaigns SET status='approved',approved_at=now() WHERE id=$1", id); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func isNotFound(e error) bool { return errors.Is(e, pgx.ErrNoRows) }
