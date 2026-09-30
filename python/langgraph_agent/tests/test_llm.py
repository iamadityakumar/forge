import pytest

from forge_langgraph_agent.llm import (
    DEFAULT_GROQ_MODEL,
    FakeLLM,
    GroqConfigError,
    require_groq_api_key,
    resolve_groq_model,
)


def test_fake_llm_returns_scripted_responses_in_order() -> None:
    llm = FakeLLM(["plan", "code"])
    assert llm.invoke([]) == "plan"
    assert llm.invoke([]) == "code"
    assert llm.invoke([]) == "code"
    assert llm.calls == 3


def test_require_groq_api_key_raises_when_missing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    with pytest.raises(GroqConfigError, match="GROQ_API_KEY"):
        require_groq_api_key()


def test_resolve_groq_model_prefers_cli_then_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GROQ_MODEL", "env-model")
    assert resolve_groq_model("cli-model") == "cli-model"
    assert resolve_groq_model(None) == "env-model"
    monkeypatch.delenv("GROQ_MODEL", raising=False)
    assert resolve_groq_model(None) == DEFAULT_GROQ_MODEL
