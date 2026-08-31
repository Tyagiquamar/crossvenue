// Command loadgen generates deterministic synthetic market data into a
// recording file (or stdout stats). Used for stress tests and offline demos.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/internal/replay"
	"crossvenue/internal/venue/synthetic"
)

func main() {
	venues := flag.Int("venues", 3, "number of synthetic venues")
	symbols := flag.Int("symbols", 1, "number of symbols")
	eps := flag.Int("events-per-second", 1000, "events per second per venue+symbol")
	dur := flag.Duration("duration", 10*time.Second, "generation duration")
	seed := flag.Int64("seed", 42, "deterministic seed")
	out := flag.String("out", "data/loadgen.recording", "output recording path")
	flag.Parse()

	if dir := filepath.Dir(*out); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	rec, err := replay.NewRecorder(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	venueNames := []domain.Venue{domain.VenueBinance, domain.VenueOKX, domain.VenueBybit, domain.VenueSynthetic}
	total := 0
	for v := 0; v < *venues && v < len(venueNames); v++ {
		for sIdx := 0; sIdx < *symbols; sIdx++ {
			sym := fmt.Sprintf("SYM%d-USDT", sIdx)
			if sIdx == 0 {
				sym = "BTC-USDT"
			}
			g := synthetic.New(synthetic.Options{
				Venue:        venueNames[v],
				Seed:         *seed + int64(v)*7919 + int64(sIdx)*104729,
				EventsPerSec: *eps,
				ManualTime:   true,
				BasePrice:    100000 + int64(sIdx)*1000,
			})
			if err := rec.Record(g.Snapshot(sym)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			n := *eps * int(dur.Seconds())
			for i := 0; i < n; i++ {
				if err := rec.Record(g.Delta(sym)); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				total++
			}
		}
	}
	if err := rec.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d events to %s (digest %016x)\n", total, *out, rec.Digest())
}
