package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UpstashBucket implements a distributed rate limiter using Upstash Redis over REST API.
type UpstashBucket struct {
	url        string
	token      string
	name       string
	maxTokens  int
	windowSec  int
	httpClient *http.Client
	clock      Clock
}

// NewUpstashBucket creates an Upstash Redis distributed rate bucket.
func NewUpstashBucket(url, token, name string, maxTokens int, windowSec int, clock Clock) *UpstashBucket {
	if clock == nil {
		clock = SystemClock{}
	}
	url = strings.TrimSuffix(url, "/")
	return &UpstashBucket{
		url:        url,
		token:      token,
		name:       name,
		maxTokens:  maxTokens,
		windowSec:  windowSec,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		clock:      clock,
	}
}

// evalLuaScript is the Lua script sent to Upstash via EVAL. It atomically
// checks capacity, increments the counter, and sets a TTL on first write.
// It returns {1, newCount} on grant or {0, currentCount} on rejection.
var evalLuaScript = strings.Join([]string{
	"local current = tonumber(redis.call('GET', KEYS[1]) or '0')",
	"local requested = tonumber(ARGV[1])",
	"local maxCap = tonumber(ARGV[2])",
	"local ttl = tonumber(ARGV[3])",
	"if current + requested > maxCap then",
	"    return {0, current}",
	"end",
	"local newVal = redis.call('INCRBY', KEYS[1], requested)",
	"if current == 0 then",
	"    redis.call('EXPIRE', KEYS[1], ttl)",
	"end",
	"return {1, newVal}",
}, "\n")

type upstashCommandResp struct {
	Result []any  `json:"result"`
	Error  string `json:"error,omitempty"`
}

func (u *UpstashBucket) key(now time.Time) string {
	windowID := now.Unix() / int64(u.windowSec)
	return fmt.Sprintf("forge:limiter:%s:%d", u.name, windowID)
}

func (u *UpstashBucket) Reserve(ctx context.Context, n int) (Reservation, error) {
	if n > u.maxTokens {
		return Reservation{}, ErrReservationTooLarge
	}

	now := u.clock.Now()
	key := u.key(now)

	// Command: EVAL script 1 key n maxTokens windowSec
	body := []any{evalLuaScript, "1", key, strconv.Itoa(n), strconv.Itoa(u.maxTokens), strconv.Itoa(u.windowSec)}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return Reservation{}, fmt.Errorf("marshal upstash req: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(jsonBytes))
	if err != nil {
		return Reservation{}, fmt.Errorf("create upstash req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return Reservation{}, fmt.Errorf("upstash request failed: %w", err)
	}
	defer resp.Body.Close()

	var upResp upstashCommandResp
	if err := json.NewDecoder(resp.Body).Decode(&upResp); err != nil {
		return Reservation{}, fmt.Errorf("decode upstash resp: %w", err)
	}
	if upResp.Error != "" {
		return Reservation{}, fmt.Errorf("upstash redis error: %s", upResp.Error)
	}

	if len(upResp.Result) < 2 {
		return Reservation{}, errors.New("invalid upstash eval response")
	}

	grantedVal, ok1 := upResp.Result[0].(float64)
	if !ok1 {
		return Reservation{}, errors.New("invalid granted flag from upstash")
	}

	if grantedVal == 1 {
		return Reservation{
			Granted:    true,
			TokenCount: n,
			Settle:     u.makeSettle(key, n),
		}, nil
	}

	// Calculate wait duration until the current window ends.
	remSec := int64(u.windowSec) - (now.Unix() % int64(u.windowSec))
	waitDur := time.Duration(remSec) * time.Second
	if waitDur < 100*time.Millisecond {
		waitDur = 100 * time.Millisecond
	}

	return Reservation{
		Granted:      false,
		WaitDuration: waitDur,
		TokenCount:   n,
		Settle:       func(actual int) {},
	}, nil
}

func (u *UpstashBucket) Wait(ctx context.Context, n int) error {
	for {
		res, err := u.Reserve(ctx, n)
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

func (u *UpstashBucket) makeSettle(key string, estimated int) func(actual int) {
	return func(actual int) {
		diff := estimated - actual
		if diff == 0 {
			return
		}
		var cmd []string
		if diff > 0 {
			cmd = []string{"DECRBY", key, strconv.Itoa(diff)}
		} else {
			cmd = []string{"INCRBY", key, strconv.Itoa(-diff)}
		}
		jsonBytes, _ := json.Marshal(cmd)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(jsonBytes))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+u.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := u.httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}
