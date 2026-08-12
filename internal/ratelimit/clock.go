package ratelimit

import (
	"time"

	"forge/internal/clock"
)

// Clock is an alias for the shared clock abstraction.
type Clock = clock.Clock

// SystemClock is an alias for the system clock implementation.
type SystemClock = clock.SystemClock

// ManualClock is an alias for the manual clock implementation.
type ManualClock = clock.ManualClock

// NewManualClock creates a ManualClock via the shared package.
func NewManualClock(t time.Time) *ManualClock {
	return clock.NewManualClock(t)
}
