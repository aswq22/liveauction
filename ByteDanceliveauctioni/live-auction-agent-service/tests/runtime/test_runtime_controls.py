from __future__ import annotations

import asyncio
import json

import pytest

from app.domain.models import RunContext
from app.harness.context import RunStatus
from app.models.base import TokenBudget, TokenBudgetExceededError
from app.runtime.repository import InMemoryRuntimeRepository


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


async def test_restart_sweep_terminalizes_each_nonterminal_once_and_preserves_terminal():
    repository = InMemoryRuntimeRepository()
    for run_id, tenant in (("created", "t1"), ("running", "t1"), ("done", "t2")):
        await repository.create_run(
            context=RunContext(run_id=run_id, tenant_id=tenant), goal="sensitive", max_steps=3
        )
    await repository.transition_run(
        "running", status=RunStatus.PLANNING, type="run.planning"
    )
    await repository.transition_run(
        "done", status=RunStatus.COMPLETED, type="run.completed", proposal_json="{}"
    )

    first = await repository.fail_nonterminal_runs_on_restart()
    second = await repository.fail_nonterminal_runs_on_restart()

    assert {item.run.run_id for item in first} == {"created", "running"}
    assert second == []
    for run_id in ("created", "running"):
        run = await repository.get_run(run_id)
        assert run is not None
        assert (run.status, run.error_code) == (
            RunStatus.FAILED,
            "AGENT_RUNTIME_RESTARTED",
        )
        events = await repository.list_events(run_id, 0)
        terminal = [event for event in events if event.status is RunStatus.FAILED]
        assert len(terminal) == 1
        assert terminal[0].type == "run.failed"
        assert json.loads(terminal[0].payload_json) == {"reason": "runtime_restarted"}
    done = await repository.get_run("done")
    assert done is not None and done.status is RunStatus.COMPLETED


async def test_nonterminal_counts_are_tenant_scoped():
    repository = InMemoryRuntimeRepository()
    for run_id, tenant in (("a", "t1"), ("b", "t1"), ("c", "t2")):
        await repository.create_run(
            context=RunContext(run_id=run_id, tenant_id=tenant), goal="g", max_steps=2
        )
    await repository.transition_run("b", status=RunStatus.FAILED, type="run.failed")

    assert await repository.count_nonterminal_runs() == 2
    assert await repository.count_nonterminal_runs(tenant_id="t1") == 1
    assert await repository.count_nonterminal_runs(tenant_id="t2") == 1


def test_token_budget_uses_exact_usage_and_fails_closed():
    budget = TokenBudget(10)
    budget.reserve_input("abcd")
    budget.commit_usage(input_tokens=3, output_tokens=4)
    assert budget.used_tokens == 7
    assert budget.remaining_tokens == 3
    with pytest.raises(TokenBudgetExceededError):
        budget.reserve_input("x" * 20)


def test_token_budget_estimation_is_deterministic():
    first = TokenBudget(100)
    second = TokenBudget(100)
    first.reserve_input("sensitive-goal")
    second.reserve_input("sensitive-goal")
    assert first.used_tokens == second.used_tokens == 5
