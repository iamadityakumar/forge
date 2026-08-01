package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpstashBucket_ReserveAndSettle(t *testing.T) {
	var receivedBody []any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode error: %v", err)
		}
		// Return Lua result [1, 100] (Granted = 1, newVal = 100)
		json.NewEncoder(w).Encode(map[string]any{
			"result": []any{1, 100},
		})
	}))
	defer ts.Close()

	clock := NewManualClock(time.Unix(1700000000, 0))
	bucket := NewUpstashBucket(ts.URL, "fake-token", "tpm", 1000, 60, clock)

	res, err := bucket.Reserve(context.Background(), 100)
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if !res.Granted {
		t.Errorf("expected Granted=true")
	}

	// Settle refund
	res.Settle(80)
}
