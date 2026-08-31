// Package binance implements the Binance Spot public depth feed adapter.
//
// Wire semantics (docs/market-data-contract.md is binding):
//   - Stream <symbol>@depth@100ms sends events {u: lastUpdateID,
//     U: firstUpdateID, b: bids, a: asks}.
//   - A REST depth snapshot seeds the book; the first WS event must bridge
//     snapshot lastUpdateID (U <= snapID <= u), thereafter u must chain
//     via U == prevU + ... — enforced by book.BinanceTracker.
//   - No REST call is made here in paper mode; the adapter emits an initial
//     synthetic-free "snapshot" by requesting the partial-book stream
//     <symbol>@depth20, which delivers a full top-20 image each message.
//     We treat depth20 messages as snapshots (their lastUpdateId anchors
//     the incremental stream). This keeps the adapter public-only.
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/internal/venue/wsbase"
	"crossvenue/pkg/decimal"
)

const wsURL = "wss://stream.binance.com:9443/stream"

// Adapter is the Binance market-data adapter.
type Adapter struct {
	conn *wsbase.Conn

	lastSeq atomic.Int64
}

// New creates the adapter.
func New() *Adapter {
	a := &Adapter{}
	cfg := wsbase.Config{
		URL:   wsURL,
		Venue: domain.VenueBinance,
		Subscribe: func(symbols []string) [][]byte {
			streams := make([]string, 0, len(symbols)*2)
			for _, s := range symbols {
				ls := strings.ToLower(strings.ReplaceAll(s, "-", ""))
				streams = append(streams, ls+"@depth20@100ms", ls+"@depth@100ms")
			}
			frame, _ := json.Marshal(map[string]any{
				"method": "SUBSCRIBE",
				"params": streams,
				"id":     1,
			})
			return [][]byte{frame}
		},
	}
	a.conn = wsbase.New(cfg, a.handle)
	return a
}

// Venue implements venue.MarketDataAdapter.
func (a *Adapter) Venue() domain.Venue { return domain.VenueBinance }

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

type combinedMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type depth20Msg struct {
	LastUpdateID int64       `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

type depthUpdateMsg struct {
	EventType string      `json:"e"`
	EventTime int64       `json:"E"`
	Symbol    string      `json:"s"`
	FirstID   int64       `json:"U"`
	LastID    int64       `json:"u"`
	Bids      [][2]string `json:"b"`
	Asks      [][2]string `json:"a"`
}

func parseLevels(raw [][2]string) ([]domain.Level, error) {
	out := make([]domain.Level, 0, len(raw))
	for _, l := range raw {
		if len(l) != 2 {
			return nil, fmt.Errorf("binance: malformed level %v", l)
		}
		px, err := decimal.Parse(l[0])
		if err != nil {
			return nil, fmt.Errorf("binance: bad price %q: %w", l[0], err)
		}
		qty, err := decimal.Parse(l[1])
		if err != nil {
			return nil, fmt.Errorf("binance: bad qty %q: %w", l[1], err)
		}
		out = append(out, domain.Level{Price: px, Qty: qty})
	}
	return out, nil
}

func (a *Adapter) handle(_ context.Context, msg []byte) ([]domain.MarketEvent, error) {
	var cm combinedMsg
	if err := json.Unmarshal(msg, &cm); err != nil || cm.Stream == "" {
		return nil, fmt.Errorf("binance: not a combined stream message")
	}
	recv := time.Now()
	parts := strings.SplitN(cm.Stream, "@", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("binance: unexpected stream %q", cm.Stream)
	}
	symbol := normalizeSymbol(parts[0])

	switch {
	case strings.HasPrefix(parts[1], "depth20"):
		var m depth20Msg
		if err := json.Unmarshal(cm.Data, &m); err != nil {
			return nil, err
		}
		bids, err := parseLevels(m.Bids)
		if err != nil {
			return nil, err
		}
		asks, err := parseLevels(m.Asks)
		if err != nil {
			return nil, err
		}
		a.lastSeq.Store(m.LastUpdateID)
		return []domain.MarketEvent{{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
			Venue: domain.VenueBinance, Symbol: symbol,
			Bids: bids, Asks: asks, Sequence: m.LastUpdateID,
			ExchangeTime: recv, ReceiveTime: recv,
		}}}, nil

	case parts[1] == "depth@100ms" || parts[1] == "depth":
		var m depthUpdateMsg
		if err := json.Unmarshal(cm.Data, &m); err != nil {
			return nil, err
		}
		bids, err := parseLevels(m.Bids)
		if err != nil {
			return nil, err
		}
		asks, err := parseLevels(m.Asks)
		if err != nil {
			return nil, err
		}
		return []domain.MarketEvent{{Type: domain.EventDelta, Delta: &domain.BookDelta{
			Venue: domain.VenueBinance, Symbol: symbol,
			Bids: bids, Asks: asks,
			Sequence: m.LastID, PrevSequence: m.FirstID,
			ExchangeTime: time.UnixMilli(m.EventTime), ReceiveTime: recv,
		}}}, nil
	}
	return nil, nil
}

func normalizeSymbol(streamSym string) string {
	s := strings.ToUpper(streamSym)
	for _, quote := range []string{"USDT", "USDC", "FDUSD"} {
		if strings.HasSuffix(s, quote) {
			base := strings.TrimSuffix(s, quote)
			// strconv used only for sanity: base must be alpha
			if _, err := strconv.Atoi(base); err == nil {
				continue
			}
			return base + "-" + quote
		}
	}
	return s
}
