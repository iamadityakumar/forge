package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"forge/internal/agent"
	"forge/internal/llm"
	"forge/internal/metrics"
	"forge/internal/ratelimit"
	"forge/internal/store"
	"forge/internal/tools"
	"forge/internal/worker"
)

func main() {
	// Read DATABASE_URL — same var as the orchestrator.
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Generate or read worker ID.
	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		hostname, _ := os.Hostname()
		workerID = fmt.Sprintf("%s-%s", hostname, shortRand())
	}

	// Open Postgres connection pool.
	pgStore, err := store.NewPgStore(dbURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pgStore.Close()
	slog.Info("worker connected to database", "worker_id", workerID)

	// Register initial heartbeat.
	hostname, _ := os.Hostname()
	if err := pgStore.Heartbeat(context.Background(), workerID, hostname); err != nil {
		slog.Warn("initial heartbeat failed", "error", err)
	}

	// Per-job lease duration (default 2m). The worker renews it every lease/3
	// while a job runs; an unresponsive worker's lease expires and the job is
	// reclaimed. WORKER_LEASE accepts Go duration strings ("90s", "2m").
	lease := 2 * time.Minute
	if s := os.Getenv("WORKER_LEASE"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			lease = d
		} else {
			slog.Warn("invalid WORKER_LEASE; using default", "value", s, "default", lease)
		}
	}

	// Number of jobs this worker runs at once (U6 bounded concurrency). Default
	// 1 keeps a worker serial (the safe baseline); set higher to run that many
	// jobs concurrently, each behind its own lease + fenced step loop.
	concurrency := 1
	if s := os.Getenv("WORKER_CONCURRENCY"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			concurrency = n
		} else {
			slog.Warn("invalid WORKER_CONCURRENCY; using default", "value", s, "default", concurrency)
		}
	}

	// Week 4: build the LLM backend (ollama | groq | fake) from env, the tool
	// registry, and register the crash-recoverable cp_solve agent handler. Any
	// other task_type falls back to the built-in segment handler (backwards
	// compatible with all Week-3 jobs and demos).
	rawBackend, err := llm.NewFromEnv()
	if err != nil {
		log.Fatalf("failed to configure LLM backend: %v", err)
	}
	slog.Info("LLM backend selected", "backend", rawBackend.Name())

	// Week 5: Prometheus seed metrics endpoint.
	// Each worker serves its own /metrics on METRICS_PORT (default 9091). After a
	// burst run the counters are non-zero, proving backpressure is observable.
	metricsStore := metrics.New()

	// Week 5: Rate Limiting configuration.
	//
	// RATE_LIMIT_BACKEND selects the limiter implementation:
	//   "off"       — no limiter (Week-4 behavior, safe default when TPM/RPM unset)
	//   "memory"    — per-process MemoryBucket (single-worker budget)
	//   "upstretch" — Upstash Redis distributed bucket (shared fleet budget)
	//                 via REST; requires UPSTASH_URL + UPSTASH_TOKEN.
	// RATE_LIMIT_TPM / RATE_LIMIT_RPM are the token and request budgets per
	// minute (0 = disabled for that window). The distributed bucket is composed
	// with the local RPM bucket via MultiLimiter when both are set.
	var backend llm.LLMBackend = rawBackend
	rateLimitBackend := os.Getenv("RATE_LIMIT_BACKEND")
	if rateLimitBackend == "" {
		rateLimitBackend = "memory"
	}
	rateLimitTPM := 0
	rateLimitRPM := 0

	if s := os.Getenv("RATE_LIMIT_TPM"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			rateLimitTPM = v
		}
	}
	if s := os.Getenv("RATE_LIMIT_RPM"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			rateLimitRPM = v
		}
	}

	if rateLimitBackend != "off" && (rateLimitTPM > 0 || rateLimitRPM > 0) {
		var lim ratelimit.Limiter
		clock := ratelimit.SystemClock{}

		switch rateLimitBackend {
		case "upstretch":
			// Distributed token bucket: all workers share one Redis counter per
			// fixed window, so the fleet budget can't overshoot even when 4
			// workers reserve concurrently (atomic Lua EVAL check-then-increment).
			upURL := os.Getenv("UPSTASH_URL")
			upToken := os.Getenv("UPSTASH_TOKEN")
			if upURL == "" || upToken == "" {
				log.Fatal("RATE_LIMIT_BACKEND=upstretch requires UPSTASH_URL and UPSTASH_TOKEN")
			}
			tpmBucket := ratelimit.NewUpstashBucket(upURL, upToken, "tpm", rateLimitTPM, 60, clock)
			if rateLimitRPM > 0 {
				rpmBucket := ratelimit.NewMemoryBucket(rateLimitRPM, time.Minute, clock)
				lim = ratelimit.NewMultiLimiter(tpmBucket, rpmBucket)
				slog.Info("distributed rate limiting enabled",
					"backend", "upstretch", "tpm", rateLimitTPM, "rpm", rateLimitRPM)
			} else {
				lim = tpmBucket
				slog.Info("distributed rate limiting enabled",
					"backend", "upstretch", "tpm", rateLimitTPM)
			}
		default: // "memory"
			if rateLimitTPM > 0 && rateLimitRPM > 0 {
				tpmBucket := ratelimit.NewMemoryBucket(rateLimitTPM, time.Minute, clock)
				rpmBucket := ratelimit.NewMemoryBucket(rateLimitRPM, time.Minute, clock)
				lim = ratelimit.NewMultiLimiter(tpmBucket, rpmBucket)
				slog.Info("rate limiting enabled", "tpm", rateLimitTPM, "rpm", rateLimitRPM)
			} else if rateLimitTPM > 0 {
				lim = ratelimit.NewMemoryBucket(rateLimitTPM, time.Minute, clock)
				slog.Info("rate limiting enabled", "tpm", rateLimitTPM)
			} else {
				lim = ratelimit.NewMemoryBucket(rateLimitRPM, time.Minute, clock)
				slog.Info("rate limiting enabled", "rpm", rateLimitRPM)
			}
		}

		backend = llm.NewRateLimitedBackend(rawBackend, lim, metricsStore)
	}

	reg := tools.NewRegistry()
	if err := reg.Register(tools.NewSearchKBTool()); err != nil {
		log.Fatalf("failed to register search_kb tool: %v", err)
	}
	if err := reg.Register(tools.NewRunTestsTool()); err != nil {
		log.Fatalf("failed to register run_tests tool: %v", err)
	}

	agentHandler := agent.New(backend, reg)
	worker.RegisterHandler("cp_solve", agentHandler)
	slog.Info("registered agent handler", "task_type", "cp_solve", "max_steps", agentHandler.MaxSteps())

	// Week 5: Prometheus seed metrics endpoint on METRICS_PORT (default 9091).
	metricsPort := 9091
	if s := os.Getenv("METRICS_PORT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			metricsPort = n
		} else {
			slog.Warn("invalid METRICS_PORT; using default", "value", s, "default", metricsPort)
		}
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metricsStore)
	go func() {
		slog.Info("metrics endpoint listening", "addr", fmt.Sprintf(":%d", metricsPort))
		if err := http.ListenAndServe(fmt.Sprintf(":%d", metricsPort), metricsMux); err != nil && err != http.ErrServerClosed {
			slog.Warn("metrics server stopped", "error", err)
		}
	}()

	// Set up graceful shutdown via SIGINT/SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run the polling loop — blocks until ctx is cancelled.
	if err := worker.Run(ctx, pgStore, workerID, lease, concurrency); err != nil && err != context.Canceled {
		log.Fatalf("worker stopped: %v", err)
	}
	slog.Info("worker shut down cleanly", "worker_id", workerID)
}

// shortRand returns an 8-char hex string for worker ID uniqueness.
func shortRand() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}