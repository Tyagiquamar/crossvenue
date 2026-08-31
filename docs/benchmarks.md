# Benchmarks

Run locally:

```
go test -bench=. -benchmem ./internal/book/ ./pkg/decimal/
```

Current benchmarks:

- `BenchmarkApplyDelta` — incremental depth update application
- `BenchmarkVWAP` — VWAP walk over 100 levels
- `FuzzParse` — decimal parser fuzz target

Pipeline throughput benchmark: `cmd/loadgen` + `cmd/replay --speed max`
measures end-to-end normalized events/second on the recording path.

**Only measured results may be published.** When reporting, include CPU,
RAM, OS, Go version, and the exact command. Do not extrapolate to
"HFT-level" claims.
