#!/usr/bin/env bash
# CrossVenue LIVE public-feed validation (optional, internet-dependent).
# Public unauthenticated WebSocket feeds only; no API keys are read.
# Execution remains paper/simulated; this script never places orders.
# Zero profitable opportunities is NOT a failure. Unreachable venues are
# reported distinctly and produce a PARTIAL result, never fabricated data.
set -u
cd "$(dirname "$0")/.."

TIMEOUT=${1:-120}
ART=artifacts/validation/live
mkdir -p "$ART"
command -v jq >/dev/null || { echo "jq is required for validate-live.sh"; exit 1; }

echo 'CrossVenue live public-feed validation (paper execution only)'
echo "commit: $(git rev-parse --short HEAD)"
echo "utc:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"

[ -x bin/crossvenue ] || go build -o bin/crossvenue ./cmd/crossvenue || { echo 'build failed'; exit 1; }

PORT=""
for p in 8479 8483 8493 8503; do
  if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then PORT=$p; break; fi
done
[ -z "$PORT" ] && { echo 'no free validation port'; exit 1; }
BASE="http://127.0.0.1:$PORT"
CROSSVENUE_API_LISTEN="127.0.0.1:$PORT" CROSSVENUE_POSTGRES_URL='' \
  ./bin/crossvenue --mode live-market-sim --config config.example.yaml \
  > "$ART/engine.log" 2> "$ART/engine.err.log" &
ENGINE_PID=$!
trap 'kill $ENGINE_PID 2>/dev/null' EXIT

ALIVE=false
for _ in $(seq 1 60); do
  if curl -fsS --max-time 2 "$BASE/health" >/dev/null 2>&1; then ALIVE=true; break; fi
  sleep 0.5
done
if [ "$ALIVE" = false ]; then
  echo 'RESULT: FAIL (engine API never became live)'
  exit 1
fi

echo "waiting up to ${TIMEOUT}s for venue feeds..."
END=$((SECONDS + TIMEOUT))
while [ $SECONDS -lt $END ]; do
  curl -fsS --max-time 5 "$BASE/api/v1/venues" > "$ART/venues.json" 2>/dev/null || true
  curl -fsS --max-time 5 "$BASE/api/v1/books" > "$ART/books.json" 2>/dev/null || true
  READY=$(jq '[.[] | select(.ready and (.stale | not))] | length' "$ART/books.json" 2>/dev/null || echo 0)
  [ "$READY" -ge 3 ] && break
  sleep 2
done
curl -fsS --max-time 5 "$BASE/api/v1/opportunities" > "$ART/opportunities.json" 2>/dev/null || echo '[]' > "$ART/opportunities.json"

echo
printf '%-9s %-10s %-11s %-6s %-14s %-14s %-8s %s\n' VENUE CONNECTED 'BOOK READY' SEQ 'BEST BID' 'BEST ASK' 'AGE MS' GAPS
SYNCED=0
for v in binance bybit okx; do
  VJ=$(jq --arg v "$v" '[.[] | select(.venue == $v)][0] // empty' "$ART/venues.json")
  BJ=$(jq --arg v "$v" '[.[] | select(.venue == $v)][0] // empty' "$ART/books.json")
  [ -z "$VJ" ] && { printf '%-9s %-10s\n' "$v" 'NO (unreachable from this network)'; continue; }
  CONN=$(jq -r .connected <<<"$VJ")
  GAPS=$(jq -r .sequence_gaps <<<"$VJ")
  if [ -n "$BJ" ]; then
    READY=$(jq -r 'if .ready and (.stale | not) then "yes" else "no" end' <<<"$BJ")
    SEQ=$(jq -r .sequence <<<"$BJ")
    AGE=$(jq -r .age_ms <<<"$BJ")
    BID=$(jq -r 'if .bids then .bids[0].price else "-" end' <<<"$BJ")
    ASK=$(jq -r 'if .asks then .asks[0].price else "-" end' <<<"$BJ")
  else
    READY=no; SEQ=-; AGE=-; BID=-; ASK=-
  fi
  if [ "$CONN" = true ] && [ "$READY" = yes ] && [ "$GAPS" = 0 ]; then SYNCED=$((SYNCED+1)); fi
  [ "$CONN" = true ] && CONN=yes || CONN='NO (unreachable)'
  printf '%-9s %-10s %-11s %-6s %-14s %-14s %-8s %s\n' "$v" "$CONN" "$READY" "$SEQ" "$BID" "$ASK" "$AGE" "$GAPS"
done
echo
OPPS=$(jq 'length' "$ART/opportunities.json")
echo "opportunities detected: $OPPS (zero is fine — spreads after fees are rare)"
echo 'execution: PAPER ONLY (no order endpoints exist; no API keys read)'

if [ "$SYNCED" -ge 3 ]; then echo 'RESULT: PASS (3/3 venues connected, books synchronized, no sequence corruption)'; exit 0; fi
if [ "$SYNCED" -ge 1 ]; then echo "RESULT: PARTIAL ($SYNCED/3 venues synchronized; see table for unreachable venues)"; exit 0; fi
echo 'RESULT: FAIL (no venue synchronized)'
exit 1
