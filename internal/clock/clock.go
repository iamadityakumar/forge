package clock

import (
	"time"
)

// Ticker abstracts time.Ticker so ManualClock can own the channel.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock abstracts time for deterministic testing. It provides a full virtual
// timer facility (After, NewTicker, Sleep) so the *process* — not just
// timestamps — becomes replayable.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	Sleep(d time.Duration)
}

// SystemClock is real time.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (SystemClock) NewTicker(d time.Duration) Ticker       { return &realTicker{t: time.NewTicker(d)} }
func (SystemClock) Sleep(d time.Duration)                  { time.Sleep(d) }

type realTicker struct {
	t *time.Ticker
}

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }
