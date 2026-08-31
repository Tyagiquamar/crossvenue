# Runbook

## Start (offline)

```
go run ./cmd/crossvenue --mode synthetic --config config.example.yaml
```

API on `127.0.0.1:8471`. Check `/health` (liveness) then `/ready`
(readiness: at least one synchronized non-stale book).

## Start (live public feeds, paper execution)

```
go run ./cmd/crossvenue --mode live-market-sim --config config.example.yaml
```

Watch `/api/v1/venues` for `connected` and reconnect counts. Spreads after
fees are rare at the default size; that is expected, not a bug.

## Replay

```
go run ./cmd/loadgen --seed 42 --duration 5s --out data/demo.recording
go run ./cmd/replay --recording data/demo.recording --speed max
```

Run twice: digests must match.

## Kill switch

```
curl -X POST http://127.0.0.1:8471/api/v1/kill-switch
curl -X POST http://127.0.0.1:8471/api/v1/kill-switch/reset
```

## Postgres (optional)

```
docker compose up postgres
export CROSSVENUE_POSTGRES_URL=postgres://crossvenue:crossvenue@localhost:5432/crossvenue?sslmode=disable
```

On start the engine migrates and restores balances/positions. Books are
never restored — they resynchronize from fresh data.

## Common issues

- **No opportunities**: check `/api/v1/risk` rejections and
  `/api/v1/books` staleness. Default `min_edge_bps=3` plus 10 bps taker
  fees means most visible spreads are correctly rejected.
- **Reconnect loops**: network egress or venue rate limits. Reconnects are
  capped with backoff + jitter; check logs for the venue field.
- **Port conflict**: set `CROSSVENUE_API_LISTEN`.
