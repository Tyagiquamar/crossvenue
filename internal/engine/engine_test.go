package engine

import (
	"testing"
	"time"

	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/journal"
	"crossvenue/internal/opportunity"
	"crossvenue/pkg/decimal"
)

func fp(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// Failure scene 4: when the second leg's depth disappears between pricing
// and the fill (the realistic partial-fill path), execution fills
// partially and the engine must journal the residual directional exposure.
// The opportunity engine rejects shallow books pre-trade, so this test
// drives the execution path directly with a book that is deep for the buy
// leg and thin for the sell leg.
func TestPartialSecondLegFillJournalsResidualExposure(t *testing.T) {
	cfg := config.Default()
	cfg.Symbols = []string{"BTC-USDT"}
	j := journal.New()
	s, err := New(cfg, j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	s.Books.Get(domain.VenueBinance, "BTC-USDT").LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fp("99990"), Qty: fp("5")}},
		Asks:        []domain.Level{{Price: fp("100000"), Qty: fp("5")}},
		ReceiveTime: now, ExchangeTime: now,
	})
	s.Books.Get(domain.VenueOKX, "BTC-USDT").LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueOKX, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fp("100030"), Qty: fp("0.01")}},
		Asks:        []domain.Level{{Price: fp("100100"), Qty: fp("5")}},
		ReceiveTime: now, ExchangeTime: now,
	})

	o := opportunity.Opportunity{
		ID: "opp-partial", Symbol: "BTC-USDT",
		BuyVenue: domain.VenueBinance, SellVenue: domain.VenueOKX,
		Quantity: fp("0.02"), BuyVWAP: fp("100000"), SellVWAP: fp("100030"),
		ExpectedNetPnL: fp("5"), EdgeBps: 5, DetectedAt: now,
	}
	s.maybeExecute(o, now)

	var residual string
	found := false
	for _, e := range j.Events() {
		if e.Type == "PositionChanged" {
			found = true
			if r, ok := e.Payload["residual"].(string); ok {
				residual = r
			}
		}
	}
	if !found {
		t.Fatal("partial second-leg fill must journal PositionChanged residual exposure")
	}
	got, err := decimal.Parse(residual)
	if err != nil {
		t.Fatalf("residual %q unparseable: %v", residual, err)
	}
	if got.Cmp(fp("0.01")) != 0 {
		t.Fatalf("residual = %s, want 0.01 (0.02 bought, 0.01 sold)", got)
	}
}
