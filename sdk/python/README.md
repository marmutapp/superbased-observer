# SuperBased Observer — Python SDK

A thin convenience layer over OpenTelemetry that sends your custom app /
agent traces to a local SuperBased Observer via OTLP, so they show up on the
**Trajectories** dashboard alongside Observer's proxy-accurate cost, cache,
and routing data.

> **Provisional package name** (`superbased-observer-sdk`, import `superbased`)
> — the main Observer distribution already owns the `superbased-observer` name
> on PyPI/npm. Final SDK naming is an open decision (plan §15 Q1).

## Install

```bash
pip install superbased-observer-sdk          # or: pip install -e sdk/python
```

Optional framework auto-capture via Arize OpenInference instrumentors:

```bash
pip install "superbased-observer-sdk[langchain]"   # or [llama-index] / [crewai] / [openai]
```

## Prerequisites

Run a local Observer with the observability subsystem on:

```toml
# ~/.observer/config.toml
[observability]
enabled = true
```

The OTLP receiver listens on `127.0.0.1:4318` (HTTP) / `:4317` (gRPC) by
default; the SDK posts to `http://127.0.0.1:4318/v1/traces`.

## Use

```python
import superbased

superbased.init(session_id="run-42", user="alice")     # once at startup

with superbased.span("plan", kind=superbased.AGENT):
    with superbased.llm_span("chat", model="gpt-4o", provider="openai") as s:
        ...  # your model call
        superbased.set_usage(s, input_tokens=1200, output_tokens=80,
                             response_id="chatcmpl-abc")
    with superbased.span("search", kind=superbased.TOOL):
        ...

superbased.shutdown()    # flush before a short-lived process exits
```

Auto-instrument a framework (after `init()`):

```python
from openinference.instrumentation.langchain import LangChainInstrumentor
LangChainInstrumentor().instrument()
```

## How it maps

The SDK tags LLM spans in **both** the OTel GenAI (`gen_ai.*`) and Arize
OpenInference (`llm.*` / `openinference.span.kind`) vocabularies, so Observer's
boundary mapper normalizes them regardless of which convention it reads.
`response_id` (the provider message id) doubles as the dedup key against the
proxy's `api_turns` when no explicit `request_id` is set.

## Design test

Anything this SDK does, a stock OTel exporter pointed at Observer does too —
see [`examples/raw_otlp.py`](examples/raw_otlp.py). The SDK is pure ergonomics;
direct third-party OTLP exporters are a supported tier (plan §15 Q2).

## Echo-guard caveat

Never set the resource attribute `sbo.emitted_by=observer` — that is
Observer's own marker for telemetry it emitted, and any resource carrying it is
**dropped** at ingestion. The SDK sets `sbo.sdk="superbased-python"` instead.
