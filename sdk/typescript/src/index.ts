/**
 * SuperBased Observer — thin TypeScript/Node SDK for sending custom app /
 * agent traces. A convenience layer over OpenTelemetry that points an OTLP
 * exporter at Observer's `/v1/traces` and tags spans with SuperBased resource
 * attributes. Design test (plan §6): anything this does, a raw OTel exporter
 * pointed at Observer does too.
 *
 * IMPORTANT: never set `sbo.emitted_by=observer` — that is Observer's own
 * echo-guard marker and such a resource is DROPPED at ingestion.
 */

import { SpanStatusCode, trace, type Span } from "@opentelemetry/api";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { Resource } from "@opentelemetry/resources";
import { BatchSpanProcessor, NodeTracerProvider } from "@opentelemetry/sdk-trace-node";

/** Canonical OpenInference span kinds (Observer normalizes either case). */
export const Kind = {
  LLM: "LLM",
  TOOL: "TOOL",
  RETRIEVER: "RETRIEVER",
  EMBEDDING: "EMBEDDING",
  CHAIN: "CHAIN",
  AGENT: "AGENT",
  GUARDRAIL: "GUARDRAIL",
  EVALUATOR: "EVALUATOR",
} as const;
export type SpanKind = (typeof Kind)[keyof typeof Kind];

const DEFAULT_ENDPOINT = "http://127.0.0.1:4318/v1/traces";
const TRACER_NAME = "superbased";

let provider: NodeTracerProvider | null = null;

export interface InitOptions {
  /** OTLP/HTTP traces endpoint. Defaults to $SUPERBASED_OTLP_ENDPOINT or the loopback receiver. */
  endpoint?: string;
  tenant?: string;
  user?: string;
  sessionId?: string;
  serviceName?: string;
  headers?: Record<string, string>;
}

/** Configure the global tracer to export to Observer over OTLP/HTTP. Call once at startup. */
export function init(opts: InitOptions = {}): void {
  const endpoint =
    opts.endpoint ?? process.env.SUPERBASED_OTLP_ENDPOINT ?? DEFAULT_ENDPOINT;

  const attrs: Record<string, string> = {
    "service.name": opts.serviceName ?? "custom-app",
    "sbo.sdk": "superbased-typescript",
  };
  if (opts.tenant) attrs["sbo.tenant"] = opts.tenant;
  if (opts.user) attrs["sbo.user"] = opts.user;
  if (opts.sessionId) attrs["session.id"] = opts.sessionId;

  provider = new NodeTracerProvider({ resource: new Resource(attrs) });
  provider.addSpanProcessor(
    new BatchSpanProcessor(new OTLPTraceExporter({ url: endpoint, headers: opts.headers })),
  );
  provider.register();
}

function tracer() {
  return trace.getTracer(TRACER_NAME);
}

/**
 * Run `fn` inside a span of the given kind, recording exceptions + ERROR
 * status automatically. Returns whatever `fn` returns (awaited).
 */
export async function withSpan<T>(
  name: string,
  kind: SpanKind,
  fn: (span: Span) => Promise<T> | T,
  attributes: Record<string, unknown> = {},
): Promise<T> {
  return tracer().startActiveSpan(
    name,
    { attributes: { "openinference.span.kind": kind, ...attributes } },
    async (span) => {
      try {
        const out = await fn(span);
        span.setStatus({ code: SpanStatusCode.OK });
        return out;
      } catch (err) {
        span.recordException(err as Error);
        span.setStatus({ code: SpanStatusCode.ERROR, message: String(err) });
        throw err;
      } finally {
        span.end();
      }
    },
  );
}

export interface LlmInfo {
  model?: string;
  provider?: string;
}

/** withSpan specialized for an LLM call: pre-tags model/provider in both the
 * GenAI and OpenInference vocabularies so Observer maps it cleanly. */
export async function withLlmSpan<T>(
  name: string,
  info: LlmInfo,
  fn: (span: Span) => Promise<T> | T,
): Promise<T> {
  const attrs: Record<string, unknown> = {};
  if (info.model) {
    attrs["gen_ai.request.model"] = info.model;
    attrs["llm.model_name"] = info.model;
  }
  if (info.provider) {
    attrs["gen_ai.system"] = info.provider;
    attrs["llm.provider"] = info.provider;
  }
  return withSpan(name, Kind.LLM, fn, attrs);
}

export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  responseId?: string;
  requestId?: string;
  costUsd?: number;
}

/** Record token usage / ids on an LLM span using the canonical keys Observer
 * reconciles against api_turns. `responseId` (provider message id) doubles as
 * the dedup key when no `requestId` is set. */
export function setUsage(span: Span, u: Usage): void {
  if (u.inputTokens != null) span.setAttribute("gen_ai.usage.input_tokens", u.inputTokens);
  if (u.outputTokens != null) span.setAttribute("gen_ai.usage.output_tokens", u.outputTokens);
  if (u.responseId) span.setAttribute("gen_ai.response.id", u.responseId);
  if (u.requestId) span.setAttribute("request_id", u.requestId);
  if (u.costUsd != null) span.setAttribute("gen_ai.usage.cost", u.costUsd);
}

/** Flush + shut down the exporter. Await before a short-lived process exits. */
export async function shutdown(): Promise<void> {
  if (provider) await provider.shutdown();
}
