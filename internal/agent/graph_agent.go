package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"forge/internal/clock"
	"forge/internal/metrics"
	"forge/internal/store"
	"forge/internal/trace"
)

// StepTypeGraphNode is the job_steps.step_type recorded for each LangGraph
// node that completes. One row per node_done event, mirroring how the native
// agent records "plan" / "tool_call" rows — so the dashboard timeline and
// crash-recovery resume logic treat a graph_solve job like any other.
const StepTypeGraphNode = "graph_node"

// GraphAgent runs the Phase-1 LangGraph workflow through the FastAPI sidecar
// (Phase 2) and checkpoints each completed node into job_steps under the
// caller's fencing epoch. It implements worker.Handler for the "graph_solve"
// task type.
//
// The division of labor mirrors the native Agent:
//   - the Python sidecar owns intra-graph state (plan/kb/solution/retries);
//   - Forge owns the durable, fenced step timeline in Postgres.
//
// Every node_done event becomes one RecordStep call validated against the
// lease epoch, so a deposed worker's late writes are rejected with
// store.ErrFenced exactly as they are for cp_solve. The worker loop
// (internal/worker/loop.go) calls CompleteJob/FailJob based on this method's
// return, so Run must NOT complete the job itself — returning nil signals
// success and the loop does the fenced completion.
type GraphAgent struct {
	sidecarURL string
	httpClient *http.Client
	maxSteps   int
	clk        clock.Clock
	metrics    *metrics.Metrics
	tracer     *trace.Tracer
}

// GraphAgentConfig holds construction parameters for a GraphAgent.
type GraphAgentConfig struct {
	// SidecarURL is the base URL of the FastAPI sidecar, e.g.
	// "http://localhost:8099". A trailing slash is trimmed.
	SidecarURL string
	// MaxSteps caps the number of checkpointed node events before the agent
	// aborts, an independent guard above the graph's own recursion_limit.
	// Defaults to 24 (12 recursion_limit × 2 events/node headroom).
	MaxSteps int
	// Clock is injected for deterministic tests; defaults to SystemClock.
	Clock clock.Clock
	// Metrics is optional; when set, StepsTotal{graph_node} is incremented.
	Metrics *metrics.Metrics
	// Timeout bounds a single /run stream. Defaults to 5 minutes.
	Timeout time.Duration
}

// NewGraphAgent constructs a GraphAgent from config, applying defaults.
func NewGraphAgent(cfg GraphAgentConfig) *GraphAgent {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 24
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.SystemClock{}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	return &GraphAgent{
		sidecarURL: strings.TrimRight(cfg.SidecarURL, "/"),
		httpClient: &http.Client{Timeout: cfg.Timeout},
		maxSteps:   cfg.MaxSteps,
		clk:        cfg.Clock,
		metrics:    cfg.Metrics,
		tracer:     trace.NewTracer("graph_agent"),
	}
}

// Run streams the graph for job and checkpoints each node_done event.
// It implements worker.Handler.Run. On success it returns nil and leaves
// CompleteJob to the worker loop; on any error (including a fenced write) it
// returns that error so the loop fails/abandons the job.
func (g *GraphAgent) Run(ctx context.Context, s store.JobStore, job *store.Job, epoch int, workerID string) error {
	ctx, span := g.tracer.StartSpan(ctx, "graph_solve",
		trace.Attribute{Key: "job_id", Value: job.ID.String()},
		trace.Attribute{Key: "worker_id", Value: workerID},
	)
	defer span.End()

	task := parseGraphTask(job.Payload)

	// Resume support: the graph is not itself resumable across a crash yet
	// (Phase 4), but we honor the existing timeline so re-runs append rather
	// than collide on step_number.
	start, err := s.LastCompletedStep(ctx, job.ID)
	if err != nil {
		span.SetStatus("error", err)
		return fmt.Errorf("graph agent last step: %w", err)
	}
	stepNumber := start

	resp, err := g.postRun(ctx, task)
	if err != nil {
		span.SetStatus("error", err)
		return fmt.Errorf("graph sidecar run: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		span.SetStatus("error", err)
		return err
	}

	var sawDone bool
	scanner := bufio.NewScanner(resp.Body)
	// Node data can be large (solutions, observations); allow up to 1 MiB/line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var ev graphEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			span.SetStatus("error", err)
			return fmt.Errorf("decode graph event %q: %w", line, err)
		}

		switch ev.Event {
		case "node_start":
			// Informational only; the durable checkpoint is node_done.
			continue

		case "node_done":
			stepNumber++
			if stepNumber-start > g.maxSteps {
				err := fmt.Errorf("graph exceeded max steps limit of %d", g.maxSteps)
				span.SetStatus("error", err)
				return err
			}
			if err := g.recordNode(ctx, s, job.ID, epoch, workerID, ev, stepNumber); err != nil {
				// Fenced writes propagate unwrapped so worker.loop and the
				// resume test can errors.Is(err, store.ErrFenced).
				span.SetStatus("error", err)
				return err
			}
			if g.metrics != nil {
				g.metrics.StepsTotal.WithLabelValues(StepTypeGraphNode).Inc()
			}

		case "done":
			sawDone = true
			span.SetAttribute("tests_passed", ev.TestsPassed)
			span.SetAttribute("attempts", ev.Attempts)
			// A graph that exhausts its attempts without passing is a failed
			// solve — surface it as an error so the worker loop fails the job
			// (and applies retry/DLQ policy) rather than marking it complete.
			if !ev.TestsPassed {
				err := fmt.Errorf("graph finished without passing tests after %d attempt(s)", ev.Attempts)
				span.SetStatus("error", err)
				return err
			}

		case "error":
			err := fmt.Errorf("graph error: %s", ev.Message)
			span.SetStatus("error", err)
			return err

		default:
			err := fmt.Errorf("unknown graph event %q", ev.Event)
			span.SetStatus("error", err)
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		span.SetStatus("error", err)
		return fmt.Errorf("read graph stream: %w", err)
	}

	if !sawDone {
		err := fmt.Errorf("graph stream ended without a done event")
		span.SetStatus("error", err)
		return err
	}

	span.SetStatus("ok", nil)
	return nil
}

// postRun issues the streaming POST /run request to the sidecar. The caller
// owns resp.Body and must Close it after consuming the stream.
func (g *GraphAgent) postRun(ctx context.Context, task string) (*http.Response, error) {
	body, err := json.Marshal(runRequest{Task: task, LLM: "fake"})
	if err != nil {
		return nil, fmt.Errorf("marshal run request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.sidecarURL+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build run request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	// Week 6 hook: propagate the W3C traceparent so sidecar spans join the
	// job's trace. No-op on the no-op tracer.
	trace.InjectW3C(ctx, req)

	return g.httpClient.Do(req)
}

// recordNode checkpoints one node_done event as a fenced job step.
func (g *GraphAgent) recordNode(ctx context.Context, s store.JobStore, jobID uuid.UUID, epoch int, workerID string, ev graphEvent, stepNumber int) error {
	out := ev.Data
	if len(out) == 0 {
		out = json.RawMessage("{}")
	}
	input, _ := json.Marshal(map[string]string{"node": ev.Node})

	_, err := s.RecordStep(ctx, jobID, epoch, store.JobStep{
		JobID:      jobID,
		StepNumber: stepNumber,
		StepType:   StepTypeGraphNode,
		Input:      json.RawMessage(input),
		Output:     json.RawMessage(out),
		WorkerID:   workerID,
	})
	return err
}

// runRequest is the POST /run body understood by the sidecar (server.py).
type runRequest struct {
	Task         string `json:"task"`
	LLM          string `json:"llm,omitempty"`
	Model        string `json:"model,omitempty"`
	TestOutcomes string `json:"test_outcomes,omitempty"`
}

// graphEvent is one NDJSON line from the sidecar stream. Fields are a superset
// across event types; only those relevant to ev.Event are populated.
type graphEvent struct {
	Event       string          `json:"event"`
	Node        string          `json:"node,omitempty"`
	Step        int             `json:"step,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	TestsPassed bool            `json:"tests_passed,omitempty"`
	Attempts    int             `json:"attempts,omitempty"`
	Solution    string          `json:"solution,omitempty"`
	Done        bool            `json:"done,omitempty"`
	Message     string          `json:"message,omitempty"`
}

// MaxSteps returns the configured step guard.
func (g *GraphAgent) MaxSteps() int { return g.maxSteps }

// HealthCheck verifies the sidecar's GET /health returns 200 within 5s.
func (g *GraphAgent) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.sidecarURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// String is a compact description for logging.
func (g *GraphAgent) String() string {
	return fmt.Sprintf("GraphAgent(sidecar=%s, max_steps=%d)", g.sidecarURL, g.maxSteps)
}

// parseGraphTask extracts the task string from job payload.
// Supports both {"prompt": "..."} and raw string payloads, mirroring jobPrompt() in agent.go.
func parseGraphTask(payload json.RawMessage) string {
	var p struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(payload, &p); err == nil {
		return p.Prompt
	}
	return string(payload)
}