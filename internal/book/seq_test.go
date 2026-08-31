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
	if tr.Check(delta(10, 9)) != NeedSnapshot {
		t.Fatal("delta before snapshot must need snapshot")
	}
	tr.OnSnapshot(100)
	// Bridging delta: prev <= 100 <= seq.
	if tr.Check(delta(105, 99)) != Apply {
		t.Fatal("bridging delta should apply")
	}
	if tr.Check(delta(105, 99)) != Duplicate {
		t.Fatal("replay of same seq should be duplicate")
	}
	if tr.Check(delta(106, 105)) != Apply {
		t.Fatal("next delta should apply")
	}
	if tr.Check(delta(110, 108)) != Gap {
		t.Fatal("prev mismatch must be gap")
	}
}

func TestBinanceStaleFirstDelta(t *testing.T) {
	tr := NewBinanceTracker()
	tr.OnSnapshot(100)
	if tr.Check(delta(90, 89)) != Stale {
		t.Fatal("delta fully below snapshot is stale")
	}
	if tr.Check(delta(200, 150)) != Gap {
		t.Fatal("first delta not bridging snapshot is a gap")
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
	if tr.Check(delta(53, 0)) != Gap {
		t.Fatal("skip is gap")
	}
}

func TestBybitSequenceRules(t *testing.T) {
	tr := NewBybitTracker()
	tr.OnSnapshot(7)
	if tr.Check(delta(9, 0)) != Gap {
		t.Fatal("jump is gap")
	}
	if tr.Check(delta(8, 0)) != Apply {
		t.Fatal("+1 applies")
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
