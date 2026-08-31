// Package marketdata owns the book-processing pipeline: one goroutine
// serially applies normalized events to books, enforces sequence rules via
// venue trackers, detects staleness, and publishes immutable snapshots.
package marketdata

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/clock"
	"crossvenue/internal/domain"
)

// Event kinds surfaced to the journal for observability.
const (
	JournalSnapshot   = "MarketSnapshotReceived"
	JournalDelta      = "MarketDeltaReceived"
	JournalInvalidate = "BookInvalidated"
	JournalResynced   = "BookResynced"
)

// JournalSink receives lifecycle events (sequence gaps, invalidations...).
type JournalSink interface {
	Record(eventType string, venue domain.Venue, symbol string, payload map[string]any)
}

// Metrics hooks (no-op by default; Prometheus adapter plugs in).
type Metrics interface {
	IncSequenceGap(venue domain.Venue)
	IncResync(venue domain.Venue)
	IncDropped(venue domain.Venue)
	SetQueueDepth(venue domain.Venue, depth int)
}

type nopMetrics struct{}

func (nopMetrics) IncSequenceGap(domain.Venue)     {}
func (nopMetrics) IncResync(domain.Venue)          {}
func (nopMetrics) IncDropped(domain.Venue)         {}
func (nopMetrics) SetQueueDepth(domain.Venue, int) {}

// Options configure the pipeline.
type Options struct {
	QueueSize       int           // per-book input channel capacity
	MaxBookAge      time.Duration // staleness threshold
	StaleSweepEvery time.Duration
	Logger          *slog.Logger
	Journal         JournalSink
	Metrics         Metrics
	Clock           clock.Clock
	// ResyncRequest is invoked when a gap or overload forces resync; the
	// venue adapter should re-snapshot. May be nil in tests.
	ResyncRequest func(venue domain.Venue, symbol string, reason string)
}

type bookLane struct {
	mu      sync.Mutex // serializes lane-goroutine apply with overload invalidation and sweeper marks
	bk      *book.Book
	tracker book.Tracker
	ch      chan domain.MarketEvent
}

// Pipeline routes events to per-book lanes and owns all book mutation.
type Pipeline struct {
	opts Options
	mgr  *book.Manager

	mu    sync.RWMutex
	lanes map[string]*bookLane

	wg     sync.WaitGroup
	cancel context.CancelFunc

	staleBooks atomic.Int64
}

// NewPipeline constructs the pipeline.
func NewPipeline(mgr *book.Manager, opts Options) *Pipeline {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.MaxBookAge <= 0 {
		opts.MaxBookAge = 750 * time.Millisecond
	}
	if opts.StaleSweepEvery <= 0 {
		opts.StaleSweepEvery = 100 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = nopMetrics{}
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	return &Pipeline{opts: opts, mgr: mgr, lanes: make(map[string]*bookLane)}
}

func laneKey(v domain.Venue, s string) string { return string(v) + "|" + s }

// Register creates a lane for venue+symbol with the venue's tracker.
func (p *Pipeline) Register(venue domain.Venue, symbol string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := laneKey(venue, symbol)
	if _, ok := p.lanes[k]; ok {
		return nil
	}
	tr, err := book.NewForVenue(venue)
	if err != nil {
		return err
	}
	lane := &bookLane{
		bk:      p.mgr.Get(venue, symbol),
		tracker: tr,
		ch:      make(chan domain.MarketEvent, p.opts.QueueSize),
	}
	p.lanes[k] = lane
	return nil
}

// ApplySync applies one event immediately on the caller's goroutine. It is
// used by deterministic replay; it MUST NOT run concurrently with Run
// (lane goroutines) — the supervisor guarantees this by mode.
func (p *Pipeline) ApplySync(ev domain.MarketEvent) error {
	k := laneKey(ev.VenueOf(), ev.SymbolOf())
	p.mu.RLock()
	lane, ok := p.lanes[k]
	p.mu.RUnlock()
	if !ok {
		if err := p.Register(ev.VenueOf(), ev.SymbolOf()); err != nil {
			return err
		}
		p.mu.RLock()
		lane = p.lanes[k]
		p.mu.RUnlock()
	}
	p.apply(lane, ev)
	return nil
}

// Submit enqueues an event. If the lane is full, the book is invalidated
// and resync is requested (correctness over liveness) — deltas are never
// silently dropped while continuing to trade.
func (p *Pipeline) Submit(ev domain.MarketEvent) error {
	p.mu.RLock()
	lane, ok := p.lanes[laneKey(ev.VenueOf(), ev.SymbolOf())]
	p.mu.RUnlock()
	if !ok {
		if err := p.Register(ev.VenueOf(), ev.SymbolOf()); err != nil {
			return err
		}
		p.mu.RLock()
		lane = p.lanes[laneKey(ev.VenueOf(), ev.SymbolOf())]
		p.mu.RUnlock()
	}
	select {
	case lane.ch <- ev:
		p.opts.Metrics.SetQueueDepth(ev.VenueOf(), len(lane.ch))
		return nil
	default:
		// Overload: invalidate + resync rather than corrupt sequencing.
		lane.mu.Lock()
		lane.bk.Invalidate()
		lane.mu.Unlock()
		p.opts.Metrics.IncDropped(ev.VenueOf())
		p.journal(JournalInvalidate, ev.VenueOf(), ev.SymbolOf(), map[string]any{"reason": "queue_overload"})
		if p.opts.ResyncRequest != nil {
			p.opts.ResyncRequest(ev.VenueOf(), ev.SymbolOf(), "queue_overload")
		}
		return nil
	}
}

func (p *Pipeline) journal(t string, v domain.Venue, s string, payload map[string]any) {
	if p.opts.Journal != nil {
		p.opts.Journal.Record(t, v, s, payload)
	}
}

// Run starts lane workers and the staleness sweeper until ctx is done.
func (p *Pipeline) Run(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.mu.RLock()
	lanes := make([]*bookLane, 0, len(p.lanes))
	for _, l := range p.lanes {
		lanes = append(lanes, l)
	}
	p.mu.RUnlock()
	for _, l := range lanes {
		p.wg.Add(1)
		go p.runLane(ctx, l)
	}
	p.wg.Add(1)
	go p.sweepStale(ctx)
	p.wg.Wait()
}

// Stop shuts the pipeline down.
func (p *Pipeline) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Pipeline) runLane(ctx context.Context, lane *bookLane) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-lane.ch:
			p.apply(lane, ev)
		}
	}
}

func (p *Pipeline) apply(lane *bookLane, ev domain.MarketEvent) {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	switch ev.Type {
	case domain.EventSnapshot:
		s := ev.Snapshot
		lane.tracker.OnSnapshot(s.Sequence)
		wasReady := lane.bk.State().Ready
		lane.bk.LoadSnapshot(*s)
		lane.bk.MarkStale(false)
		p.journal(JournalSnapshot, s.Venue, s.Symbol, map[string]any{"sequence": s.Sequence})
		if !wasReady {
			p.journal(JournalResynced, s.Venue, s.Symbol, map[string]any{"sequence": s.Sequence})
		}
	case domain.EventDelta:
		d := ev.Delta
		v := lane.tracker.Check(*d)
		switch v {
		case book.Apply:
			lane.bk.ApplyDelta(*d)
			lane.bk.MarkStale(false)
			p.journal(JournalDelta, d.Venue, d.Symbol, map[string]any{"sequence": d.Sequence})
		case book.Duplicate, book.Stale:
			// safe to ignore
		case book.Gap:
			lane.bk.Invalidate()
			p.opts.Metrics.IncSequenceGap(d.Venue)
			p.journal(JournalInvalidate, d.Venue, d.Symbol, map[string]any{
				"reason": "sequence_gap", "sequence": d.Sequence, "prev": d.PrevSequence,
			})
			if p.opts.ResyncRequest != nil {
				p.opts.ResyncRequest(d.Venue, d.Symbol, "sequence_gap")
			}
		case book.NeedSnapshot:
			lane.bk.Invalidate()
			if p.opts.ResyncRequest != nil {
				p.opts.ResyncRequest(d.Venue, d.Symbol, "need_snapshot")
			}
		}
	case domain.EventTrade:
		// trades currently pass through; books are depth-driven
	}
}

func (p *Pipeline) sweepStale(ctx context.Context) {
	defer p.wg.Done()
	t := time.NewTicker(p.opts.StaleSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := p.opts.Clock.Now()
			var stale int64
			p.mu.RLock()
			for _, lane := range p.lanes {
				lane.mu.Lock()
				st := lane.bk.State()
				if st.Ready {
					isStale := now.Sub(st.LastUpdatedAt) > p.opts.MaxBookAge
					lane.bk.MarkStale(isStale)
					if isStale {
						stale++
					}
				}
				lane.mu.Unlock()
			}
			p.mu.RUnlock()
			p.staleBooks.Store(stale)
		}
	}
}

// StaleBooks returns count of currently stale books.
func (p *Pipeline) StaleBooks() int64 { return p.staleBooks.Load() }
