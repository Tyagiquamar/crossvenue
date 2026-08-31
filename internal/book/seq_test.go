package book

import (
	"testing"

	"crossvenue/internal/domain"
)

func delta(seq, prev int64) domain.BookDelta {
	return domain.BookDelta{Sequence: seq, PrevSequence: prev}
}

func TestBinanceSequenceRules(t *testing.T) {
	tr := NewBinanceTracker()
	// First delta anchors (no snapshot seen yet).
	if tr.Check(delta(105, 100)) != Apply {
		t.Fatal("first delta anchors the book")
	}
	if tr.Check(delta(105, 100)) != Duplicate {
		t.Fatal("replay of same seq should be duplicate")
	}
	// Delta whose range continues from lastSeq applies (U <= last < u).
	if tr.Check(delta(120, 105)) != Apply {
		t.Fatal("delta continuing the range applies")
	}
	// True discontinuity: U starts well above last+1.
	if tr.Check(delta(200, 150)) != Gap {
		t.Fatal("delta with U far above last is a gap")
	}
}

func TestBinanceSnapshotReanchors(t *testing.T) {
	tr := NewBinanceTracker()
	tr.Check(delta(105, 100)) // anchor via delta
	tr.OnSnapshot(300)        // depth20 partial image re-anchors forward
	if tr.Check(delta(310, 300)) != Apply {
		t.Fatal("delta chaining to re-anchored snapshot applies")
	}
	if tr.Check(delta(400, 350)) != Gap {
		t.Fatal("discontinuous delta still gaps")
	}
}

func TestOKXSequenceRules(t *testing.T) {
	tr := NewOKXTracker()
	tr.OnSnapshot(50)
	if tr.Check(delta(50, 0)) != Duplicate {
		t.Fatal("same seq duplicate")
	}
	if tr.Check(delta(51, 0)) != Apply {
		t.Fatal("next applies")
	}
	if tr.Check(delta(53, 52)) != Gap {
		t.Fatal("skip with unrelated prev is a gap")
	}
}

// After a reconnect, OKX resumes at a non-contiguous seqId but chains
// prevSeqId to our last known seq — that must bridge, not gap.
func TestOKXBridgeAfterReconnect(t *testing.T) {
	tr := NewOKXTracker()
	tr.OnSnapshot(50)
	tr.Check(delta(51, 0))
	// Reconnect re-snapshot jumps forward; tracker re-anchors via OnSnapshot.
	tr.OnSnapshot(900)
	// First update chains prevSeqId to the new snapshot.
	if tr.Check(delta(901, 900)) != Apply {
		t.Fatal("update chaining to re-snapshot must apply")
	}
	// A non-contiguous resume that still chains to lastSeq bridges.
	if tr.Check(delta(950, 901)) != Apply {
		t.Fatal("non-contiguous resume with valid prevSeqId must bridge")
	}
}

func TestBybitSequenceRules(t *testing.T) {
	tr := NewBybitTracker()
	tr.OnSnapshot(7)
	if tr.Check(delta(9, 0)) != Gap {
		t.Fatal("jump with unrelated prev is gap")
	}
	if tr.Check(delta(8, 7)) != Apply {
		t.Fatal("+1 applies")
	}
}

// Bybit reconnect resume: u jumps but prev chains to last known u.
func TestBybitBridgeAfterReconnect(t *testing.T) {
	tr := NewBybitTracker()
	tr.OnSnapshot(7)
	tr.Check(delta(8, 7))
	tr.OnSnapshot(500) // reconnect re-snapshot
	if tr.Check(delta(501, 500)) != Apply {
		t.Fatal("post-reconnect delta must apply")
	}
	if tr.Check(delta(600, 501)) != Apply {
		t.Fatal("non-contiguous resume with valid prev must bridge")
	}
	if tr.Check(delta(700, 650)) != Gap {
		t.Fatal("unrelated prev still gaps")
	}
}

func TestNewForVenue(t *testing.T) {
	for _, v := range []domain.Venue{domain.VenueBinance, domain.VenueOKX, domain.VenueBybit, domain.VenueSynthetic} {
		if _, err := NewForVenue(v); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if _, err := NewForVenue("nope"); err == nil {
		t.Fatal("unknown venue must error")
	}
}
