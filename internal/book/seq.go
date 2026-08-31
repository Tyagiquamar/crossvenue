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
//   - First event after snapshot: prevUpdateID (PrevSequence) <= snapshot
//     lastUpdateID AND updateID (Sequence) >= snapshot lastUpdateID.
//     We adopt event.Sequence as the new anchor on first accepted delta.
//   - Thereafter each event must have PrevSequence == last Sequence.
//   - PrevSequence == last-? duplicates: if Sequence <= last, drop.
type BinanceTracker struct {
	ready    bool
	lastSeq  int64
	anchored bool
	snapSeq  int64
}

// NewBinanceTracker constructs the tracker.
func NewBinanceTracker() *BinanceTracker { return &BinanceTracker{} }

// OnSnapshot implements Tracker.
func (t *BinanceTracker) OnSnapshot(seq int64) {
	t.ready, t.anchored = true, false
	t.snapSeq = seq
	t.lastSeq = seq
}

// Ready implements Tracker.
func (t *BinanceTracker) Ready() bool { return t.ready }

// Check implements Tracker.
func (t *BinanceTracker) Check(d domain.BookDelta) Verdict {
	if !t.ready {
		return NeedSnapshot
	}
	if !t.anchored {
		// First delta after snapshot must bridge the snapshot id.
		if d.Sequence < t.snapSeq {
			return Stale
		}
		if d.PrevSequence > t.snapSeq {
			return Gap // missed the bridging event
		}
		t.anchored = true
		t.lastSeq = d.Sequence
		return Apply
	}
	if d.Sequence <= t.lastSeq {
		return Duplicate
	}
	if d.PrevSequence != t.lastSeq {
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
