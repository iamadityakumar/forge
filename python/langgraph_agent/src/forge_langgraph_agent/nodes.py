from __future__ import annotations

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage

from forge_langgraph_agent.state import MAX_ATTEMPTS, AgentState
from forge_langgraph_agent.llm import LLM
from forge_langgraph_agent.tools import KnowledgeBase, TestRunner


def _content(state: AgentState, key: str, default: str = "") -> str:
    value = state.get(key, default)
    return default if value is None else str(value)


def make_nodes(llm: LLM, kb: KnowledgeBase, tests: TestRunner) -> dict:
    def plan(state: AgentState) -> dict:
        task = _content(state, "task")
        messages = [
            SystemMessage(content="You are a competitive programming planner. Reply with a concise implementation plan."),
            HumanMessage(content=f"Task:\n{task}"),
        ]
        response = llm.invoke(messages)
        return {
            "plan": response,
            "messages": [AIMessage(content=response)],
        }

    def kb_search(state: AgentState) -> dict:
        observation = kb.search(_content(state, "task"), _content(state, "plan"))
        return {
            "last_observation": observation,
            "messages": [HumanMessage(content=f"KB observation:\n{observation}")],
        }

    def write_solution(state: AgentState) -> dict:
        task = _content(state, "task")
        plan_text = _content(state, "plan")
        observation = _content(state, "last_observation")
        prior = _content(state, "solution")
        prompt = [
            SystemMessage(
                content=(
                    "Write a complete Python solution for the task. "
                    "Return only the implementation, no markdown fences."
                )
            ),
            HumanMessage(
                content=(
                    f"Task:\n{task}\n\nPlan:\n{plan_text}\n\n"
                    f"Latest observation:\n{observation}\n\n"
                    f"Previous solution:\n{prior or '(none)'}"
                )
            ),
        ]
        solution = llm.invoke(prompt)
        return {
            "solution": solution,
            "messages": [AIMessage(content=solution)],
        }

    def run_tests(state: AgentState) -> dict:
        result = tests.run(_content(state, "task"), _content(state, "solution"))
        passed = bool(result.get("passed"))
        observation = str(result.get("observation") or "")
        attempts = int(state.get("attempts") or 0) + 1
        return {
            "tests_passed": passed,
            "attempts": attempts,
            "last_observation": observation,
            "messages": [HumanMessage(content=f"Test observation:\n{observation}")],
        }

    def verify(state: AgentState) -> dict:
        passed = bool(state.get("tests_passed"))
        attempts = int(state.get("attempts") or 0)
        done = passed or attempts >= MAX_ATTEMPTS
        if passed:
            status = f"verify: tests passed after {attempts} attempt(s)"
        elif done:
            status = f"verify: exhausted {MAX_ATTEMPTS} attempts without passing"
        else:
            status = f"verify: tests failed on attempt {attempts}; retrying"
        return {
            "done": done,
            "messages": [AIMessage(content=status)],
        }

    return {
        "plan": plan,
        "kb_search": kb_search,
        "write_solution": write_solution,
        "run_tests": run_tests,
        "verify": verify,
    }
