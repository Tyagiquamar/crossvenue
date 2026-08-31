// Package risk implements deterministic pre-trade risk checks and the kill
// switch. Every simulated order passes through here before submission.
package risk

import (
	"sync"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// Limits configure the risk engine.
type Limits struct {
	MaxTradeNotional    decimal.Fixed
	MaxSymbolExposure   decimal.Fixed // per venue+symbol, in quote terms
	MaxVenueExposure    decimal.Fixed // per venue, quote terms
	MaxTotalExposure    decimal.Fixed // across venues, quote terms
	MinExpectedEdge     decimal.Fixed
	MaxQuoteAge         time.Duration
	MaxBookAge          time.Duration
	MaxOutstanding      int
	MaxConsecutiveFails int
	DailyLossLimit      decimal.Fixed // halt if realized <= -limit
	KillSwitch          bool
}

// Decision is the outcome of a pre-trade check.
type Decision struct {
	Allowed bool
	Reasons []string
}

func (d *Decision) deny(reason string) {
	d.Allowed = false
	d.Reasons = append(d.Reasons, reason)
}

// State is mutable runtime risk state (counters, kill switch).
type State struct {
	mu               sync.RWMutex
	kill             bool
	killReason       string
	consecutiveFails int
	outstanding      int
}

// Engine evaluates pre-trade checks against books, portfolio, and limits.
type Engine struct {
	limits Limits
	books  *book.Manager
	expo   ExposureSource
	state  *State
}

// ExposureSource abstracts portfolio queries so risk stays decoupled.
type ExposureSource interface {
	VenueExposure(domain.Venue) decimal.Fixed
	TotalExposure() decimal.Fixed
	Realized() decimal.Fixed
}

// New constructs the engine. killSwitch initial state comes from config.
func New(l Limits, books *book.Manager, expo ExposureSource) *Engine {
	return &Engine{limits: l, books: books, expo: expo, state: &State{kill: l.KillSwitch}}
}

// ActivateKillSwitch blocks all new orders immediately.
func (e *Engine) ActivateKillSwitch(reason string) {
	e.state.mu.Lock()
	defer e.state.mu.Unlock()
	e.state.kill = true
	e.state.killReason = reason
}

// ResetKillSwitch clears the switch.
func (e *Engine) ResetKillSwitch() {
	e.state.mu.Lock()
	defer e.state.mu.Unlock()
	e.state.kill = false
	e.state.killReason = ""
}

// KillSwitchActive reports current state and reason.
func (e *Engine) KillSwitchActive() (bool, string) {
	e.state.mu.RLock()
	defer e.state.mu.RUnlock()
	return e.state.kill, e.state.killReason
}

// NoteOrderOpened / NoteOrderClosed track outstanding simulated orders.
func (e *Engine) NoteOrderOpened() {
	e.state.mu.Lock()
	defer e.state.mu.Unlock()
	e.state.outstanding++
}

// NoteOrderClosed decrements outstanding count.
func (e *Engine) NoteOrderClosed() {
	e.state.mu.Lock()
	defer e.state.mu.Unlock()
	if e.state.outstanding > 0 {
		e.state.outstanding--
	}
}

// NoteExecutionResult updates consecutive-failure tracking.
func (e *Engine) NoteExecutionResult(success bool) {
	e.state.mu.Lock()
	defer e.state.mu.Unlock()
	if success {
		e.state.consecutiveFails = 0
	} else {
		e.state.consecutiveFails++
	}
}

// ConsecutiveFails reports the counter (used by supervisor to auto-kill).
func (e *Engine) ConsecutiveFails() int {
	e.state.mu.RLock()
	defer e.state.mu.RUnlock()
	return e.state.consecutiveFails
}

// Outstanding reports open simulated order count.
func (e *Engine) Outstanding() int {
	e.state.mu.RLock()
	defer e.state.mu.RUnlock()
	return e.state.outstanding
}

// CheckOpportunity runs all pre-trade checks for a two-leg arbitrage
// opportunity. Checks are deterministic and order-stable.
func (e *Engine) CheckOpportunity(
	now time.Time,
	symbol string,
	buyV, sellV domain.Venue,
	qty, buyVWAP, sellVWAP decimal.Fixed,
	expectedNet decimal.Fixed,
	midBySymbolVenue func(domain.Venue) (decimal.Fixed, bool),
) Decision {
	d := Decision{Allowed: true}

	e.state.mu.RLock()
	kill, killReason := e.state.kill, e.state.killReason
	outstanding := e.state.outstanding
	e.state.mu.RUnlock()
	if kill {
		d.deny("kill_switch_active:" + killReason)
	}
	if e.limits.MaxOutstanding > 0 && outstanding+2 > e.limits.MaxOutstanding {
		d.deny("max_outstanding_orders")
	}
	if e.limits.MaxConsecutiveFails > 0 && e.ConsecutiveFails() >= e.limits.MaxConsecutiveFails {
		d.deny("max_consecutive_failures")
	}

	notional := buyVWAP.Mul(qty)
	if e.limits.MaxTradeNotional.IsPositive() && notional.Cmp(e.limits.MaxTradeNotional) > 0 {
		d.deny("max_trade_notional")
	}
	if e.limits.MinExpectedEdge.IsPositive() && expectedNet.Cmp(e.limits.MinExpectedEdge) < 0 {
		d.deny("below_min_expected_edge")
	}
	if e.limits.DailyLossLimit.IsPositive() &&
		e.expo.Realized().Abs().Cmp(e.limits.DailyLossLimit) >= 0 &&
		e.expo.Realized().IsNegative() {
		d.deny("daily_loss_limit")
	}

	// Book freshness / readiness.
	for _, v := range []domain.Venue{buyV, sellV} {
		snap, ok := e.books.Snapshot(v, symbol, 1)
		if !ok || !snap.State.Ready {
			d.deny("book_not_ready:" + string(v))
			continue
		}
		if snap.State.Stale {
			d.deny("book_stale:" + string(v))
		}
		age := now.Sub(snap.State.LastUpdatedAt)
		if e.limits.MaxQuoteAge > 0 && age > e.limits.MaxQuoteAge {
			d.deny("quote_too_old:" + string(v))
		}
	}

	// Exposure checks use mid price to convert base qty to quote notional.
	if midBySymbolVenue != nil {
		if mid, ok := midBySymbolVenue(buyV); ok {
			venueExp := e.expo.VenueExposure(buyV).Mul(mid)
			if e.limits.MaxVenueExposure.IsPositive() && venueExp.Cmp(e.limits.MaxVenueExposure) > 0 {
				d.deny("max_venue_exposure:" + string(buyV))
			}
		}
		if mid, ok := midBySymbolVenue(sellV); ok {
			venueExp := e.expo.VenueExposure(sellV).Mul(mid)
			if e.limits.MaxVenueExposure.IsPositive() && venueExp.Cmp(e.limits.MaxVenueExposure) > 0 {
				d.deny("max_venue_exposure:" + string(sellV))
			}
			// Approximate symbol exposure with the sell-side mid.
			if e.limits.MaxSymbolExposure.IsPositive() {
				if symExp := venueExp; symExp.Cmp(e.limits.MaxSymbolExposure) > 0 {
					d.deny("max_symbol_exposure")
				}
			}
			totalExp := e.expo.TotalExposure().Mul(mid)
			if e.limits.MaxTotalExposure.IsPositive() && totalExp.Cmp(e.limits.MaxTotalExposure) > 0 {
				d.deny("max_total_exposure")
			}
		}
	}
	return d
}
