// Package integration_test exercises end-to-end failure scenes through the
// real pipeline: gap invalidation, stale protection, partial two-leg fills,
// idempotency, restart, kill switch, and overload resync.
package integration_test

import (
	"context"
	"testing"
	"time"

	"crossvenue/internal/clock"
	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/engine"
	"crossvenue/internal/execution"
	"crossvenue/internal/journal"
	"crossvenue/pkg/decimal"
)

func fp(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func testCfg() *config.Config {
	c := config.Default()
	c.Symbols = []string{"BTC-USDT"}
	c.Execution.TradeQty = "0.01"
	c.Risk.MinProfitUSD = "-1000000" // never gate on profit in scene tests
	c.Risk.MinEdgeBps = -1000000
	c.Risk.MaxQuoteAgeMs = 60000 // generous for manual clocks
	return c
}

func snapEvent(v domain.Venue, seq int64, bid, ask string, ts time.Time) domain.MarketEvent {
	return domain.MarketEvent{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
		Venue: v, Symbol: "BTC-USDT", Sequence: seq,
		Bids:         []domain.Level{{Price: fp(bid), Qty: fp("5")}},
		Asks:         []domain.Level{{Price: fp(ask), Qty: fp("5")}},
		ExchangeTime: ts, ReceiveTime: ts,
	}}
}

func deltaEvent(v domain.Venue, seq, prev int64, ask string, ts time.Time) domain.MarketEvent {
	return domain.MarketEvent{Type: domain.EventDelta, Delta: &domain.BookDelta{
		Venue: v, Symbol: "BTC-USDT", Sequence: seq, PrevSequence: prev,
		Asks:        []domain.Level{{Price: fp(ask), Qty: fp("5")}},
		ReceiveTime: ts, ExchangeTime: ts,
	}}
}

// ingestSync submits events and waits briefly for the async lane to apply.
func ingestSync(t *testing.T, eng *engine.Supervisor, evs ...domain.MarketEvent) {
	t.Helper()
	for _, ev := range evs {
		eng.Ingest(ev)
	}
	time.Sleep(20 * time.Millisecond)
}

func startEngine(t *testing.T, cfg *config.Config) *engine.Supervisor {
	t.Helper()
	j := journal.New()
	eng, err := engine.New(cfg, j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go eng.Pipe.Run(ctx)
	t.Cleanup(func() {
		cancel()
		eng.Pipe.Stop()
	})
	return eng
}

func TestScene1SequenceGapInvalidatesAndResyncs(t *testing.T) {
	eng := startEngine(t, testCfg())
	ts := time.Now()
	b, okx := domain.VenueBinance, domain.VenueOKX

	ingestSync(t, eng,
		snapEvent(b, 100, "99990", "100000", ts),
		snapEvent(okx, 1, "100030", "100100", ts),
	)
	bookState := func() (bool, bool) {
		v, ok := eng.Books.Snapshot(b, "BTC-USDT", 1)
		if !ok {
			return false, false
		}
		return v.State.Ready, true
	}
	if ready, _ := bookState(); !ready {
		t.Fatal("book should be ready")
	}
	// Gap: binance delta with prev way off.
	ingestSync(t, eng, deltaEvent(b, 200, 150, "100001", ts))
	if ready, _ := bookState(); ready {
		t.Fatal("gap must invalidate the book")
	}
	// Resync: fresh snapshot.
	ingestSync(t, eng, snapEvent(b, 500, "99990", "100000", ts))
	if ready, _ := bookState(); !ready {
		t.Fatal("book must be ready after resnapshot")
	}
	// Journal must record invalidation + resync.
	types := map[string]bool{}
	for _, e := range eng.Journal.Events() {
		types[e.Type] = true
	}
	if !types["BookInvalidated"] || !types["BookResynced"] {
		t.Fatalf("journal missing transitions: %v", types)
	}
}

func TestScene3StaleQuoteRejected(t *testing.T) {
	cfg := testCfg()
	cfg.Risk.MaxQuoteAgeMs = 50
	j := journal.New()
	clk := clock.NewManualClock(time.Unix(1_000_000, 0))
	eng, err := engine.New(cfg, j, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Pipe.Run(ctx)
	t.Cleanup(eng.Pipe.Stop)

	ts := clk.Now()
	ingestSync(t, eng,
		snapEvent(domain.VenueBinance, 100, "99990", "100000", ts),
		snapEvent(domain.VenueOKX, 1, "100030", "100100", ts),
	)
	// Advance manual clock past max quote age.
	clk.Advance(5 * time.Second)
	opps := eng.Opp.Evaluate("BTC-USDT", []domain.Venue{domain.VenueBinance, domain.VenueOKX})
	if len(opps) != 0 {
		t.Fatal("stale quotes must not produce opportunities")
	}
}

func TestScene5DuplicateOrderID(t *testing.T) {
	eng := startEngine(t, testCfg())
	ts := time.Now()
	ingestSync(t, eng, snapEvent(domain.VenueBinance, 1, "99990", "100000", ts))
	now := time.Now()
	o1, err := eng.Exec.Submit(dupOrder("dup-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := eng.Exec.Submit(dupOrder("dup-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if o1.ID != o2.ID {
		t.Fatal("duplicate client order ID must return the same order")
	}
}

func dupOrder(id string) execution.Order {
	return execution.Order{
		ClientOrderID: id, Venue: domain.VenueBinance, Symbol: "BTC-USDT",
		Side: domain.Buy, Type: execution.Market, Quantity: fp("0.01"),
	}
}

func TestScene7KillSwitchBlocksExecution(t *testing.T) {
	eng := startEngine(t, testCfg())
	ts := time.Now()
	// Create a profitable spread: buy binance @100000, sell okx @100030.
	ingestSync(t, eng,
		snapEvent(domain.VenueBinance, 1, "99990", "100000", ts),
		snapEvent(domain.VenueOKX, 1, "100030", "100100", ts),
	)
	eng.Risk.ActivateKillSwitch("test")
	before := len(eng.Exec.Orders())
	eng.EvaluateOnce()
	if len(eng.Exec.Orders()) != before {
		t.Fatal("kill switch must block order submission")
	}
	found := false
	for _, e := range eng.Journal.Events() {
		if e.Type == "RiskRejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected RiskRejected journal event")
	}
}

func TestScene8QueueOverloadInvalidates(t *testing.T) {
	cfg := testCfg()
	j := journal.New()
	eng, err := engine.New(cfg, j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No pipeline running: lane queue fills instantly.
	ts := time.Now()
	eng.Ingest(snapEvent(domain.VenueBinance, 1, "99990", "100000", ts))
	for i := 0; i < 5000; i++ {
		eng.Ingest(deltaEvent(domain.VenueBinance, int64(i+2), int64(i+1), "100001", ts))
	}
	v, ok := eng.Books.Snapshot(domain.VenueBinance, "BTC-USDT", 1)
	if ok && v.State.Ready {
		t.Fatal("overload must invalidate the book, not silently drop deltas")
	}
}
