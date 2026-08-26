package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"forge/internal/agent"
	"forge/internal/api"
	"forge/internal/llm"
	"forge/internal/metrics"
	"forge/internal/store"
	"forge/internal/tools"
	"forge/internal/worker"
)

func main() {
	// 1. Shared In-Memory Store & Metrics
	jobStore := store.NewMemStore()
	m := metrics.New("monolith")

	// 2. Start Orchestrator API
	r := chi.NewRouter()
	h := api.NewHandler(jobStore)
	api.RegisterRoutes(r, h)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	go func() {
		slog.Info("Starting monolith orchestrator API on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("orchestrator server error", "error", err)
		}
	}()

	// 3. Register tools & agent
	rawBackend, err := llm.NewFromEnv()
	if err != nil {
		slog.Error("Failed to init llm backend", "error", err)
		os.Exit(1)
	}

	// For monolith, don't use rate limiter (or simple one)
	backend := llm.NewRateLimitedBackend(rawBackend, nil, m)
	reg := tools.NewRegistry()
	reg.Register(tools.NewSearchKBTool())
	reg.Register(tools.NewRunTestsTool())
	ag := agent.New(backend, reg, m)
	worker.RegisterHandler("cp_solve", ag)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		slog.Info("Starting monolith worker loop")
		worker.Run(ctx, jobStore, "local-monolith", 2*time.Minute, 1, m)
	}()

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	cancel()
	srv.Shutdown(context.Background())
}
