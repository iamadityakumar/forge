package llm

import (
	"context"
	"errors"
	"net"
)

// ClassifyError maps any error to one of the bounded categories:
// "timeout", "rate_limit", "auth", "provider", "network", "internal".
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429:
			return "rate_limit"
		case httpErr.StatusCode == 401 || httpErr.StatusCode == 403:
			return "auth"
		case httpErr.StatusCode >= 500:
			return "provider"
		default:
			return "internal"
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}

	return "internal"
}
