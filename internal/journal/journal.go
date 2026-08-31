// Package journal is the append-oriented engine event log. In-memory by
// default; a PostgreSQL sink mirrors events durably when configured.
package journal

import (
	"sync"
	"time"

	"crossvenue/internal/domain"
)

// Event is one journaled occurrence.
type Event struct {
	ID          uint64
	Type        string
	Venue       domain.Venue
	Symbol      string
	AggregateID string
	Payload     map[string]any
	OccurredAt  time.Time
	RecordedAt  time.Time
}

// Sink persists events (Postgres adapter implements this).
type Sink interface {
	Append(Event) error
}

// Journal is the in-memory event log with optional durable sink fan-out.
type Journal struct {
	mu     sync.Mutex
	seq    uint64
	events []Event
	sinks  []Sink
	// MaxEvents bounds memory; 0 = unbounded (replay/tests use unbounded).
	MaxEvents int
}

// New creates a journal with optional durable sinks.
func New(sinks ...Sink) *Journal { return &Journal{sinks: sinks} }

// Record appends an event, assigning a monotonic sequence.
func (j *Journal) Record(eventType string, venue domain.Venue, symbol string, payload map[string]any) {
	j.RecordFull(eventType, venue, symbol, "", payload)
}

// RecordFull appends with an aggregate id (e.g. order id, opportunity id).
func (j *Journal) RecordFull(eventType string, venue domain.Venue, symbol, aggregateID string, payload map[string]any) {
	now := time.Now().UTC()
	j.mu.Lock()
	j.seq++
	ev := Event{
		ID:          j.seq,
		Type:        eventType,
		Venue:       venue,
		Symbol:      symbol,
		AggregateID: aggregateID,
		Payload:     payload,
		OccurredAt:  now,
		RecordedAt:  now,
	}
	j.events = append(j.events, ev)
	if j.MaxEvents > 0 && len(j.events) > j.MaxEvents {
		j.events = j.events[len(j.events)-j.MaxEvents:]
	}
	sinks := append([]Sink(nil), j.sinks...)
	j.mu.Unlock()
	for _, s := range sinks {
		_ = s.Append(ev) // durable sink failures must not stall the engine
	}
}

// Events returns a copy of all in-memory events.
func (j *Journal) Events() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Event, len(j.events))
	copy(out, j.events)
	return out
}

// Digest is a deterministic FNV-1a hash of the event stream for replay
// parity checks.
func (j *Journal) Digest() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	h := uint64(1469598103934665603)
	mix := func(b byte) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	mixStr := func(s string) {
		for i := 0; i < len(s); i++ {
			mix(s[i])
		}
		mix(0)
	}
	for _, e := range j.events {
		mixStr(e.Type)
		mixStr(string(e.Venue))
		mixStr(e.Symbol)
		mixStr(e.AggregateID)
	}
	return h
}
