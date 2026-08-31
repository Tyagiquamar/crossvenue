// Package replay implements deterministic record/replay of normalized
// market events. Live adapters write RecordedEvents; replay feeds the same
// events into the same pipeline. Given the same recording, config, and
// seed, the engine reproduces identical output.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"sync"
	"time"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func unixNano(ns int64) time.Time { return time.Unix(0, ns).UTC() }

// Version of the recording envelope format.
const Version uint16 = 1

// RecordedEvent is the versioned on-disk envelope. Times are unix nanos.
type RecordedEvent struct {
	Version      uint16        `json:"v"`
	Sequence     uint64        `json:"seq"`
	EventType    string        `json:"type"` // snapshot|delta|trade
	Venue        domain.Venue  `json:"venue"`
	Symbol       string        `json:"symbol"`
	ExchangeTime int64         `json:"exchange_time"`
	ReceiveTime  int64         `json:"receive_time"`
	Snapshot     *SnapshotJSON `json:"snapshot,omitempty"`
	Delta        *DeltaJSON    `json:"delta,omitempty"`
	Trade        *TradeJSON    `json:"trade,omitempty"`
}

// LevelJSON is a serialized level.
type LevelJSON struct {
	Price int64 `json:"p"`
	Qty   int64 `json:"q"`
}

// SnapshotJSON serializes BookSnapshot.
type SnapshotJSON struct {
	Bids     []LevelJSON `json:"bids"`
	Asks     []LevelJSON `json:"asks"`
	Sequence int64       `json:"sequence"`
}

// DeltaJSON serializes BookDelta.
type DeltaJSON struct {
	Bids         []LevelJSON `json:"bids"`
	Asks         []LevelJSON `json:"asks"`
	Sequence     int64       `json:"sequence"`
	PrevSequence int64       `json:"prev_sequence"`
}

// TradeJSON serializes Trade.
type TradeJSON struct {
	Price     int64 `json:"price"`
	Qty       int64 `json:"qty"`
	Aggressor uint8 `json:"aggressor"`
}

func levelsToJSON(ls []domain.Level) []LevelJSON {
	out := make([]LevelJSON, len(ls))
	for i, l := range ls {
		out[i] = LevelJSON{Price: l.Price.Raw(), Qty: l.Qty.Raw()}
	}
	return out
}

func levelsFromJSON(ls []LevelJSON) []domain.Level {
	out := make([]domain.Level, len(ls))
	for i, l := range ls {
		out[i] = domain.Level{Price: decimal.FromRaw(l.Price), Qty: decimal.FromRaw(l.Qty)}
	}
	return out
}

// FromEvent converts a normalized event into the envelope.
func FromEvent(seq uint64, ev domain.MarketEvent) (RecordedEvent, error) {
	re := RecordedEvent{Version: Version, Sequence: seq}
	switch ev.Type {
	case domain.EventSnapshot:
		s := ev.Snapshot
		re.EventType = "snapshot"
		re.Venue, re.Symbol = s.Venue, s.Symbol
		re.ExchangeTime = s.ExchangeTime.UnixNano()
		re.ReceiveTime = s.ReceiveTime.UnixNano()
		re.Snapshot = &SnapshotJSON{Bids: levelsToJSON(s.Bids), Asks: levelsToJSON(s.Asks), Sequence: s.Sequence}
	case domain.EventDelta:
		d := ev.Delta
		re.EventType = "delta"
		re.Venue, re.Symbol = d.Venue, d.Symbol
		re.ExchangeTime = d.ExchangeTime.UnixNano()
		re.ReceiveTime = d.ReceiveTime.UnixNano()
		re.Delta = &DeltaJSON{Bids: levelsToJSON(d.Bids), Asks: levelsFromAsks(d), Sequence: d.Sequence, PrevSequence: d.PrevSequence}
	case domain.EventTrade:
		t := ev.Trade
		re.EventType = "trade"
		re.Venue, re.Symbol = t.Venue, t.Symbol
		re.ExchangeTime = t.ExchangeTime.UnixNano()
		re.ReceiveTime = t.ReceiveTime.UnixNano()
		re.Trade = &TradeJSON{Price: t.Price.Raw(), Qty: t.Qty.Raw(), Aggressor: uint8(t.Aggressor)}
	default:
		return RecordedEvent{}, fmt.Errorf("replay: unknown event type %d", ev.Type)
	}
	return re, nil
}

func levelsFromAsks(d *domain.BookDelta) []LevelJSON { return levelsToJSON(d.Asks) }

// ToEvent reconstructs the normalized event.
func (re RecordedEvent) ToEvent() (domain.MarketEvent, error) {
	if re.Version != Version {
		return domain.MarketEvent{}, fmt.Errorf("replay: unsupported version %d", re.Version)
	}
	switch re.EventType {
	case "snapshot":
		return domain.MarketEvent{Type: domain.EventSnapshot, Snapshot: &domain.BookSnapshot{
			Venue: re.Venue, Symbol: re.Symbol,
			Bids: levelsFromJSON(re.Snapshot.Bids), Asks: levelsFromJSON(re.Snapshot.Asks),
			Sequence:     re.Snapshot.Sequence,
			ExchangeTime: unixNano(re.ExchangeTime), ReceiveTime: unixNano(re.ReceiveTime),
		}}, nil
	case "delta":
		return domain.MarketEvent{Type: domain.EventDelta, Delta: &domain.BookDelta{
			Venue: re.Venue, Symbol: re.Symbol,
			Bids: levelsFromJSON(re.Delta.Bids), Asks: levelsFromJSON(re.Delta.Asks),
			Sequence: re.Delta.Sequence, PrevSequence: re.Delta.PrevSequence,
			ExchangeTime: unixNano(re.ExchangeTime), ReceiveTime: unixNano(re.ReceiveTime),
		}}, nil
	case "trade":
		return domain.MarketEvent{Type: domain.EventTrade, Trade: &domain.Trade{
			Venue: re.Venue, Symbol: re.Symbol,
			Price: decimal.FromRaw(re.Trade.Price), Qty: decimal.FromRaw(re.Trade.Qty),
			Aggressor:    domain.Side(re.Trade.Aggressor),
			ExchangeTime: unixNano(re.ExchangeTime), ReceiveTime: unixNano(re.ReceiveTime),
		}}, nil
	}
	return domain.MarketEvent{}, fmt.Errorf("replay: unknown event type %q", re.EventType)
}

// Recorder writes newline-delimited JSON envelopes (v1). The envelope is
// deliberately encoding-agnostic so a length-prefixed binary writer can
// replace the transport without changing semantics.
type Recorder struct {
	mu  sync.Mutex
	w   *bufio.Writer
	f   *os.File
	seq uint64
	h   hash.Hash64
}

// NewRecorder creates a recorder writing to path.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Recorder{
		w: bufio.NewWriterSize(f, 1<<20),
		f: f,
		h: fnv.New64a(),
	}, nil
}

// Record appends one event.
func (r *Recorder) Record(ev domain.MarketEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	re, err := FromEvent(r.seq, ev)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(re)
	if err != nil {
		return err
	}
	if _, err := r.h.Write(buf); err != nil {
		return err
	}
	if _, err := r.w.Write(buf); err != nil {
		return err
	}
	return r.w.WriteByte('\n')
}

// Digest returns the running checksum of recorded payloads.
func (r *Recorder) Digest() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.h.Sum64()
}

// Close flushes and closes the file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.w.Flush(); err != nil {
		return err
	}
	return r.f.Close()
}

// Reader streams recorded events in order.
type Reader struct {
	f  *os.File
	sc *bufio.Scanner
}

// NewReader opens a recording.
func NewReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	return &Reader{f: f, sc: sc}, nil
}

// Next returns the next normalized event, or io.EOF.
func (r *Reader) Next() (RecordedEvent, domain.MarketEvent, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return RecordedEvent{}, domain.MarketEvent{}, err
		}
		return RecordedEvent{}, domain.MarketEvent{}, io.EOF
	}
	var re RecordedEvent
	if err := json.Unmarshal(r.sc.Bytes(), &re); err != nil {
		return RecordedEvent{}, domain.MarketEvent{}, err
	}
	ev, err := re.ToEvent()
	return re, ev, err
}

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }
