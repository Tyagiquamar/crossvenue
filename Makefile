BINARY=crossvenue
GOFLAGS?=

.PHONY: build test race lint verify replay demo bench docker clean fmt vet

build:
	go build -o bin/crossvenue.exe ./cmd/crossvenue
	go build -o bin/replay.exe ./cmd/replay
	go build -o bin/loadgen.exe ./cmd/loadgen

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

lint: vet
	@which staticcheck >nul 2>&1 && staticcheck ./... || echo "staticcheck not installed; skipping"

verify: fmt vet test race build

replay: build
	./bin/loadgen.exe --venues 3 --symbols 1 --events-per-second 200 --duration 5s --seed 42 --out data/demo.recording
	./bin/replay.exe --recording data/demo.recording --speed max
	./bin/replay.exe --recording data/demo.recording --speed max

demo: build
	./bin/crossvenue.exe --mode synthetic --config config.example.yaml

bench:
	go test -bench=. -benchmem ./internal/book/ ./pkg/decimal/

docker:
	docker compose up --build

clean:
	rm -rf bin data
