# Failure Semantics

The system never silently continues on corrupt or missing market state.

| Failure | Detection | Response | Observability |
|---|---|---|---|
| Sequence gap | Venue tracker (`u`/`seqId` discontinuity) | Book invalidated; resnapshot requested; no opportunities from that book | `crossvenue_sequence_gaps_total`, `BookInvalidated` event |
| Venue disconnect | WS read error / heartbeat failure | Reconnect with capped exponential backoff + jitter; venue health shows disconnected; pairs involving it produce nothing | `crossvenue_ws_reconnect_total`, `/api/v1/venues` |
| Stale feed | `now - lastBookUpdate > max_book_age` (sweeper) | `Stale=true`; excluded from evaluation; recovers only via valid sequencing | `crossvenue_book_age_seconds`, book view `stale` |
| Queue overload | Bounded lane channel full | Book invalidated + resync requested (deltas are never silently dropped while continuing to trade) | `crossvenue_events_dropped_total` |
| Partial leg fill | Execution simulator | Residual exposure position; journaled; visible to risk limits | `PositionChanged` event, `/api/v1/positions` |
| Duplicate submission | ClientOrderID index | Original order returned; no second order | idempotent `Submit` |
| Process restart | Startup path | Balances/positions restored from Postgres; books start invalid; trading resumes only after resync | `/ready` gates on synchronized books |
| Kill switch | API / config / auto on repeated failures | All new submissions blocked immediately | `KillSwitchActivated` event, risk endpoint |
| Malformed message | Adapter parse error | Message dropped, debug-logged; no state change | adapter logs |

## Reconnect is not readiness

A venue that reconnects is not usable until a fresh snapshot has been
applied and subsequent deltas chain correctly. Readiness (`/ready`)
requires synchronized, non-stale books.
