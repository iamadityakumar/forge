from __future__ import annotations

from langgraph.graph import END, START, StateGraph

from forge_langgraph_agent.llm import LLM
from forge_langgraph_agent.nodes import make_nodes
from forge_langgraph_agent.routing import decide_next
from forge_langgraph_agent.state import AgentState
from forge_langgraph_agent.tools import KnowledgeBase, TestRunner

RECURSION_LIMIT = 12


def build_graph(llm: LLM, kb: KnowledgeBase, tests: TestRunner):
    nodes = make_nodes(llm, kb, tests)
    builder = StateGraph(AgentState)
    builder.add_node("plan", nodes["plan"])
    builder.add_node("kb_search", nodes["kb_search"])
    builder.add_node("write_solution", nodes["write_solution"])
    builder.add_node("run_tests", nodes["run_tests"])
    builder.add_node("verify", nodes["verify"])
    builder.add_edge(START, "plan")
    builder.add_edge("plan", "kb_search")
    builder.add_edge("kb_search", "write_solution")
    builder.add_edge("write_solution", "run_tests")
    builder.add_edge("run_tests", "verify")
    builder.add_conditional_edges(
        "verify",
        decide_next,
        {"retry": "write_solution", "end": END},
    )
    return builder.compile()


def initial_state(task: str) -> AgentState:
    return {
        "messages": [],
        "task": task,
        "plan": "",
        "last_observation": "",
        "tests_passed": False,
        "attempts": 0,
        "done": False,
        "solution": "",
    }


_NODE_NAMES = frozenset({"plan", "kb_search", "write_solution", "run_tests", "verify"})


def run_graph(graph, task: str) -> tuple[AgentState, list[str]]:
    visited: list[str] = []
    final: AgentState | None = None
    for item in graph.stream(
        initial_state(task),
        {"recursion_limit": RECURSION_LIMIT},
        stream_mode=["updates", "values"],
    ):
        mode, data = item if isinstance(item, tuple) else ("updates", item)
        if mode == "updates":
            for node_name in data:
                if node_name in _NODE_NAMES:
                    visited.append(node_name)
        elif mode == "values":
            final = data
    if final is None:
        raise RuntimeError("graph produced no updates")
    return final, visited
