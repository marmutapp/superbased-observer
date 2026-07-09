"""The SAME trace as quickstart.py, but with stock OpenTelemetry and NO
SuperBased SDK — the design test (plan §6): anything the SDK does, a raw OTel
exporter pointed at Observer does too. The SDK is pure convenience.

    pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
    python sdk/python/examples/raw_otlp.py
"""

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

# Resource tags Observer reads onto the trace. Do NOT set
# sbo.emitted_by=observer — that is Observer's echo-guard marker and the
# resource would be dropped at ingestion.
resource = Resource.create(
    {"service.name": "custom-app", "session.id": "raw-1", "sbo.user": "demo"}
)
provider = TracerProvider(resource=resource)
provider.add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://127.0.0.1:4318/v1/traces"))
)
trace.set_tracer_provider(provider)
tracer = trace.get_tracer("raw-example")

with tracer.start_as_current_span(
    "agent.run", attributes={"openinference.span.kind": "AGENT"}
):
    with tracer.start_as_current_span(
        "chat",
        attributes={
            "openinference.span.kind": "LLM",
            "gen_ai.system": "openai",
            "gen_ai.request.model": "gpt-4o",
            "gen_ai.usage.input_tokens": 1200,
            "gen_ai.usage.output_tokens": 64,
            "gen_ai.response.id": "chatcmpl-demo-123",
        },
    ):
        pass

provider.shutdown()
print("sent one trace to Observer — check /trajectories")
