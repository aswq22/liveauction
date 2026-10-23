"""Minimal OpenTelemetry spans with an enforced safe attribute allowlist."""

from __future__ import annotations

from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from time import perf_counter
from typing import Any

from opentelemetry import trace
from opentelemetry.context import Context
from opentelemetry.trace import Span, Tracer
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator


_SAFE_ATTRIBUTES = frozenset(
    {
        "run_id",
        "skill",
        "tool",
        "status",
        "latency_ms",
        "input_tokens",
        "output_tokens",
        "total_tokens",
    }
)


class Tracing:
    def __init__(
        self,
        *,
        tracer: Tracer | None = None,
        parent_context: Context | None = None,
    ) -> None:
        self._tracer = tracer or trace.get_tracer("live-auction-agent-service")
        self._parent_context = parent_context if parent_context is not None else Context()

    @contextmanager
    def span(
        self,
        name: str,
        attributes: Mapping[str, Any],
        *,
        root: bool = False,
    ) -> Iterator[Span]:
        safe_attributes = {
            key: value for key, value in attributes.items() if key in _SAFE_ATTRIBUTES
        }
        started_at = perf_counter()
        context = self._parent_context if root else None
        with self._tracer.start_as_current_span(
            name,
            context=context,
            attributes=safe_attributes,
            record_exception=False,
            set_status_on_exception=False,
        ) as span:
            try:
                yield span
            finally:
                span.set_attribute("latency_ms", (perf_counter() - started_at) * 1000)


def set_safe_attribute(span: Span, key: str, value: Any) -> None:
    if key in _SAFE_ATTRIBUTES:
        span.set_attribute(key, value)


def extract_trace_context(metadata) -> Context:
    """Extract one valid W3C traceparent or return an explicit empty context."""
    values = [
        value
        for key, value in _metadata_pairs(metadata)
        if key.lower() == "traceparent"
    ]
    if len(values) != 1:
        return Context()
    extracted = TraceContextTextMapPropagator().extract(
        {"traceparent": values[0]}, context=Context()
    )
    span_context = trace.get_current_span(extracted).get_span_context()
    return extracted if span_context.is_valid else Context()


def _metadata_pairs(metadata):
    for item in metadata:
        if hasattr(item, "key") and hasattr(item, "value"):
            yield str(item.key), str(item.value)
        else:
            key, value = item
            yield str(key), str(value)
