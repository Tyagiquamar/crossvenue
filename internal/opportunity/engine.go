// Package opportunity computes depth-aware, cost-adjusted cross-venue
// opportunities from ready, non-stale books.
package opportunity

import (
	"fmt"
	"sync/atomic"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/clock"
	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// FeeSchedule holds per-venue fee configuration in basis points.
type FeeSchedule map[domain.Venue]Fees

// Fees for one venue.
type Fees struct {
	TakerBps int64
	MakerBps int64
}

// LatencyModel converts venue latency config into a money penalty for a
// given notional. Penalties are conservative simulation parameters.
type LatencyModel struct {
	// PenaltyBpsPerMs: money penalty charged per millisecond of total
	// modeled latency, applied to notional. Configurable; 0 disables.
	PenaltyBpsPerMs int64
	// OutboundUS per venue: one-way order latency in microseconds.
	OutboundUS map[domain.Venue]int64
	// DecisionUS: engine decision time in microseconds.
	DecisionUS int64
}

// TotalUS returns decision + both venues' outbound latency.
func (m LatencyModel) TotalUS(buy, sell domain.Venue) int64 {
	return m.DecisionUS + m.OutboundUS[buy] + m.OutboundUS[sell]
}

// Penalty converts modeled latency into money against notional.
func (m LatencyModel) Penalty(notional decimal.Fixed, buy, sell domain.Venue) decimal.Fixed {
	if m.PenaltyBpsPerMs <= 0 {
		return 0
	}
	ms := m.TotalUS(buy, sell) / 1000
	if ms <= 0 {
		return 0
	}
	return notional.MulBps(m.PenaltyBpsPerMs).MulInt(ms)
}

// Opportunity is a fully costed, executable cross-venue spread.
type Opportunity struct {
	ID              string
	Symbol          string
	BuyVenue        domain.Venue
	SellVenue       domain.Venue
	Quantity        domain.Quantity
	BuyVWAP         domain.Price
	SellVWAP        domain.Price
	GrossPnL        domain.Money
	Fees            domain.Money
	SlippagePenalty domain.Money
	LatencyPenalty  domain.Money
	ExpectedNetPnL  domain.Money
	EdgeBps         int64
	BuyQuoteAge     time.Duration
	SellQuoteAge    time.Duration
	DetectedAt      time.Time
}

// Rejection explains why a candidate was discarded.
type Rejection struct {
	Symbol    string
	BuyVenue  domain.Venue
	SellVenue domain.Venue
	Reason    string
}

// Config gates opportunity emission.
type Config struct {
	MinProfit     decimal.Fixed // minimum absolute net PnL
	MinEdgeBps    int64
	MaxQuoteAge   time.Duration
	TradeQuantity decimal.Fixed // requested size per opportunity
	// SlippageBps: extra conservative penalty on notional.
	SlippageBps int64
}

// Engine evaluates all venue pairs per symbol.
type Engine struct {
	books   *book.Manager
	fees    FeeSchedule
	latency LatencyModel
	cfg     Config
	clk     clock.Clock
	ctr     atomic.Uint64

	// Rejections receives structured rejections (may be nil).
	Rejections chan<- Rejection
}

// New constructs the engine.
func New(mgr *book.Manager, fees FeeSchedule, lat LatencyModel, cfg Config, clk clock.Clock) *Engine {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Engine{books: mgr, fees: fees, latency: lat, cfg: cfg, clk: clk}
}

// Evaluate scans one symbol across all venues and returns valid
// opportunities. Books that are not ready, stale, or too old are skipped
// (and counted as rejections only when they would otherwise pair).
func (e *Engine) Evaluate(symbol string, venues []domain.Venue) []Opportunity {
	now := e.clk.Now()
	var out []Opportunity
	for _, buyV := range venues {
		for _, sellV := range venues {
			if buyV == sellV {
				continue
			}
			if opp, ok := e.evaluatePair(symbol, buyV, sellV, now); ok {
				out = append(out, opp)
			}
		}
	}
	return out
}

func (e *Engine) reject(symbol string, buy, sell domain.Venue, reason string) {
	if e.Rejections != nil {
		select {
		case e.Rejections <- Rejection{Symbol: symbol, BuyVenue: buy, SellVenue: sell, Reason: reason}:
		default: // never block the hot path on rejection reporting
		}
	}
}

func (e *Engine) evaluatePair(symbol string, buyV, sellV domain.Venue, now time.Time) (Opportunity, bool) {
	buySnap, okB := e.books.Snapshot(buyV, symbol, 256)
	sellSnap, okS := e.books.Snapshot(sellV, symbol, 256)
	if !okB || !okS {
		return Opportunity{}, false
	}
	if !buySnap.State.Ready || !sellSnap.State.Ready {
		e.reject(symbol, buyV, sellV, "book_not_ready")
		return Opportunity{}, false
	}
	if buySnap.State.Stale || sellSnap.State.Stale {
		e.reject(symbol, buyV, sellV, "book_stale")
		return Opportunity{}, false
	}
	buyAge := now.Sub(buySnap.State.LastUpdatedAt)
	sellAge := now.Sub(sellSnap.State.LastUpdatedAt)
	if buyAge > e.cfg.MaxQuoteAge || sellAge > e.cfg.MaxQuoteAge {
		e.reject(symbol, buyV, sellV, "quote_too_old")
		return Opportunity{}, false
	}

	// Walk depth manually against immutable snapshot views.
	buyVWAP, buyFilled, buyCost, buyOK := vwapOf(sellSide(buySnap.Asks), e.cfg.TradeQuantity)
	sellVWAP, sellFilled, sellProceeds, sellOK := vwapOf(sellSnap.Bids, e.cfg.TradeQuantity)
	if !buyOK || !sellOK {
		e.reject(symbol, buyV, sellV, "insufficient_depth")
		return Opportunity{}, false
	}
	qty := buyFilled
	if sellFilled.Cmp(qty) < 0 {
		qty = sellFilled
	}
	if qty.Cmp(e.cfg.TradeQuantity) < 0 {
		// treat any shortfall as insufficient for the requested size
		e.reject(symbol, buyV, sellV, "insufficient_depth")
		return Opportunity{}, false
	}

	gross := sellProceeds.Sub(buyCost)
	buyFee := buyCost.MulBps(e.fees[buyV].TakerBps)
	sellFee := sellProceeds.MulBps(e.fees[sellV].TakerBps)
	fees := buyFee.Add(sellFee)
	slippage := buyCost.MulBps(e.cfg.SlippageBps)
	latPen := e.latency.Penalty(buyCost, buyV, sellV)
	net := gross.Sub(fees).Sub(slippage).Sub(latPen)

	if net.Cmp(e.cfg.MinProfit) <= 0 {
		e.reject(symbol, buyV, sellV, "below_min_profit")
		return Opportunity{}, false
	}
	// edge_bps = net / buy_cost * 10000
	edgeBps := net.MulInt(10_000).Div(buyCost).Raw() / decimal.Scale
	if edgeBps < e.cfg.MinEdgeBps {
		e.reject(symbol, buyV, sellV, "below_min_edge_bps")
		return Opportunity{}, false
	}
	if buyVWAP.Cmp(sellVWAP) >= 0 {
		// net was positive only via inconsistent depth walk; guard anyway
		e.reject(symbol, buyV, sellV, "non_positive_vwap_spread")
		return Opportunity{}, false
	}

	id := fmt.Sprintf("opp-%d", e.ctr.Add(1))
	return Opportunity{
		ID:              id,
		Symbol:          symbol,
		BuyVenue:        buyV,
		SellVenue:       sellV,
		Quantity:        qty,
		BuyVWAP:         buyVWAP,
		SellVWAP:        sellVWAP,
		GrossPnL:        gross,
		Fees:            fees,
		SlippagePenalty: slippage,
		LatencyPenalty:  latPen,
		ExpectedNetPnL:  net,
		EdgeBps:         edgeBps,
		BuyQuoteAge:     buyAge,
		SellQuoteAge:    sellAge,
		DetectedAt:      now,
	}, true
}

// sellSide is a readability alias: when buying we walk the asks.
func sellSide(asks []domain.Level) []domain.Level { return asks }

// vwapOf walks levels (best first) computing vwap/filled/cost for qty.
func vwapOf(levels []domain.Level, qty decimal.Fixed) (vwap, filled, cost decimal.Fixed, ok bool) {
	remaining := qty
	for _, l := range levels {
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
	return cost.Div(filled), filled, cost, remaining.IsZero()
}
