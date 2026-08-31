package opportunity

import (
	"testing"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/clock"
	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func fx(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func baseCfg() Config {
	return Config{
		MinProfit:     fx("1"),
		MinEdgeBps:    0,
		MaxQuoteAge:   750 * time.Millisecond,
		TradeQuantity: fx("1"),
		SlippageBps:   1,
	}
}

func setupBooks(tb testing.TB, now time.Time) *book.Manager {
	tb.Helper()
	m := book.NewManager()
	// Binance cheap ask; OKX rich bid: 30 USDT gross at size 1.
	bin := m.Get(domain.VenueBinance, "BTC-USDT")
	bin.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 1,
		Asks:        []domain.Level{{Price: fx("100000"), Qty: fx("2")}},
		Bids:        []domain.Level{{Price: fx("99990"), Qty: fx("2")}},
		ReceiveTime: now,
	})
	okx := m.Get(domain.VenueOKX, "BTC-USDT")
	okx.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueOKX, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fx("100030"), Qty: fx("2")}},
		Asks:        []domain.Level{{Price: fx("100100"), Qty: fx("2")}},
		ReceiveTime: now,
	})
	return m
}

func TestOpportunityAfterCosts(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(t, now)
	fees := FeeSchedule{
		domain.VenueBinance: {TakerBps: 10},
		domain.VenueOKX:     {TakerBps: 10},
	}
	cfg := baseCfg()
	cfg.MinProfit = fx("-500") // allow emission to inspect cost math
	cfg.MinEdgeBps = -1000000  // negative edge allowed for math inspection
	e := New(m, fees, LatencyModel{}, cfg, clock.NewManualClock(now))
	venues := []domain.Venue{domain.VenueBinance, domain.VenueOKX}
	opps := e.Evaluate("BTC-USDT", venues)
	if len(opps) != 1 {
		t.Fatalf("expected 1 opportunity, got %d", len(opps))
	}
	o := opps[0]
	if o.BuyVenue != domain.VenueBinance || o.SellVenue != domain.VenueOKX {
		t.Fatalf("venues %s/%s", o.BuyVenue, o.SellVenue)
	}
	// gross 30; fees 10bps*(100000+100030) = 200.03; slippage 1bps*100000 = 10
	if o.GrossPnL.String() != "30" {
		t.Fatalf("gross %s", o.GrossPnL)
	}
	if o.Fees.String() != "200.03" {
		t.Fatalf("fees %s", o.Fees)
	}
	if !o.ExpectedNetPnL.IsNegative() {
		t.Fatalf("net %s should be negative after fees", o.ExpectedNetPnL)
	}
}

// BenchmarkEvaluate prices all venue pairs for one symbol (3 venues = 6
// directed pairs) including depth walk, fees, slippage, and latency penalty.
func BenchmarkEvaluate(b *testing.B) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(b, now)
	m.Get(domain.VenueBybit, "BTC-USDT").LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBybit, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fx("100020"), Qty: fx("2")}},
		Asks:        []domain.Level{{Price: fx("100090"), Qty: fx("2")}},
		ReceiveTime: now,
	})
	fees := FeeSchedule{
		domain.VenueBinance: {TakerBps: 10},
		domain.VenueOKX:     {TakerBps: 10},
		domain.VenueBybit:   {TakerBps: 10},
	}
	e := New(m, fees, LatencyModel{}, baseCfg(), clock.NewManualClock(now))
	venues := []domain.Venue{domain.VenueBinance, domain.VenueOKX, domain.VenueBybit}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Evaluate("BTC-USDT", venues)
	}
}

func TestRejectedWhenNotProfitable(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(t, now)
	fees := FeeSchedule{
		domain.VenueBinance: {TakerBps: 10},
		domain.VenueOKX:     {TakerBps: 10},
	}
	rej := make(chan Rejection, 16)
	e := New(m, fees, LatencyModel{}, baseCfg(), clock.NewManualClock(now))
	e.Rejections = rej
	opps := e.Evaluate("BTC-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX})
	if len(opps) != 0 {
		t.Fatal("high fees must kill the opportunity")
	}
	select {
	case r := <-rej:
		if r.Reason != "below_min_profit" {
			t.Fatalf("reason %s", r.Reason)
		}
	default:
		t.Fatal("expected rejection record")
	}
}

func TestStaleBookExcluded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(t, now)
	m.Get(domain.VenueOKX, "BTC-USDT").MarkStale(true)
	e := New(m, FeeSchedule{}, LatencyModel{}, baseCfg(), clock.NewManualClock(now))
	if opps := e.Evaluate("BTC-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX}); len(opps) != 0 {
		t.Fatal("stale books must not produce opportunities")
	}
}

func TestOldQuoteExcluded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(t, now)
	// advance clock 2s past the quote
	clk := clock.NewManualClock(now.Add(2 * time.Second))
	e := New(m, FeeSchedule{}, LatencyModel{}, baseCfg(), clk)
	if opps := e.Evaluate("BTC-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX}); len(opps) != 0 {
		t.Fatal("old quotes must not produce opportunities")
	}
}

func TestNotReadyBookExcluded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := setupBooks(t, now)
	m.Get(domain.VenueBinance, "BTC-USDT").Invalidate()
	e := New(m, FeeSchedule{}, LatencyModel{}, baseCfg(), clock.NewManualClock(now))
	if opps := e.Evaluate("BTC-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX}); len(opps) != 0 {
		t.Fatal("not-ready books must not produce opportunities")
	}
}

func TestDepthAwareQuantity(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := book.NewManager()
	// buy side: 0.3 @ 100 + 0.7 @ 101; sell side 1 @ 104
	buy := m.Get(domain.VenueBinance, "X-USDT")
	buy.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "X-USDT", Sequence: 1,
		Asks: []domain.Level{
			{Price: fx("100"), Qty: fx("0.3")},
			{Price: fx("101"), Qty: fx("0.7")},
		},
		Bids:        []domain.Level{{Price: fx("99"), Qty: fx("5")}},
		ReceiveTime: now,
	})
	sell := m.Get(domain.VenueOKX, "X-USDT")
	sell.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueOKX, Symbol: "X-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fx("104"), Qty: fx("1")}},
		Asks:        []domain.Level{{Price: fx("105"), Qty: fx("5")}},
		ReceiveTime: now,
	})
	e := New(m, FeeSchedule{}, LatencyModel{}, baseCfg(), clock.NewManualClock(now))
	opps := e.Evaluate("X-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX})
	if len(opps) != 1 {
		t.Fatalf("expected 1, got %d", len(opps))
	}
	// buy vwap = (0.3*100 + 0.7*101)/1 = 100.7 ; gross = 3.3
	o := opps[0]
	if o.BuyVWAP.String() != "100.7" {
		t.Fatalf("buy vwap %s", o.BuyVWAP)
	}
	if o.GrossPnL.String() != "3.3" {
		t.Fatalf("gross %s", o.GrossPnL)
	}
}
