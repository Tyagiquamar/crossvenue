package book

import (
	"testing"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func px(t *testing.T, s string) decimal.Fixed {
	t.Helper()
	v, err := decimal.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func lvl(p, q string) domain.Level {
	return domain.Level{Price: decimal.FromRaw(must(p)), Qty: decimal.FromRaw(must(q))}
}

func must(s string) int64 {
	v, _ := decimal.Parse(s)
	return v.Raw()
}

func snap(seq int64) domain.BookSnapshot {
	return domain.BookSnapshot{
		Venue:       domain.VenueBinance,
		Symbol:      "BTC-USDT",
		Bids:        []domain.Level{lvl("100000", "1"), lvl("99990", "2")},
		Asks:        []domain.Level{lvl("100010", "1.5"), lvl("100020", "3")},
		Sequence:    seq,
		ReceiveTime: time.Unix(1000, 0),
	}
}

func TestSnapshotAndBest(t *testing.T) {
	b := New(domain.VenueBinance, "BTC-USDT")
	if _, ok := b.BestBid(); ok {
		t.Fatal("empty book should have no bid")
	}
	b.LoadSnapshot(snap(100))
	bid, _ := b.BestBid()
	ask, _ := b.BestAsk()
	if bid.Price.String() != "100000" || ask.Price.String() != "100010" {
		t.Fatalf("bad top: %s/%s", bid.Price, ask.Price)
	}
	if !b.State().Ready {
		t.Fatal("should be ready after snapshot")
	}
}

func TestDeltaAddUpdateRemove(t *testing.T) {
	b := New(domain.VenueBinance, "BTC-USDT")
	b.LoadSnapshot(snap(100))
	// Update ask 100010 qty 2, remove ask 100020, add bid 99995.
	b.ApplyDelta(domain.BookDelta{
		Sequence:    101,
		Bids:        []domain.Level{lvl("99995", "0.5")},
		Asks:        []domain.Level{lvl("100010", "2"), lvl("100020", "0")},
		ReceiveTime: time.Unix(1001, 0),
	})
	if got := len(b.Depth(domain.Sell, 10)); got != 1 {
		t.Fatalf("asks after remove: %d", got)
	}
	ask, _ := b.BestAsk()
	if ask.Qty.String() != "2" {
		t.Fatalf("ask qty: %s", ask.Qty)
	}
	depth := b.Depth(domain.Buy, 10)
	if len(depth) != 3 || depth[1].Price.String() != "99995" {
		t.Fatalf("bids: %+v", depth)
	}
}

func TestVWAPPartial(t *testing.T) {
	b := New(domain.VenueBinance, "BTC-USDT")
	b.LoadSnapshot(snap(1))
	// Asks: 1.5@100010, 3@100020. Buy 2 -> 1.5@100010 + 0.5@100020.
	vwap, filled, cost, ok := b.VWAP(domain.Sell, px(t, "2"))
	if !ok {
		t.Fatal("should fully fill")
	}
	if filled.String() != "2" {
		t.Fatalf("filled %s", filled)
	}
	// cost = 1.5*100010 + 0.5*100020 = 150015 + 50010 = 200025; vwap = 100012.5
	if cost.String() != "200025" || vwap.String() != "100012.5" {
		t.Fatalf("cost %s vwap %s", cost, vwap)
	}
	// Buy 10: insufficient depth.
	_, filled, _, ok = b.VWAP(domain.Sell, px(t, "10"))
	if ok {
		t.Fatal("should be partial")
	}
	if filled.String() != "4.5" {
		t.Fatalf("partial filled %s", filled)
	}
}

func TestInvalidateAndStale(t *testing.T) {
	b := New(domain.VenueBinance, "BTC-USDT")
	b.LoadSnapshot(snap(1))
	b.Invalidate()
	if b.State().Ready {
		t.Fatal("should be not ready")
	}
	b.MarkStale(true)
	if !b.State().Stale {
		t.Fatal("should be stale")
	}
}

func TestCrossed(t *testing.T) {
	b := New(domain.VenueBinance, "BTC-USDT")
	b.LoadSnapshot(snap(1))
	if b.Crossed() {
		t.Fatal("not crossed")
	}
	b.ApplyDelta(domain.BookDelta{Sequence: 2, Asks: []domain.Level{lvl("99999", "1")}})
	if !b.Crossed() {
		t.Fatal("should detect crossed book")
	}
}

func TestDigestDeterministic(t *testing.T) {
	mk := func() *Book {
		b := New(domain.VenueBinance, "BTC-USDT")
		b.LoadSnapshot(snap(7))
		return b
	}
	if mk().Digest() != mk().Digest() {
		t.Fatal("digest must be deterministic")
	}
	b := mk()
	b.ApplyDelta(domain.BookDelta{Sequence: 8, Bids: []domain.Level{lvl("1", "1")}})
	if b.Digest() == mk().Digest() {
		t.Fatal("digest should change with content")
	}
}

func BenchmarkApplyDelta(b *testing.B) {
	bk := New(domain.VenueBinance, "BTC-USDT")
	bk.LoadSnapshot(snap(1))
	d := domain.BookDelta{
		Bids: []domain.Level{lvl("100001", "1"), lvl("100002", "2")},
		Asks: []domain.Level{lvl("100011", "1")},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Sequence = int64(i + 2)
		bk.ApplyDelta(d)
	}
}

func BenchmarkVWAP(b *testing.B) {
	bk := New(domain.VenueBinance, "BTC-USDT")
	s := snap(1)
	for i := 0; i < 100; i++ {
		s.Asks = append(s.Asks, domain.Level{
			Price: decimal.FromInt(100030 + int64(i)),
			Qty:   decimal.FromInt(1),
		})
	}
	bk.LoadSnapshot(s)
	qty := px(&testing.T{}, "50")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bk.VWAP(domain.Sell, qty)
	}
}
