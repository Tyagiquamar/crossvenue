// Package portfolio tracks per-venue balances, inventory, positions, and
// PnL. It models the real constraint that assets do not move between
// exchanges instantly: each venue has independent cash and base inventory.
package portfolio

import (
	"errors"
	"fmt"
	"sync"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// ErrInsufficientFunds rejects reservations beyond available balance.
var ErrInsufficientFunds = errors.New("portfolio: insufficient funds")

// Balance is one venue+asset balance.
type Balance struct {
	Venue     domain.Venue
	Asset     string
	Available decimal.Fixed
	Reserved  decimal.Fixed
}

// Position is net base-asset exposure at one venue for one symbol.
type Position struct {
	Venue   domain.Venue
	Symbol  string
	Qty     decimal.Fixed // positive = long, negative = short (residual)
	AvgCost domain.Price
}

// Ledger is the in-memory portfolio. PostgreSQL persistence mirrors it
// asynchronously; this remains the hot-path source of truth.
type Ledger struct {
	mu        sync.RWMutex
	balances  map[string]*Balance  // venue|asset
	positions map[string]*Position // venue|symbol
	realized  decimal.Fixed
}

// New creates an empty ledger.
func New() *Ledger {
	return &Ledger{
		balances:  make(map[string]*Balance),
		positions: make(map[string]*Position),
	}
}

func balKey(v domain.Venue, asset string) string  { return string(v) + "|" + asset }
func posKey(v domain.Venue, symbol string) string { return string(v) + "|" + symbol }

// Credit adds to an available balance.
func (l *Ledger) Credit(v domain.Venue, asset string, amt decimal.Fixed) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.balanceLocked(v, asset)
	b.Available = b.Available.Add(amt)
}

// SetBalance initializes a balance (used by config seeding and recovery).
func (l *Ledger) SetBalance(v domain.Venue, asset string, available decimal.Fixed) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.balanceLocked(v, asset)
	b.Available = available
}

func (l *Ledger) balanceLocked(v domain.Venue, asset string) *Balance {
	k := balKey(v, asset)
	if b, ok := l.balances[k]; ok {
		return b
	}
	b := &Balance{Venue: v, Asset: asset}
	l.balances[k] = b
	return b
}

// Balance returns a copy of one balance.
func (l *Ledger) Balance(v domain.Venue, asset string) Balance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if b, ok := l.balances[balKey(v, asset)]; ok {
		return *b
	}
	return Balance{Venue: v, Asset: asset}
}

// Balances returns copies of all balances.
func (l *Ledger) Balances() []Balance {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Balance, 0, len(l.balances))
	for _, b := range l.balances {
		out = append(out, *b)
	}
	return out
}

// ApplyBuy settles a simulated buy: spend quote (price*qty+fee), gain base.
func (l *Ledger) ApplyBuy(v domain.Venue, symbol, base, quote string, px, qty, fee decimal.Fixed) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cost := px.Mul(qty).Add(fee)
	quoteBal := l.balanceLocked(v, quote)
	if quoteBal.Available.Cmp(cost) < 0 {
		return fmt.Errorf("%w: %s %s need %s have %s", ErrInsufficientFunds, v, quote, cost, quoteBal.Available)
	}
	quoteBal.Available = quoteBal.Available.Sub(cost)
	baseBal := l.balanceLocked(v, base)
	baseBal.Available = baseBal.Available.Add(qty)
	l.positionLocked(v, symbol).addBuy(px, qty)
	return nil
}

// ApplySell settles a simulated sell: spend base, gain quote (net of fee).
func (l *Ledger) ApplySell(v domain.Venue, symbol, base, quote string, px, qty, fee decimal.Fixed) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	baseBal := l.balanceLocked(v, base)
	if baseBal.Available.Cmp(qty) < 0 {
		return fmt.Errorf("%w: %s %s need %s have %s", ErrInsufficientFunds, v, base, qty, baseBal.Available)
	}
	baseBal.Available = baseBal.Available.Sub(qty)
	proceeds := px.Mul(qty).Sub(fee)
	quoteBal := l.balanceLocked(v, quote)
	quoteBal.Available = quoteBal.Available.Add(proceeds)
	l.positionLocked(v, symbol).addSell(px, qty)
	return nil
}

func (l *Ledger) positionLocked(v domain.Venue, symbol string) *Position {
	k := posKey(v, symbol)
	if p, ok := l.positions[k]; ok {
		return p
	}
	p := &Position{Venue: v, Symbol: symbol}
	l.positions[k] = p
	return p
}

func (p *Position) addBuy(px, qty decimal.Fixed) {
	newQty := p.Qty.Add(qty)
	if p.Qty.IsNegative() {
		// reducing a short: realize into avg cost tracking simplistically
		if newQty.IsNegative() || newQty.IsZero() {
			p.Qty = newQty
			return
		}
		p.Qty = newQty
		p.AvgCost = px
		return
	}
	if newQty.IsZero() {
		p.Qty = 0
		return
	}
	total := p.AvgCost.Mul(p.Qty).Add(px.Mul(qty))
	p.AvgCost = total.Div(newQty)
	p.Qty = newQty
}

func (p *Position) addSell(px, qty decimal.Fixed) {
	p.addBuy(px, qty.MulInt(-1))
}

// AddRealized records realized PnL from matched arbitrage legs.
func (l *Ledger) AddRealized(pnl decimal.Fixed) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.realized = l.realized.Add(pnl)
}

// Realized returns cumulative realized PnL.
func (l *Ledger) Realized() decimal.Fixed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.realized
}

// Positions returns copies of all non-zero positions.
func (l *Ledger) Positions() []Position {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Position, 0, len(l.positions))
	for _, p := range l.positions {
		if !p.Qty.IsZero() {
			out = append(out, *p)
		}
	}
	return out
}

// VenueExposure returns total absolute base exposure at a venue in base qty.
func (l *Ledger) VenueExposure(v domain.Venue) decimal.Fixed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := decimal.Zero
	for _, p := range l.positions {
		if p.Venue == v {
			total = total.Add(p.Qty.Abs())
		}
	}
	return total
}

// TotalExposure sums absolute exposure across venues.
func (l *Ledger) TotalExposure() decimal.Fixed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := decimal.Zero
	for _, p := range l.positions {
		total = total.Add(p.Qty.Abs())
	}
	return total
}
