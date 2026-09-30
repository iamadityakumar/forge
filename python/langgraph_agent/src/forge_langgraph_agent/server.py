"""FastAPI sidecar — Phase 2.

Exposes two endpoints:

  GET  /health           liveness check
  POST /run              stream graph node events as NDJSON

Each POST /run call accepts a JSON body describing the task and LLM/tool
configuration, then runs the compiled LangGraph workflow and streams one
NDJSON line per completed node.  The Go graph_solve handler reads these
lines and checkpoints them into job_steps with fencing-token validation.

Wire format (one JSON object per line, newline-delimited):

  {"event": "node_start",  "node": "plan",            "step": 1}
  {"event": "node_done",   "node": "plan",            "step": 1,
   "data": {"plan": "..."}}
  {"event": "node_start",  "node": "kb_search",       "step": 2}
  {"event": "node_done",   "node": "kb_search",       "step": 2,
   "data": {"last_observation": "..."}}
  ...
  {"event": "done",        "tests_passed": true,
   "attempts": 1,          "solution": "..."}
  {"event": "error",       "message": "..."}   (terminal on fatal error)
"""

from __future__ import annotations

import json
import logging
import os
from typing import AsyncGenerator

from fastapi import FastAPI
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

from forge_langgraph_agent.graph import build_graph, initial_state
from forge_langgraph_agent.llm import FakeLLM, GroqLLM
from forge_langgraph_agent.tools import FakeKnowledgeBase, FakeTestRunner, parse_test_outcomes

logger = logging.getLogger(__name__)

app = FastAPI(title="forge-langgraph-sidecar", version="2.0.0")


# ---------------------------------------------------------------------------
# Request / response models
# ---------------------------------------------------------------------------

class RunRequest(BaseModel):
    """Body for POST /run.

    Fields
    ------
    task:
        The competitive-programming problem description.
    llm:
        "fake" (deterministic, no network) or "groq" (requires GROQ_API_KEY).
    model:
        Optional model override (Groq only).  Falls back to GROQ_MODEL env var,
        then the LangChain-groq default.
    test_outcomes:
        Comma-separated list of "pass"/"fail" tokens controlling what the
        FakeTestRunner returns — e.g. "fail,pass" → fail first attempt, pass
        second.  Ignored when llm != "fake".
    """

    task: str
    llm: str = "fake"
    model: str | None = None
    test_outcomes: str = "pass"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_NODE_NAMES = frozenset({"plan", "kb_search", "write_solution", "run_tests", "verify"})

# State keys to forward per-node in node_done events; avoids leaking the full
# messages list (which can be large) in the streaming response.
_NODE_DATA_KEYS: dict[str, list[str]] = {
    "plan":           ["plan"],
    "kb_search":      ["last_observation"],
    "write_solution": ["solution"],
    "run_tests":      ["tests_passed", "attempts", "last_observation"],
    "verify":         ["done", "tests_passed", "attempts"],
}


def _build_components(req: RunRequest):
    """Instantiate LLM/KB/test adapters from the request body."""
    if req.llm == "groq":
        api_key = os.environ.get("GROQ_API_KEY", "")
        if not api_key:
            raise ValueError("GROQ_API_KEY is not set in the environment")
        llm = GroqLLM(model=req.model)
    else:
        llm = FakeLLM()

    kb = FakeKnowledgeBase()

    if req.llm == "fake":
        outcomes = parse_test_outcomes(req.test_outcomes)
        tests = FakeTestRunner(outcomes)
    else:
        # When using a real LLM we still use the FakeTestRunner; a real
        # TestRunner adapter (Phase 3) will replace this once the MCP tool
        # server is in place.
        tests = FakeTestRunner([True])

    return llm, kb, tests


async def _stream_graph(req: RunRequest) -> AsyncGenerator[str, None]:
    """Run the graph and yield NDJSON lines for the caller.

    Yields one line per graph event.  The Go handler maps these to a
    step_type of "graph_node" in job_steps, one row per node_done event.
    """
    try:
        llm, kb, tests = _build_components(req)
    except ValueError as exc:
        yield json.dumps({"event": "error", "message": str(exc)}) + "\n"
        return

    graph = build_graph(llm, kb, tests)
    state = initial_state(req.task)

    # step counter: 1-indexed, matches job_steps.step_number
    step = 0
    pending_node: str | None = None

    try:
        for item in graph.stream(
            state,
            {"recursion_limit": 12},
            stream_mode=["updates", "values"],
        ):
            mode, data = item if isinstance(item, tuple) else ("updates", item)

            if mode == "updates":
                for node_name, update in data.items():
                    if node_name not in _NODE_NAMES:
                        continue

                    # node_start fires before the update is applied
                    step += 1
                    pending_node = node_name
                    yield json.dumps({
                        "event": "node_start",
                        "node":  node_name,
                        "step":  step,
                    }) + "\n"

                    # Collect the relevant subset of the update for node_done.
                    keys = _NODE_DATA_KEYS.get(node_name, [])
                    node_data: dict = {}
                    for k in keys:
                        if k in update:
                            v = update[k]
                            # Serialize non-primitive values to a string so the
                            # Go side receives clean JSON without LangChain types
                            if isinstance(v, (str, int, float, bool)) or v is None:
                                node_data[k] = v
                            else:
                                node_data[k] = str(v)

                    yield json.dumps({
                        "event": "node_done",
                        "node":  node_name,
                        "step":  step,
                        "data":  node_data,
                    }) + "\n"
                    pending_node = None

            elif mode == "values":
                # Final graph state — emit the terminal summary event.
                # This fires after the last update so 'data' is the full state.
                final: dict = data  # type: ignore[assignment]
                yield json.dumps({
                    "event":        "done",
                    "tests_passed": bool(final.get("tests_passed")),
                    "attempts":     int(final.get("attempts") or 0),
                    "done":         bool(final.get("done")),
                    "solution":     str(final.get("solution") or ""),
                }) + "\n"

    except Exception as exc:  # pylint: disable=broad-except
        logger.exception("graph stream error")
        node_hint = f" (in node {pending_node})" if pending_node else ""
        yield json.dumps({
            "event":   "error",
            "message": f"graph error{node_hint}: {exc}",
        }) + "\n"


# ---------------------------------------------------------------------------
# Routes
# ---------------------------------------------------------------------------

@app.get("/health")
async def health() -> dict:
    """Kubernetes / TCP liveness probe."""
    return {"status": "ok"}


@app.post("/run")
async def run(req: RunRequest) -> StreamingResponse:
    """Stream graph node events as NDJSON.

    Each line is a valid JSON object terminated by ``\\n``.  The Go handler
    reads lines via ``bufio.Scanner`` and checkpoints each ``node_done`` event
    into ``job_steps``, providing the timeline view used by the dashboard and
    crash recovery.
    """
    return StreamingResponse(
        _stream_graph(req),
        media_type="application/x-ndjson",
    )
