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
    "set_content",
    "admit",
    "Verdict",
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

# Content capture (prompt/response bodies attached to spans) is ON by default.
# Turn it off per-process via init(capture_content=False) or the
# OBSERVER_CAPTURE_CONTENT env var (0/false/no/off). A defensive per-body char
# cap keeps a giant prompt from bloating the OTLP export — Observer's ingest
# imposes only a 512-message count cap, no byte cap, so the client owns this.
DEFAULT_MAX_CONTENT_CHARS = 8000

_TRACER = None  # set by init(); None → spans are cheap no-ops via the OTel API.
_PROVIDER = None
_CAPTURE_CONTENT = True
_MAX_CONTENT_CHARS = DEFAULT_MAX_CONTENT_CHARS


def _env_flag(name: str, default: bool) -> bool:
    """Parse a boolean-ish env var; unset → default."""
    raw = os.environ.get(name)
    if raw is None:
        return default
    return raw.strip().lower() not in ("0", "false", "no", "off", "")


def _truncate(value: str) -> str:
    """Clip a content body to the configured per-body char cap (marking it)."""
    if _MAX_CONTENT_CHARS > 0 and len(value) > _MAX_CONTENT_CHARS:
        return value[:_MAX_CONTENT_CHARS] + "…[truncated]"
    return value

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
    capture_content: Optional[bool] = None,
    max_content_chars: int = DEFAULT_MAX_CONTENT_CHARS,
) -> None:
    """Configure the global tracer to export to Observer over OTLP/HTTP.

    endpoint defaults to ``$SUPERBASED_OTLP_ENDPOINT`` or
    ``http://127.0.0.1:4318/v1/traces`` (the OTLP receiver's loopback bind).
    tenant/user/session_id become resource attributes Observer reads onto the
    trace. Call once at process start. Safe to import without OpenTelemetry
    installed; init() is where the dependency is required.

    capture_content controls whether prompt/response bodies passed to the span
    helpers are attached to spans (ON by default). Resolution order:
    explicit arg → ``$OBSERVER_CAPTURE_CONTENT`` (0/false/no/off disables) →
    default True. max_content_chars clips each body client-side (0 = no clip).
    """
    global _TRACER, _PROVIDER, _CAPTURE_CONTENT, _MAX_CONTENT_CHARS

    _CAPTURE_CONTENT = _env_flag("OBSERVER_CAPTURE_CONTENT", True) if capture_content is None else bool(capture_content)
    _MAX_CONTENT_CHARS = int(max_content_chars)

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
    prompt: Optional[str] = None,
    attributes: Optional[dict] = None,
) -> Iterator[Any]:
    """Open an LLM span pre-tagged with model/provider in BOTH the GenAI and
    OpenInference vocabularies so Observer maps it cleanly regardless of which
    convention it prefers. Set token usage inside via set_usage.

    prompt (when content capture is enabled) is attached as the request body;
    pass the response after the call via set_usage(..., response=...) or
    set_content(sp, response=...)."""
    attrs: dict = {}
    if model:
        attrs["gen_ai.request.model"] = model
        attrs["llm.model_name"] = model
    if provider:
        attrs["gen_ai.system"] = provider
        attrs["llm.provider"] = provider
    if prompt is not None and _CAPTURE_CONTENT:
        # input.value ∈ ingest keysPromptValue (llm) and keysToolArgs (tool);
        # extractContent disambiguates by span kind (ingest.go:91-94, 416).
        attrs["input.value"] = _truncate(str(prompt))
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
    prompt: Optional[str] = None,
    response: Optional[str] = None,
) -> None:
    """Record token usage / ids on an LLM span using the canonical keys
    Observer reconciles against api_turns. response_id (the provider message
    id, e.g. Anthropic ``msg_…`` or OpenAI ``chatcmpl_…``) is what the proxy
    commonly stored, so it doubles as the dedup key when no request_id is set.

    prompt/response, when passed and content capture is enabled, attach the
    request/response bodies (a convenience wrapper over set_content).
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
    if prompt is not None or response is not None:
        set_content(sp, prompt=prompt, response=response)


def set_content(
    sp: Any,
    *,
    prompt: Optional[str] = None,
    response: Optional[str] = None,
    tool_args: Optional[str] = None,
    tool_result: Optional[str] = None,
) -> None:
    """Attach prompt/response (LLM span) or tool args/result (TOOL span) bodies
    to a span so Observer can persist the conversation content.

    No-op when content capture is disabled (init(capture_content=False) or
    ``OBSERVER_CAPTURE_CONTENT=0``). Each body is clipped to max_content_chars.

    The request body is written to ``input.value`` and the response body to
    ``output.value`` — flat keys the ingest reads for BOTH LLM and TOOL spans,
    disambiguated by span kind at ingestion (ingest.go:91-94 keysPromptValue /
    keysResponseValue / keysToolArgs / keysToolResult; promptBody/responseBody
    fallbacks at ingest.go:416,427).
    """
    if not _CAPTURE_CONTENT:
        return
    req = prompt if prompt is not None else tool_args
    res = response if response is not None else tool_result
    if req is not None:
        sp.set_attribute("input.value", _truncate(str(req)))
    if res is not None:
        sp.set_attribute("output.value", _truncate(str(res)))


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


DEFAULT_ADMISSION_ENDPOINT = "http://127.0.0.1:8081/api/obs/admission/check"


class Verdict:
    """The result of an ``admit()`` check.

    ``allowed`` is what the app should honor. In ``observe`` mode the server
    always returns ``allowed=True`` and records the shadow verdict; in
    ``enforce`` mode it returns the real decision (``ask``/``deny`` →
    ``allowed=False``, while ``flag`` still admits). ``enforce_decision`` always
    previews what enforce mode would decide, so you can log/measure while still
    in observe before flipping. ``decision`` is the raw verdict
    (allow/flag/ask/deny), ``reason`` a short string to surface to the user.
    """

    def __init__(self, data: dict):
        self.allowed: bool = bool(data.get("allowed", True))
        self.decision: str = data.get("decision", "allow")
        self.severity: str = data.get("severity", "")
        self.criterion: str = data.get("criterion", "")
        self.reason: str = data.get("reason", "")
        self.mode: str = data.get("mode", "")
        self.judge_used: bool = bool(data.get("judge_used", False))
        self.degraded: str = data.get("degraded", "")
        self.enforce_decision: str = data.get("enforce_decision", "")
        self.latency_ms: int = int(data.get("latency_ms", 0))

    def __repr__(self) -> str:
        return (
            f"Verdict(allowed={self.allowed}, decision={self.decision!r}, "
            f"enforce_decision={self.enforce_decision!r}, reason={self.reason!r})"
        )


def admit(
    message: str,
    *,
    tenant: Optional[str] = None,
    user: Optional[str] = None,
    session: Optional[str] = None,
    trace_id: Optional[str] = None,
    request_id: Optional[str] = None,
    endpoint: Optional[str] = None,
    timeout: float = 3.0,
) -> Verdict:
    """Check an incoming user message against the co-resident app's admission
    policy BEFORE running the agent, and get an allow/flag/ask/deny verdict.

    Call this at your app's front door::

        v = superbased.admit(user_message, user="alice", session="s1")
        if not v.allowed:
            return v.reason  # or raise / route to a human on `ask`

    Fails OPEN by design: any transport error returns an allow Verdict so a
    down/unreachable Observer never blocks your app (the server applies its own
    fail-open/closed judge policy separately). ``endpoint`` defaults to
    ``$SUPERBASED_ADMISSION_ENDPOINT`` or the local dashboard
    (``http://127.0.0.1:8081/api/obs/admission/check``). Uses only stdlib —
    no extra dependency.
    """
    import json
    import urllib.error
    import urllib.request

    url = endpoint or os.environ.get("SUPERBASED_ADMISSION_ENDPOINT", DEFAULT_ADMISSION_ENDPOINT)
    payload = json.dumps(
        {
            "text": message,
            "tenant": tenant or "",
            "user": user or "",
            "session": session or "",
            "trace_id": trace_id or "",
            "request_id": request_id or "",
        }
    ).encode("utf-8")
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return Verdict(data)
    except (urllib.error.URLError, TimeoutError, ValueError, OSError):
        # Fail open: never block the app on a transport/parse error.
        return Verdict({"allowed": True, "decision": "allow", "degraded": "client-failopen"})


def shutdown() -> None:
    """Flush + shut down the exporter. Call before a short-lived process exits
    so the final batch is sent."""
    if _PROVIDER is not None:
        _PROVIDER.shutdown()
