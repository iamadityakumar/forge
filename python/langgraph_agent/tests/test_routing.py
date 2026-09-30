from forge_langgraph_agent.routing import decide_next


def test_success_routes_to_end() -> None:
    assert decide_next({"tests_passed": True, "attempts": 1, "done": False}) == "end"


def test_failure_before_attempt_limit_routes_to_retry() -> None:
    assert decide_next({"tests_passed": False, "attempts": 2, "done": False}) == "retry"


def test_failure_at_attempt_limit_routes_to_end() -> None:
    assert decide_next({"tests_passed": False, "attempts": 3, "done": False}) == "end"


def test_done_always_routes_to_end() -> None:
    assert decide_next({"tests_passed": False, "attempts": 1, "done": True}) == "end"
