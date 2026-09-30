from __future__ import annotations

import argparse
import json
import sys
from typing import Sequence

from forge_langgraph_agent.graph import build_graph, run_graph
from forge_langgraph_agent.llm import FakeLLM, GroqConfigError, GroqLLM
from forge_langgraph_agent.tools import FakeKnowledgeBase, FakeTestRunner, parse_test_outcomes


class CLIError(Exception):
    def __init__(self, message: str, exit_code: int = 2) -> None:
        super().__init__(message)
        self.exit_code = exit_code


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Standalone Forge LangGraph agent (Phase 1)")
    parser.add_argument("--task", required=True, help="Competitive programming task to solve")
    parser.add_argument("--llm", choices=("fake", "groq"), default="fake")
    parser.add_argument("--model", default=None, help="Groq model override")
    parser.add_argument(
        "--test-outcomes",
        default="pass",
        help="Comma-separated fake test outcomes, e.g. fail,pass",
    )
    parser.add_argument("--json", action="store_true", help="Print machine-readable final state")
    return parser.parse_args(argv)


def build_runtime(args: argparse.Namespace):
    try:
        outcomes = parse_test_outcomes(args.test_outcomes)
    except ValueError as exc:
        raise CLIError(str(exc), 2) from exc

    if args.llm == "groq":
        try:
            llm = GroqLLM(model=args.model)
        except GroqConfigError as exc:
            raise CLIError(str(exc), 2) from exc
    else:
        llm = FakeLLM()
    return llm, FakeKnowledgeBase(), FakeTestRunner(outcomes)


def public_state(state: dict) -> dict:
    return {
        "task": state.get("task", ""),
        "plan": state.get("plan", ""),
        "last_observation": state.get("last_observation", ""),
        "tests_passed": bool(state.get("tests_passed")),
        "attempts": int(state.get("attempts") or 0),
        "done": bool(state.get("done")),
        "solution": state.get("solution", ""),
    }


def format_human(state: dict, visited: list[str]) -> str:
    status = "passed" if state.get("tests_passed") else "failed"
    return (
        f"status: {status}\n"
        f"attempts: {state.get('attempts', 0)}\n"
        f"nodes: {', '.join(visited)}\n"
        f"plan: {state.get('plan', '')}\n"
        f"observation: {state.get('last_observation', '')}\n"
    )


def run(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        llm, kb, tests = build_runtime(args)
    except CLIError as exc:
        print(str(exc), file=sys.stderr)
        return exc.exit_code

    graph = build_graph(llm, kb, tests)
    state, visited = run_graph(graph, args.task)
    if args.json:
        payload = public_state(state)
        payload["nodes"] = visited
        print(json.dumps(payload, indent=2))
    else:
        print(format_human(state, visited), end="")

    if state.get("tests_passed"):
        return 0
    return 1


def main() -> None:
    sys.exit(run())
