package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"

	"forge/internal/api"
	"forge/internal/store"
)

func main() {
	// Read DATABASE_URL â€” required for Postgres-backed persistence.
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Open Postgres connection pool.
	pgStore, err := store.NewPgStore(dbURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pgStore.Close()
	slog.Info("connected to database")

	maxPendingJobs := 0
	if envMax := os.Getenv("MAX_PENDING_JOBS"); envMax != "" {
		if v, err := strconv.Atoi(envMax); err == nil && v > 0 {
			maxPendingJobs = v
		}
	}
	if maxPendingJobs > 0 {
		slog.Info("admission control enabled", "max_pending_jobs", maxPendingJobs)
	}

	handler := api.NewHandler(pgStore, maxPendingJobs)

	r := chi.NewRouter()
	api.RegisterRoutes(r, handler)

	addr := ":8080"
	slog.Info("orchestrator listening", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
