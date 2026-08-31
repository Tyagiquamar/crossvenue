// Package wsbase provides the shared WebSocket plumbing for venue
// adapters: reconnect with capped exponential backoff + jitter, heartbeats,
// subscription handling, bounded output, and health tracking.
package wsbase

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"crossvenue/internal/domain"
)

// Handler parses one raw WS message into zero or more normalized events.
// Returning ErrNeedsResync signals the adapter to re-snapshot.
type Handler func(ctx context.Context, msg []byte) ([]domain.MarketEvent, error)

// Config controls the connection loop.
type Config struct {
	URL    string
	Venue  domain.Venue
	Logger *slog.Logger
	// Subscribe returns venue-specific subscription frames for symbols.
	Subscribe func(symbols []string) [][]byte
	// PingInterval; 0 uses 30s. Binance requires client pings or responds
	// to server pings; OKX requires application-level "ping" text frames
	// (handled via AppPing).
	PingInterval time.Duration
	// AppPing, if non-nil, is sent as a text frame on the ping ticker.
	AppPing []byte
	// MaxBackoff caps exponential backoff.
	MaxBackoff time.Duration
	// OutQueue bounds the normalized-event channel per venue.
	OutQueue int
}

// Conn is a reconnecting venue WebSocket.
type Conn struct {
	cfg     Config
	handler Handler

	health domain.VenueHealth
	mu     sync.RWMutex

	reconnects atomic.Uint64
	closed     atomic.Bool
}

// New constructs the connection manager.
func New(cfg Config, h Handler) *Conn {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.OutQueue <= 0 {
		cfg.OutQueue = 4096
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Conn{cfg: cfg, handler: h}
}

// Health returns a snapshot of venue health.
func (c *Conn) Health() domain.VenueHealth {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := c.health
	h.Reconnects = c.reconnects.Load()
	return h
}

func (c *Conn) setConnected(b bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Connected = b
	if !b {
		c.health.Stale = true
	}
}

func (c *Conn) noteMessage(isBook bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.LastMessageAt = time.Now()
	if isBook {
		c.health.LastBookUpdate = c.health.LastMessageAt
		c.health.Stale = false
	}
}

// Run connects and streams events into out until ctx is cancelled or Close
// is called. Reconnects with capped exponential backoff + jitter.
func (c *Conn) Run(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil || c.closed.Load() {
			return ctx.Err()
		}
		err := c.session(ctx, symbols, out)
		c.setConnected(false)
		if ctx.Err() != nil || c.closed.Load() {
			return nil
		}
		c.reconnects.Add(1)
		c.cfg.Logger.Warn("venue disconnected, reconnecting",
			"venue", c.cfg.Venue, "err", err, "backoff", backoff)
		// jitter: 50-100% of backoff
		jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff/2 + jitter):
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

func (c *Conn) session(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, c.cfg.URL, &websocket.DialOptions{})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 22)

	for _, frame := range c.cfg.Subscribe(symbols) {
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			return err
		}
	}
	c.setConnected(true)
	c.cfg.Logger.Info("venue connected", "venue", c.cfg.Venue, "symbols", symbols)

	// heartbeat
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(c.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				var err error
				if c.cfg.AppPing != nil {
					err = conn.Write(pctx, websocket.MessageText, c.cfg.AppPing)
				} else {
					err = conn.Ping(pctx)
				}
				pcancel()
				if err != nil {
					cancel() // force reconnect
					return
				}
			}
		}
	}()
	defer close(pingDone)

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		events, herr := c.handler(ctx, msg)
		if herr != nil {
			c.cfg.Logger.Debug("message handler", "venue", c.cfg.Venue, "err", herr)
			continue
		}
		for _, ev := range events {
			c.noteMessage(ev.Type == domain.EventSnapshot || ev.Type == domain.EventDelta)
			select {
			case out <- ev:
			case <-ctx.Done():
				return ctx.Err()
			default:
				// never block the read loop; the pipeline decides policy
				// (invalidate+resync) on its own bounded queues.
			}
		}
	}
}

// Close stops the connection loop.
func (c *Conn) Close() error {
	c.closed.Store(true)
	return nil
}
