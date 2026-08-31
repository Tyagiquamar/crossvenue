// Package domain defines the venue-independent normalized event model.
// Venue adapters translate exchange-specific wire formats into these types.
package domain

import (
	"time"

	"crossvenue/pkg/decimal"
)

// Price is a fixed-point price in quote currency (USDT).
type Price = decimal.Fixed

// Quantity is a fixed-point base-asset amount.
type Quantity = decimal.Fixed

// Money is a fixed-point quote-currency amount.
type Money = decimal.Fixed

// Venue identifies an exchange.
type Venue string

const (
	VenueBinance   Venue = "binance"
	VenueOKX       Venue = "okx"
	VenueBybit     Venue = "bybit"
	VenueSynthetic Venue = "synthetic"
)

// Side of an order or trade.
type Side uint8

const (
	Buy Side = iota + 1
	Sell
)

func (s Side) String() string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

// Opposite returns the other side.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// Level is one price level in an order book.
type Level struct {
	Price Price
	Qty   Quantity
}

// BookSnapshot is a full depth image from a venue.
type BookSnapshot struct {
	Venue        Venue
	Symbol       string
	Bids         []Level
	Asks         []Level
	Sequence     int64
	ExchangeTime time.Time
	ReceiveTime  time.Time
}

// BookDelta is an incremental depth update. Sequence semantics are
// venue-specific and enforced by the venue's SequenceTracker.
type BookDelta struct {
	Venue        Venue
	Symbol       string
	Bids         []Level
	Asks         []Level
	Sequence     int64
	PrevSequence int64
	ExchangeTime time.Time
	ReceiveTime  time.Time
}

// Trade is a normalized public trade print.
type Trade struct {
	Venue        Venue
	Symbol       string
	Price        Price
	Qty          Quantity
	Aggressor    Side
	ExchangeTime time.Time
	ReceiveTime  time.Time
}

// EventType classifies normalized market events.
type EventType uint8

const (
	EventSnapshot EventType = iota + 1
	EventDelta
	EventTrade
)

// MarketEvent wraps any normalized market-data event.
type MarketEvent struct {
	Type     EventType
	Snapshot *BookSnapshot
	Delta    *BookDelta
	Trade    *Trade
}

// VenueOf returns the event venue.
func (e MarketEvent) VenueOf() Venue {
	switch e.Type {
	case EventSnapshot:
		return e.Snapshot.Venue
	case EventDelta:
		return e.Delta.Venue
	case EventTrade:
		return e.Trade.Venue
	}
	return ""
}

// SymbolOf returns the event symbol.
func (e MarketEvent) SymbolOf() string {
	switch e.Type {
	case EventSnapshot:
		return e.Snapshot.Symbol
	case EventDelta:
		return e.Delta.Symbol
	case EventTrade:
		return e.Trade.Symbol
	}
	return ""
}

// VenueHealth reports connectivity and synchronization state of one venue.
type VenueHealth struct {
	Connected      bool
	LastMessageAt  time.Time
	LastBookUpdate time.Time
	Reconnects     uint64
	SequenceGaps   uint64
	Resyncs        uint64
	Stale          bool
}
