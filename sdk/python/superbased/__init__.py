"""SuperBased Observer — thin Python SDK for sending agent/app traces.

A convenience layer over standard OpenTelemetry tracing that points an OTLP
exporter at Observer's ``/v1/traces`` endpoint and tags spans with SuperBased
resource attributes. The design test (plan §6): anything this SDK does, a raw
OTel exporter pointed at Observer can do too — so this stays thin.

Quick start::

    import superbased

    superbased.init(session_id="run-42", user="alice")

    with superbased.span("plan", kind=superbased.AGENT):
        with superbased.llm_span("chat", model="gpt-4o", provider="openai") as s:
            ...  # call your model
            superbased.set_usage(s, input_tokens=1200, output_tokens=80,
                                 response_id="chatcmpl-abc")

To capture a framework automatically, reuse an OpenInference instrumentor
after ``init()`` (installed via an extra, e.g. ``pip install
superbased-observer-sdk[langchain]``)::

    from openinference.instrumentation.langchain import LangChainInstrumentor
    LangChainInstrumentor().instrument()

IMPORTANT: this SDK never sets ``sbo.emitted_by=observer`` — that value is
Observer's own echo-guard marker; a resource carrying it is DROPPED at
ingestion. Your app's traces must not use it.
"""

from __future__ import annotations

import functools
import os
from contextlib import contextmanager
from typing import Any, Callable, Iterator, Optional, TypeVar

__all__ = [
    "init",
    "span",
    "llm_span",
    "observe",
    "set_usage",
    "shutdown",
    "LLM",
    "TOOL",
    "RETRIEVER",
    "EMBEDDING",
    "CHAIN",
    "AGENT",
    "GUARDRAIL",
    "EVALUATOR",
]

# Canonical OpenInference span kinds (uppercase — Observer's mapper normalizes
# either case). Pass one of these as ``kind=`` to span()/observe().
LLM = "LLM"
TOOL = "TOOL"
RETRIEVER = "RETRIEVER"
EMBEDDING = "EMBEDDING"
CHAIN = "CHAIN"
AGENT = "AGENT"
GUARDRAIL = "GUARDRAIL"
EVALUATOR = "EVALUATOR"

DEFAULT_ENDPOINT = "http://127.0.0.1:4318/v1/traces"

_TRACER = None  # set by init(); None → spans are cheap no-ops via the OTel API.
_PROVIDER = None

F = TypeVar("F", bound=Callable[..., Any])


def init(
    *,
    endpoint: Optional[str] = None,
    tenant: Optional[str] = None,
    user: Optional[str] = None,
    session_id: Optional[str] = None,
    service_name: str = "custom-app",
    headers: Optional[dict] = None,
    console: bool = False,
) -> None:
    """Configure the global tracer to export to Observer over OTLP/HTTP.

    endpoint defaults to ``$SUPERBASED_OTLP_ENDPOINT`` or
    ``http://127.0.0.1:4318/v1/traces`` (the OTLP receiver's loopback bind).
    tenant/user/session_id become resource attributes Observer reads onto the
    trace. Call once at process start. Safe to import without OpenTelemetry
    installed; init() is where the dependency is required.
    """
    global _TRACER, _PROVIDER

    from opentelemetry import trace
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
        OTLPSpanExporter,
    )
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    ep = endpoint or os.environ.get("SUPERBASED_OTLP_ENDPOINT", DEFAULT_ENDPOINT)

    attrs = {"service.name": service_name, "sbo.sdk": "superbased-python"}
    if tenant:
        attrs["sbo.tenant"] = tenant
    if user:
        attrs["sbo.user"] = user
    if session_id:
        attrs["session.id"] = session_id

    provider = TracerProvider(resource=Resource.create(attrs))
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=ep, headers=headers)))
    if console:
        from opentelemetry.sdk.trace.export import (
            ConsoleSpanExporter,
            SimpleSpanProcessor,
        )

        provider.add_span_processor(SimpleSpanProcessor(ConsoleSpanExporter()))

    trace.set_tracer_provider(provider)
    _PROVIDER = provider
    _TRACER = trace.get_tracer("superbased")


def _tracer():
    if _TRACER is not None:
        return _TRACER
    # Fall back to the API's (possibly no-op) tracer so spans never crash a
    # caller who forgot init().
    from opentelemetry import trace

    return trace.get_tracer("superbased")


@contextmanager
def span(name: str, kind: str = CHAIN, attributes: Optional[dict] = None) -> Iterator[Any]:
    """Open a span of the given OpenInference kind as a context manager.

    Records exceptions and sets ERROR status automatically; the yielded span
    can carry more attributes (see set_usage for LLM spans).
    """
    from opentelemetry.trace import Status, StatusCode

    attrs = {"openinference.span.kind": kind}
    if attributes:
        attrs.update(attributes)
    with _tracer().start_as_current_span(name, attributes=attrs) as sp:
        try:
            yield sp
        except Exception as exc:  # noqa: BLE001 — re-raised after recording
            sp.record_exception(exc)
            sp.set_status(Status(StatusCode.ERROR, str(exc)))
            raise
        else:
            sp.set_status(Status(StatusCode.OK))


@contextmanager
def llm_span(
    name: str,
    *,
    model: Optional[str] = None,
    provider: Optional[str] = None,
    attributes: Optional[dict] = None,
) -> Iterator[Any]:
    """Open an LLM span pre-tagged with model/provider in BOTH the GenAI and
    OpenInference vocabularies so Observer maps it cleanly regardless of which
    convention it prefers. Set token usage inside via set_usage."""
    attrs: dict = {}
    if model:
        attrs["gen_ai.request.model"] = model
        attrs["llm.model_name"] = model
    if provider:
        attrs["gen_ai.system"] = provider
        attrs["llm.provider"] = provider
    if attributes:
        attrs.update(attributes)
    with span(name, kind=LLM, attributes=attrs) as sp:
        yield sp


def set_usage(
    sp: Any,
    *,
    input_tokens: Optional[int] = None,
    output_tokens: Optional[int] = None,
    response_id: Optional[str] = None,
    request_id: Optional[str] = None,
    cost_usd: Optional[float] = None,
) -> None:
    """Record token usage / ids on an LLM span using the canonical keys
    Observer reconciles against api_turns. response_id (the provider message
    id, e.g. Anthropic ``msg_…`` or OpenAI ``chatcmpl_…``) is what the proxy
    commonly stored, so it doubles as the dedup key when no request_id is set.
    """
    if input_tokens is not None:
        sp.set_attribute("gen_ai.usage.input_tokens", int(input_tokens))
    if output_tokens is not None:
        sp.set_attribute("gen_ai.usage.output_tokens", int(output_tokens))
    if response_id:
        sp.set_attribute("gen_ai.response.id", response_id)
    if request_id:
        sp.set_attribute("request_id", request_id)
    if cost_usd is not None:
        sp.set_attribute("gen_ai.usage.cost", float(cost_usd))


def observe(name: Optional[str] = None, kind: str = CHAIN) -> Callable[[F], F]:
    """Decorator that wraps a function call in a span (name defaults to the
    function's qualified name). Works for sync functions."""

    def deco(fn: F) -> F:
        span_name = name or getattr(fn, "__qualname__", fn.__name__)

        @functools.wraps(fn)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            with span(span_name, kind=kind):
                return fn(*args, **kwargs)

        return wrapper  # type: ignore[return-value]

    return deco


def shutdown() -> None:
    """Flush + shut down the exporter. Call before a short-lived process exits
    so the final batch is sent."""
    if _PROVIDER is not None:
        _PROVIDER.shutdown()
