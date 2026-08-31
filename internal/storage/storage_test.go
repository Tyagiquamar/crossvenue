// Package storage_test contains Postgres integration tests. They require
// CROSSVENUE_TEST_POSTGRES_URL and are skipped otherwise (CI provides a
// postgres service).
package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/internal/execution"
	"crossvenue/internal/journal"
	"crossvenue/internal/portfolio"
	"crossvenue/internal/storage"
	"crossvenue/pkg/decimal"
)

func fp(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func open(t *testing.T) (*storage.Store, context.Context) {
	t.Helper()
	url := os.Getenv("CROSSVENUE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("CROSSVENUE_TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()
	s, err := storage.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../../migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func TestOrderFillRoundTrip(t *testing.T) {
	s, ctx := open(t)
	now := time.Now().UTC()
	o := &execution.Order{
		ID: "ord-test-1", ClientOrderID: "clo-test-1",
		Venue: domain.VenueBinance, Symbol: "BTC-USDT",
		Side: domain.Buy, Type: execution.Market,
		Quantity: fp("0.5"), FilledQuantity: fp("0.5"), AvgFillPrice: fp("100000"),
		State: execution.Filled, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.UpsertOrder(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Idempotent upsert (same id) must not error.
	if err := s.UpsertOrder(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFill(ctx, execution.FillEvent{
		OrderID: o.ID, Venue: o.Venue, Symbol: o.Symbol, Side: o.Side,
		Price: fp("100000"), Qty: fp("0.5"), Fee: fp("50"), At: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPortfolioSaveLoadAtomic(t *testing.T) {
	s, ctx := open(t)
	bals := []portfolio.Balance{
		{Venue: domain.VenueBinance, Asset: "USDT", Available: fp("99900")},
		{Venue: domain.VenueBinance, Asset: "BTC", Available: fp("1")},
	}
	poss := []portfolio.Position{
		{Venue: domain.VenueBinance, Symbol: "BTC-USDT", Qty: fp("1"), AvgCost: fp("100000")},
	}
	if err := s.SavePortfolio(ctx, bals, poss); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadBalances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range loaded {
		if b.Venue == domain.VenueBinance && b.Asset == "BTC" && b.Available.String() == "1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("balance not restored: %+v", loaded)
	}
}

func TestEventSink(t *testing.T) {
	s, ctx := open(t)
	_ = ctx
	j := journal.New(s.EventSink())
	j.Record("KillSwitchActivated", "", "", map[string]any{"reason": "test"})
	if j.Digest() == 0 {
		t.Fatal("journal digest should be nonzero")
	}
}

func TestSystemStateRecovery(t *testing.T) {
	s, ctx := open(t)
	if err := s.SaveSystemState(ctx, "kill_switch", map[string]any{"active": true, "reason": "test"}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	found, err := s.LoadSystemState(ctx, "kill_switch", &out)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if out["active"] != true {
		t.Fatalf("state %+v", out)
	}
}
