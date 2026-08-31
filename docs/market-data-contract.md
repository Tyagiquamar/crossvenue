# Market Data Contract (binding for tests)

This document defines ordering and sequence expectations. Tests in
`internal/book/seq_test.go` and `tests/integration/` encode it.

## Common rules

1. A book is not ready until a snapshot has been applied.
2. Deltas before any snapshot are ignored and trigger a resync request.
3. A detected gap invalidates the book immediately:
   - `crossvenue_sequence_gaps_total` increments
   - a `BookInvalidated` journal event is written
   - the venue adapter is asked to resnapshot
   - the book returns to ready only after a fresh snapshot applies
4. Duplicate and stale deltas are dropped without state change.
5. Zero-quantity levels are removals.
6. A crossed book (best bid >= best ask) is detectable via `Crossed()`;
   opportunities additionally require positive VWAP spread.
7. If `now - lastBookUpdate > max_book_age` the book is stale and excluded
   from opportunity evaluation. Recovery requires valid sequencing to
   resume, not merely the passage of time.

## Binance Spot

- Streams used: `<symbol>@depth20@100ms` (partial book image, treated as a
  snapshot anchor: `lastUpdateId`) and `<symbol>@depth@100ms`
  (incremental: `U` first update id, `u` final update id).
- First delta after a snapshot must bridge the snapshot id:
  `U <= snapshotId <= u`. A delta fully below the snapshot is stale; one
  that starts above it is a gap.
- Thereafter each delta must satisfy `U == previous u` (tracked via
  `PrevSequence == lastSeq`). Otherwise: gap.
- Duplicate: `u <= lastSeq`.

## OKX Spot

- Channel `books`: `action:"snapshot"` then `action:"update"`.
- Within a clean session `seqId` advances by exactly 1 per instrument;
  `seqId <= last` is a duplicate, `seqId > last+1` is checked next.
- Reconnect/resume: OKX re-sends a snapshot and updates carry
  `prevSeqId` chaining to the prior seqId. After a session hiccup the
  venue may resume at a non-contiguous `seqId`; a delta whose
  `prevSeqId` equals our last seqId is accepted as a bridge (re-anchor)
  rather than a gap. An unrelated `prevSeqId` is a true gap.
- Application-level heartbeat: client sends `"ping"`, expects `"pong"`.

## Bybit Spot

- Topic `orderbook.50.<symbol>`: `type:"snapshot"` then `type:"delta"`.
- Within a session `u` advances by exactly 1 (prev u = u-1). Duplicates
  (`u <= last`) are dropped.
- Reconnect: re-subscribe re-sends a snapshot; `u` may resume
  non-contiguously. A delta whose prev (`u-1`... tracked lastSeq) matches
  our last seq is accepted as a bridge; an unrelated jump is a gap.

## Synthetic venue

- Strict `+1` sequence semantics (OKX-style), used to exercise gap
  handling deterministically (`gap_every` option).
