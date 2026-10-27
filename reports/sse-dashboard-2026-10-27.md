# Dashboard SSE

- Browser setInterval timers removed, including four-second global refresh and one-second queue refresh.
- GET /api/dashboard/events sends global counts, channel counts, actual Asynq queue state, campaign list and dependency health in one named dashboard event.
- Every connection receives a complete current snapshot. EventSource reconnect restores state even when updates happened while disconnected. This stream uses snapshots rather than a replay cursor.
- Existing per-campaign content generation SSE remains separate.
- Server samples DB/Asynq every 500 ms and transmits only changed snapshots. This is SSE transport, not a claim of a native Asynq event subscription. Heartbeat every 15 seconds. Client cancellation closes the handler; write deadlines bound slow clients.
- Frontend test advances 12 seconds and asserts no periodic REST calls; streamed updates and reconnect snapshot update the counters.

Local validation: nginx-proxied localhost:18080 endpoint returns text/event-stream and a complete dashboard event. Browser shows SSE · CANLI for SMS and Email. Frontend: 16 tests passed and production build passed.
