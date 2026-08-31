// Package bybit implements the Bybit Spot public orderbook feed adapter.
//
// Wire semantics (docs/market-data-contract.md is binding):
//   - Topic orderbook.<depth>.<symbol> sends type:"snapshot" then
//     type:"delta" messages.
//   - Each message carries sequence u; deltas must advance u by exactly 1
//     (per-symbol). ts is ms epoch. Levels are [price, qty].
package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/internal/venue/wsbase"
	"crossvenue/pkg/decimal"
)

const wsURL = "wss://stream.bybit.com/v5/public/spot"

// Adapter is the Bybit market-data adapter.
type Adapter struct {
	conn *wsbase.Conn
}

// New creates the adapter.
func New() *Adapter {
	a := &Adapter{}
	cfg := wsbase.Config{
		URL:   wsURL,
		Venue: domain.VenueBybit,
		Subscribe: func(symbols []string) [][]byte {
			args := make([]string, 0, len(symbols))
			for _, s := range symbols {
				args = append(args, "orderbook.50."+strings.ReplaceAll(s, "-", ""))
			}
			frame, _ := json.Marshal(map[string]any{
				"op":   "subscribe",
				"args": args,
			})
			return [][]byte{frame}
		},
	}
	a.conn = wsbase.New(cfg, a.handle)
	return a
}

// Venue implements venue.MarketDataAdapter.
func (a *Adapter) Venue() domain.Venue { return domain.VenueBybit }

// Connect implements venue.MarketDataAdapter.
func (a *Adapter) Connect(context.Context) error { return nil }

// Health implements venue.MarketDataAdapter.
func (a *Adapter) Health() domain.VenueHealth { return a.conn.Health() }

// Close implements venue.MarketDataAdapter.
func (a *Adapter) Close() error { return a.conn.Close() }

// SubscribeBook implements venue.MarketDataAdapter.
func (a *Adapter) SubscribeBook(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error {
	return a.conn.Run(ctx, symbols, out)
}

type msg struct {
	Topic string `json:"topic"`
	Type  string `json:"type"`
	TS    int64  `json:"ts"`
	Data  struct {
		Symbol string      `json:"s"`
		Bids   [][2]string `json:"b"`
		Asks   [][2]string `json:"a"`
		U      int64       `json:"u"`
		Seq    int64       `json:"seq"`
	} `json:"data"`
	// ack frames
	Success bool   `json:"success"`
	Op      string `json:"op"`
}

func parseLevels(raw [][2]string) ([]domain.Level, error) {
	out := make([]domain.Level, 0, len(raw))
	for _, l := range raw {
		if len(l) != 2 {
			return nil, fmt.Errorf("bybit: malformed level %v", l)
		}
		px, err := decimal.Parse(l[0])
		if err != nil {
			return nil, fmt.Errorf("bybit: bad price %q: %w", l[0], err)
		}
		qty, err := decimal.Parse(l[1])
		if err != nil {
			return nil, fmt.Errorf("bybit: bad qty %q: %w", l[1], err)
		}
		out = append(out, domain.Level{Price: px, Qty: qty})
	}
	return out, nil
}

func normalizeSymbol(s string) string {
	s = strings.ToUpper(s)
	for _, quote := range []string{"USDT", "USDC"} {
		if strings.HasSuffix(s, quote) {
			return strings.TrimSuffix(s, quote) + "-" + quote
		}
	}
	return s
}

func (a *Adapter) handle(_ context.Context, raw []byte) ([]domain.MarketEvent, error) {
	var m msg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Topic == "" || !strings.HasPrefix(m.Topic, "orderbook.") {
		return nil, nil // ack/pong frames
	}
	bids, err := parseLevels(m.Data.Bids)
	if err != nil {
		return nil, err
	}
	asks, err := parseLevels(m.Data.Asks)
	if err != nil {
		return nil, err
	}
	symbol := normalizeSymbol(m.Data.Symbol)
	exch := time.UnixMilli(m.TS)
	recv := time.Now()
	switch m.Type {
	case "snapshot":
		return []domain.MarketEvent{{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
			Venue: domain.VenueBybit, Symbol: symbol,
			Bids: bids, Asks: asks, Sequence: m.Data.U,
			ExchangeTime: exch, ReceiveTime: recv,
		}}}, nil
	case "delta":
		return []domain.MarketEvent{{Type: domain.EventDelta, Delta: &domain.BookDelta{
			Venue: domain.VenueBybit, Symbol: symbol,
			Bids: bids, Asks: asks,
			Sequence: m.Data.U, PrevSequence: m.Data.U - 1,
			ExchangeTime: exch, ReceiveTime: recv,
		}}}, nil
	}
	return nil, nil
}
