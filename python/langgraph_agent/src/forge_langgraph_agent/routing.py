from forge_langgraph_agent.state import MAX_ATTEMPTS


def decide_next(state: dict) -> str:
    """Return 'end' or 'retry' from the verify node."""
    if state.get("done") or state.get("tests_passed"):
        return "end"
    if int(state.get("attempts") or 0) >= MAX_ATTEMPTS:
        return "end"
    return "retry"
