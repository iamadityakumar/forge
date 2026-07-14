package main

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"forge/internal/api"
)

func main() {
	store := api.NewStore()
	handler := api.NewHandler(store)

	r := chi.NewRouter()
	api.RegisterRoutes(r, handler)

	addr := ":8080"
	log.Printf("orchestrator listening on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
