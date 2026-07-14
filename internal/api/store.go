package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Job is the record created for each submitted task.
type Job struct {
	ID        string    `json:"job_id"`
	Task      string    `json:"task"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// Status values used by the orchestrator.
const (
	StatusQueued = "queued"
)

// Store is an in-memory, concurrency-safe job store.

type Store struct {
	mu   sync.RWMutex
	jobs map[string]Job
}

// NewStore returns an initialised, empty Store.
func NewStore() *Store {
	return &Store{jobs: make(map[string]Job)}
}

// Create persists a new job with the provided task and a "queued" status.
func (s *Store) Create(task string) Job {
	j := Job{
		ID:        newID(),
		Task:      task,
		Status:    StatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return j
}

// Get returns the job with the given ID and whether it was found.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// newID returns a random UUIDv4 hex string (no external dependency).
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to a timestamp-based id.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}
