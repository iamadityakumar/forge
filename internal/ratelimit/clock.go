package ratelimit

import "time"

// Clock abstracts time for deterministic testing of rate limiters.
type Clock interface {
	Now() time.Time
}

// SystemClock delegates to time.Now().
type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now()
}

// ManualClock is a thread-safe mock clock for unit tests.
type ManualClock struct {
	now time.Time
}

func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t}
}

func (m *ManualClock) Now() time.Time {
	return m.now
}

func (m *ManualClock) Advance(d time.Duration) {
	m.now = m.now.Add(d)
}