// Package execution implements the paper execution engine: order state
// machine, depth-consuming fill simulation, two-leg arbitrage with leg
// risk, and idempotent submission via client order IDs.
package execution

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// OrderType enumerates supported simulated order types.
type OrderType uint8

const (
	Market OrderType = iota + 1
	IOCLimit
)

func (t OrderType) String() string {
	switch t {
	case Market:
		return "market"
	case IOCLimit:
		return "ioc_limit"
	}
	return "unknown"
}

// OrderState is the lifecycle state of a simulated order.
type OrderState uint8

const (
	Pending OrderState = iota + 1
	Accepted
	PartiallyFilled
	Filled
	CancelPending
	Cancelled
	Rejected
	Expired
)

func (s OrderState) String() string {
	switch s {
	case Pending:
		return "pending"
	case Accepted:
		return "accepted"
	case PartiallyFilled:
		return "partially_filled"
	case Filled:
		return "filled"
	case CancelPending:
		return "cancel_pending"
	case Cancelled:
		return "cancelled"
	case Rejected:
		return "rejected"
	case Expired:
		return "expired"
	}
	return "unknown"
}

// validTransitions defines the state machine. Anything absent is illegal.
var validTransitions = map[OrderState][]OrderState{
	Pending:         {Accepted, Rejected, Cancelled, Expired},
	Accepted:        {PartiallyFilled, Filled, CancelPending, Cancelled, Expired},
	PartiallyFilled: {PartiallyFilled, Filled, CancelPending, Cancelled, Expired},
	CancelPending:   {Cancelled, PartiallyFilled, Filled},
	Filled:          {},
	Cancelled:       {},
	Rejected:        {},
	Expired:         {},
}

// ErrIllegalTransition is returned for state changes not in the machine.
var ErrIllegalTransition = errors.New("execution: illegal state transition")

// CanTransition reports whether from->to is legal.
func CanTransition(from, to OrderState) bool {
	for _, t := range validTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Order is a simulated order.
type Order struct {
	ID             string
	ClientOrderID  string
	Venue          domain.Venue
	Symbol         string
	Side           domain.Side
	Type           OrderType
	Price          domain.Price
	Quantity       domain.Quantity
	FilledQuantity domain.Quantity
	AvgFillPrice   domain.Price
	State          OrderState
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Transition moves the order to a new state, rejecting illegal jumps.
func (o *Order) Transition(to OrderState, at time.Time) error {
	if !CanTransition(o.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, o.State, to)
	}
	o.State = to
	o.UpdatedAt = at
	return nil
}

// Fill records a fill at price/qty, updating VWAP and state.
func (o *Order) Fill(price domain.Price, qty decimal.Fixed, at time.Time) error {
	if qty.IsZero() || qty.IsNegative() {
		return errors.New("execution: fill qty must be positive")
	}
	if o.FilledQuantity.Add(qty).Cmp(o.Quantity) > 0 {
		return errors.New("execution: overfill")
	}
	newCost := o.AvgFillPrice.Mul(o.FilledQuantity).Add(price.Mul(qty))
	newFilled := o.FilledQuantity.Add(qty)
	o.FilledQuantity = newFilled
	o.AvgFillPrice = newCost.Div(newFilled)
	if o.State == Pending {
		if err := o.Transition(Accepted, at); err != nil {
			return err
		}
	}
	if newFilled.Cmp(o.Quantity) == 0 {
		return o.Transition(Filled, at)
	}
	return o.Transition(PartiallyFilled, at)
}

// LegResult is the outcome of one side of an arbitrage execution.
type LegResult struct {
	Order       *Order
	FilledQty   decimal.Fixed
	VWAP        domain.Price
	Notional    domain.Money
	Fee         domain.Money
	RejectCause string
}

// Result is the combined two-leg outcome.
type Result struct {
	OpportunityID string
	BuyLeg        LegResult
	SellLeg       LegResult
	ResidualQty   domain.Quantity // net directional exposure in base asset
	RealizedPnL   domain.Money
}

// FillEvent records a single simulated fill for the journal.
type FillEvent struct {
	OrderID string
	Venue   domain.Venue
	Symbol  string
	Side    domain.Side
	Price   domain.Price
	Qty     domain.Quantity
	Fee     domain.Money
	At      time.Time
}

// FillSink receives fills as they happen.
type FillSink interface {
	OnFill(FillEvent)
}

// Simulator consumes book depth to produce deterministic paper fills.
type Simulator struct {
	books *book.Manager
	fees  map[domain.Venue]int64 // taker bps

	mu      sync.Mutex
	byID    map[string]*Order
	byCloID map[string]*Order
	ctr     int

	sink FillSink
}

// NewSimulator constructs the simulator.
func NewSimulator(books *book.Manager, fees map[domain.Venue]int64, sink FillSink) *Simulator {
	if fees == nil {
		fees = map[domain.Venue]int64{}
	}
	return &Simulator{
		books:   books,
		fees:    fees,
		byID:    make(map[string]*Order),
		byCloID: make(map[string]*Order),
		sink:    sink,
	}
}

// ErrDuplicateClientOrderID is never surfaced to callers; resubmission
// returns the original order (idempotency).
var ErrBookNotReady = errors.New("execution: book not ready")

// Submit is idempotent on ClientOrderID: a duplicate returns the original
// order unchanged.
func (s *Simulator) Submit(o Order, at time.Time) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byCloID[o.ClientOrderID]; ok {
		return existing, nil
	}
	s.ctr++
	o.ID = fmt.Sprintf("ord-%d", s.ctr)
	o.State = Pending
	o.CreatedAt, o.UpdatedAt = at, at
	cp := o
	s.byID[o.ID] = &cp
	s.byCloID[o.ClientOrderID] = &cp
	return &cp, nil
}

// Get returns an order by ID.
func (s *Simulator) Get(id string) (*Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.byID[id]
	return o, ok
}

// Orders returns all orders (defensive copies).
func (s *Simulator) Orders() []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Order, 0, len(s.byID))
	for _, o := range s.byID {
		out = append(out, *o)
	}
	return out
}

// FillAgainstBook executes qty on venue+symbol consuming depth from the
// current book snapshot. The book itself is NOT mutated (simulation reads
// observable depth; it does not assume our order moves the market).
// Returns legs filled with per-level detail collapsed to VWAP.
func (s *Simulator) FillAgainstBook(o *Order, at time.Time) (LegResult, error) {
	snap, ok := s.books.Snapshot(o.Venue, o.Symbol, 256)
	if !ok || !snap.State.Ready {
		if err := o.Transition(Rejected, at); err != nil {
			return LegResult{}, err
		}
		return LegResult{Order: o, RejectCause: "book_not_ready"}, ErrBookNotReady
	}
	levels := snap.Asks
	if o.Side == domain.Sell {
		levels = snap.Bids
	}
	remaining := o.Quantity.Sub(o.FilledQuantity)
	feeBps := s.fees[o.Venue]
	var totalCost, totalFilled, totalFee decimal.Fixed
	for _, l := range levels {
		if remaining.IsZero() {
			break
		}
		// IOC limit: skip levels beyond limit price.
		if o.Type == IOCLimit {
			if o.Side == domain.Buy && l.Price.Cmp(o.Price) > 0 {
				break
			}
			if o.Side == domain.Sell && l.Price.Cmp(o.Price) < 0 {
				break
			}
		}
		take := l.Qty
		if take.Cmp(remaining) > 0 {
			take = remaining
		}
		fee := l.Price.Mul(take).MulBps(feeBps)
		totalCost = totalCost.Add(l.Price.Mul(take))
		totalFee = totalFee.Add(fee)
		totalFilled = totalFilled.Add(take)
		remaining = remaining.Sub(take)
		if err := o.Fill(l.Price, take, at); err != nil {
			return LegResult{}, err
		}
		if s.sink != nil {
			s.sink.OnFill(FillEvent{
				OrderID: o.ID, Venue: o.Venue, Symbol: o.Symbol, Side: o.Side,
				Price: l.Price, Qty: take, Fee: fee, At: at,
			})
		}
	}
	if o.Type == IOCLimit && o.State == PartiallyFilled {
		// IOC cancels the remainder.
		if err := o.Transition(Cancelled, at); err != nil {
			return LegResult{}, err
		}
	}
	return LegResult{
		Order:     o,
		FilledQty: totalFilled,
		VWAP:      o.AvgFillPrice,
		Notional:  totalCost,
		Fee:       totalFee,
	}, nil
}

// Policy selects leg ordering for two-leg execution.
type Policy uint8

const (
	PolicyParallel Policy = iota + 1 // default: both legs against the same snapshot instant
	PolicyBuyFirst
	PolicySellFirst
)

// ExecuteArbitrage runs both legs of an opportunity. Execution is NOT
// atomic: each leg fills against its venue's book independently, and a
// partial second leg produces residual directional exposure.
func (s *Simulator) ExecuteArbitrage(oppID string, buyOrd, sellOrd *Order, pol Policy, at time.Time) (Result, error) {
	res := Result{OpportunityID: oppID}
	var err error
	run := func(o *Order) LegResult {
		lr, lerr := FillAgainstBookSafe(s, o, at)
		if lerr != nil && !errors.Is(lerr, ErrBookNotReady) {
			err = lerr
		}
		return lr
	}
	switch pol {
	case PolicyBuyFirst:
		res.BuyLeg = run(buyOrd)
		res.SellLeg = run(sellOrd)
	case PolicySellFirst:
		res.SellLeg = run(sellOrd)
		res.BuyLeg = run(buyOrd)
	default: // parallel — same snapshot instant; sequential here because books are immutable views
		res.BuyLeg = run(buyOrd)
		res.SellLeg = run(sellOrd)
	}
	res.ResidualQty = res.BuyLeg.FilledQty.Sub(res.SellLeg.FilledQty)
	// Realized PnL only on matched quantity.
	matched := res.BuyLeg.FilledQty
	if res.SellLeg.FilledQty.Cmp(matched) < 0 {
		matched = res.SellLeg.FilledQty
	}
	if matched.IsPositive() {
		buyCost := res.BuyLeg.VWAP.Mul(matched)
		sellProceeds := res.SellLeg.VWAP.Mul(matched)
		fees := res.BuyLeg.Fee.Add(res.SellLeg.Fee)
		res.RealizedPnL = sellProceeds.Sub(buyCost).Sub(fees)
	}
	return res, err
}

// FillAgainstBookSafe wraps the method for internal use (avoids method value
// gymnastics with unexported locking).
func FillAgainstBookSafe(s *Simulator, o *Order, at time.Time) (LegResult, error) {
	return s.FillAgainstBook(o, at)
}
