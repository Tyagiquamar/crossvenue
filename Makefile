BINARY=crossvenue
GOFLAGS?=

ifeq ($(OS),Windows_NT)
EXE := .exe
VALIDATE := pwsh -NoProfile -File scripts/validate-local.ps1
LIVE := pwsh -NoProfile -File scripts/validate-live.ps1
else
EXE :=
VALIDATE := bash scripts/validate-local.sh
LIVE := bash scripts/validate-live.sh
endif

.PHONY: build test race lint staticcheck verify proof replay-proof live-proof bench docker docker-build demo clean fmt vet

build:
	go build -o bin/crossvenue$(EXE) ./cmd/crossvenue
	go build -o bin/replay$(EXE) ./cmd/replay
	go build -o bin/loadgen$(EXE) ./cmd/loadgen

fmt:
	gofmt -l -w .

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...

lint: vet staticcheck

test:
	go test ./...

race:
	go test -race ./...

# Full local verification: gofmt, vet, staticcheck, tests, race,
# deterministic replay parity, synthetic engine probe, failure scenes.
verify:
	$(VALIDATE)

# Screenshot-friendly compact proof: same validations, summary-only output.
proof:
ifeq ($(OS),Windows_NT)
	pwsh -NoProfile -File scripts/validate-local.ps1 -Compact
else
	bash scripts/validate-local.sh --compact
endif

# Deterministic replay parity: same recording + seed, run twice — the two
# digest sets printed must be identical (automated comparison runs in
# verify/proof and tests/replay).
replay-proof: build
	./bin/loadgen$(EXE) --venues 3 --symbols 1 --events-per-second 200 --duration 3s --seed 42 --out data/demo.recording
	./bin/replay$(EXE) --recording data/demo.recording --speed max
	./bin/replay$(EXE) --recording data/demo.recording --speed max

# Live public-feed validation (internet required, paper-only).
live-proof:
	$(LIVE)

demo: build
	./bin/crossvenue$(EXE) --mode synthetic --config config.example.yaml

bench:
	go test -bench=. -benchmem ./...

docker-build:
	docker build -t crossvenue .

docker:
	docker compose up --build

clean:
ifeq ($(OS),Windows_NT)
	pwsh -NoProfile -Command "Remove-Item -Recurse -Force bin, data, artifacts -ErrorAction SilentlyContinue"
else
	rm -rf bin data artifacts
endif
