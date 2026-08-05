package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"forge/internal/api"
	forgelog "forge/internal/log"
	"forge/internal/metrics"
	"forge/internal/store"
	"forge/internal/trace"
)

func main() {
	workerID := "orchestrator"
	if _, err := forgelog.Setup(workerID); err != nil {
		log.Fatalf("failed to setup logging: %v", err)
	}

	tp, err := trace.Setup(workerID)
	if err != nil {
		slog.Warn("failed to setup tracing", "error", err)
	} else {
		defer tp.Shutdown(context.Background())
	}

	dbURL := os.Getenv("DATABASE_URL")
	var jobStore store.JobStore
	if dbURL == "" || dbURL == "memory" || dbURL == "mock" {
		slog.Info("DATABASE_URL not set or set to memory; initializing in-memory demo store")
		jobStore = store.NewMemStore()
	} else {
		pgStore, err := store.NewPgStore(dbURL)
		if err != nil {
			slog.Warn("failed to connect to postgresql database, falling back to in-memory demo store", "error", err)
			jobStore = store.NewMemStore()
		} else {
			defer pgStore.Close()
			jobStore = pgStore
			slog.Info("connected to database")
		}
	}

	maxPendingJobs := 0
	if envMax := os.Getenv("MAX_PENDING_JOBS"); envMax != "" {
		if v, err := strconv.Atoi(envMax); err == nil && v > 0 {
			maxPendingJobs = v
		}
	}
	if maxPendingJobs > 0 {
		slog.Info("admission control enabled", "max_pending_jobs", maxPendingJobs)
	}

	apiMetrics := metrics.New("forge_api")
	workerURLs := parseWorkerURLs(os.Getenv("WORKER_METRICS_URLS"))

	handler := api.NewHandler(jobStore, maxPendingJobs).
		WithMetrics(apiMetrics).
		WithWorkerURLs(workerURLs)

	r := chi.NewRouter()
	api.RegisterRoutes(r, handler)

	addr := ":8080"
	slog.Info("orchestrator listening", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func parseWorkerURLs(envVal string) map[string]string {
	res := make(map[string]string)
	if envVal == "" {
		res["worker-1"] = "http://localhost:9091/metrics"
		return res
	}
	parts := strings.Split(envVal, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if idx := strings.Index(p, "="); idx != -1 {
			name := strings.TrimSpace(p[:idx])
			url := strings.TrimSpace(p[idx+1:])
			if !strings.HasSuffix(url, "/metrics") && !strings.Contains(url, "/metrics?") {
				url = strings.TrimRight(url, "/") + "/metrics"
			}
			res[name] = url
		} else {
			name := p
			if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
				trim := strings.TrimPrefix(strings.TrimPrefix(p, "http://"), "https://")
				host := strings.Split(trim, ":")[0]
				host = strings.Split(host, "/")[0]
				if host != "" {
					name = host
				}
			}
			url := p
			if !strings.HasSuffix(url, "/metrics") {
				url = strings.TrimRight(url, "/") + "/metrics"
			}
			res[name] = url
		}
	}
	return res
}