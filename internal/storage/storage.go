// Package storage provides the PostgreSQL persistence adapter for
// operational state (orders, fills, balances, positions, events).
// High-volume raw market data never goes through Postgres; it is recorded
// to files by internal/replay.
package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"crossvenue/internal/domain"
	"crossvenue/internal/execution"
	"crossvenue/internal/journal"
	"crossvenue/internal/portfolio"
	"crossvenue/pkg/decimal"
)

// Store wraps a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and pings.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Migrate applies the schema (idempotent).
func (s *Store) Migrate(ctx context.Context, schema string) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// EventSink returns a journal.Sink writing engine_events.
func (s *Store) EventSink() journal.Sink { return eventSink{s} }

type eventSink struct{ s *Store }

// Append implements journal.Sink.
func (e eventSink) Append(ev journal.Event) error {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		payload = []byte("{}")
	}
	_, err = e.s.pool.Exec(context.Background(), `
		INSERT INTO engine_events (sequence, type, venue, symbol, aggregate_id, payload, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ev.ID, ev.Type, string(ev.Venue), ev.Symbol, ev.AggregateID, payload, ev.OccurredAt)
	return err
}

// UpsertOrder persists an order snapshot.
func (s *Store) UpsertOrder(ctx context.Context, o *execution.Order) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO orders (id, client_order_id, venue, symbol, side, type, price, quantity,
			filled_quantity, avg_fill_price, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			filled_quantity = EXCLUDED.filled_quantity,
			avg_fill_price  = EXCLUDED.avg_fill_price,
			state           = EXCLUDED.state,
			updated_at      = EXCLUDED.updated_at`,
		o.ID, o.ClientOrderID, string(o.Venue), o.Symbol, int16(o.Side), int16(o.Type),
		o.Price.String(), o.Quantity.String(), o.FilledQuantity.String(),
		o.AvgFillPrice.String(), int16(o.State), o.CreatedAt, o.UpdatedAt)
	return err
}

// InsertFill records a fill.
func (s *Store) InsertFill(ctx context.Context, f execution.FillEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO fills (order_id, venue, symbol, side, price, qty, fee, filled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		f.OrderID, string(f.Venue), f.Symbol, int16(f.Side),
		f.Price.String(), f.Qty.String(), f.Fee.String(), f.At)
	return err
}

// SaveBalancesTx upserts balances and positions atomically. Fill + position
// + balance consistency is maintained by callers using one transaction
// through this API for correlated updates.
func (s *Store) SavePortfolio(ctx context.Context, bals []portfolio.Balance, poss []portfolio.Position) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	for _, b := range bals {
		if _, err := tx.Exec(ctx, `
			INSERT INTO balances (venue, asset, available, reserved) VALUES ($1,$2,$3,$4)
			ON CONFLICT (venue, asset) DO UPDATE SET available = EXCLUDED.available, reserved = EXCLUDED.reserved`,
			string(b.Venue), b.Asset, b.Available.String(), b.Reserved.String()); err != nil {
			return err
		}
	}
	for _, p := range poss {
		if _, err := tx.Exec(ctx, `
			INSERT INTO positions (venue, symbol, qty, avg_cost) VALUES ($1,$2,$3,$4)
			ON CONFLICT (venue, symbol) DO UPDATE SET qty = EXCLUDED.qty, avg_cost = EXCLUDED.avg_cost`,
			string(p.Venue), p.Symbol, p.Qty.String(), p.AvgCost.String()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LoadBalances restores balances on restart (recovery path).
func (s *Store) LoadBalances(ctx context.Context) ([]portfolio.Balance, error) {
	rows, err := s.pool.Query(ctx, `SELECT venue, asset, available::text, reserved::text FROM balances`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []portfolio.Balance
	for rows.Next() {
		var b portfolio.Balance
		var venue, asset, avail, res string
		if err := rows.Scan(&venue, &asset, &avail, &res); err != nil {
			return nil, err
		}
		b.Venue = domain.Venue(venue)
		b.Asset = asset
		a, err := decimal.Parse(avail)
		if err != nil {
			return nil, fmt.Errorf("storage: balance %s/%s: %w", venue, asset, err)
		}
		b.Available = a
		if r, err := decimal.Parse(res); err == nil {
			b.Reserved = r
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveSystemState stores a control key (e.g. kill switch) durably.
func (s *Store) SaveSystemState(ctx context.Context, key string, value any) error {
	v, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO system_state (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, v)
	return err
}

// LoadSystemState reads a control key.
func (s *Store) LoadSystemState(ctx context.Context, key string, out any) (bool, error) {
	var v []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM system_state WHERE key = $1`, key).Scan(&v)
	if err != nil {
		return false, nil // not found is not an error for recovery
	}
	return true, json.Unmarshal(v, out)
}
