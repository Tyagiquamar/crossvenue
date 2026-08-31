// Package engine wires the full pipeline: venue sources -> book pipeline
// -> opportunity evaluation -> risk -> execution -> portfolio -> journal.
// The same supervisor drives live, synthetic, and replay modes; business
// logic is never duplicated per mode.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/clock"
	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/execution"
	"crossvenue/internal/journal"
	"crossvenue/internal/marketdata"
	"crossvenue/internal/opportunity"
	"crossvenue/internal/portfolio"
	"crossvenue/internal/replay"
	"crossvenue/internal/risk"
	"crossvenue/internal/venue"
	"crossvenue/pkg/decimal"
)

// Event source abstraction: live adapters, synthetic generators, and the
// replay reader all satisfy this.
type EventSource interface {
	// Stream sends events until ctx is done or the source exhausts.
	Stream(ctx context.Context, out chan<- domain.MarketEvent) error
}

// Supervisor owns the engine lifecycle.
type Supervisor struct {
	cfg *config.Config
	log *slog.Logger
	clk clock.Clock

	Books   *book.Manager
	Pipe    *marketdata.Pipeline
	Opp     *opportunity.Engine
	Risk    *risk.Engine
	Exec    *execution.Simulator
	Ledger  *portfolio.Ledger
	Journal *journal.Journal

	recorder *replay.Recorder

	mu            sync.Mutex
	opps          []opportunity.Opportunity // recent, bounded
	oppIndex      map[string]opportunity.Opportunity
	rejections    []string
	executedCount int
	maxRecent     int

	venues []domain.Venue

	// adapters, keyed by venue, enable resync requests to reach the live
	// feed. Nil in replay mode.
	adapters map[domain.Venue]venue.MarketDataAdapter

	// RecordSink receives every normalized event pre-pipeline (recorder).
	cancel context.CancelFunc

	// Deterministic mode (replay): events are applied synchronously on the
	// ingestion goroutine and evaluation runs per event — no wall-clock
	// timers, so output depends only on the recording + config + seed.
	Deterministic bool
}

// New builds the supervisor from config. Journal may carry durable sinks.
func New(cfg *config.Config, j *journal.Journal, clk clock.Clock, log *slog.Logger) (*Supervisor, error) {
	if log == nil {
		log = slog.Default()
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	if j == nil {
		j = journal.New()
	}
	s := &Supervisor{
		cfg:           cfg,
		log:           log,
		clk:           clk,
		Books:         book.NewManager(),
		Journal:       j,
		Ledger:        portfolio.New(),
		oppIndex:      make(map[string]opportunity.Opportunity),
		maxRecent:     256,
		venues:        cfg.EnabledVenues(),
		Deterministic: cfg.Mode == config.ModeReplay,
	}

	// Seed inventory per venue config.
	for name, vc := range cfg.Venues {
		if !vc.Enabled {
			continue
		}
		v := domain.Venue(name)
		if vc.InitialQuote != "" {
			q, err := decimal.Parse(vc.InitialQuote)
			if err != nil {
				return nil, fmt.Errorf("engine: %s initial_quote: %w", name, err)
			}
			s.Ledger.SetBalance(v, "USDT", q)
		}
		if vc.InitialBase != "" {
			b, err := decimal.Parse(vc.InitialBase)
			if err != nil {
				return nil, fmt.Errorf("engine: %s initial_base: %w", name, err)
			}
			for _, sym := range cfg.Symbols {
				s.Ledger.SetBalance(v, baseAsset(sym), b)
			}
		}
	}

	// Pipeline.
	s.Pipe = marketdata.NewPipeline(s.Books, marketdata.Options{
		QueueSize:  4096,
		MaxBookAge: cfg.MaxQuoteAge(),
		Logger:     log,
		Journal:    journalAdapter{j},
		Clock:      clk,
		ResyncRequest: func(v domain.Venue, sym, reason string) {
			log.Warn("resync requested", "venue", v, "symbol", sym, "reason", reason)
			s.mu.Lock()
			a := s.adapters[v]
			s.mu.Unlock()
			if a != nil {
				a.RequestResync(sym)
			}
		},
	})
	for _, v := range s.venues {
		for _, sym := range cfg.Symbols {
			if err := s.Pipe.Register(v, sym); err != nil {
				return nil, err
			}
		}
	}

	// Fees + latency.
	fees := opportunity.FeeSchedule{}
	lat := opportunity.LatencyModel{OutboundUS: map[domain.Venue]int64{}, DecisionUS: 250}
	feeBps := map[domain.Venue]int64{}
	for name, vc := range cfg.Venues {
		v := domain.Venue(name)
		fees[v] = opportunity.Fees{TakerBps: vc.TakerBps, MakerBps: vc.MakerBps}
		feeBps[v] = vc.TakerBps
		lat.OutboundUS[v] = vc.OutboundUS
	}
	tq, err := decimal.Parse(cfg.Execution.TradeQty)
	if err != nil {
		return nil, err
	}
	minProfit, err := decimal.Parse(cfg.Risk.MinProfitUSD)
	if err != nil {
		return nil, err
	}
	s.Opp = opportunity.New(s.Books, fees, lat, opportunity.Config{
		MinProfit:     minProfit,
		MinEdgeBps:    cfg.Risk.MinEdgeBps,
		MaxQuoteAge:   cfg.MaxQuoteAge(),
		TradeQuantity: tq,
		SlippageBps:   cfg.Execution.SlippageBps,
	}, clk)

	// Risk.
	lims := risk.Limits{
		MinExpectedEdge:     minProfit,
		MaxQuoteAge:         cfg.MaxQuoteAge(),
		MaxBookAge:          cfg.MaxQuoteAge(),
		MaxOutstanding:      cfg.Risk.MaxOutstandingOrders,
		MaxConsecutiveFails: cfg.Risk.MaxConsecutiveFails,
		KillSwitch:          cfg.Risk.KillSwitch,
	}
	lims.MaxTradeNotional = mustDecimal(cfg.Risk.MaxTradeNotionalUSD)
	lims.MaxSymbolExposure = mustDecimal(cfg.Risk.MaxSymbolExposureUSD)
	lims.MaxVenueExposure = mustDecimal(cfg.Risk.MaxVenueExposureUSD)
	lims.MaxTotalExposure = mustDecimal(cfg.Risk.MaxTotalExposureUSD)
	lims.DailyLossLimit = mustDecimal(cfg.Risk.DailyLossLimitUSD)
	s.Risk = risk.New(lims, s.Books, s.Ledger)

	// Execution (paper).
	s.Exec = execution.NewSimulator(s.Books, feeBps, fillJournal{j})
	return s, nil
}

func mustDecimal(s string) decimal.Fixed {
	if s == "" {
		return decimal.Zero
	}
	v, err := decimal.Parse(s)
	if err != nil {
		return decimal.Zero
	}
	return v
}

func baseAsset(symbol string) string {
	for i, c := range symbol {
		if c == '-' {
			return symbol[:i]
		}
	}
	return symbol
}

type journalAdapter struct{ j *journal.Journal }

func (a journalAdapter) Record(t string, v domain.Venue, s string, p map[string]any) {
	a.j.Record(t, v, s, p)
}

type fillJournal struct{ j *journal.Journal }

func (f fillJournal) OnFill(fe execution.FillEvent) {
	t := "OrderFilled"
	f.j.RecordFull(t, fe.Venue, fe.Symbol, fe.OrderID, map[string]any{
		"price": fe.Price.String(), "qty": fe.Qty.String(), "fee": fe.Fee.String(),
	})
}

// AttachRecorder tees every inbound event to the recorder.
func (s *Supervisor) AttachRecorder(r *replay.Recorder) { s.recorder = r }

// AttachAdapters registers live adapters so resync requests reach them.
func (s *Supervisor) AttachAdapters(adapters []venue.MarketDataAdapter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapters = make(map[domain.Venue]venue.MarketDataAdapter, len(adapters))
	for _, a := range adapters {
		s.adapters[a.Venue()] = a
	}
}

// Ingest is the single entry point for normalized events (all modes).
func (s *Supervisor) Ingest(ev domain.MarketEvent) {
	if s.recorder != nil {
		if err := s.recorder.Record(ev); err != nil {
			s.log.Error("recorder", "err", err)
		}
	}
	if err := s.Pipe.Submit(ev); err != nil {
		s.log.Error("pipeline submit", "err", err)
	}
}

// Run starts the pipeline and the evaluation loop against src.
func (s *Supervisor) Run(ctx context.Context, src EventSource) error {
	if s.Deterministic {
		return s.runDeterministic(ctx, src)
	}
	ctx, s.cancel = context.WithCancel(ctx)
	go s.Pipe.Run(ctx)

	raw := make(chan domain.MarketEvent, 8192)
	var srcErr error
	var srcWG sync.WaitGroup
	srcWG.Add(1)
	go func() {
		defer srcWG.Done()
		defer close(raw)
		srcErr = src.Stream(ctx, raw)
	}()

	// Ingestion fan-in.
	ingestDone := make(chan struct{})
	go func() {
		defer close(ingestDone)
		for ev := range raw {
			s.Ingest(ev)
		}
	}()

	// Evaluation loop.
	evalTicker := time.NewTicker(250 * time.Millisecond)
	defer evalTicker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ingestDone:
			break loop // source exhausted (replay end)
		case <-evalTicker.C:
			s.EvaluateOnce()
		}
	}
	s.cancel()
	srcWG.Wait()
	s.Pipe.Stop()
	return srcErr
}

// runDeterministic consumes the source event-by-event: each event is
// applied synchronously, then one evaluation pass runs. No wall clock is
// consulted, so identical inputs produce identical outputs.
func (s *Supervisor) runDeterministic(ctx context.Context, src EventSource) error {
	raw := make(chan domain.MarketEvent, 1)
	var srcErr error
	var srcWG sync.WaitGroup
	srcWG.Add(1)
	go func() {
		defer srcWG.Done()
		defer close(raw)
		srcErr = src.Stream(ctx, raw)
	}()
	for ev := range raw {
		if s.recorder != nil {
			if err := s.recorder.Record(ev); err != nil {
				s.log.Error("recorder", "err", err)
			}
		}
		if err := s.Pipe.ApplySync(ev); err != nil {
			s.log.Error("apply", "err", err)
		}
		s.EvaluateOnce()
	}
	srcWG.Wait()
	return srcErr
}

// Stop shuts the engine down.
func (s *Supervisor) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// EvaluateOnce runs one opportunity scan + execution pass. Exposed for
// deterministic replay stepping and tests.
func (s *Supervisor) EvaluateOnce() {
	now := s.clk.Now()
	for _, sym := range s.cfg.Symbols {
		opps := s.Opp.Evaluate(sym, s.venues)
		for _, o := range opps {
			s.recordOpp(o)
			s.maybeExecute(o, now)
		}
	}
}

func (s *Supervisor) recordOpp(o opportunity.Opportunity) {
	s.mu.Lock()
	s.opps = append(s.opps, o)
	if len(s.opps) > s.maxRecent {
		s.opps = s.opps[len(s.opps)-s.maxRecent:]
	}
	s.oppIndex[o.ID] = o
	s.mu.Unlock()
	s.Journal.RecordFull("OpportunityDetected", o.BuyVenue, o.Symbol, o.ID, map[string]any{
		"buy": string(o.BuyVenue), "sell": string(o.SellVenue),
		"net": o.ExpectedNetPnL.String(), "edge_bps": o.EdgeBps,
	})
}

func (s *Supervisor) maybeExecute(o opportunity.Opportunity, now time.Time) {
	midLookup := func(v domain.Venue) (decimal.Fixed, bool) {
		snap, ok := s.Books.Snapshot(v, o.Symbol, 1)
		if !ok || len(snap.Bids) == 0 || len(snap.Asks) == 0 {
			return 0, false
		}
		return snap.Bids[0].Price.Add(snap.Asks[0].Price).Div(decimal.FromInt(2)), true
	}
	dec := s.Risk.CheckOpportunity(now, o.Symbol, o.BuyVenue, o.SellVenue,
		o.Quantity, o.BuyVWAP, o.SellVWAP, o.ExpectedNetPnL, midLookup)
	if !dec.Allowed {
		s.Journal.RecordFull("RiskRejected", o.BuyVenue, o.Symbol, o.ID, map[string]any{"reasons": dec.Reasons})
		s.mu.Lock()
		s.rejections = append(s.rejections, dec.Reasons...)
		s.mu.Unlock()
		return
	}

	base, quote := baseQuote(o.Symbol)
	// Pre-trade inventory check: buy leg needs quote on buy venue; sell leg
	// needs base on sell venue.
	buyNotional := o.BuyVWAP.Mul(o.Quantity)
	if s.Ledger.Balance(o.BuyVenue, quote).Available.Cmp(buyNotional) < 0 {
		s.Journal.RecordFull("RiskRejected", o.BuyVenue, o.Symbol, o.ID,
			map[string]any{"reasons": []string{"insufficient_quote_inventory"}})
		return
	}
	if s.Ledger.Balance(o.SellVenue, base).Available.Cmp(o.Quantity) < 0 {
		s.Journal.RecordFull("RiskRejected", o.SellVenue, o.Symbol, o.ID,
			map[string]any{"reasons": []string{"insufficient_base_inventory"}})
		return
	}

	cloBase := o.ID
	buyOrd, err := s.Exec.Submit(execution.Order{
		ClientOrderID: cloBase + "-buy", Venue: o.BuyVenue, Symbol: o.Symbol,
		Side: domain.Buy, Type: execution.Market, Quantity: o.Quantity,
	}, now)
	if err != nil {
		s.log.Error("submit buy", "err", err)
		return
	}
	sellOrd, err := s.Exec.Submit(execution.Order{
		ClientOrderID: cloBase + "-sell", Venue: o.SellVenue, Symbol: o.Symbol,
		Side: domain.Sell, Type: execution.Market, Quantity: o.Quantity,
	}, now)
	if err != nil {
		s.log.Error("submit sell", "err", err)
		return
	}
	s.Risk.NoteOrderOpened()
	s.Risk.NoteOrderOpened()
	s.Journal.RecordFull("OrderSubmitted", o.BuyVenue, o.Symbol, buyOrd.ID, map[string]any{"side": "buy"})
	s.Journal.RecordFull("OrderSubmitted", o.SellVenue, o.Symbol, sellOrd.ID, map[string]any{"side": "sell"})

	pol := execution.PolicyParallel
	switch s.cfg.Execution.Policy {
	case "buy-first":
		pol = execution.PolicyBuyFirst
	case "sell-first":
		pol = execution.PolicySellFirst
	}
	res, err := s.Exec.ExecuteArbitrage(o.ID, buyOrd, sellOrd, pol, now)
	s.Risk.NoteOrderClosed()
	s.Risk.NoteOrderClosed()
	if err != nil {
		s.log.Warn("execution", "opp", o.ID, "err", err)
	}
	success := res.BuyLeg.FilledQty.IsPositive() && res.SellLeg.FilledQty.IsPositive()
	s.Risk.NoteExecutionResult(success)
	if s.Risk.ConsecutiveFails() >= s.cfg.Risk.MaxConsecutiveFails && s.cfg.Risk.MaxConsecutiveFails > 0 {
		s.Risk.ActivateKillSwitch("max_consecutive_failures")
		s.Journal.Record("KillSwitchActivated", "", o.Symbol, map[string]any{"reason": "max_consecutive_failures"})
	}

	// Settle into the ledger.
	if res.BuyLeg.FilledQty.IsPositive() {
		if err := s.Ledger.ApplyBuy(o.BuyVenue, o.Symbol, base, quote,
			res.BuyLeg.VWAP, res.BuyLeg.FilledQty, res.BuyLeg.Fee); err != nil {
			s.log.Error("settle buy", "err", err)
		}
	}
	if res.SellLeg.FilledQty.IsPositive() {
		if err := s.Ledger.ApplySell(o.SellVenue, o.Symbol, base, quote,
			res.SellLeg.VWAP, res.SellLeg.FilledQty, res.SellLeg.Fee); err != nil {
			s.log.Error("settle sell", "err", err)
		}
	}
	s.Ledger.AddRealized(res.RealizedPnL)
	if res.ResidualQty.IsPositive() || res.ResidualQty.IsNegative() {
		s.Journal.RecordFull("PositionChanged", o.BuyVenue, o.Symbol, o.ID, map[string]any{
			"residual": res.ResidualQty.String(),
		})
	}
	s.mu.Lock()
	s.executedCount++
	s.mu.Unlock()
}

func baseQuote(symbol string) (string, string) {
	for i, c := range symbol {
		if c == '-' {
			return symbol[:i], symbol[i+1:]
		}
	}
	return symbol, "USDT"
}

// RecentOpportunities returns the bounded recent list.
func (s *Supervisor) RecentOpportunities() []opportunity.Opportunity {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]opportunity.Opportunity, len(s.opps))
	copy(out, s.opps)
	return out
}

// Opportunity returns one by ID.
func (s *Supervisor) Opportunity(id string) (opportunity.Opportunity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.oppIndex[id]
	return o, ok
}

// Rejections returns recent rejection reasons.
func (s *Supervisor) Rejections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.rejections))
	copy(out, s.rejections)
	return out
}

// BookDigests returns per-book digests computed from the latest published
// immutable views (safe to call from any goroutine) for replay parity.
func (s *Supervisor) BookDigests() map[string]uint64 {
	out := map[string]uint64{}
	for _, v := range s.Books.All(1) {
		h := uint64(1469598103934665603)
		mix := func(x uint64) {
			for i := 0; i < 8; i++ {
				h ^= (x >> (i * 8)) & 0xff
				h *= 1099511628211
			}
		}
		mix(uint64(v.State.Sequence))
		for _, l := range v.Bids {
			mix(uint64(l.Price.Raw()))
			mix(uint64(l.Qty.Raw()))
		}
		for _, l := range v.Asks {
			mix(uint64(l.Price.Raw()))
			mix(uint64(l.Qty.Raw()))
		}
		out[string(v.Venue)+"|"+v.Symbol] = h
	}
	return out
}
