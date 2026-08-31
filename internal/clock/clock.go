// Package clock abstracts time so replay runs deterministically.
package clock

import "time"

// Clock supplies the current time.
type Clock interface {
	Now() time.Time
}

// RealClock is wall-clock time.
type RealClock struct{}

// Now returns time.Now().
func (RealClock) Now() time.Time { return time.Now() }

// ManualClock is advanced explicitly by tests and the replayer.
type ManualClock struct {
	now time.Time
}

// NewManualClock starts at t.
func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

// Now returns the current manual time.
func (m *ManualClock) Now() time.Time { return m.now }

// Advance moves the clock forward by d.
func (m *ManualClock) Advance(d time.Duration) { m.now = m.now.Add(d) }

// Set jumps the clock to t.
func (m *ManualClock) Set(t time.Time) { m.now = t }
