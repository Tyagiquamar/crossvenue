// Package engine contains sources: adapters from live venues, synthetic
// generators, and the replay reader — all satisfy EventSource.
package engine

import (
	"context"
	"fmt"
	"io"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/internal/replay"
	"crossvenue/internal/venue"
)

// LiveSource multiplexes venue adapters into one event stream.
type LiveSource struct {
	Adapters []venue.MarketDataAdapter
	Symbols  []string
}

// Stream implements EventSource.
func (s *LiveSource) Stream(ctx context.Context, out chan<- domain.MarketEvent) error {
	errCh := make(chan error, len(s.Adapters))
	for _, a := range s.Adapters {
		a := a
		go func() {
			if err := a.Connect(ctx); err != nil {
				errCh <- fmt.Errorf("%s connect: %w", a.Venue(), err)
				return
			}
			errCh <- a.SubscribeBook(ctx, s.Symbols, out)
		}()
	}
	// Return the first terminal error; adapters retry internally.
	return <-errCh
}

// ReplaySource streams a recording at 1x/10x/max speed. In deterministic
// mode (Speed "max" or ManualClock), no wall-clock sleeping occurs — the
// caller steps time.
type ReplaySource struct {
	Path  string
	Speed string
}

// Stream implements EventSource.
func (s *ReplaySource) Stream(ctx context.Context, out chan<- domain.MarketEvent) error {
	r, err := replay.NewReader(s.Path)
	if err != nil {
		return err
	}
	defer r.Close()
	var prevRecv time.Time
	first := true
	for {
		_, ev, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		recv := eventRecv(ev)
		if s.Speed != "max" && !first {
			mult := speedMultiplier(s.Speed)
			gap := recv.Sub(prevRecv)
			if gap > 0 {
				wait := time.Duration(float64(gap) / mult)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}
		first = false
		prevRecv = recv
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func eventRecv(ev domain.MarketEvent) time.Time {
	switch ev.Type {
	case domain.EventSnapshot:
		return ev.Snapshot.ReceiveTime
	case domain.EventDelta:
		return ev.Delta.ReceiveTime
	case domain.EventTrade:
		return ev.Trade.ReceiveTime
	}
	return time.Time{}
}

func speedMultiplier(s string) float64 {
	mult := 1.0
	if n := len(s); n > 1 && s[n-1] == 'x' {
		fmt.Sscanf(s[:n-1], "%f", &mult)
	}
	if mult <= 0 {
		mult = 1
	}
	return mult
}
