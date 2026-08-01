package ratelimit

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrReservationTooLarge is returned by Reserve when the requested amount
	// exceeds the total capacity of the rate limiter.
	ErrReservationTooLarge = errors.New("requested capacity exceeds limiter max tokens")
)

// Reservation represents an authorized or pending token reservation.
type Reservation struct {
	// Granted is true if tokens were reserved immediately.
	Granted bool

	// WaitDuration indicates how long the caller must wait before tokens become available.
	WaitDuration time.Duration

	// TokenCount is the number of tokens requested/reserved.
	TokenCount int

	// Settle adjusts the reservation after actual usage is known.
	// If actual < estimated, the unused difference (estimated - actual) is refunded immediately.
	// If actual > estimated, the deficit (actual - estimated) is debited against future capacity.
	Settle func(actual int)
}

// Limiter defines the core interface for rate limiting token spend and request counts.
type Limiter interface {
	// Reserve attempts to reserve n tokens. It returns a Reservation.
	// If Granted is false, WaitDuration specifies how long until the reservation can be fulfilled.
	Reserve(ctx context.Context, n int) (Reservation, error)

	// Wait blocks until n tokens are available or ctx is cancelled.
	Wait(ctx context.Context, n int) error
}