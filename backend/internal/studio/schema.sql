CREATE TABLE IF NOT EXISTS users (
 id bigserial PRIMARY KEY, first_name text NOT NULL, email text NOT NULL UNIQUE,
 phone text NOT NULL UNIQUE, city text NOT NULL, cart_at timestamptz NOT NULL, purchased boolean NOT NULL
);
CREATE INDEX IF NOT EXISTS users_segment_idx ON users(city,cart_at) WHERE NOT purchased;
CREATE TABLE IF NOT EXISTS consents(user_id bigint REFERENCES users(id), channel text NOT NULL, allowed boolean NOT NULL, PRIMARY KEY(user_id,channel));
CREATE TABLE IF NOT EXISTS campaigns (
 id text PRIMARY KEY, brief text NOT NULL, status text NOT NULL DEFAULT 'generating', version integer NOT NULL DEFAULT 1,
 proposal jsonb NOT NULL DEFAULT '{}', audience jsonb NOT NULL DEFAULT '{}', error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), approved_at timestamptz
);
CREATE TABLE IF NOT EXISTS recipients(campaign_id text REFERENCES campaigns(id),user_id bigint REFERENCES users(id), PRIMARY KEY(campaign_id,user_id));
CREATE TABLE IF NOT EXISTS deliveries (
 id text PRIMARY KEY, campaign_id text REFERENCES campaigns(id), version integer NOT NULL, user_id bigint REFERENCES users(id),
 channel text NOT NULL, state text NOT NULL DEFAULT 'pending', attempts integer NOT NULL DEFAULT 0,
 payload jsonb, provider_id text, error text NOT NULL DEFAULT '', lease_until timestamptz, lease_token text,
 updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(campaign_id,version,user_id,channel)
);
CREATE INDEX IF NOT EXISTS deliveries_campaign_idx ON deliveries(campaign_id,state);
CREATE INDEX IF NOT EXISTS deliveries_reconcile_idx ON deliveries(updated_at) WHERE state IN ('pending','retrying','processing');
CREATE TABLE IF NOT EXISTS outbox(id bigserial PRIMARY KEY, task_type text NOT NULL, task_id text NOT NULL, queue text NOT NULL, payload jsonb NOT NULL, published_at timestamptz);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox(id) WHERE published_at IS NULL;
CREATE TABLE IF NOT EXISTS events(id bigserial PRIMARY KEY, campaign_id text NOT NULL REFERENCES campaigns(id), kind text NOT NULL, data jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS events_campaign_idx ON events(campaign_id,id);
CREATE TABLE IF NOT EXISTS attempts(id bigserial PRIMARY KEY,delivery_id text REFERENCES deliveries(id), kind text NOT NULL, code text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS provider_requests(idempotency_key text PRIMARY KEY,payload jsonb NOT NULL,provider_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS provider_attempts(idempotency_key text PRIMARY KEY,count integer NOT NULL DEFAULT 1);
INSERT INTO users(first_name,email,phone,city,cart_at,purchased)
SELECT (ARRAY['Deniz','Ece','Mehmet','Zeynep','Ali','Yağmur','Can','İpek','Abdullah Muhammed','Selin'])[1+(i%10)],
 'user'||i||'@example.invalid','+90000'||lpad(i::text,7,'0'),
 (ARRAY['İstanbul','Ankara','İzmir'])[1+(i%3)],now()-(i%60)*interval '1 day',(i%5=0)
FROM generate_series(1,5000) i ON CONFLICT DO NOTHING;
INSERT INTO consents(user_id,channel,allowed) SELECT id,'sms',id%4<>0 FROM users ON CONFLICT DO NOTHING;
INSERT INTO consents(user_id,channel,allowed) SELECT id,'email',id%3<>0 OR id%7=0 FROM users ON CONFLICT DO NOTHING;
-- Additive migrations preserve existing demo deliveries and error history.
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS failure_count integer NOT NULL DEFAULT 0;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS attempt_no integer NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS outcome text NOT NULL DEFAULT 'error';
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS message text NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS http_status integer NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS request_id text NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS retry_at timestamptz;
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS finished_at timestamptz;
CREATE INDEX IF NOT EXISTS attempts_delivery_idx ON attempts(delivery_id,id DESC);
CREATE INDEX IF NOT EXISTS attempts_errors_idx ON attempts(id DESC) WHERE outcome IN ('error','unknown');
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_sms_sent_at timestamptz;
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_email_sent_at timestamptz;
-- Historical rows are backfilled only with matching, persisted provider proof.
WITH verified AS (
 SELECT d.user_id,max(p.created_at) FILTER(WHERE d.channel='sms') AS sms_at,
 max(p.created_at) FILTER(WHERE d.channel='email') AS email_at
 FROM deliveries d JOIN provider_requests p ON p.idempotency_key=d.id AND p.provider_id=d.provider_id AND p.payload=d.payload
 WHERE d.state='accepted' GROUP BY d.user_id
)
UPDATE users u SET last_sms_sent_at=GREATEST(u.last_sms_sent_at,v.sms_at),last_email_sent_at=GREATEST(u.last_email_sent_at,v.email_at)
FROM verified v WHERE u.id=v.user_id AND ((v.sms_at IS NOT NULL AND (u.last_sms_sent_at IS NULL OR u.last_sms_sent_at<v.sms_at)) OR (v.email_at IS NOT NULL AND (u.last_email_sent_at IS NULL OR u.last_email_sent_at<v.email_at)));
