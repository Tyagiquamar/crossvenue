package replay

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func fp(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func events() []domain.MarketEvent {
	ts := time.Unix(1_700_000_000, 123).UTC()
	return []domain.MarketEvent{
		{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
			Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 100,
			Bids:         []domain.Level{{Price: fp("100000"), Qty: fp("1")}},
			Asks:         []domain.Level{{Price: fp("100010"), Qty: fp("2")}},
			ExchangeTime: ts, ReceiveTime: ts.Add(time.Millisecond),
		}},
		{Type: domain.EventDelta, Delta: &domain.BookDelta{
			Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 101, PrevSequence: 100,
			Bids:         []domain.Level{{Price: fp("100001"), Qty: fp("0.5")}},
			Asks:         []domain.Level{{Price: fp("100010"), Qty: fp("0")}},
			ExchangeTime: ts.Add(2 * time.Millisecond), ReceiveTime: ts.Add(3 * time.Millisecond),
		}},
		{Type: domain.EventTrade, Trade: &domain.Trade{
			Venue: domain.VenueBinance, Symbol: "BTC-USDT",
			Price: fp("100005"), Qty: fp("0.25"), Aggressor: domain.Buy,
			ExchangeTime: ts.Add(4 * time.Millisecond), ReceiveTime: ts.Add(5 * time.Millisecond),
		}},
	}
}

func TestRecordReplayRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.recording")
	rec, err := NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events() {
		if err := rec.Record(ev); err != nil {
			t.Fatal(err)
		}
	}
	d1 := rec.Digest()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got []domain.MarketEvent
	for {
		_, ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Snapshot.Sequence != 100 || got[1].Delta.PrevSequence != 100 {
		t.Fatalf("sequences wrong: %+v", got)
	}
	if got[2].Trade.Price.String() != "100005" {
		t.Fatalf("trade price %s", got[2].Trade.Price)
	}

	// Second recording of the same events must have identical digest.
	rec2, _ := NewRecorder(filepath.Join(t.TempDir(), "t2.recording"))
	for _, ev := range events() {
		if err := rec2.Record(ev); err != nil {
			t.Fatal(err)
		}
	}
	if rec2.Digest() != d1 {
		t.Fatal("digest must be deterministic")
	}
	rec2.Close()
}
