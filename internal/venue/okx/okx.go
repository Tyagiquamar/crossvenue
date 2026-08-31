// Package okx implements the OKX Spot public books feed adapter.
//
// Wire semantics (docs/market-data-contract.md is binding):
//   - Channel "books" sends action:"snapshot" then action:"update".
//   - Each message carries seqId; updates must advance seqId by exactly 1.
//   - ts is ms epoch. Levels are [price, qty, liquidOrders, numOrders].
package okx

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

const wsURL = "wss://ws.okx.com:8443/ws/v5/public"

// Adapter is the OKX market-data adapter.
type Adapter struct {
	conn *wsbase.Conn
}

// New creates the adapter.
func New() *Adapter {
	a := &Adapter{}
	cfg := wsbase.Config{
		URL:          wsURL,
		Venue:        domain.VenueOKX,
		AppPing:      []byte("ping"),
		PingInterval: 25 * time.Second,
		Subscribe: func(symbols []string) [][]byte {
			args := make([]map[string]string, 0, len(symbols))
			for _, s := range symbols {
				args = append(args, map[string]string{"channel": "books", "instId": s})
			}
			frame, _ := json.Marshal(map[string]any{"op": "subscribe", "args": args})
			return [][]byte{frame}
		},
	}
	a.conn = wsbase.New(cfg, a.handle)
	return a
}

// Venue implements venue.MarketDataAdapter.
func (a *Adapter) Venue() domain.Venue { return domain.VenueOKX }

// Connect implements venue.MarketDataAdapter.
func (a *Adapter) Connect(context.Context) error { return nil }

// RequestResync implements venue.MarketDataAdapter: drop the session so the
// reconnect re-subscribes and a fresh snapshot re-anchors the book.
func (a *Adapter) RequestResync(string) { a.conn.RequestResync() }

// Health implements venue.MarketDataAdapter.
func (a *Adapter) Health() domain.VenueHealth { return a.conn.Health() }

// Close implements venue.MarketDataAdapter.
func (a *Adapter) Close() error { return a.conn.Close() }

// SubscribeBook implements venue.MarketDataAdapter.
func (a *Adapter) SubscribeBook(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error {
	return a.conn.Run(ctx, symbols, out)
}

type msg struct {
	Event string `json:"event"`
	Arg   struct {
		Channel string `json:"channel"`
		InstID  string `json:"instId"`
	} `json:"arg"`
	Action string `json:"action"`
	Data   []struct {
		Bids      [][4]string `json:"bids"`
		Asks      [][4]string `json:"asks"`
		Ts        string      `json:"ts"`
		SeqID     int64       `json:"seqId"`
		PrevSeqID int64       `json:"prevSeqId"`
	} `json:"data"`
}

func parseLevels(raw [][4]string) ([]domain.Level, error) {
	out := make([]domain.Level, 0, len(raw))
	for _, l := range raw {
		px, err := decimal.Parse(l[0])
		if err != nil {
			return nil, fmt.Errorf("okx: bad price %q: %w", l[0], err)
		}
		qty, err := decimal.Parse(l[1])
		if err != nil {
			return nil, fmt.Errorf("okx: bad qty %q: %w", l[1], err)
		}
		out = append(out, domain.Level{Price: px, Qty: qty})
	}
	return out, nil
}

func (a *Adapter) handle(_ context.Context, raw []byte) ([]domain.MarketEvent, error) {
	if string(raw) == "pong" {
		return nil, nil
	}
	var m msg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Event != "" { // subscribe ack / error frames
		return nil, nil
	}
	if m.Arg.Channel != "books" || len(m.Data) == 0 {
		return nil, nil
	}
	d := m.Data[0]
	symbol := strings.ToUpper(m.Arg.InstID)
	bids, err := parseLevels(d.Bids)
	if err != nil {
		return nil, err
	}
	asks, err := parseLevels(d.Asks)
	if err != nil {
		return nil, err
	}
	var ts int64
	fmt.Sscanf(d.Ts, "%d", &ts)
	exch := time.UnixMilli(ts)
	recv := time.Now()
	switch m.Action {
	case "snapshot":
		return []domain.MarketEvent{{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
			Venue: domain.VenueOKX, Symbol: symbol,
			Bids: bids, Asks: asks, Sequence: d.SeqID,
			ExchangeTime: exch, ReceiveTime: recv,
		}}}, nil
	case "update":
		return []domain.MarketEvent{{Type: domain.EventDelta, Delta: &domain.BookDelta{
			Venue: domain.VenueOKX, Symbol: symbol,
			Bids: bids, Asks: asks,
			Sequence: d.SeqID, PrevSequence: d.PrevSeqID,
			ExchangeTime: exch, ReceiveTime: recv,
		}}}, nil
	}
	return nil, nil
}
