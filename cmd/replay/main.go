// Command replay runs a recording through the engine and prints the final
// deterministic digest. Run it twice on the same recording and config: the
// digests must match (see tests/replay for the automated parity test).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"crossvenue/internal/config"
	"crossvenue/internal/engine"
	"crossvenue/internal/journal"
)

func main() {
	recPath := flag.String("recording", "", "recording file (required)")
	cfgPath := flag.String("config", "", "config.yaml")
	speed := flag.String("speed", "max", "1x|10x|max")
	flag.Parse()
	if *recPath == "" {
		fmt.Fprintln(os.Stderr, "--recording is required")
		os.Exit(2)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg.Mode = config.ModeReplay
	cfg.Replay.File = *recPath
	cfg.Replay.Speed = *speed

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	j := journal.New()
	eng, err := engine.New(cfg, j, nil, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	src := &engine.ReplaySource{Path: *recPath, Speed: *speed}
	if err := eng.Run(context.Background(), src); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Sorted output: replay stdout must be byte-identical across runs so
	// its SHA256 is a stable parity proof (map iteration order is random).
	digests := eng.BookDigests()
	keys := make([]string, 0, len(digests))
	for k := range digests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("book %s digest %016x\n", k, digests[k])
	}
	fmt.Printf("journal digest %016x\n", j.Digest())
	fmt.Printf("realized_pnl %s\n", eng.Ledger.Realized().String())
}
