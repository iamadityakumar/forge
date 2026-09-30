"""Tests for the FastAPI sidecar server (Phase 2)."""

from __future__ import annotations

import json
import pytest
from httpx import ASGITransport, AsyncClient

from forge_langgraph_agent.server import app


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.mark.anyio
async def test_health_endpoint():
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.get("/health")
        assert resp.status_code == 200
        assert resp.json() == {"status": "ok"}


@pytest.mark.anyio
async def test_run_stream_fake_immediate_pass():
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.post(
            "/run",
            json={
                "task": "Two Sum: return indices of two numbers that add up to target",
                "llm": "fake",
                "test_outcomes": "pass",
            },
        )
        assert resp.status_code == 200
        assert resp.headers["content-type"].startswith("application/x-ndjson")

        lines = [line for line in resp.text.strip().split("\n") if line.strip()]
        events = [json.loads(line) for line in lines]

        # Expect node_start/node_done pairs for plan, kb_search, write_solution, run_tests, verify, then done
        event_types = [e["event"] for e in events]
        assert "node_start" in event_types
        assert "node_done" in event_types
        assert event_types[-1] == "done"

        done_event = events[-1]
        assert done_event["tests_passed"] is True
        assert done_event["attempts"] == 1
        assert done_event["done"] is True


@pytest.mark.anyio
async def test_run_stream_fake_retry_then_pass():
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.post(
            "/run",
            json={
                "task": "Two Sum",
                "llm": "fake",
                "test_outcomes": "fail,pass",
            },
        )
        assert resp.status_code == 200

        lines = [line for line in resp.text.strip().split("\n") if line.strip()]
        events = [json.loads(line) for line in lines]

        done_event = events[-1]
        assert done_event["event"] == "done"
        assert done_event["tests_passed"] is True
        assert done_event["attempts"] == 2


@pytest.mark.anyio
async def test_run_stream_fake_exhaustion():
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.post(
            "/run",
            json={
                "task": "Two Sum",
                "llm": "fake",
                "test_outcomes": "fail,fail,fail",
            },
        )
        assert resp.status_code == 200

        lines = [line for line in resp.text.strip().split("\n") if line.strip()]
        events = [json.loads(line) for line in lines]

        done_event = events[-1]
        assert done_event["event"] == "done"
        assert done_event["tests_passed"] is False
        assert done_event["attempts"] == 3


@pytest.mark.anyio
async def test_run_stream_groq_missing_api_key(monkeypatch):
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    async with AsyncClient(
        transport=ASGITransport(app=app), base_url="http://test"
    ) as client:
        resp = await client.post(
            "/run",
            json={
                "task": "Two Sum",
                "llm": "groq",
            },
        )
        assert resp.status_code == 200
        lines = [line for line in resp.text.strip().split("\n") if line.strip()]
        events = [json.loads(line) for line in lines]
        assert len(events) == 1
        assert events[0]["event"] == "error"
        assert "GROQ_API_KEY" in events[0]["message"]
