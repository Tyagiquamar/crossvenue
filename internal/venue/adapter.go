// Package venue defines the adapter interfaces connecting exchanges to the
// normalized pipeline. Execution defaults to simulation; authenticated
// adapters are feature-gated and never required.
package venue

import (
	"context"

	"crossvenue/internal/domain"
)

// MarketDataAdapter streams normalized events for one venue.
type MarketDataAdapter interface {
	Venue() domain.Venue
	Connect(ctx context.Context) error
	SubscribeBook(ctx context.Context, symbols []string, out chan<- domain.MarketEvent) error
	Health() domain.VenueHealth
	Close() error
}

// ExecutionAdapter is the optional authenticated interface. The default
// build uses the paper simulator only; this exists for feature-gated live
// adapters and must never be required for demos.
type ExecutionAdapter interface {
	Submit(ctx context.Context, req OrderRequest) (OrderAck, error)
	Cancel(ctx context.Context, req CancelRequest) (CancelAck, error)
	OpenOrders(ctx context.Context, symbol string) ([]ExchangeOrder, error)
}

// OrderRequest is a normalized order submission.
type OrderRequest struct {
	ClientOrderID string
	Symbol        string
	Side          domain.Side
	Type          string
	Price         domain.Price
	Quantity      domain.Quantity
}

// OrderAck acknowledges submission.
type OrderAck struct {
	ExchangeOrderID string
	Accepted        bool
	Reason          string
}

// CancelRequest identifies an order to cancel.
type CancelRequest struct {
	Symbol          string
	ExchangeOrderID string
	ClientOrderID   string
}

// CancelAck acknowledges cancellation.
type CancelAck struct{ Cancelled bool }

// ExchangeOrder is a venue-native open order view.
type ExchangeOrder struct {
	ExchangeOrderID string
	ClientOrderID   string
	Symbol          string
	Side            domain.Side
	Price           domain.Price
	Quantity        domain.Quantity
	FilledQuantity  domain.Quantity
}
