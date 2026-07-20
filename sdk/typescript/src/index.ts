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
const DEFAULT_MAX_CONTENT_CHARS = 8000;

let provider: NodeTracerProvider | null = null;

// Content capture (prompt/response bodies attached to spans) is ON by default.
// Turn it off per-process via init({ captureContent: false }) or the
// OBSERVER_CAPTURE_CONTENT env var (0/false/no/off). A defensive per-body char
// cap keeps a giant prompt from bloating the OTLP export — Observer's ingest
// imposes only a 512-message count cap, no byte cap, so the client owns this.
let captureContent = true;
let maxContentChars = DEFAULT_MAX_CONTENT_CHARS;

function envFlag(name: string, def: boolean): boolean {
  const raw = process.env[name];
  if (raw == null) return def;
  return !["0", "false", "no", "off", ""].includes(raw.trim().toLowerCase());
}

function truncateContent(value: string): string {
  if (maxContentChars > 0 && value.length > maxContentChars) {
    return value.slice(0, maxContentChars) + "…[truncated]";
  }
  return value;
}

export interface InitOptions {
  /** OTLP/HTTP traces endpoint. Defaults to $SUPERBASED_OTLP_ENDPOINT or the loopback receiver. */
  endpoint?: string;
  tenant?: string;
  user?: string;
  sessionId?: string;
  serviceName?: string;
  headers?: Record<string, string>;
  /**
   * Attach prompt/response bodies passed to the span helpers (ON by default).
   * Resolution: this option → $OBSERVER_CAPTURE_CONTENT (0/false/no/off
   * disables) → default true.
   */
  captureContent?: boolean;
  /** Client-side per-body char cap (default 8000; 0 = no clip). */
  maxContentChars?: number;
}

/** Configure the global tracer to export to Observer over OTLP/HTTP. Call once at startup. */
export function init(opts: InitOptions = {}): void {
  const endpoint =
    opts.endpoint ?? process.env.SUPERBASED_OTLP_ENDPOINT ?? DEFAULT_ENDPOINT;

  captureContent =
    opts.captureContent ?? envFlag("OBSERVER_CAPTURE_CONTENT", true);
  maxContentChars = opts.maxContentChars ?? DEFAULT_MAX_CONTENT_CHARS;

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
  /** Request body; attached as content when capture is enabled. */
  prompt?: string;
}

/** withSpan specialized for an LLM call: pre-tags model/provider in both the
 * GenAI and OpenInference vocabularies so Observer maps it cleanly. Pass the
 * response body after the call via setUsage({ response }) or setContent(). */
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
  if (info.prompt != null && captureContent) {
    // input.value ∈ ingest keysPromptValue (llm) and keysToolArgs (tool);
    // extractContent disambiguates by span kind (ingest.go:91-94, 416).
    attrs["input.value"] = truncateContent(String(info.prompt));
  }
  return withSpan(name, Kind.LLM, fn, attrs);
}

export interface Usage {
  inputTokens?: number;
  outputTokens?: number;
  responseId?: string;
  requestId?: string;
  costUsd?: number;
  /** Request body; attached as content when capture is enabled. */
  prompt?: string;
  /** Response body; attached as content when capture is enabled. */
  response?: string;
}

/** Record token usage / ids on an LLM span using the canonical keys Observer
 * reconciles against api_turns. `responseId` (provider message id) doubles as
 * the dedup key when no `requestId` is set. `prompt`/`response`, when passed
 * and capture is enabled, attach the request/response bodies. */
export function setUsage(span: Span, u: Usage): void {
  if (u.inputTokens != null) span.setAttribute("gen_ai.usage.input_tokens", u.inputTokens);
  if (u.outputTokens != null) span.setAttribute("gen_ai.usage.output_tokens", u.outputTokens);
  if (u.responseId) span.setAttribute("gen_ai.response.id", u.responseId);
  if (u.requestId) span.setAttribute("request_id", u.requestId);
  if (u.costUsd != null) span.setAttribute("gen_ai.usage.cost", u.costUsd);
  if (u.prompt != null || u.response != null) {
    setContent(span, { prompt: u.prompt, response: u.response });
  }
}

export interface Content {
  /** LLM request body (or, on a TOOL span, the tool args). */
  prompt?: string;
  /** LLM response body (or, on a TOOL span, the tool result). */
  response?: string;
  /** Alias for `prompt` on TOOL spans. */
  toolArgs?: string;
  /** Alias for `response` on TOOL spans. */
  toolResult?: string;
}

/** Attach prompt/response (LLM span) or tool args/result (TOOL span) bodies to
 * a span so Observer can persist the conversation content. No-op when capture
 * is disabled (init({ captureContent: false }) or OBSERVER_CAPTURE_CONTENT=0);
 * each body is clipped to maxContentChars.
 *
 * The request body is written to `input.value` and the response body to
 * `output.value` — flat keys the ingest reads for BOTH LLM and TOOL spans,
 * disambiguated by span kind at ingestion (ingest.go:91-94 keysPromptValue /
 * keysResponseValue / keysToolArgs / keysToolResult; promptBody/responseBody
 * fallbacks at ingest.go:416,427). */
export function setContent(span: Span, c: Content): void {
  if (!captureContent) return;
  const req = c.prompt ?? c.toolArgs;
  const res = c.response ?? c.toolResult;
  if (req != null) span.setAttribute("input.value", truncateContent(String(req)));
  if (res != null) span.setAttribute("output.value", truncateContent(String(res)));
}

/** Flush + shut down the exporter. Await before a short-lived process exits. */
export async function shutdown(): Promise<void> {
  if (provider) await provider.shutdown();
}

// --- Input admission (admission spec §6.1) -----------------------------------

/** Default admission-check endpoint: the node obs API on the dashboard port. */
export const DEFAULT_ADMISSION_ENDPOINT =
  "http://127.0.0.1:8081/api/obs/admission/check";

/** The result of an {@link admit} check. */
export interface Verdict {
  /**
   * What the app should honor. In `observe` mode the server always returns
   * `true` and records the shadow verdict; in `enforce` mode it returns the
   * real decision (`ask`/`deny` → `false`, `flag` still admits).
   */
  allowed: boolean;
  /** The raw decision, one of: allow / flag / ask / deny. */
  decision: string;
  severity: string;
  /** The criterion id that fired, if any. */
  criterion: string;
  /** A short string to surface to the user on a block. */
  reason: string;
  mode: string;
  /** What enforce mode WOULD decide, so you can measure before flipping. */
  enforceDecision: string;
  degraded: string;
}

export interface AdmitOptions {
  tenant?: string;
  user?: string;
  session?: string;
  traceId?: string;
  requestId?: string;
  /** Admission-check endpoint. Defaults to $SUPERBASED_ADMISSION_ENDPOINT or the local dashboard. */
  endpoint?: string;
  /** Per-call timeout in ms (default 3000). */
  timeoutMs?: number;
}

/**
 * Check an incoming user message against the co-resident app's admission
 * policy BEFORE running the agent, and get an allow/flag/ask/deny verdict.
 *
 * ```ts
 * const v = await admit(userMessage, { user: "alice", session: "s1" });
 * if (!v.allowed) return v.reason; // or raise / route to a human on `ask`
 * ```
 *
 * Fails OPEN by design: any transport error resolves to an allow verdict so a
 * down/unreachable Observer never blocks your app (the server applies its own
 * fail-open/closed judge policy separately).
 */
export async function admit(
  message: string,
  opts: AdmitOptions = {},
): Promise<Verdict> {
  const url =
    opts.endpoint ??
    process.env.SUPERBASED_ADMISSION_ENDPOINT ??
    DEFAULT_ADMISSION_ENDPOINT;
  const failOpen: Verdict = {
    allowed: true,
    decision: "allow",
    severity: "",
    criterion: "",
    reason: "",
    mode: "",
    enforceDecision: "",
    degraded: "client-failopen",
  };
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? 3000);
  try {
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        text: message,
        tenant: opts.tenant ?? "",
        user: opts.user ?? "",
        session: opts.session ?? "",
        trace_id: opts.traceId ?? "",
        request_id: opts.requestId ?? "",
      }),
      signal: controller.signal,
    });
    if (!resp.ok) return failOpen;
    const d = (await resp.json()) as Record<string, unknown>;
    return {
      allowed: d.allowed !== false,
      decision: typeof d.decision === "string" ? d.decision : "allow",
      severity: typeof d.severity === "string" ? d.severity : "",
      criterion: typeof d.criterion === "string" ? d.criterion : "",
      reason: typeof d.reason === "string" ? d.reason : "",
      mode: typeof d.mode === "string" ? d.mode : "",
      enforceDecision:
        typeof d.enforce_decision === "string" ? d.enforce_decision : "",
      degraded: typeof d.degraded === "string" ? d.degraded : "",
    };
  } catch {
    // Fail open: never block the app on a transport/parse error.
    return failOpen;
  } finally {
    clearTimeout(timer);
  }
}
