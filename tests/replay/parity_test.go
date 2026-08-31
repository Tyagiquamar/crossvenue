// Package replay_test proves deterministic replay parity: the same
// recording + config + seed produce identical book digests, journal digest,
// and realized PnL across independent runs.
package replay_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"crossvenue/internal/clock"
	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/engine"
	"crossvenue/internal/journal"
	"crossvenue/internal/replay"
	"crossvenue/internal/venue/synthetic"
)

func buildFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parity.recording")
	rec, err := replay.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic manual-time generators on three venues.
	for i, v := range []domain.Venue{domain.VenueBinance, domain.VenueOKX, domain.VenueBybit} {
		g := synthetic.New(synthetic.Options{
			Venue: v, Seed: 42 + int64(i)*7919, EventsPerSec: 1000,
			ManualTime: true, BasePrice: 100000 + int64(i)*3,
			StartTime: time.Unix(1_700_000_000, 0).UTC(),
		})
		if err := rec.Record(g.Snapshot("BTC-USDT")); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < 300; n++ {
			if err := rec.Record(g.Delta("BTC-USDT")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func runReplay(t *testing.T, path string) (map[string]uint64, uint64, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeReplay
	cfg.Symbols = []string{"BTC-USDT"}
	cfg.Risk.MinProfitUSD = "-1000000"
	cfg.Risk.MinEdgeBps = -1000000
	cfg.Risk.MaxQuoteAgeMs = 3600000 // manual clock pinned near fixture era

	// Pin decision time close to fixture timestamps so quote-age checks are
	// deterministic and satisfied.
	clk := clock.NewManualClock(time.Unix(1_700_000_001, 0).UTC())
	j := journal.New()
	eng, err := engine.New(cfg, j, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := &engine.ReplaySource{Path: path, Speed: "max"}
	if err := eng.Run(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	return eng.BookDigests(), j.Digest(), eng.Ledger.Realized().String()
}

func TestReplayParity(t *testing.T) {
	path := buildFixture(t)
	d1, j1, p1 := runReplay(t, path)
	d2, j2, p2 := runReplay(t, path)

	if j1 != j2 {
		t.Fatalf("journal digests differ: %016x vs %016x", j1, j2)
	}
	if p1 != p2 {
		t.Fatalf("pnl differs: %s vs %s", p1, p2)
	}
	if len(d1) == 0 {
		t.Fatal("no books")
	}
	for k, v1 := range d1 {
		if d2[k] != v1 {
			t.Fatalf("book %s digest differs: %016x vs %016x", k, v1, d2[k])
		}
	}
}
