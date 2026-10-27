# Campaign history and queue reporting

- Campaign creation opens at step one, including when selected from the sidebar. The separate new-campaign toolbar action is removed.
- Monitoring opens at the top and lists campaigns newest first, with independent SMS/email progress and state counts. Error history can be scoped to a campaign.
- Rate-limit scheduling (`retrying` with `rate_wait` in durable storage) is reported as queued work in channel counters. Provider 429/503 retries remain retries. Aggregate durable state counts retain their recovery semantics.
- Workers check pending/delayed tasks every 100ms instead of the default 1s/5s intervals. The shared admission limiter reserves 200ms for ledger/consent work between admission and provider invocation to reduce boundary bursts.

## Validation

- Frontend: 17 tests and production build passed.
- Complete backend suite passed with race detection (71.967s).
- Queue classification and multi-worker throughput tests passed with race detection across three repetitions. In the controlled provider test, 120 messages per channel completed in approximately 1.5 seconds; observed rolling one-second provider calls did not exceed 100. The mock HTTP provider deliberately injects failures and channel cooldowns, so these timings are not end-to-end demo throughput claims.
- Local demo history was reset on request: PostgreSQL and Redis volumes recreated; campaign, delivery, attempt, event, outbox and provider history empty; 5,000 synthetic users seeded with no send timestamps. Browser monitoring was verified empty. This reset is local runtime state, not a source-controlled database change.
