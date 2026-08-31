# Architecture

## Pipeline

```
Exchange WS ──> Adapter (venue-specific parse) ──> Normalized MarketEvent
                                                      │
                                              ┌───────┴────────┐
                                              │ Recorder (tee) │
                                              └───────┬────────┘
                                                      v
                              Pipeline: one goroutine per venue+symbol book
                              (bounded channel, sequence tracker per venue)
                                                      │
                              immutable snapshots ────v────
                              Opportunity engine (depth VWAP, costs)
                                                      │
                              Risk engine (pre-trade checks, kill switch)
                                                      │
                              Execution simulator (state machine, 2-leg)
                                                      │
                              Portfolio ledger (per-venue balances)
                                                      v
                              Journal -> PostgreSQL (operational events)
```

## Ownership and concurrency

- Every `(venue, symbol)` order book is owned by exactly one goroutine
  (its pipeline lane). All mutation flows through a bounded channel.
- Readers never touch the book directly; they receive immutable
  `SnapshotView` copies or computed values.
- There is no global mutex around market state. The only shared locks are
  the manager registry (brief, for lane lookup) and the portfolio ledger.
- `go test -race ./...` is part of CI and must pass.

## Backpressure

- Venue read loops never block on downstream capacity; the pipeline's
  per-lane queue is bounded (default 4096).
- Book deltas must not be silently dropped (sequence integrity). On queue
  saturation the lane **invalidates the book and requests resync** instead.
  Metrics: `crossvenue_events_dropped_total`, `crossvenue_event_queue_depth`.

## Modes share one code path

Live adapters, the synthetic generator, and the replay reader all produce
`domain.MarketEvent` into the same `Supervisor.Ingest` path. There is no
separate replay business logic. In replay mode the supervisor runs
*deterministically*: events are applied synchronously and evaluation runs
per event, with no wall-clock timers.

## Recovery

On restart: portfolio balances/positions are restored from PostgreSQL;
books always start **invalid** and must resynchronize from fresh market
data before the readiness endpoint reports ready and before the
opportunity engine will consider them.
