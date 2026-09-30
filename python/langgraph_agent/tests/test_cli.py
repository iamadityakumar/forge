import json

from forge_langgraph_agent.cli import run


def test_cli_json_success(capsys) -> None:
    code = run(
        [
            "--task",
            "Solve two sum",
            "--llm",
            "fake",
            "--test-outcomes",
            "pass",
            "--json",
        ]
    )
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert code == 0
    assert payload["tests_passed"] is True
    assert payload["attempts"] == 1
    assert payload["done"] is True
    assert payload["nodes"] == ["plan", "kb_search", "write_solution", "run_tests", "verify"]
    assert "GROQ_API_KEY" not in captured.out


def test_cli_exhausted_attempts_exits_one(capsys) -> None:
    code = run(
        [
            "--task",
            "Solve two sum",
            "--llm",
            "fake",
            "--test-outcomes",
            "fail,fail,fail",
            "--json",
        ]
    )
    payload = json.loads(capsys.readouterr().out)
    assert code == 1
    assert payload["tests_passed"] is False
    assert payload["attempts"] == 3
    assert payload["done"] is True


def test_cli_missing_groq_key_exits_two(monkeypatch, capsys) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    code = run(["--task", "Solve two sum", "--llm", "groq"])
    captured = capsys.readouterr()
    assert code == 2
    assert "GROQ_API_KEY" in captured.err
