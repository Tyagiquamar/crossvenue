// Package book maintains locally reconstructed order books per venue+symbol.
// A book is owned by a single goroutine (the Manager); all mutation is
// serialized through its input channel. Readers receive immutable snapshots.
package book

import (
	"sort"
	"sync"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// State describes synchronization state of a book.
type State struct {
	Ready         bool
	Stale         bool
	Sequence      int64
	LastUpdatedAt time.Time
	Version       uint64
}

// Book is a two-sided price-level map kept sorted on demand. Not safe for
// concurrent use; the ownership model serializes access. Every mutation
// publishes an immutable SnapshotView through the publish hook so readers
// never touch the book itself.
type Book struct {
	venue  domain.Venue
	symbol string

	bids map[int64]decimal.Fixed // price.Raw() -> qty
	asks map[int64]decimal.Fixed

	sortedBids []domain.Level // descending
	sortedAsks []domain.Level // ascending
	dirty      bool

	state State

	// publish, when set, receives an immutable view after each mutation.
	// Set by Manager.Get; invoked synchronously on the owner's goroutine.
	publish func(SnapshotView)
}

// publishDepth bounds levels included in published views.
const publishDepth = 512

// emit publishes the current view if a hook is installed.
func (b *Book) emit() {
	if b.publish != nil {
		b.publish(b.Snapshot(publishDepth))
	}
}

// New creates an empty, not-ready book.
func New(venue domain.Venue, symbol string) *Book {
	return &Book{
		venue:  venue,
		symbol: symbol,
		bids:   make(map[int64]decimal.Fixed),
		asks:   make(map[int64]decimal.Fixed),
		state:  State{Ready: false},
	}
}

// Venue returns the owning venue.
func (b *Book) Venue() domain.Venue { return b.venue }

// Symbol returns the instrument.
func (b *Book) Symbol() string { return b.symbol }

// State returns current synchronization state.
func (b *Book) State() State { return b.state }

// LoadSnapshot replaces all depth and marks the book ready at seq.
func (b *Book) LoadSnapshot(snap domain.BookSnapshot) {
	b.bids = make(map[int64]decimal.Fixed, len(snap.Bids))
	b.asks = make(map[int64]decimal.Fixed, len(snap.Asks))
	for _, l := range snap.Bids {
		if l.Qty.IsPositive() {
			b.bids[l.Price.Raw()] = l.Qty
		}
	}
	for _, l := range snap.Asks {
		if l.Qty.IsPositive() {
			b.asks[l.Price.Raw()] = l.Qty
		}
	}
	b.dirty = true
	b.state.Sequence = snap.Sequence
	b.state.Ready = true
	b.state.Stale = false
	b.state.LastUpdatedAt = snap.ReceiveTime
	b.state.Version++
	b.emit()
}

// ApplyDelta applies an incremental update. Caller (sequence tracker) must
// already have validated ordering. Zero-qty levels are removed.
func (b *Book) ApplyDelta(d domain.BookDelta) {
	for _, l := range d.Bids {
		if l.Qty.IsZero() {
			delete(b.bids, l.Price.Raw())
		} else if l.Qty.IsPositive() {
			b.bids[l.Price.Raw()] = l.Qty
		}
	}
	for _, l := range d.Asks {
		if l.Qty.IsZero() {
			delete(b.asks, l.Price.Raw())
		} else if l.Qty.IsPositive() {
			b.asks[l.Price.Raw()] = l.Qty
		}
	}
	b.dirty = true
	b.state.Sequence = d.Sequence
	b.state.LastUpdatedAt = d.ReceiveTime
	b.state.Version++
	b.emit()
}

// Invalidate marks the book unusable for opportunity detection. Depth is
// retained for diagnostics but Ready is false until resnapshot.
func (b *Book) Invalidate() {
	b.state.Ready = false
	b.state.Version++
	b.emit()
}

// MarkStale sets the staleness flag.
func (b *Book) MarkStale(stale bool) {
	if b.state.Stale != stale {
		b.state.Stale = stale
		b.state.Version++
		b.emit()
	}
}

func (b *Book) resort() {
	if !b.dirty {
		return
	}
	b.sortedBids = b.sortedBids[:0]
	for p, q := range b.bids {
		b.sortedBids = append(b.sortedBids, domain.Level{Price: decimal.FromRaw(p), Qty: q})
	}
	sort.Slice(b.sortedBids, func(i, j int) bool {
		return b.sortedBids[i].Price.Raw() > b.sortedBids[j].Price.Raw()
	})
	b.sortedAsks = b.sortedAsks[:0]
	for p, q := range b.asks {
		b.sortedAsks = append(b.sortedAsks, domain.Level{Price: decimal.FromRaw(p), Qty: q})
	}
	sort.Slice(b.sortedAsks, func(i, j int) bool {
		return b.sortedAsks[i].Price.Raw() < b.sortedAsks[j].Price.Raw()
	})
	b.dirty = false
}

// BestBid returns the highest bid, or false if empty/not ready.
func (b *Book) BestBid() (domain.Level, bool) {
	b.resort()
	if len(b.sortedBids) == 0 {
		return domain.Level{}, false
	}
	return b.sortedBids[0], true
}

// BestAsk returns the lowest ask, or false if empty/not ready.
func (b *Book) BestAsk() (domain.Level, bool) {
	b.resort()
	if len(b.sortedAsks) == 0 {
		return domain.Level{}, false
	}
	return b.sortedAsks[0], true
}

// Mid returns the midpoint of best bid/ask.
func (b *Book) Mid() (decimal.Fixed, bool) {
	bid, okB := b.BestBid()
	ask, okA := b.BestAsk()
	if !okB || !okA {
		return 0, false
	}
	return bid.Price.Add(ask.Price).Div(decimal.FromInt(2)), true
}

// Spread returns ask-bid.
func (b *Book) Spread() (decimal.Fixed, bool) {
	bid, okB := b.BestBid()
	ask, okA := b.BestAsk()
	if !okB || !okA {
		return 0, false
	}
	return ask.Price.Sub(bid.Price), true
}

// Crossed reports whether best bid >= best ask (invalid market state).
func (b *Book) Crossed() bool {
	bid, okB := b.BestBid()
	ask, okA := b.BestAsk()
	if !okB || !okA {
		return false
	}
	return bid.Price.Cmp(ask.Price) >= 0
}

// Depth returns up to limit levels on side, best first.
func (b *Book) Depth(side domain.Side, limit int) []domain.Level {
	b.resort()
	src := b.sortedBids
	if side == domain.Sell {
		src = b.sortedAsks
	}
	if limit > len(src) {
		limit = len(src)
	}
	out := make([]domain.Level, limit)
	copy(out, src[:limit])
	return out
}

// VWAP computes the average execution price to trade qty on side, walking
// the book. Returns vwap, filledQty, cost. If insufficient depth, filledQty
// < qty and ok is false (partial result still returned for IOC modeling).
func (b *Book) VWAP(side domain.Side, qty decimal.Fixed) (vwap decimal.Fixed, filled decimal.Fixed, cost decimal.Fixed, ok bool) {
	if !qty.IsPositive() {
		return 0, 0, 0, false
	}
	remaining := qty
	for _, l := range b.Depth(side, 1<<30) {
		take := l.Qty
		if take.Cmp(remaining) > 0 {
			take = remaining
		}
		cost = cost.Add(l.Price.Mul(take))
		filled = filled.Add(take)
		remaining = remaining.Sub(take)
		if remaining.IsZero() {
			break
		}
	}
	if filled.IsZero() {
		return 0, 0, 0, false
	}
	vwap = cost.Div(filled)
	return vwap, filled, cost, remaining.IsZero()
}

// Snapshot returns an immutable copy of top depth levels plus state.
func (b *Book) Snapshot(depth int) SnapshotView {
	return SnapshotView{
		Venue:  b.venue,
		Symbol: b.symbol,
		Bids:   b.Depth(domain.Buy, depth),
		Asks:   b.Depth(domain.Sell, depth),
		State:  b.state,
	}
}

// SnapshotView is an immutable book view for readers.
type SnapshotView struct {
	Venue  domain.Venue
	Symbol string
	Bids   []domain.Level
	Asks   []domain.Level
	State  State
}

// Digest is a deterministic hash of book content for replay parity.
func (b *Book) Digest() uint64 {
	b.resort()
	// FNV-1a over sorted levels + sequence.
	h := uint64(1469598103934665603)
	mix := func(v uint64) {
		for i := 0; i < 8; i++ {
			h ^= (v >> (i * 8)) & 0xff
			h *= 1099511628211
		}
	}
	mix(uint64(b.state.Sequence))
	for _, l := range b.sortedBids {
		mix(uint64(l.Price.Raw()))
		mix(uint64(l.Qty.Raw()))
	}
	for _, l := range b.sortedAsks {
		mix(uint64(l.Price.Raw()))
		mix(uint64(l.Qty.Raw()))
	}
	return h
}

// ---- Manager: publishes immutable views; books are mutated only by ----
// ---- their owning lane goroutine (or by tests, single-threaded).     ----

// Manager owns one Book per venue+symbol and hands out immutable,
// atomically published views to readers. It additionally computes a
// staleness overlay at read time: a ready book whose LastUpdatedAt exceeds
// MaxAge relative to Now is reported stale without any shared mutation.
type Manager struct {
	mu    sync.Mutex
	books map[string]*Book // key venue|symbol
	views map[string]*atomicView

	// MaxAge > 0 enables the read-time staleness overlay.
	MaxAge time.Duration
	// Now supplies time for the overlay; defaults to time.Now.
	Now func() time.Time
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{
		books: make(map[string]*Book),
		views: make(map[string]*atomicView),
		Now:   time.Now,
	}
}

func key(venue domain.Venue, symbol string) string { return string(venue) + "|" + symbol }

// Get returns the book for venue+symbol, creating it if needed. The
// returned book is for the OWNER goroutine only; readers must use
// Snapshot/All.
func (m *Manager) Get(venue domain.Venue, symbol string) *Book {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(venue, symbol)
	if b, ok := m.books[k]; ok {
		return b
	}
	av := &atomicView{}
	b := New(venue, symbol)
	b.publish = av.Store
	m.books[k] = b
	m.views[k] = av
	return b
}

// overlayStale applies the read-time staleness check.
func (m *Manager) overlayStale(v SnapshotView) SnapshotView {
	if m.MaxAge > 0 && v.State.Ready && !v.State.Stale {
		now := time.Now
		if m.Now != nil {
			now = m.Now
		}
		if now().Sub(v.State.LastUpdatedAt) > m.MaxAge {
			v.State.Stale = true
		}
	}
	return v
}

// Snapshot returns the latest published immutable view, or false if the
// book is unknown or never mutated.
func (m *Manager) Snapshot(venue domain.Venue, symbol string, depth int) (SnapshotView, bool) {
	m.mu.Lock()
	av, ok := m.views[key(venue, symbol)]
	m.mu.Unlock()
	if !ok {
		return SnapshotView{}, false
	}
	v, ok := av.Load()
	if !ok {
		return SnapshotView{}, false
	}
	return m.overlayStale(v), true
}

// All returns the latest published views of every book.
func (m *Manager) All(depth int) []SnapshotView {
	m.mu.Lock()
	views := make([]*atomicView, 0, len(m.views))
	for _, av := range m.views {
		views = append(views, av)
	}
	m.mu.Unlock()
	out := make([]SnapshotView, 0, len(views))
	for _, av := range views {
		if v, ok := av.Load(); ok {
			out = append(out, m.overlayStale(v))
		}
	}
	return out
}
