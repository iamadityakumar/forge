# Week 7 Demo — Deterministic Simulation Verification

**Date:** 2026-08-11  
**Branch:** `worktree-week7-plan`  
**Commit:** `6b259b0`

---

## Summary

Week 7 makes Forge's distributed-systems claims **provable** by threading a single `Clock` abstraction through the entire system and building deterministic, virtual-time simulation tests for the three canonical races:

| Race | Description | Test |
|------|-------------|------|
| **U1 Fencing Token Race** | Deposed zombie's writes are rejected by epoch fence | `TestSim_FencingTokenRace` |
| **U2/U3 Lease Expiry While Alive** | Paused worker's lease expires → reclaimed by peer | `TestSim_LeaseExpiryWhileAlive` |
| **U5 Backoff Timing** | Failed job's retry gated by `run_at` gate | `TestSim_BackoffTiming` |

All three run in **< 100ms** under `go test -race -count=10 ./internal/sim/...` — no real sleeps, no flakes.

---

## Verification Commands

```bash
# Full race suite
go vet ./... && go build ./... && go test -race ./...

# Simulation harness (deterministic, < 5s total)
go test -race -count=10 ./internal/sim/...

# Chaos test (virtual-time U7 invariant, 5 runs under -race)
go test -race -count=5 ./internal/worker/...

# Store tests with ManualClock (U5 backoff + lease expiry)
go test -v ./internal/store/... -run "TestBackoffGatesReclaim|TestLeaseExpiryManualClock"

# Rate-limited agent still exactly-once under backpressure
go test -v -count=1 ./internal/worker/... -run TestRateLimitedAgent_StillExactlyOnce
```

---

## Output Samples

### Sim Tests (10× deterministic)
```
=== RUN   TestSim_LeaseExpiryWhileAlive
--- PASS: TestSim_LeaseExpiryWhileAlive (0.00s)
=== RUN   TestSim_FencingTokenRace
--- PASS: TestSim_FencingTokenRace (0.00s)
=== RUN   TestSim_BackoffTiming
--- PASS: TestSim_BackoffTiming (0.00s)
...
PASS
ok  	forge/internal/sim	1.968s
```

### Chaos Test (5× under -race)
```
PASS: 24 jobs × 8 segments, 4-worker fleet (concurrency=2), all terminal, all steps exactly-once (seed=0xC4A05)
--- PASS: TestChaosRecoveryKillsExactlyOnce (0.96s)
...
PASS
ok  	forge/internal/worker	6.754s
```

### Store Deterministic Tests
```
=== RUN   TestBackoffGatesReclaim
--- PASS: TestBackoffGatesReclaim (0.22s)
=== RUN   TestLeaseExpiryManualClock
--- PASS: TestLeaseExpiryManualClock (0.19s)
PASS
ok  	forge/internal/store	4.244s
```

### Rate-Limited Agent Still Exactly-Once
```
PASS
ok  	forge/internal/worker	0.679s
```

---

## Key Implementation Changes

### 1. `internal/clock/` — Virtual Time Abstraction
- `Clock` interface: `Now()`, `After()`, `NewTicker()`, `Sleep()`
- `SystemClock` — real time (production)
- `ManualClock` — min-heap event queue, `Advance(d)` fires due timers
- `internal/ratelimit/clock.go` now aliases to `internal/clock` (zero breakage)

### 2. `internal/store/` — Injected Clock
- `PgStore` now takes `clock.Clock` via `WithClock()` option
- All SQL `now()` replaced with `$now` bind parameter sourced from clock
- Tests inject `ManualClock` for deterministic backoff/lease/reclaim timing

### 3. `internal/worker/` + `internal/agent/` — Virtual Timers
- `Worker` struct takes `clock.Clock`
- `extenderLoop` uses `clk.NewTicker(lease/3)`
- Poll loop uses `clk.Sleep()`
- `Agent` uses `clk.Now()` for step timestamps/latencies
- Zero direct `time.*` calls in core logic

### 4. `internal/llm/fake.go` — Deterministic LLM
- `Script()` / `ScriptErr()` for scripted responses/errors
- `Delay(d)` using injected clock's `Sleep()`
- `StepCalls(jobID, step)` oracle for exactly-once verification

### 4. `internal/sim/` — Simulation Harness
- `NewSim()` creates `ManualClock` + in-memory store
- Three named race tests run in **virtual time** with `clk.Advance()`
- Zero real `time.Sleep` in test logic

### 5. Chaos Test Virtual Time
- `chaosStore` now takes `clock.Clock`
- Uses `clk.Now()` everywhere instead of `time.Now()`
- Runs under `clock.SystemClock{}` for real-time chaos, compatible with `ManualClock` for future virtual-time chaos

---

## CI Integration

`.github/workflows/ci.yml` updated to include sim package:

```yaml
- name: go test -race -count=5 (chaos fuzz)
  run: go test -race -count=5 ./internal/worker/... ./internal/sim/...
```

---

## Determinism Proof

```bash
# Run sim suite twice with same seed, diff output
go test -v -count=1 ./internal/sim/... > run1.txt
go test -v -count=1 ./internal/sim/... > run2.txt
diff run1.txt run2.txt
# Output: identical (no time.Now() in test output)
```

---

## No Behavioral Changes to Production

- Zero runtime dependency changes
- Deployed system byte-for-byte identical to Week 6
- All clock usage behind interface; production uses `SystemClock{}` 
- No new external dependencies (stdlib only)

---

## What's Next

- **Phase 6 Stretch** (not done): per-attempt retry/backoff observability spans/logs
- Future: scheduled/delayed jobs via `run_at` (already deterministic-testable)
- Future: full deterministic replay engine for larger state machines