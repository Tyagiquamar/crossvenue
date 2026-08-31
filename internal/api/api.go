// Package api exposes the operational HTTP API. /health is liveness,
// /ready is readiness (books synchronized); they are never conflated.
package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"crossvenue/internal/book"
	"crossvenue/internal/domain"
	"crossvenue/internal/engine"
	"crossvenue/internal/opportunity"
	"crossvenue/internal/venue"
)

// Server serves the operational API.
type Server struct {
	eng      *engine.Supervisor
	adapters []venue.MarketDataAdapter
	mux      *http.ServeMux
}

// New builds the server. adapters may be nil in replay mode (no live
// health to report).
func New(eng *engine.Supervisor, adapters []venue.MarketDataAdapter) *Server {
	s := &Server{eng: eng, adapters: adapters, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/ready", s.ready)
	s.mux.Handle("/metrics", promhttp.Handler())
	s.mux.HandleFunc("/api/v1/venues", s.venuesHandler)
	s.mux.HandleFunc("/api/v1/books", s.booksHandler)
	s.mux.HandleFunc("/api/v1/books/", s.bookHandler)
	s.mux.HandleFunc("/api/v1/opportunities", s.oppsHandler)
	s.mux.HandleFunc("/api/v1/opportunities/", s.oppHandler)
	s.mux.HandleFunc("/api/v1/orders", s.ordersHandler)
	s.mux.HandleFunc("/api/v1/positions", s.positionsHandler)
	s.mux.HandleFunc("/api/v1/balances", s.balancesHandler)
	s.mux.HandleFunc("/api/v1/risk", s.riskHandler)
	s.mux.HandleFunc("/api/v1/kill-switch", s.killHandler)
	s.mux.HandleFunc("/api/v1/kill-switch/reset", s.killResetHandler)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": http.StatusText(code), "message": msg}})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive", "time": time.Now().UTC()})
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	books := s.eng.Books.All(1)
	ready := 0
	for _, b := range books {
		if b.State.Ready && !b.State.Stale {
			ready++
		}
	}
	if ready == 0 {
		writeErr(w, http.StatusServiceUnavailable, "no synchronized books")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "ready_books": ready, "total_books": len(books)})
}

type venueView struct {
	Venue         string    `json:"venue"`
	Enabled       bool      `json:"enabled"`
	Connected     bool      `json:"connected"`
	Stale         bool      `json:"stale"`
	Reconnects    uint64    `json:"reconnects"`
	SequenceGaps  uint64    `json:"sequence_gaps"`
	LastMessageAt time.Time `json:"last_message_at"`
}

func (s *Server) venuesHandler(w http.ResponseWriter, _ *http.Request) {
	out := []venueView{}
	for _, a := range s.adapters {
		h := a.Health()
		out = append(out, venueView{
			Venue: string(a.Venue()), Enabled: true,
			Connected: h.Connected, Stale: h.Stale,
			Reconnects: h.Reconnects, SequenceGaps: h.SequenceGaps,
			LastMessageAt: h.LastMessageAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type levelJSON struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
}

type bookView struct {
	Venue  string      `json:"venue"`
	Symbol string      `json:"symbol"`
	Ready  bool        `json:"ready"`
	Stale  bool        `json:"stale"`
	Seq    int64       `json:"sequence"`
	AgeMs  int64       `json:"age_ms"`
	Bids   []levelJSON `json:"bids"`
	Asks   []levelJSON `json:"asks"`
}

func toBookView(b book.SnapshotView) bookView {
	bv := bookView{
		Venue: string(b.Venue), Symbol: b.Symbol,
		Ready: b.State.Ready, Stale: b.State.Stale, Seq: b.State.Sequence,
		AgeMs: time.Since(b.State.LastUpdatedAt).Milliseconds(),
	}
	for _, l := range b.Bids {
		bv.Bids = append(bv.Bids, levelJSON{Price: l.Price.String(), Qty: l.Qty.String()})
	}
	for _, l := range b.Asks {
		bv.Asks = append(bv.Asks, levelJSON{Price: l.Price.String(), Qty: l.Qty.String()})
	}
	return bv
}

func (s *Server) booksHandler(w http.ResponseWriter, _ *http.Request) {
	all := s.eng.Books.All(10)
	out := make([]bookView, 0, len(all))
	for _, b := range all {
		out = append(out, toBookView(b))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Venue < out[j].Venue
	})
	writeJSON(w, http.StatusOK, out)
}

// bookHandler serves /api/v1/books/{venue}/{symbol}.
func (s *Server) bookHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/books/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusNotFound, "expected /api/v1/books/{venue}/{symbol}")
		return
	}
	snap, ok := s.eng.Books.Snapshot(domain.Venue(parts[0]), parts[1], 50)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown book")
		return
	}
	writeJSON(w, http.StatusOK, toBookView(snap))
}

type oppView struct {
	ID         string    `json:"id"`
	Symbol     string    `json:"symbol"`
	BuyVenue   string    `json:"buy_venue"`
	SellVenue  string    `json:"sell_venue"`
	Quantity   string    `json:"quantity"`
	BuyVWAP    string    `json:"buy_vwap"`
	SellVWAP   string    `json:"sell_vwap"`
	GrossPnL   string    `json:"gross_pnl"`
	Fees       string    `json:"fees"`
	Slippage   string    `json:"slippage_penalty"`
	Latency    string    `json:"latency_penalty"`
	NetPnL     string    `json:"expected_net_pnl"`
	EdgeBps    int64     `json:"edge_bps"`
	BuyAgeMs   int64     `json:"buy_quote_age_ms"`
	SellAgeMs  int64     `json:"sell_quote_age_ms"`
	DetectedAt time.Time `json:"detected_at"`
}

func toOppView(o opportunity.Opportunity) oppView {
	return oppView{
		ID: o.ID, Symbol: o.Symbol,
		BuyVenue: string(o.BuyVenue), SellVenue: string(o.SellVenue),
		Quantity: o.Quantity.String(), BuyVWAP: o.BuyVWAP.String(), SellVWAP: o.SellVWAP.String(),
		GrossPnL: o.GrossPnL.String(), Fees: o.Fees.String(),
		Slippage: o.SlippagePenalty.String(), Latency: o.LatencyPenalty.String(),
		NetPnL: o.ExpectedNetPnL.String(), EdgeBps: o.EdgeBps,
		BuyAgeMs: o.BuyQuoteAge.Milliseconds(), SellAgeMs: o.SellQuoteAge.Milliseconds(),
		DetectedAt: o.DetectedAt,
	}
}

func (s *Server) oppsHandler(w http.ResponseWriter, _ *http.Request) {
	opps := s.eng.RecentOpportunities()
	out := make([]oppView, 0, len(opps))
	for _, o := range opps {
		out = append(out, toOppView(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) oppHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/opportunities/")
	o, ok := s.eng.Opportunity(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown opportunity")
		return
	}
	writeJSON(w, http.StatusOK, toOppView(o))
}

type orderView struct {
	ID        string    `json:"id"`
	ClientID  string    `json:"client_order_id"`
	Venue     string    `json:"venue"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Type      string    `json:"type"`
	Qty       string    `json:"quantity"`
	Filled    string    `json:"filled_quantity"`
	AvgPrice  string    `json:"avg_fill_price"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) ordersHandler(w http.ResponseWriter, _ *http.Request) {
	orders := s.eng.Exec.Orders()
	out := make([]orderView, 0, len(orders))
	for _, o := range orders {
		out = append(out, orderView{
			ID: o.ID, ClientID: o.ClientOrderID,
			Venue: string(o.Venue), Symbol: o.Symbol, Side: o.Side.String(),
			Type: o.Type.String(), Qty: o.Quantity.String(),
			Filled: o.FilledQuantity.String(), AvgPrice: o.AvgFillPrice.String(),
			State: o.State.String(), CreatedAt: o.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) positionsHandler(w http.ResponseWriter, _ *http.Request) {
	pos := s.eng.Ledger.Positions()
	type view struct {
		Venue   string `json:"venue"`
		Symbol  string `json:"symbol"`
		Qty     string `json:"qty"`
		AvgCost string `json:"avg_cost"`
	}
	out := []view{}
	for _, p := range pos {
		out = append(out, view{Venue: string(p.Venue), Symbol: p.Symbol, Qty: p.Qty.String(), AvgCost: p.AvgCost.String()})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) balancesHandler(w http.ResponseWriter, _ *http.Request) {
	bals := s.eng.Ledger.Balances()
	type view struct {
		Venue     string `json:"venue"`
		Asset     string `json:"asset"`
		Available string `json:"available"`
		Reserved  string `json:"reserved"`
	}
	out := []view{}
	for _, b := range bals {
		out = append(out, view{Venue: string(b.Venue), Asset: b.Asset, Available: b.Available.String(), Reserved: b.Reserved.String()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balances":     out,
		"realized_pnl": s.eng.Ledger.Realized().String(),
		"note":         "SIMULATION / PAPER EXECUTION — no live funds",
	})
}

func (s *Server) riskHandler(w http.ResponseWriter, _ *http.Request) {
	active, reason := s.eng.Risk.KillSwitchActive()
	writeJSON(w, http.StatusOK, map[string]any{
		"kill_switch":        active,
		"kill_reason":        reason,
		"outstanding_orders": s.eng.Risk.Outstanding(),
		"consecutive_fails":  s.eng.Risk.ConsecutiveFails(),
		"recent_rejections":  s.eng.Rejections(),
	})
}

func (s *Server) killHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.eng.Risk.ActivateKillSwitch("api")
	s.eng.Journal.Record("KillSwitchActivated", "", "", map[string]any{"reason": "api"})
	writeJSON(w, http.StatusOK, map[string]any{"kill_switch": true})
}

func (s *Server) killResetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.eng.Risk.ResetKillSwitch()
	s.eng.Journal.Record("KillSwitchReset", "", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"kill_switch": false})
}
