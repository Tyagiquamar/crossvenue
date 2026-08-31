// Sequence trackers enforce venue-specific sequence/gap rules. The rules
// live adjacent to the book package (injected per venue) rather than
// pretending all venues share semantics.
package book

import (
	"fmt"

	"crossvenue/internal/domain"
)

// Verdict classifies an incoming delta relative to current state.
type Verdict int

const (
	Apply        Verdict = iota // apply normally
	Duplicate                   // already seen; drop silently
	Stale                       // older than current; drop
	Gap                         // sequence gap; invalidate + resync required
	NeedSnapshot                // delta arrived before any snapshot
)

func (v Verdict) String() string {
	switch v {
	case Apply:
		return "apply"
	case Duplicate:
		return "duplicate"
	case Stale:
		return "stale"
	case Gap:
		return "gap"
	case NeedSnapshot:
		return "need_snapshot"
	}
	return "unknown"
}

// Tracker validates delta ordering for one venue+symbol feed.
// Implementations are venue-specific and stateful; not goroutine-safe
// (owned by the book manager goroutine).
type Tracker interface {
	// OnSnapshot resets state at a new snapshot sequence.
	OnSnapshot(seq int64)
	// Check classifies a delta. On Apply the tracker records progress.
	Check(d domain.BookDelta) Verdict
	// Ready reports whether a snapshot has been seen.
	Ready() bool
}

// ---- Binance spot ----
// Rules (docs/market-data-contract.md):
//   - The adapter interleaves two streams on one tracker: depth20 (a
//     periodic partial-image snapshot, sequence = lastUpdateId) and depth
//     (incremental deltas, sequence = u / prev = U). These are independent
//     sequence counters, so the tracker anchors on whichever arrives first
//     and thereafter enforces continuity within the delta stream only.
//   - Deltas before any anchor: the first delta anchors the book (adopt
//     its u); a snapshot always re-anchors and is applied immediately.
//   - Thereafter each delta must chain: prev (U) <= lastSeq <= seq (u).
//     Deltas carry ranges, so strict equality (prev == lastSeq) would
//     false-positive on partial-image resyncs; we accept any delta whose
//     range covers lastSeq and reject only true discontinuities.
type BinanceTracker struct {
	ready   bool
	lastSeq int64
}

// NewBinanceTracker constructs the tracker.
func NewBinanceTracker() *BinanceTracker { return &BinanceTracker{} }

// OnSnapshot implements Tracker. Snapshots always re-anchor.
func (t *BinanceTracker) OnSnapshot(seq int64) {
	t.ready = true
	// Anchor the delta chain at the snapshot's lastUpdateId. Deltas whose
	// range covers this id continue the book.
	if seq > t.lastSeq {
		t.lastSeq = seq
	}
}

// Ready implements Tracker.
func (t *BinanceTracker) Ready() bool { return t.ready }

// Check implements Tracker.
func (t *BinanceTracker) Check(d domain.BookDelta) Verdict {
	if !t.ready {
		// No snapshot yet: anchor on the first delta so the book can start
		// producing (the depth20 image will refine it when it arrives).
		t.ready = true
		t.lastSeq = d.Sequence
		return Apply
	}
	if d.Sequence <= t.lastSeq {
		return Duplicate
	}
	// Accept the delta if its [U, u] range continues from our last seq,
	// i.e. U <= lastSeq < u. A delta starting strictly above lastSeq+1
	// (U > lastSeq+1) means we missed events: true gap.
	if d.PrevSequence > t.lastSeq+1 {
		return Gap
	}
	t.lastSeq = d.Sequence
	return Apply
}

// ---- OKX spot ----
// Rules: checksum-validated depth messages carry monotonically increasing
// seqId per instrument; any seqId > last+1 is a gap; <= last is duplicate.
type OKXTracker struct {
	ready   bool
	lastSeq int64
}

// NewOKXTracker constructs the tracker.
func NewOKXTracker() *OKXTracker { return &OKXTracker{} }

// OnSnapshot implements Tracker.
func (t *OKXTracker) OnSnapshot(seq int64) { t.ready, t.lastSeq = true, seq }

// Ready implements Tracker.
func (t *OKXTracker) Ready() bool { return t.ready }

// Check implements Tracker.
//
// OKX re-sends a snapshot on reconnect and each update carries prevSeqId
// chaining to the prior seqId. Within a clean session seqId advances by 1,
// but after a session-level hiccup the venue may resume at a non-contiguous
// seqId while still carrying a valid prevSeqId that matches our last known
// seq. Treat a delta whose PrevSequence equals our lastSeq as a valid bridge
// (re-anchor) rather than a gap — the alternative is permanent churn because
// a strict +1 check can never recover once the venue resumes out of step.
func (t *OKXTracker) Check(d domain.BookDelta) Verdict {
	if !t.ready {
		return NeedSnapshot
	}
	switch {
	case d.Sequence <= t.lastSeq:
		return Duplicate
	case d.Sequence == t.lastSeq+1:
		t.lastSeq = d.Sequence
		return Apply
	case d.PrevSequence == t.lastSeq:
		// Bridge: venue resumed non-contiguously but chains to our state.
		t.lastSeq = d.Sequence
		return Apply
	default:
		return Gap
	}
}

// ---- Bybit spot ----
// Rules: "snapshot" messages replace the book; "delta" messages carry
// sequence u which must be last+1. Reconnect always re-snapshots.
type BybitTracker struct {
	ready   bool
	lastSeq int64
}

// NewBybitTracker constructs the tracker.
func NewBybitTracker() *BybitTracker { return &BybitTracker{} }

// OnSnapshot implements Tracker.
func (t *BybitTracker) OnSnapshot(seq int64) { t.ready, t.lastSeq = true, seq }

// Ready implements Tracker.
func (t *BybitTracker) Ready() bool { return t.ready }

// Check implements Tracker.
//
// Bybit re-sends a snapshot on reconnect; deltas chain on u (prev u = u-1
// within a session). After a reconnect the venue may resume at a
// non-contiguous u. A strict +1 rule can never recover from that, so accept
// a delta whose PrevSequence equals our lastSeq as a valid bridge.
func (t *BybitTracker) Check(d domain.BookDelta) Verdict {
	if !t.ready {
		return NeedSnapshot
	}
	switch {
	case d.Sequence <= t.lastSeq:
		return Duplicate
	case d.Sequence == t.lastSeq+1:
		t.lastSeq = d.Sequence
		return Apply
	case d.PrevSequence == t.lastSeq:
		t.lastSeq = d.Sequence
		return Apply
	default:
		return Gap
	}
}

// NewForVenue returns the venue-appropriate tracker.
func NewForVenue(v domain.Venue) (Tracker, error) {
	switch v {
	case domain.VenueBinance:
		return NewBinanceTracker(), nil
	case domain.VenueOKX:
		return NewOKXTracker(), nil
	case domain.VenueBybit:
		return NewBybitTracker(), nil
	case domain.VenueSynthetic:
		return NewOKXTracker(), nil // strict +1 semantics
	}
	return nil, fmt.Errorf("no sequence tracker for venue %q", v)
}
