package ratelimit

import (
	"context"
	"time"
)

// MultiLimiter composes multiple Limiters (e.g. TPM and RPM buckets) into a single logical limiter.
// All underlying limiters must authorize a reservation before it is granted.
type MultiLimiter struct {
	tpm Limiter
	rpm Limiter
}

// NewMultiLimiter returns a composite Limiter wrapping TPM and RPM limiters.
func NewMultiLimiter(tpm Limiter, rpm Limiter) *MultiLimiter {
	return &MultiLimiter{
		tpm: tpm,
		rpm: rpm,
	}
}

// Reserve attempts to reserve estimatedTokens from the TPM limiter and 1 request from the RPM limiter.
func (m *MultiLimiter) Reserve(ctx context.Context, estimatedTokens int) (Reservation, error) {
	tpmRes, err := m.tpm.Reserve(ctx, estimatedTokens)
	if err != nil {
		return Reservation{}, err
	}

	rpmRes, err := m.rpm.Reserve(ctx, 1)
	if err != nil {
		// Roll back TPM reservation if RPM errored out
		if tpmRes.Granted && tpmRes.Settle != nil {
			tpmRes.Settle(0)
		}
		return Reservation{}, err
	}

	if tpmRes.Granted && rpmRes.Granted {
		return Reservation{
			Granted:    true,
			TokenCount: estimatedTokens,
			Settle: func(actual int) {
				tpmRes.Settle(actual)
				rpmRes.Settle(1)
			},
		}, nil
	}

	// Calculate maximum wait duration between the two limiters
	waitDur := tpmRes.WaitDuration
	if !rpmRes.Granted && rpmRes.WaitDuration > waitDur {
		waitDur = rpmRes.WaitDuration
	}

	// Roll back any partially granted reservation
	if tpmRes.Granted && tpmRes.Settle != nil {
		tpmRes.Settle(0) // refund tokens
	}
	if rpmRes.Granted && rpmRes.Settle != nil {
		rpmRes.Settle(0) // refund request slot
	}

	return Reservation{
		Granted:      false,
		WaitDuration: waitDur,
		TokenCount:   estimatedTokens,
		Settle:       func(actual int) {},
	}, nil
}

// Wait blocks until both TPM and RPM limits are satisfied.
func (m *MultiLimiter) Wait(ctx context.Context, estimatedTokens int) error {
	for {
		res, err := m.Reserve(ctx, estimatedTokens)
		if err != nil {
			return err
		}
		if res.Granted {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(res.WaitDuration):
		}
	}
}