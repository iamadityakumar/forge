from __future__ import annotations

from typing import Protocol


class KnowledgeBase(Protocol):
    def search(self, task: str, plan: str) -> str:
        ...


class TestRunner(Protocol):
    def run(self, task: str, solution: str) -> dict:
        ...


class FakeKnowledgeBase:
    """Stable offline stand-in for Forge's search_kb tool."""

    def search(self, task: str, plan: str) -> str:
        del task, plan
        return (
            "KB: Hash map / two-sum pattern. Store each seen value with its index; "
            "for every n look up target-n in O(1)."
        )


class FakeTestRunner:
    """Scripted test outcomes such as fail, then pass. Does not execute code."""

    def __init__(self, outcomes: list[bool] | None = None) -> None:
        self.outcomes = list(outcomes if outcomes is not None else [True])
        if not self.outcomes:
            raise ValueError("FakeTestRunner requires at least one outcome")
        self.calls = 0

    def run(self, task: str, solution: str) -> dict:
        del task, solution
        index = min(self.calls, len(self.outcomes) - 1)
        passed = bool(self.outcomes[index])
        self.calls += 1
        if passed:
            observation = f"tests passed on attempt {self.calls}"
        else:
            observation = (
                f"tests failed on attempt {self.calls}: expected [0, 1], got []"
            )
        return {"passed": passed, "observation": observation}


def parse_test_outcomes(raw: str) -> list[bool]:
    parts = [part.strip().lower() for part in raw.split(",") if part.strip()]
    if not parts:
        raise ValueError("test outcomes cannot be empty")
    mapping = {"pass": True, "fail": False}
    outcomes: list[bool] = []
    for part in parts:
        if part not in mapping:
            raise ValueError(
                f"invalid test outcome {part!r}; use pass or fail, comma-separated"
            )
        outcomes.append(mapping[part])
    return outcomes
