from forge_langgraph_agent.graph import build_graph, run_graph
from forge_langgraph_agent.llm import FakeLLM
from forge_langgraph_agent.tools import FakeKnowledgeBase, FakeTestRunner

PASS_PATH = ["plan", "kb_search", "write_solution", "run_tests", "verify"]
RETRY_TAIL = ["write_solution", "run_tests", "verify"]


def _run(outcomes: list[bool]):
    tests = FakeTestRunner(outcomes)
    graph = build_graph(FakeLLM(), FakeKnowledgeBase(), tests)
    state, visited = run_graph(graph, "Solve two sum")
    return state, visited, tests


def test_immediate_pass_visits_each_node_once() -> None:
    state, visited, tests = _run([True])
    assert visited == PASS_PATH
    assert state["done"] is True
    assert state["tests_passed"] is True
    assert state["attempts"] == 1
    assert tests.calls == 1


def test_fail_then_pass_retries_write_tests_verify_only() -> None:
    state, visited, tests = _run([False, True])
    assert visited == PASS_PATH + RETRY_TAIL
    assert state["done"] is True
    assert state["tests_passed"] is True
    assert state["attempts"] == 2
    assert tests.calls == 2


def test_three_failures_stop_without_fourth_test() -> None:
    state, visited, tests = _run([False, False, False])
    assert visited == PASS_PATH + RETRY_TAIL + RETRY_TAIL
    assert state["done"] is True
    assert state["tests_passed"] is False
    assert state["attempts"] == 3
    assert tests.calls == 3
