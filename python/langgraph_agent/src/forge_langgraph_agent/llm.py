from __future__ import annotations

import os
from typing import Protocol

DEFAULT_GROQ_MODEL = "llama-3.1-8b-instant"


class GroqConfigError(ValueError):
    """Raised when Groq is requested without a usable configuration."""


class LLM(Protocol):
    def invoke(self, messages: list[object]) -> str:
        ...


def resolve_groq_model(explicit: str | None = None) -> str:
    if explicit and explicit.strip():
        return explicit.strip()
    env_model = os.environ.get("GROQ_MODEL", "").strip()
    if env_model:
        return env_model
    return DEFAULT_GROQ_MODEL


def require_groq_api_key(explicit: str | None = None) -> str:
    key = (explicit if explicit is not None else os.environ.get("GROQ_API_KEY", "")).strip()
    if not key:
        raise GroqConfigError(
            "GROQ_API_KEY is not set. Export GROQ_API_KEY before using --llm groq."
        )
    return key


class FakeLLM:
    """Deterministic scripted LLM. Repeats the last response if asked again."""

    def __init__(self, responses: list[str] | None = None) -> None:
        self.responses = list(
            responses
            or [
                "Plan: use a hash map so each value is checked against target-n in one pass.",
                (
                    "def two_sum(nums, target):\n"
                    "    seen = {}\n"
                    "    for i, n in enumerate(nums):\n"
                    "        complement = target - n\n"
                    "        if complement in seen:\n"
                    "            return [seen[complement], i]\n"
                    "        seen[n] = i\n"
                    "    return []\n"
                ),
            ]
        )
        self.calls = 0

    def invoke(self, messages: list[object]) -> str:
        del messages
        self.calls += 1
        index = min(self.calls - 1, len(self.responses) - 1)
        return self.responses[index]


class GroqLLM:
    def __init__(self, model: str | None = None, api_key: str | None = None) -> None:
        key = require_groq_api_key(api_key)
        self.model = resolve_groq_model(model)
        from langchain_groq import ChatGroq

        self._chat = ChatGroq(model=self.model, temperature=0, api_key=key)

    def invoke(self, messages: list[object]) -> str:
        result = self._chat.invoke(_to_lc_messages(messages))
        content = getattr(result, "content", result)
        if isinstance(content, str):
            return content
        return str(content)


def _to_lc_messages(messages: list[object]) -> list[object]:
    from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage

    converted: list[object] = []
    for message in messages:
        if isinstance(message, BaseMessage):
            converted.append(message)
            continue
        if not isinstance(message, dict):
            converted.append(HumanMessage(content=str(message)))
            continue
        role = str(message.get("role", "user"))
        content = str(message.get("content", ""))
        if role == "system":
            converted.append(SystemMessage(content=content))
        elif role == "assistant":
            converted.append(AIMessage(content=content))
        else:
            converted.append(HumanMessage(content=content))
    return converted
