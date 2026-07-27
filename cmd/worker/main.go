package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"forge/internal/store"
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
