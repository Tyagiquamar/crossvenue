# Benchmarks

Run locally:

```
go test -bench=. -benchmem ./...
```

Benchmarks:

- `BenchmarkApplyDelta` — incremental depth update application (book)
- `BenchmarkVWAP` — VWAP walk over 100 ask levels (book)
- `BenchmarkEvaluate` — full opportunity pricing over 3 venues / 6
  directed pairs (depth walk + fees + slippage + latency penalty)
- `FuzzParse` — decimal parser fuzz target

Pipeline throughput: `cmd/loadgen` + `cmd/replay --speed max` measures
end-to-end normalized events/second on the recording path.

## Measured results

Measured locally on 13th Gen Intel Core i3-1315U, Windows 11
(windows/amd64), Go 1.26.5, commit 54a15ba. Results are illustrative and
not exchange/network latency.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| ApplyDelta | 41.42 | 0 | 0 |
| VWAP (100 levels) | 13795 | 1792 | 1 |
| Evaluate (3 venues, 6 pairs) | 1856 | 0 | 0 |

The VWAP walk is allocation-free apart from the fixed snapshot view; the
hot paths (delta apply, opportunity evaluation) perform zero allocations
per operation.

**Only measured results may be published.** When reporting, include CPU,
RAM, OS, Go version, and the exact command. Do not extrapolate to
"HFT-level" claims.
