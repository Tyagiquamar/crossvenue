// Package synthetic generates deterministic market data for demos, tests,
// and offline development. It implements venue.MarketDataAdapter with a
// seeded RNG so replays and demos are reproducible.
package synthetic

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// Options configure the generator.
type Options struct {
	Venue         domain.Venue
	Seed          int64
	BasePrice     int64 // whole USDT, e.g. 100000
	EventsPerSec  int   // approximate rate
	VolatilityBps int   // per-tick random walk step
	DepthLevels   int
	// GapEvery, when >0, injects a sequence gap every N events (failure
	// scene testing).
	GapEvery int
	// BurstEvery/StaleForMs inject stalls; 0 disables.
	BurstEvery int
	StaleForMs int
	ManualTime bool // when true, timestamps come from a caller-driven clock
	StartTime  time.Time
}

// Generator is a deterministic market-data source.
type Generator struct {
	opts Options
	rng  *rand.Rand

	mu      sync.Mutex
	seq     int64
	mid     decimal.Fixed
	healthy atomic.Bool
	lastMsg atomic.Int64
	emitted atomic.Uint64
	now     time.Time
}

// New creates a generator.
func New(opts Options) *Generator {
	if opts.EventsPerSec <= 0 {
		opts.EventsPerSec = 100
	}
	if opts.DepthLevels <= 0 {
		opts.DepthLevels = 10
	}
	if opts.BasePrice <= 0 {
		opts.BasePrice = 100000
	}
	if opts.VolatilityBps <= 0 {
		opts.VolatilityBps = 5
	}
	if opts.StartTime.IsZero() {
		opts.StartTime = time.Unix(1_700_000_000, 0).UTC()
	}
	g := &Generator{
		opts: opts,
		rng:  rand.New(rand.NewSource(opts.Seed)),
		mid:  decimal.FromInt(opts.BasePrice),
		now:  opts.StartTime,
	}
	g.healthy.Store(true)
	return g
}

// Venue implements venue.MarketDataAdapter.
func (g *Generator) Venue() domain.Venue { return g.opts.Venue }

// Connect implements venue.MarketDataAdapter.
func (g *Generator) Connect(context.Context) error { return nil }

// RequestResync implements venue.MarketDataAdapter: the generator re-emits
// a snapshot on its next cycle; no external action is needed.
func (g *Generator) RequestResync(string) {}

// Close implements venue.MarketDataAdapter.
func (g *Generator) Close() error {
	g.healthy.Store(false)
	return nil
}

// Health implements venue.MarketDataAdapter.
func (g *Generator) Health() domain.VenueHealth {
	return domain.VenueHealth{
		Connected:     g.healthy.Load(),
		LastMessageAt: time.Unix(0, g.lastMsg.Load()),
	}
}

func (g *Generator) tickNow() time.Time {
	if g.opts.ManualTime {
		g.now = g.now.Add(time.Second / time.Duration(g.opts.EventsPerSec))
		return g.now
	}
	return time.Now()
}

// Snapshot builds a full-depth snapshot around the current mid.
func (g *Generator) Snapshot(symbol string) domain.MarketEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	ts := g.tickNow()
	g.lastMsg.Store(ts.UnixNano())
	return domain.MarketEvent{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
		Venue: g.opts.Venue, Symbol: symbol, Sequence: g.seq,
		Bids: g.sideLocked(domain.Buy), Asks: g.sideLocked(domain.Sell),
		ExchangeTime: ts, ReceiveTime: ts,
	}}
}

func (g *Generator) sideLocked(side domain.Side) []domain.Level {
	levels := make([]domain.Level, g.opts.DepthLevels)
	for i := 0; i < g.opts.DepthLevels; i++ {
		offset := decimal.FromInt(int64(i + 1)).MulInt(10) // 10 USDT spacing
		px := g.mid.Add(offset)
		if side == domain.Buy {
			px = g.mid.Sub(offset)
		}
		qty := decimal.FromInt(int64(g.rng.Intn(5) + 1)).Div(decimal.FromInt(10))
		levels[i] = domain.Level{Price: px, Qty: qty}
	}
	return levels
}

// Delta generates the next incremental update.
func (g *Generator) Delta(symbol string) domain.MarketEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	// random walk the mid
	stepBps := int64(g.rng.Intn(g.opts.VolatilityBps*2+1) - g.opts.VolatilityBps)
	g.mid = g.mid.Add(g.mid.MulBps(stepBps))
	if g.mid.Cmp(decimal.FromInt(1000)) < 0 {
		g.mid = decimal.FromInt(1000)
	}
	g.seq++
	seq := g.seq
	// inject a gap deterministically for failure-scene testing
	if g.opts.GapEvery > 0 && seq%int64(g.opts.GapEvery) == 0 {
		g.seq += 2 // skip sequences
		seq = g.seq
	}
	ts := g.tickNow()
	g.lastMsg.Store(ts.UnixNano())
	g.emitted.Add(1)
	// touch 1-3 random levels near the top
	n := g.rng.Intn(3) + 1
	bids := make([]domain.Level, 0, n)
	asks := make([]domain.Level, 0, n)
	for i := 0; i < n; i++ {
		lvl := int64(g.rng.Intn(g.opts.DepthLevels) + 1)
		off := decimal.FromInt(lvl).MulInt(10)
		qty := decimal.FromInt(int64(g.rng.Intn(50) + 1)).Div(decimal.FromInt(100))
		if g.rng.Intn(10) == 0 {
			qty = decimal.Zero // occasional level removal
		}
		bids = append(bids, domain.Level{Price: g.mid.Sub(off), Qty: qty})
		asks = append(asks, domain.Level{Price: g.mid.Add(off), Qty: qty})
	}
	return domain.MarketEvent{Type: domain.EventDelta, Delta: &domain.BookDelta{
		Venue: g.opts.Venue, Symbol: symbol,
		Bids: bids, Asks: asks,
		Sequence: seq, PrevSequence: seq - 1,
		ExchangeTime: ts, ReceiveTime: ts,
	}}
}

// SubscribeBook emits an initial snapshot then a stream of deltas at the
// configured rate until ctx is cancelled.
func (g *Generator) SubscribeBook(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error {
	for _, s := range symbols {
		select {
		case out <- g.Snapshot(s):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	interval := time.Second / time.Duration(g.opts.EventsPerSec)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			i++
			if g.opts.BurstEvery > 0 && i%g.opts.BurstEvery == 0 && g.opts.StaleForMs > 0 {
				// simulate a feed stall (staleness failure scene)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(g.opts.StaleForMs) * time.Millisecond):
				}
			}
			for _, s := range symbols {
				select {
				case out <- g.Delta(s):
				case <-ctx.Done():
					return ctx.Err()
				default:
					// downstream saturated: generator does not block; the
					// pipeline's own backpressure policy handles overflow.
				}
			}
		}
	}
}

// Emitted returns total events produced.
func (g *Generator) Emitted() uint64 { return g.emitted.Load() }
