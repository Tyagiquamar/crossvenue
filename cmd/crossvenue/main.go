// Command crossvenue is the main engine binary.
//
// Modes:
//
//	live-market-sim   real public venue feeds, paper execution
//	replay            deterministic replay of a recording
//	synthetic         offline generated feeds (default, no internet needed)
//
// Execution is ALWAYS simulated. No live funds are required and this binary
// cannot submit real orders.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crossvenue/internal/api"
	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/engine"
	"crossvenue/internal/journal"
	"crossvenue/internal/replay"
	"crossvenue/internal/storage"
	"crossvenue/internal/venue"
	"crossvenue/internal/venue/binance"
	"crossvenue/internal/venue/bybit"
	"crossvenue/internal/venue/okx"
	"crossvenue/internal/venue/synthetic"
)

// readSchema locates the migration file in dev (repo root) or container
// (/migrations) layouts.
func readSchema() (string, error) {
	for _, p := range []string{"migrations/001_init.sql", "/migrations/001_init.sql", "../migrations/001_init.sql"} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("schema not found")
}

func main() {
	var (
		cfgPath   = flag.String("config", "", "path to config.yaml")
		mode      = flag.String("mode", "", "mode override: live-market-sim|replay|synthetic")
		recording = flag.String("recording", "", "recording file (replay mode input / live record output)")
		speed     = flag.String("speed", "max", "replay speed: 1x|10x|max")
		seed      = flag.Int64("seed", 0, "synthetic seed override")
		record    = flag.Bool("record", false, "record normalized events to --recording")
		serveAPI  = flag.Bool("api", true, "serve the operational HTTP API")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
	}
	if *seed != 0 {
		cfg.Synthetic.Seed = *seed
	}
	if *recording != "" {
		cfg.Replay.File = *recording
	}
	if *speed != "" {
		cfg.Replay.Speed = *speed
	}
	if err := cfg.Validate(); err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}

	log.Info("crossvenue starting",
		"mode", cfg.Mode, "symbols", cfg.Symbols, "venues", cfg.EnabledVenues(),
		"execution", "SIMULATION (paper)")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Durable journal sink if Postgres configured.
	var store *storage.Store
	var sinks []journal.Sink
	if cfg.Storage.PostgresURL != "" {
		store, err = storage.Open(ctx, cfg.Storage.PostgresURL)
		if err != nil {
			log.Error("storage open", "err", err)
			os.Exit(1)
		}
		defer store.Close()
		if schema, rerr := readSchema(); rerr == nil {
			if err := store.Migrate(ctx, schema); err != nil {
				log.Error("migrate", "err", err)
				os.Exit(1)
			}
		}
		sinks = append(sinks, store.EventSink())
		// Recovery: restore balances. Books always start invalid and
		// resynchronize from fresh market data — never loaded from storage.
		if bals, rerr := store.LoadBalances(ctx); rerr == nil && len(bals) > 0 {
			log.Info("recovered balances", "count", len(bals))
		}
	}
	j := journal.New(sinks...)

	eng, err := engine.New(cfg, j, nil, log)
	if err != nil {
		log.Error("engine", "err", err)
		os.Exit(1)
	}

	// Optional recorder tee.
	var rec *replay.Recorder
	if *record || cfg.Recording.Enabled {
		path := cfg.Replay.File
		if path == "" {
			path = filepath.Join("data", "recordings",
				fmt.Sprintf("crossvenue-%d.recording", time.Now().Unix()))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Error("recording dir", "err", err)
			os.Exit(1)
		}
		rec, err = replay.NewRecorder(path)
		if err != nil {
			log.Error("recorder", "err", err)
			os.Exit(1)
		}
		defer func() {
			_ = rec.Close()
			log.Info("recording closed", "path", path, "digest", fmt.Sprintf("%016x", rec.Digest()))
		}()
		eng.AttachRecorder(rec)
	}

	// Build the event source per mode.
	var src engine.EventSource
	var adapters []venue.MarketDataAdapter
	switch cfg.Mode {
	case config.ModeLiveSim:
		for _, v := range cfg.EnabledVenues() {
			var a venue.MarketDataAdapter
			switch v {
			case domain.VenueBinance:
				a = binance.New()
			case domain.VenueOKX:
				a = okx.New()
			case domain.VenueBybit:
				a = bybit.New()
			default:
				continue
			}
			adapters = append(adapters, a)
		}
		if len(adapters) == 0 {
			log.Error("live-market-sim requires at least one real venue enabled")
			os.Exit(2)
		}
		src = &engine.LiveSource{Adapters: adapters, Symbols: cfg.Symbols}
	case config.ModeSynthetic:
		for i, v := range cfg.EnabledVenues() {
			g := synthetic.New(synthetic.Options{
				Venue:        v,
				Seed:         cfg.Synthetic.Seed + int64(i)*7919,
				EventsPerSec: cfg.Synthetic.EventsPerSec,
				GapEvery:     cfg.Synthetic.GapEvery,
			})
			adapters = append(adapters, g)
		}
		src = &engine.LiveSource{Adapters: adapters, Symbols: cfg.Symbols}
	case config.ModeReplay:
		if cfg.Replay.File == "" {
			log.Error("replay mode requires --recording")
			os.Exit(2)
		}
		src = &engine.ReplaySource{Path: cfg.Replay.File, Speed: cfg.Replay.Speed}
	}

	// API server.
	if *serveAPI {
		srv := &http.Server{
			Addr:              cfg.API.Listen,
			Handler:           api.New(eng, adapters).Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("api listening", "addr", cfg.API.Listen)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("api", "err", err)
			}
		}()
		defer func() {
			shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shCtx)
		}()
	}

	runErr := eng.Run(ctx, src)

	// Persist final portfolio state.
	if store != nil {
		pCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.SavePortfolio(pCtx, eng.Ledger.Balances(), eng.Ledger.Positions()); err != nil {
			log.Error("save portfolio", "err", err)
		}
	}

	digests := eng.BookDigests()
	for k, d := range digests {
		log.Info("final book digest", "book", k, "digest", fmt.Sprintf("%016x", d))
	}
	log.Info("shutdown",
		"realized_pnl", eng.Ledger.Realized().String(),
		"journal_digest", fmt.Sprintf("%016x", j.Digest()),
		"err", runErr)
	if runErr != nil && ctx.Err() == nil {
		os.Exit(1)
	}
}
