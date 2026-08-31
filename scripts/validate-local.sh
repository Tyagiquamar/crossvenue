#!/usr/bin/env bash
# CrossVenue local validation proof: reproducible, offline, paper-only.
# Mirrors scripts/validate-local.ps1. jq is used for JSON parsing when
# available (ubuntu runners and most dev machines have it).
set -u
cd "$(dirname "$0")/.."

ART=artifacts/validation
mkdir -p "$ART"
COMPACT=0
SKIP_DOCKER=0
for a in "$@"; do
  case "$a" in
    --compact) COMPACT=1 ;;
    --skip-docker) SKIP_DOCKER=1 ;;
  esac
done

FAILED=0
declare -A REPORT

step() {
  local name="$1"; shift
  [ "$COMPACT" = 0 ] && echo "== $name"
  local out
  out="$("$@" 2>&1)"
  local code=$?
  [ "$COMPACT" = 0 ] && [ -n "$out" ] && echo "$out" | sed 's/^/   /'
  if [ $code -eq 0 ]; then REPORT[$name]=pass; else REPORT[$name]=fail; FAILED=1; fi
  [ "$COMPACT" = 0 ] && echo "   -> ${REPORT[$name]^^}"
}

# ---------- A. environment ----------
GO_VERSION="$(go version | sed 's/^go version //')"
OS_ARCH="$(go env GOOS)/$(go env GOARCH)"
COMMIT="$(git rev-parse HEAD)"
SHORT_COMMIT="${COMMIT:0:7}"
TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if [ "$COMPACT" = 0 ]; then
  echo "go:        $GO_VERSION"
  echo "os/arch:   $OS_ARCH"
  echo "commit:    $COMMIT"
  echo "utc:       $TIMESTAMP"
fi

# ---------- B. static verification ----------
step build bash -c 'go build -o bin/crossvenue ./cmd/crossvenue && go build -o bin/replay ./cmd/replay && go build -o bin/loadgen ./cmd/loadgen'
step gofmt bash -c 'test -z "$(gofmt -l .)"'
step "go vet" go vet ./...
step staticcheck go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...
step "go test" go test ./...
step "go test -race" go test -race ./...

# ---------- C. deterministic replay parity (seed 42) ----------
REC="$ART/parity.recording"
[ "$COMPACT" = 0 ] && echo '== replay parity (seed 42)'
./bin/loadgen --venues 3 --symbols 1 --events-per-second 200 --duration 3s --seed 42 --out "$REC" >/dev/null
./bin/replay --recording "$REC" --speed max > "$ART/replay-run1.txt"
./bin/replay --recording "$REC" --speed max > "$ART/replay-run2.txt"
DIGEST1="$(sha256sum "$ART/replay-run1.txt" | cut -d' ' -f1)"
DIGEST2="$(sha256sum "$ART/replay-run2.txt" | cut -d' ' -f1)"
[ "$DIGEST1" = "$DIGEST2" ] && PARITY=true || { PARITY=false; FAILED=1; }
if [ "$COMPACT" = 0 ]; then
  echo "   run1: $DIGEST1"
  echo "   run2: $DIGEST2"
  echo "   -> $([ "$PARITY" = true ] && echo PASS || echo FAIL)"
fi

# ---------- D. synthetic system proof ----------
PORT=""
for p in 8477 8481 8491 8501; do
  if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then PORT=$p; break; fi
done
[ -z "$PORT" ] && { echo 'no free validation port'; exit 1; }
BASE="http://127.0.0.1:$PORT"
CROSSVENUE_API_LISTEN="127.0.0.1:$PORT" CROSSVENUE_POSTGRES_URL='' \
  ./bin/crossvenue --mode synthetic --config config.example.yaml \
  > "$ART/synthetic.log" 2> "$ART/synthetic.err.log" &
ENGINE_PID=$!
API_READY=false
for _ in $(seq 1 180); do
  if curl -fsS --max-time 2 "$BASE/ready" 2>/dev/null | grep -q '"ready"'; then API_READY=true; break; fi
  sleep 0.5
done
VENUES_CONNECTED=0
BOOKS_READY=0
if [ "$API_READY" = true ]; then
  for ep in health ready api/v1/venues api/v1/books api/v1/opportunities api/v1/balances api/v1/risk; do
    curl -fsS --max-time 5 "$BASE/$ep" | jq . > "$ART/$(echo "$ep" | tr / -).json" 2>/dev/null \
      || curl -fsS --max-time 5 "$BASE/$ep" > "$ART/$(echo "$ep" | tr / -).json"
  done
  if command -v jq >/dev/null; then
    BOOKS_READY=$(jq '[.[] | select(.ready and (.stale | not))] | length' "$ART/api-v1-books.json")
    VENUES_CONNECTED=$(jq '[.[] | select(.connected)] | length' "$ART/api-v1-venues.json")
  else
    BOOKS_READY=$(grep -o '"ready":true' "$ART/api-v1-books.json" | wc -l)
    VENUES_CONNECTED=$(grep -o '"connected":true' "$ART/api-v1-venues.json" | wc -l)
  fi
fi
kill "$ENGINE_PID" 2>/dev/null
wait "$ENGINE_PID" 2>/dev/null
[ "$API_READY" = true ] || FAILED=1
[ "$COMPACT" = 0 ] && echo "== synthetic proof: api_ready=$API_READY venues_connected=$VENUES_CONNECTED books_ready=$BOOKS_READY"

# ---------- E. failure-scene suite ----------
SCENES=(
  "sequence gap:TestScene1SequenceGapInvalidatesAndResyncs"
  "disconnect:TestScene2DisconnectedVenueExcludedOthersContinue"
  "stale quote:TestScene3StaleQuoteRejected"
  "partial leg fill:TestPartialSecondLegFillJournalsResidualExposure"
  "idempotency:TestScene5DuplicateOrderID"
  "restart recovery:TestRestartBooksStartInvalid"
  "kill switch:TestScene7KillSwitchBlocksExecution"
  "queue overload:TestScene8QueueOverloadInvalidates"
  "live-exec guard:TestLoadRejectsLiveExecutionEnv"
)
PATTERN="$(printf '%s|' "${SCENES[@]#*:}" | sed 's/|$//')"
go test -count=1 -v -run "$PATTERN" ./tests/integration/ ./internal/engine/ ./internal/config/ > "$ART/failure-scenes.txt" 2>&1
declare -A SCENE_RESULTS
for s in "${SCENES[@]}"; do
  name="${s%%:*}"; tn="${s##*:}"
  if grep -q -- "--- PASS: $tn " "$ART/failure-scenes.txt" && ! grep -q -- "--- FAIL: $tn" "$ART/failure-scenes.txt"; then
    SCENE_RESULTS[$name]=PASS
  else
    SCENE_RESULTS[$name]=FAIL; FAILED=1
  fi
  [ "$COMPACT" = 0 ] && echo "   ${SCENE_RESULTS[$name]}  $name"
done

# ---------- F. docker build ----------
DOCKER_STATUS=skip
if [ "$SKIP_DOCKER" = 0 ] && command -v docker >/dev/null; then
  if docker build -q -t crossvenue:validation . >/dev/null 2>&1; then DOCKER_STATUS=pass; else DOCKER_STATUS=fail; FAILED=1; fi
fi
[ "$COMPACT" = 0 ] && echo "== docker build: $DOCKER_STATUS"

# ---------- validation.json ----------
SCENES_ALL_PASS=true
for s in "${SCENES[@]}"; do [ "${SCENE_RESULTS[${s%%:*}]}" = PASS ] || SCENES_ALL_PASS=false; done
cat > "$ART/validation.json" <<EOF
{
  "commit": "$COMMIT",
  "go_version": "$GO_VERSION",
  "os_arch": "$OS_ARCH",
  "timestamp_utc": "$TIMESTAMP",
  "tests": {
    "unit": "${REPORT[go test]:-fail}",
    "race": "${REPORT[go test -race]:-fail}",
    "vet": "${REPORT[go vet]:-fail}",
    "staticcheck": "${REPORT[staticcheck]:-fail}",
    "failure_scenes": "$([ "$SCENES_ALL_PASS" = true ] && echo pass || echo fail)"
  },
  "replay": {
    "seed": 42,
    "digest_run_1": "$DIGEST1",
    "digest_run_2": "$DIGEST2",
    "identical": $PARITY
  },
  "synthetic": {
    "venues_connected": $VENUES_CONNECTED,
    "books_ready": $BOOKS_READY,
    "api_ready": $API_READY
  },
  "docker": { "build": "$DOCKER_STATUS" }
}
EOF

# ---------- compact proof block (screenshot-friendly) ----------
echo
echo 'CrossVenue validation'
echo "commit: $SHORT_COMMIT"
echo 'mode: synthetic (paper execution)'
echo
echo 'VENUES'
if command -v jq >/dev/null && [ -f "$ART/api-v1-books.json" ]; then
  jq -r '.[] | [.venue, (if .ready and (.stale | not) then "READY" else "NOT-READY" end), .symbol, (if .bids then "bid=" + .bids[0].price else "bid=-" end)] | @tsv' "$ART/api-v1-books.json" \
    | while IFS=$'\t' read -r v st sym bid; do printf '%-9s %-9s %-9s %s\n' "$v" "$st" "$sym" "$bid"; done
fi
echo
echo 'ENGINE'
TOTAL_BOOKS=$(jq 'length' "$ART/api-v1-books.json" 2>/dev/null || echo '?')
echo "books ready:        $BOOKS_READY/$TOTAL_BOOKS"
echo "api ready:          $API_READY"
KILL=$(jq -r '.kill_switch' "$ART/api-v1-risk.json" 2>/dev/null || echo unknown)
[ "$KILL" = false ] && KILL='inactive (ACTIVE risk checks)'
echo "risk kill switch:   $KILL"
echo 'execution:          PAPER ONLY'
echo
echo 'REPLAY'
echo 'seed:               42'
echo "digest run 1:       $DIGEST1"
echo "digest run 2:       $DIGEST2"
echo "parity:             $([ "$PARITY" = true ] && echo PASS || echo FAIL)"
echo
echo 'FAILURE SCENES'
for s in "${SCENES[@]}"; do printf '%-19s %s\n' "${s%%:*}:" "${SCENE_RESULTS[${s%%:*}]}"; done
echo
echo 'TESTS'
for t in gofmt "go vet" staticcheck "go test" "go test -race"; do
  st="${REPORT[$t]:-fail}"
  printf '%-19s %s\n' "$t:" "${st^^}"
done
printf '%-19s %s\n' 'docker build:' "${DOCKER_STATUS^^}"
echo
if [ $FAILED -ne 0 ]; then echo 'RESULT: VALIDATION FAILED'; exit 1; fi
echo 'RESULT: ALL LOCAL VALIDATIONS PASSED'
