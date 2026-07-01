// Wire types for the obs trajectory surface — co-located under pages/obs/ so
// the whole subsystem UI is one removable subtree (decision D4). Mirrors the
// Go JSON tags in internal/obs/store/read.go.

export type ObsTraceRow = {
  trace_id: string;
  root_name: string;
  source: string;
  session_id: string;
  status: string;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  span_count: number;
  cost_usd: number;
  // Per-trace token sums (Gap C). total_tokens prefers the reported total,
  // falling back to input+output.
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
};

export type ObsTracesResponse = { traces: ObsTraceRow[] };

export type ObsSpanRow = {
  span_id: string;
  parent_span_id: string;
  kind: string;
  name: string;
  status: string;
  started_at: string;
  ended_at: string;
  duration_ms: number;
  model: string;
  provider: string;
  input_tokens: number | null;
  output_tokens: number | null;
  cache_read_tokens: number | null;
  cache_write_tokens: number | null;
  reasoning_tokens: number | null;
  cost_usd: number | null;
  cost_source: string; // "reported" | "computed" | ""
  cost_detail?: ObsCostBreakdown;
  request_id: string;
};

// ObsCostBreakdown is the per-component cost split (USD), present only when the
// instrumentor reported it or the host cost engine computed it. Display-only —
// the hero/list aggregate sums cost_usd.
export type ObsCostBreakdown = {
  input?: number;
  output?: number;
  cache_read?: number;
  cache_write?: number;
  reasoning?: number;
  tool?: number;
};

export type ObsSpanEventRow = {
  span_id: string;
  time: string;
  name: string;
  attributes: string;
};

export type ObsSpanLinkRow = {
  span_id: string;
  linked_trace: string;
  linked_span: string;
};

// ObsSpanContentRow is one captured body for a span (prompt / response /
// tool_io). content is the raw body, present only when the node's content
// posture allowed it; content_hash is always set, so a hashed-only row still
// proves a body existed (the metadata-first default, §10).
export type ObsSpanContentRow = {
  span_id: string;
  kind: string;
  content: string;
  content_hash: string;
  time: string;
};

// ObsEnrichment is the pull-only proxy bundle rendered ON a span the proxy
// also saw (P6 / §9) — proxy-exact cost + cache-tier split + routing rationale
// + guard verdict. Mirrors internal/obs.Enrichment. guard_verdict is the
// proxy guard's decision for the matched api_turn (anchored via api_turn_id;
// empty when the turn raised no verdict).
export type ObsEnrichment = {
  found: boolean;
  provider: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
  cost_usd: number;
  routing_reason: string;
  guard_verdict: string;
};

export type ObsTraceDetail = {
  trace: ObsTraceRow;
  spans: ObsSpanRow[];
  events: ObsSpanEventRow[];
  links: ObsSpanLinkRow[];
  content: ObsSpanContentRow[];
  // span_id → enrichment; present only for spans matched to a proxy turn.
  enrichments?: Record<string, ObsEnrichment>;
};

// --- Eval plane (plan §8) — datasets, runs, scores -------------------------
// Mirrors the Go JSON tags in internal/obs/store/eval.go. All node-local
// (obs_eval/dataset tables), never on the org-push wire.

export type ObsDatasetRow = {
  id: number;
  name: string;
  description: string;
  created_at: string;
  item_count: number;
};

export type ObsEvalRunRow = {
  id: number;
  dataset_id: number;
  name: string;
  scorers: string; // JSON-encoded scorer spec list
  started_at: string;
  ended_at: string;
  total: number;
  passed: number;
  mean_score: number;
  status: string;
  dataset_name: string;
};

export type ObsRunScoreRow = {
  item_id: number;
  span_id: string;
  trace_id: string;
  scorer: string;
  score: number;
  passed: boolean;
  rationale: string;
};

export type ObsDatasetsResponse = { datasets: ObsDatasetRow[] };
export type ObsEvalRunsResponse = { runs: ObsEvalRunRow[] };
export type ObsEvalRunDetail = { run: ObsEvalRunRow; scores: ObsRunScoreRow[] };
