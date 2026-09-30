from typing import Annotated, TypedDict

from langgraph.graph.message import add_messages

MAX_ATTEMPTS = 3


class AgentState(TypedDict):
    messages: Annotated[list, add_messages]
    task: str
    plan: str
    last_observation: str
    tests_passed: bool
    attempts: int
    done: bool
    solution: str
