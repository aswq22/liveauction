"""Construction of the real LangGraph planning state machine."""

from __future__ import annotations

import asyncio
import inspect
import re
from time import monotonic
from typing import Awaitable, Callable, Protocol

from langgraph.graph import END, START, StateGraph

from app.domain.models import (
    AgentContext,
    FinalProposalDecision,
    Observation,
    SessionProposal,
    ToolCallDecision,
)
from app.graph.state import AgentState
from app.harness.context import RunError, RunStatus
from app.models.base import (
    ModelAdapter,
    ModelError,
    TokenBudget,
    TokenBudgetExceededError,
    TokenUsage,
)
from app.observability.tracing import Tracing, set_safe_attribute
from app.skills.registry import LoadedSkill, SkillRegistry
from app.tools.registry import ToolError, ToolRegistry


_SKILL_NAME = "plan_live_session"
EventSink = Callable[[str, dict], Awaitable[None]]


async def _discard_event(event_type: str, payload: dict) -> None:
    del event_type, payload


class CancellationChecker(Protocol):
    async def is_cancelled(self, run_id: str) -> bool: ...


class NeverCancelled:
    async def is_cancelled(self, run_id: str) -> bool:
        del run_id
        return False


class PlanningWorkflow:
    def __init__(
        self,
        *,
        model: ModelAdapter,
        tools: ToolRegistry,
        skills: SkillRegistry,
        checkpointer,
        cancellation: CancellationChecker,
        tracing: Tracing,
        event_sink: EventSink | None = None,
        token_budget: int = 8192,
    ) -> None:
        self._model = model
        self._tools = tools
        self._skills = skills
        self._cancellation = cancellation
        self._tracing = tracing
        self._event_sink = event_sink or _discard_event
        self._token_budget_limit = token_budget
        self._budgets: dict[str, TokenBudget] = {}
        self.graph = self._build_graph().compile(checkpointer=checkpointer)

    def _build_graph(self) -> StateGraph:
        builder = StateGraph(AgentState)
        builder.add_node("load_skill", self._load_skill)
        builder.add_node("decide", self._decide)
        builder.add_node("execute_tool", self._execute_tool)
        builder.add_node("finalize", self._finalize)
        builder.add_node("fail", self._fail)
        builder.add_edge(START, "load_skill")
        builder.add_conditional_edges(
            "load_skill",
            self._route_after_skill,
            {"execute_tool": "execute_tool", "fail": "fail"},
        )
        builder.add_conditional_edges(
            "decide",
            self._route_after_decide,
            {"execute_tool": "execute_tool", "finalize": "finalize", "fail": "fail"},
        )
        builder.add_conditional_edges(
            "execute_tool",
            self._route_after_tool,
            {"decide": "decide", "fail": "fail"},
        )
        builder.add_conditional_edges(
            "finalize",
            self._route_after_finalize,
            {"decide": "decide", "end": END},
        )
        builder.add_edge("fail", END)
        return builder

    async def _load_skill(self, state: AgentState) -> dict:
        del state
        try:
            skill = self._skills.load(_SKILL_NAME)
        except Exception:
            return _failed("SKILL_ERROR", "planning skill could not be loaded")
        return {
            "skill_name": skill.name,
            "decision": ToolCallDecision(
                kind="tool_call", tool="inventory.query", arguments={"limit": 100}
            ),
            "status": RunStatus.COLLECTING_CONTEXT,
        }

    async def _decide(self, state: AgentState) -> dict:
        run = state["run"]
        validation_retry_pending = state.get("validation_retry_count", 0) == 1
        if await self._cancellation.is_cancelled(run.context.run_id):
            return _cancelled()
        try:
            skill = self._skill_from_state(state)
        except Exception:
            return _failed("SKILL_ERROR", "planning skill could not be loaded")
        context = AgentContext(
            goal=run.goal,
            skill_name=skill.name,
            skill_instructions=_skill_prompt(skill),
            observations=state["observations"],
        )
        next_step = state["step_count"] + 1
        budget = self._budgets.setdefault(
            run.context.run_id, TokenBudget(self._token_budget_limit)
        )
        try:
            budget_text = context.model_dump_json()
            provider_budget_text = getattr(self._model, "budget_input_text", None)
            if callable(provider_budget_text):
                budget_text = provider_budget_text(context)
            budget.reserve_input(budget_text)
        except TokenBudgetExceededError:
            return {
                "step_count": next_step,
                **_failed(
                    "AGENT_TOKEN_BUDGET_EXCEEDED", "agent token budget exceeded"
                ),
            }
        started = monotonic()
        await self._event_sink(
            "model.started", {"step": next_step, "status": "started"}
        )
        try:
            with self._tracing.span(
                "agent.model",
                {"run_id": run.context.run_id, "skill": skill.name},
            ) as span:
                try:
                    decision = await self._model_decide(
                        context, max_output_tokens=budget.remaining_tokens
                    )
                except asyncio.CancelledError:
                    set_safe_attribute(span, "status", "cancelled")
                    raise
                except Exception:
                    set_safe_attribute(span, "status", "failed")
                    raise
                else:
                    set_safe_attribute(span, "status", "completed")
        except asyncio.CancelledError:
            await self._event_sink(
                "model.completed",
                {
                    "step": next_step,
                    "status": "cancelled",
                    "duration_bucket": _duration_bucket(started),
                },
            )
            return {"step_count": next_step, **_cancelled()}
        except ModelError:
            await self._event_sink(
                "model.completed",
                {
                    "step": next_step,
                    "status": "failed",
                    "duration_bucket": _duration_bucket(started),
                },
            )
            return {
                "step_count": next_step,
                **_failed("MODEL_ERROR", "model decision failed"),
            }
        except Exception:
            await self._event_sink(
                "model.completed",
                {
                    "step": next_step,
                    "status": "failed",
                    "duration_bucket": _duration_bucket(started),
                },
            )
            return {
                "step_count": next_step,
                **_failed("MODEL_ERROR", "model decision failed"),
            }
        try:
            usage = getattr(self._model, "last_usage", None)
            if isinstance(usage, TokenUsage):
                budget.commit_usage(
                    input_tokens=usage.input_tokens, output_tokens=usage.output_tokens
                )
            else:
                budget.commit_estimated_output(decision.model_dump_json())
        except TokenBudgetExceededError:
            await self._event_sink(
                "model.completed",
                {
                    "step": next_step,
                    "status": "failed",
                    "duration_bucket": _duration_bucket(started),
                },
            )
            return {
                "step_count": next_step,
                **_failed(
                    "AGENT_TOKEN_BUDGET_EXCEEDED", "agent token budget exceeded"
                ),
            }
        await self._event_sink(
            "model.completed",
            {
                "step": next_step,
                "status": "completed",
                "duration_bucket": _duration_bucket(started),
            },
        )
        if await self._cancellation.is_cancelled(run.context.run_id):
            return {"step_count": next_step, "decision": decision, **_cancelled()}
        if validation_retry_pending and isinstance(decision, ToolCallDecision):
            return {
                "step_count": next_step,
                "validation_retry_count": 2,
                "decision": decision,
                **_failed(
                    "UNGROUNDED_PROPOSAL",
                    "grounding retry did not return a final proposal",
                ),
            }
        if isinstance(decision, ToolCallDecision) and next_step >= run.max_steps:
            return {
                "step_count": next_step,
                "decision": decision,
                **_failed(
                    "STEP_BUDGET_EXHAUSTED",
                    "model requested another tool after the step budget was exhausted",
                ),
            }
        return {
            "step_count": next_step,
            **(
                {"validation_retry_count": 2}
                if validation_retry_pending
                else {}
            ),
            "decision": decision,
            "status": (
                RunStatus.COLLECTING_CONTEXT
                if isinstance(decision, ToolCallDecision)
                else RunStatus.VERIFYING
            ),
        }

    async def _execute_tool(self, state: AgentState) -> dict:
        run = state["run"]
        if await self._cancellation.is_cancelled(run.context.run_id):
            return _cancelled()
        decision = state["decision"]
        if not isinstance(decision, ToolCallDecision):
            return _failed("HARNESS_ERROR", "workflow has no tool call to execute")
        try:
            skill = self._skill_from_state(state)
        except Exception:
            return _failed("SKILL_ERROR", "planning skill could not be loaded")
        try:
            started = monotonic()
            await self._event_sink(
                "tool.started",
                {"step": state["step_count"], "status": "started", "tool": decision.tool},
            )
            with self._tracing.span(
                "agent.tool",
                {
                    "run_id": run.context.run_id,
                    "skill": skill.name,
                    "tool": decision.tool,
                },
            ) as span:
                try:
                    observation = await self._tools.execute(
                        decision.tool,
                        decision.arguments,
                        run.context,
                        allowed_tools=skill.allowed_tools,
                    )
                except asyncio.CancelledError:
                    set_safe_attribute(span, "status", "cancelled")
                    raise
                except Exception:
                    set_safe_attribute(span, "status", "failed")
                    raise
                else:
                    set_safe_attribute(span, "status", "completed")
        except asyncio.CancelledError:
            await self._event_sink(
                "tool.completed",
                {"step": state["step_count"], "status": "cancelled", "tool": decision.tool,
                 "duration_bucket": _duration_bucket(started)},
            )
            return _cancelled()
        except ToolError:
            await self._event_sink(
                "tool.completed",
                {"step": state["step_count"], "status": "failed", "tool": decision.tool,
                 "duration_bucket": _duration_bucket(started)},
            )
            return _failed("TOOL_ERROR", "tool execution failed")
        except Exception:
            await self._event_sink(
                "tool.completed",
                {"step": state["step_count"], "status": "failed", "tool": decision.tool,
                 "duration_bucket": _duration_bucket(started)},
            )
            return _failed("TOOL_ERROR", "tool execution failed")
        await self._event_sink(
            "tool.completed",
            {"step": state["step_count"], "status": "completed", "tool": decision.tool,
             "duration_bucket": _duration_bucket(started)},
        )
        if await self._cancellation.is_cancelled(run.context.run_id):
            return _cancelled()
        if (
            decision.tool == "inventory.query"
            and state["step_count"] == 0
            and not _inventory_lot_ids_from_observation(observation)
        ):
            return _failed(
                "NO_ELIGIBLE_INVENTORY", "no eligible inventory lots are available"
            )
        return {
            "observations": [*state["observations"], observation],
            "status": RunStatus.PLANNING,
        }

    async def _finalize(self, state: AgentState) -> dict:
        decision = state["decision"]
        if not isinstance(decision, FinalProposalDecision):
            return _failed("HARNESS_ERROR", "workflow has no proposal to finalize")
        inventory_ids = _inventory_lot_ids(state)
        proposal_ids = {lot.lot_id for lot in decision.proposal.lots}
        if not proposal_ids.issubset(inventory_ids):
            await self._event_sink(
                "proposal.validated", {"status": "failed", "count": len(proposal_ids)}
            )
            if state.get("validation_retry_count", 0) < 1:
                return {
                    "validation_retry_count": 1,
                    "observations": [
                        *state["observations"],
                        Observation(
                            tool="proposal.validation",
                            evidence={
                                "allowed_lot_ids": sorted(inventory_ids),
                                "reason": "proposal contains a lot absent from inventory observations",
                            },
                        ),
                    ],
                    "decision": None,
                    "status": RunStatus.PLANNING,
                    "error": None,
                }
            return _failed(
                "UNGROUNDED_PROPOSAL",
                "proposal contains a lot absent from inventory observations",
            )
        constraint_error = _proposal_constraint_error(state, decision.proposal)
        if constraint_error is not None:
            await self._event_sink(
                "proposal.validated", {"status": "failed", "count": len(proposal_ids)}
            )
            repaired = _repair_proposal_selection(state, decision.proposal)
            if (
                repaired is not None
                and _proposal_constraint_error(state, repaired) is None
            ):
                await self._event_sink(
                    "proposal.validated",
                    {"status": "completed", "count": len(repaired.lots)},
                )
                return {
                    "proposal": repaired,
                    "status": RunStatus.COMPLETED,
                    "error": None,
                }
            if state.get("validation_retry_count", 0) < 1:
                return {
                    "validation_retry_count": 1,
                    "observations": [
                        *state["observations"],
                        Observation(tool="proposal.validation", evidence=constraint_error),
                    ],
                    "decision": None,
                    "status": RunStatus.PLANNING,
                    "error": None,
                }
            return _failed(
                "UNSATISFIED_PLAN_CONSTRAINTS",
                "proposal does not satisfy the requested live-session constraints",
            )
        await self._event_sink(
            "proposal.validated", {"status": "completed", "count": len(proposal_ids)}
        )
        return {
            "proposal": decision.proposal,
            "status": RunStatus.COMPLETED,
            "error": None,
        }

    async def _fail(self, state: AgentState) -> dict:
        del state
        return {}

    @staticmethod
    def _route_after_skill(state: AgentState) -> str:
        return "fail" if state["status"] is RunStatus.FAILED else "execute_tool"

    @staticmethod
    def _route_after_decide(state: AgentState) -> str:
        if state["status"] in {RunStatus.FAILED, RunStatus.CANCELLED}:
            return "fail"
        if isinstance(state["decision"], FinalProposalDecision):
            return "finalize"
        return "execute_tool"

    @staticmethod
    def _route_after_tool(state: AgentState) -> str:
        if state["status"] in {RunStatus.FAILED, RunStatus.CANCELLED}:
            return "fail"
        return "decide"

    @staticmethod
    def _route_after_finalize(state: AgentState) -> str:
        if state["status"] is RunStatus.PLANNING:
            return "decide"
        return "end"

    def _skill_from_state(self, state: AgentState) -> LoadedSkill:
        skill_name = state["skill_name"]
        if skill_name != _SKILL_NAME:
            raise ValueError("checkpoint references an unsupported skill")
        return self._skills.load(skill_name)

    async def _model_decide(
        self, context: AgentContext, *, max_output_tokens: int
    ):
        parameters = inspect.signature(self._model.decide).parameters
        if "max_output_tokens" in parameters:
            return await self._model.decide(
                context, max_output_tokens=max_output_tokens
            )
        return await self._model.decide(context)


def _skill_prompt(skill: LoadedSkill) -> str:
    sections = [skill.instructions.content]
    sections.extend(item.content for item in skill.references)
    sections.extend(item.content for item in skill.examples)
    return "\n\n".join(sections)


def _inventory_lot_ids(state: AgentState) -> set[str]:
    lot_ids: set[str] = set()
    for observation in state["observations"]:
        lot_ids.update(_inventory_lot_ids_from_observation(observation))
    return lot_ids


def _inventory_lot_ids_from_observation(observation: Observation) -> set[str]:
    if observation.tool != "inventory.query":
        return set()
    lots = observation.evidence.get("lots")
    if not isinstance(lots, list):
        return set()
    return {
        lot_id.strip()
        for lot in lots
        if isinstance(lot, dict)
        and isinstance((lot_id := lot.get("id")), str)
        and lot_id.strip()
    }


def _proposal_constraint_error(state: AgentState, proposal) -> dict | None:
    inventory = _inventory_lots(state)
    target_seconds = _target_duration_seconds(state["run"].goal)
    required_ids = _required_lot_ids(state["run"].goal, inventory)
    selected_ids = [lot.lot_id.strip() for lot in proposal.lots]
    reasons: list[str] = []
    missing_required = sorted(required_ids.difference(selected_ids))
    if missing_required:
        reasons.append("proposal omits a user-mandated inventory lot")

    actual_seconds = 0
    durations_known = True
    for planned in proposal.lots:
        evidence = inventory.get(planned.lot_id.strip(), {})
        duration = evidence.get("duration_seconds")
        if not isinstance(duration, int) or isinstance(duration, bool) or duration <= 0:
            durations_known = False
            continue
        actual_seconds += duration
        if planned.duration_seconds != duration:
            reasons.append("proposal rewrites an inventory lot duration")

    best_seconds: int | None = None
    if target_seconds is not None:
        if not durations_known:
            reasons.append("inventory duration evidence is incomplete")
        else:
            available = {
                lot_id: value["duration_seconds"]
                for lot_id, value in inventory.items()
                if isinstance(value.get("duration_seconds"), int)
                and not isinstance(value.get("duration_seconds"), bool)
                and value["duration_seconds"] > 0
            }
            best_seconds = _nearest_duration_sum(
                available, target_seconds, required_ids
            )
            if best_seconds is not None and abs(actual_seconds - target_seconds) > abs(
                best_seconds - target_seconds
            ):
                reasons.append("proposal is not the closest feasible duration combination")
            expected_minutes = max(1, round(actual_seconds / 60))
            if proposal.estimated_duration_minutes != expected_minutes:
                reasons.append("estimated duration does not equal the selected inventory lots")
    if not reasons:
        return None
    return {
        "reason": "; ".join(dict.fromkeys(reasons)),
        "target_duration_seconds": target_seconds,
        "actual_duration_seconds": actual_seconds if durations_known else None,
        "closest_feasible_duration_seconds": best_seconds,
        "required_lot_ids": sorted(required_ids),
        "selected_lot_ids": selected_ids,
        "allowed_lot_ids": sorted(inventory),
    }


def _inventory_lots(state: AgentState) -> dict[str, dict]:
    result: dict[str, dict] = {}
    for observation in state["observations"]:
        if observation.tool != "inventory.query":
            continue
        lots = observation.evidence.get("lots")
        if not isinstance(lots, list):
            continue
        for lot in lots:
            if not isinstance(lot, dict):
                continue
            lot_id = lot.get("id")
            if isinstance(lot_id, str) and lot_id.strip():
                result[lot_id.strip()] = lot
    return result


def _target_duration_seconds(goal: str) -> int | None:
    normalized = re.sub(r"\s+", "", goal)
    match = re.search(r"(\d+(?:\.\d+)?)个?小时", normalized)
    if match:
        return round(float(match.group(1)) * 3600)
    match = re.search(r"(\d+(?:\.\d+)?)分钟", normalized)
    if match:
        return round(float(match.group(1)) * 60)
    if "半小时" in normalized:
        return 1800
    if "一个小时" in normalized or "一小时" in normalized:
        return 3600
    return None


def _required_lot_ids(goal: str, inventory: dict[str, dict]) -> set[str]:
    if not re.search(r"必须|一定要|务必", goal):
        return set()
    matched: dict[str, str] = {}
    for lot_id, lot in inventory.items():
        title = lot.get("title")
        if isinstance(title, str) and title.strip() and title.strip() in goal:
            matched.setdefault(title.strip(), lot_id)
    return set(matched.values())


def _nearest_duration_sum(
    durations: dict[str, int], target: int, required_ids: set[str]
) -> int | None:
    selected = _nearest_duration_lot_ids(durations, target, required_ids)
    if selected is None:
        return None
    return sum(durations[lot_id] for lot_id in selected)


def _nearest_duration_lot_ids(
    durations: dict[str, int], target: int, required_ids: set[str]
) -> set[str] | None:
    if not required_ids.issubset(durations):
        return None
    required_sum = sum(durations[lot_id] for lot_id in required_ids)
    if required_sum >= target:
        return set(required_ids)
    optional = [
        (lot_id, duration)
        for lot_id, duration in durations.items()
        if lot_id not in required_ids
    ]
    upper_bound = target + max((duration for _, duration in optional), default=0)
    selections: dict[int, set[str]] = {required_sum: set(required_ids)}
    for lot_id, duration in optional:
        for value, selected in tuple(selections.items()):
            candidate = value + duration
            if candidate > upper_bound:
                continue
            candidate_ids = {*selected, lot_id}
            current = selections.get(candidate)
            if current is None or sorted(candidate_ids) < sorted(current):
                selections[candidate] = candidate_ids
    best = min(
        selections,
        key=lambda value: (abs(value - target), value > target, value),
    )
    return selections[best]


def _repair_proposal_selection(
    state: AgentState, proposal: SessionProposal
) -> SessionProposal | None:
    """Deterministically select the closest feasible subset after one model retry.

    The model remains responsible for strategy and per-lot rationale. The harness owns
    hard constraints, so a model that repeatedly selects every inventory item must not
    turn an otherwise feasible plan into a failed user turn.
    """
    target = _target_duration_seconds(state["run"].goal)
    if target is None:
        return None
    inventory = _inventory_lots(state)
    required_ids = _required_lot_ids(state["run"].goal, inventory)
    durations = {
        lot_id: duration
        for lot_id, lot in inventory.items()
        if isinstance((duration := lot.get("duration_seconds")), int)
        and not isinstance(duration, bool)
        and duration > 0
    }
    selected_ids = _nearest_duration_lot_ids(durations, target, required_ids)
    if not selected_ids:
        return None
    planned_by_id = {lot.lot_id.strip(): lot for lot in proposal.lots}
    if not selected_ids.issubset(planned_by_id):
        return None

    selected = sorted(
        (planned_by_id[lot_id] for lot_id in selected_ids),
        key=lambda lot: lot.sequence,
    )
    repaired_lots = [
        lot.model_copy(
            update={
                "sequence": index,
                "duration_seconds": durations[lot.lot_id.strip()],
            }
        )
        for index, lot in enumerate(selected, start=1)
    ]
    actual_seconds = sum(lot.duration_seconds for lot in repaired_lots)
    warning = (
        f"库存固定时长下最接近目标的是 {max(1, round(actual_seconds / 60))} 分钟，"
        "Harness 已自动选择可执行组合"
    )
    return proposal.model_copy(
        update={
            "estimated_duration_minutes": max(1, round(actual_seconds / 60)),
            "lots": repaired_lots,
            "risks": list(dict.fromkeys([*proposal.risks, warning])),
        }
    )


def _failed(code: str, message: str) -> dict:
    return {"status": RunStatus.FAILED, "error": RunError(code=code, message=message)}


def _cancelled() -> dict:
    return {
        "status": RunStatus.CANCELLED,
        "error": RunError(code="RUN_CANCELLED", message="run cancelled"),
    }


def _duration_bucket(started: float) -> str:
    elapsed = monotonic() - started
    if elapsed < 0.1:
        return "lt_100ms"
    if elapsed < 1:
        return "lt_1s"
    if elapsed < 10:
        return "lt_10s"
    return "gte_10s"
