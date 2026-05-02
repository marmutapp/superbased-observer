// SuperBased Observer — In-platform help system.
//
// This file owns the metric/term registry, the hover-tooltip popover, and
// the slide-out Help drawer. Every column header, KPI tile, chart label,
// filter, and domain term in the dashboard should have a registry entry.
// The same registry powers tooltips on hover, the searchable drawer, and
// is the reference target for docs/dashboard-walkthrough.md.
//
// Registry shape:
//   id (dot-namespaced — tab.*, tile.*, chart.*, col.*, filter.*,
//       metric.*, calc.*, term.*) → {
//     category   : 'Tabs' | 'Tiles' | 'Charts' | 'Columns' | 'Filters' |
//                  'Metrics' | 'Calculations' | 'Glossary',
//     title      : short human-readable name,
//     oneliner   : one sentence shown in the hover popover,
//     description: longer-form prose for the drawer,
//     formula    : optional math/logic spelled out (`null` to omit),
//     source     : optional note about which DB column / endpoint feeds it,
//     example    : optional concrete example,
//     related    : optional [registry-id, ...] cross-links,
//   }
//
// Adding a new metric? Drop an entry here AND a `data-help="<id>"`
// attribute on the rendering element. That's the contract.

window.HELP = (function () {
  const E = {};

  // ---------- Tabs ----------
  E['tab.overview'] = {
    category: 'Tabs',
    title: 'Overview tab',
    oneliner: 'High-level snapshot — KPI cards, cost & actions over time, top models and tools.',
    description: 'Your first stop. Combines four KPI tiles, two time-series charts (cost and actions per day), and two top-N rollups (models by tokens, tools by action count) for the selected window. Use it to spot whether anything looks off before drilling into a tab.',
    related: ['filter.window', 'tab.cost', 'tab.tools'],
  };
  E['tab.cost'] = {
    category: 'Tabs',
    title: 'Cost tab',
    oneliner: 'Per-model token consumption and dollar cost over the selected window.',
    description: 'Breaks tokens into the four billable buckets (net input, cache read, cache write/creation, output), shows the cost engine\'s reliability, and renders a stacked-bar chart of token volume per day. Driven by /api/cost.',
    related: ['metric.net_input', 'metric.cache_read', 'metric.cache_creation', 'metric.output', 'metric.cost_usd', 'metric.reliability', 'calc.token_math'],
  };
  E['tab.sessions'] = {
    category: 'Tabs',
    title: 'Sessions tab',
    oneliner: 'One row per AI-coding session, with project, started time, action count, and (when scored) quality metrics.',
    description: 'A session is one continuous AI-coding conversation in a single tool. Click a row to open the session modal with action breakdown, token buckets, cost, and recent actions. Quality / Errors / Redundancy columns appear only when `observer score` has been run.',
    related: ['term.session', 'tile.sessions', 'metric.quality_score', 'metric.error_rate', 'metric.redundancy_ratio'],
  };
  E['tab.actions'] = {
    category: 'Tabs',
    title: 'Actions tab',
    oneliner: 'The flat firehose — every recorded tool-call action, filterable by type.',
    description: 'Each row is one normalised tool call (read_file, run_command, web_fetch, etc.). Use the type dropdown to scope. Pagination caps at 50 rows per page; total count is shown next to the heading.',
    related: ['term.action', 'filter.action_type'],
  };
  E['tab.tools'] = {
    category: 'Tabs',
    title: 'Tools tab',
    oneliner: 'Per-tool aggregates with charts showing when each AI client was active and what kind of work it did.',
    description: 'Four sections: (1) KPI tiles — total actions, distinct tools, overall success rate, busiest tool. (2) Activity over time — stacked area showing per-tool action volume per day. (3) Action-type mix — horizontal stacked bar showing what each tool actually does (read_file vs edit_file vs run_command vs …). (4) The full per-tool table at the bottom.',
    why: 'Tells you which AI client is doing the most work, whether it\'s healthy (success rate), and whether the work matches what you expect (e.g. cursor should be mostly edit_file; claude-code should be a mix). A sudden shift in the action-type mix is often the first sign that something changed in your workflow.',
    related: ['term.tool', 'filter.tool', 'chart.tools_activity', 'chart.tools_breakdown'],
  };
  E['chart.tools_activity'] = {
    category: 'Charts',
    title: 'Tools › Activity over time chart',
    oneliner: 'Per-tool action volume per day, stacked area. Top-6 tools shown; remainder rolled into "other".',
    description: 'Same data structure as the Top tools chart on Overview. Reuses /api/timeseries/actions\'s by_tool field, bucketed by day. Each band is one AI client; total height per day = total actions that day.',
    why: 'Shows when each tool is being used, not just the aggregate ranking. Spot patterns like "claude-code only on weekends" or "cursor went silent after the migration" — those are workflow changes worth understanding.',
    source: '/api/timeseries/actions?bucket=day (by_tool field)',
  };
  E['chart.tools_breakdown'] = {
    category: 'Charts',
    title: 'Tools › Action-type mix chart',
    oneliner: 'Horizontal stacked bar — one row per tool, segments coloured by action type (read_file / edit_file / run_command / search_text / …).',
    description: 'For each tool in the window, shows what fraction of its actions were each type. Useful for spotting workflow asymmetries: cursor is typically edit-heavy, claude-code is typically a mix, codex is typically run-command-heavy. A tool with an unexpected mix (e.g. claude-code suddenly 90% search) usually means a session-design change.',
    why: 'Helps you decide whether each tool is being used for the kind of work it\'s good at. If a tool is doing mostly low-leverage actions (e.g. lots of search but few edits), you might be paying for orchestration that another tool would do more efficiently.',
    source: '/api/tools/breakdown?days=N',
  };
  E['tab.compression'] = {
    category: 'Tabs',
    title: 'Compression tab',
    oneliner: 'How many tokens and dollars the proxy saved by trimming conversations before forwarding upstream.',
    description: 'Three sections: (1) KPI tiles for tokens saved, $ saved, bytes saved, and turns compressed; (2) a per-day chart showing the savings trajectory; (3) a per-model table breaking down where the savings came from. Bytes are what the proxy can measure directly (it sees the JSON request body before and after the pipeline runs); tokens are derived as bytes ÷ 4 (~Claude tokenizer ratio); dollars value those tokens at the row\'s model input rate (since the dropped/compressed content is prompt context).',
    why: 'Conversation compression is the biggest cost lever the proxy gives you — long agent sessions can balloon prompt context to the point where most spend is on context the model already has. This tab tells you whether the pipeline is actually saving tokens, and on which models. Negative savings on small payloads are normal and acceptable; persistent negatives across high-volume models are a signal the pipeline\'s thresholds need tuning.',
    actionable: 'Empty here? You\'re not using the proxy. Set the env var matching your AI client in the shell that launches it: Claude Code → ANTHROPIC_BASE_URL=http://127.0.0.1:8820; Codex / OpenAI-compatible → OPENAI_BASE_URL=http://127.0.0.1:8820/v1 (note the /v1). Negative savings on a high-volume model? Bump [compression.conversation].min_bytes_to_compress in observer config.',
    related: ['term.conversation_compression', 'metric.tokens_saved_est', 'metric.cost_saved_usd_est', 'metric.saved_bytes', 'calc.compression_savings', 'term.marker'],
  };

  E['chart.compression_by_mechanism'] = {
    category: 'Charts',
    title: 'Savings by mechanism chart',
    oneliner: 'Per-day stacked bar — bytes saved attributed to each compression mechanism (json, code, drop, …).',
    description: 'Reads the compression_events table (one row per individual compression decision, recorded post migration 009). Each segment of the stacked bar is one mechanism: per-content-type compressors (json/code/logs/text/diff/html) plus drop (low-importance message replacements). Stacked height = total bytes saved that day. Days with no compression activity are filtered out.',
    why: 'Tells you WHICH compression mechanism is doing the heavy lifting. If most savings come from `json` you can lean further on that compressor with stricter settings. If `drop` dominates, the conversation has too much low-importance noise — investigate the importance scorer\'s thresholds. Different from the per-day chart above (which just shows total bytes/tokens saved) because that one doesn\'t tell you HOW.',
    source: '/api/compression/timeseries?bucket=day',
    related: ['term.compression_events', 'mechanism.json', 'mechanism.code', 'mechanism.drop'],
  };

  E['term.compression_events'] = {
    category: 'Glossary',
    title: 'Compression events',
    oneliner: 'One row per individual compression decision recorded by the proxy pipeline.',
    description: 'When the proxy compresses a request body, two passes run: (1) per-content-type compressors rewrite individual tool_result blocks based on detected content type — json, code, logs, text, diff, html; (2) the budget enforcer drops low-importance messages and replaces them with markers. Each compress and each drop becomes one row in the compression_events table (migration 009) with mechanism / original_bytes / compressed_bytes / msg_index / importance_score. The aggregate columns on api_turns (compression_count, etc.) sum these per turn, but the events table is where you see the breakdown.',
    why: 'Aggregates hide which mechanism saved the bytes. The events view tells you "the json compressor fired 12 times today and saved 8 KB; the drop pass kicked in 3 times and saved 22 KB." That lets you tune the right knob.',
    related: ['chart.compression_by_mechanism', 'term.conversation_compression', 'mechanism.drop'],
  };

  E['mechanism.json'] = {
    category: 'Glossary',
    title: 'Mechanism: json',
    oneliner: 'JSON value-erasure — replaces every scalar with a type sentinel while preserving structure.',
    description: 'Detects JSON content via parser sniffing, then replaces every scalar value (strings, numbers, booleans, nulls) with a sentinel naming its type ("<string>", "<number>", "<bool>", "<null>"). All structure (object keys, array nesting, depth) is preserved. The model still sees the JSON shape and can navigate it; only the actual data values disappear.',
    why: 'JSON tool_results are the largest compression target in most workflows — every web_fetch, every tool_call response with structured output, every API listing. The structure usually carries the meaning the model needs; the actual values rarely do once the model has already incorporated them. 50%+ reductions here often dwarf all other mechanisms combined.',
    methodology: '**Algorithm**: parse body as JSON; recursively walk the tree replacing every scalar leaf with a sentinel string `"<string>"`, `"<number>"`, `"<bool>"`, or `"<null>"`. Object keys are preserved verbatim. Array structure is preserved with one optimisation: arrays of length > 1 collapse to a single representative element with a sibling `"_len": N` field on the parent object (or a suffix comment when the parent isn\'t an object).\n\n**JSON encoding**: `json.Marshal` is configured to NOT escape `<` and `>` to `\\u003c`/`\\u003e` — that escaping triples the byte cost of every sentinel and would cancel the compression. Output is forwarded as plain JSON to the upstream API.\n\n**Failure mode**: if the body isn\'t valid JSON (parse error), the original bytes are returned unchanged so the budget enforcer can still ship them upstream — the mechanism is fail-safe.\n\n**Trade-off**: lossy. Cases where the model needs actual values (evaluating a specific record, reading a config file the user pasted) should not be JSON-compressed. Configure `[compression.conversation.compress_types]` to exclude `json` for those workflows, or raise `min_bytes_to_compress` so small JSON tool_results that already fit the budget pass through untouched.',
  };
  E['mechanism.code'] = {
    category: 'Glossary',
    title: 'Mechanism: code',
    oneliner: 'Source-code skeleton extractor — keeps imports + signatures, drops bodies.',
    description: 'Heuristic skeleton extractor for source code. Keeps the top-of-file imports/uses region plus any line that looks like a signature declaration (function/method/class/struct/interface/type) across popular languages. Drops everything else — function bodies, in-line comments, blank lines.',
    why: 'Code tool_results dominate session bytes when the AI is browsing a codebase. The skeleton view shows the model "what exists" without paying for "how it\'s implemented" — usually the right tradeoff for navigation/search workflows. Wrong tradeoff for understanding-the-implementation workflows, which is why this mechanism is opt-in only.',
    methodology: '**Detection**: triggered when the content-type sniffer classifies the body as `code`. Sniffer uses filename hints (.go, .ts, .py, .rs, .java, .c, .cpp, .js) plus content patterns (brace-balanced regions, common keywords) to distinguish code from prose.\n\n**Top-of-file imports**: the first contiguous run of lines matching one of `import X`, `from X import`, `use X`, `require X`, `#include X` is preserved verbatim — this gives the model the dependency graph for free.\n\n**Signature pattern**: a regex matches lines with declaration keywords across languages: `func`, `def`, `fn`, `class`, `struct`, `interface`, `type`, `enum`, `trait`, `protocol`, `module`, `package`. Lines matching this pattern are kept. Everything else is dropped — including bodies, inner comments, blank lines, and any prose between declarations.\n\n**Limitations**: this is a best-effort skeleton — without a real language parser, a body whose text contains a regex-matching keyword (e.g. a string literal containing `"def"`) would be falsely retained. Erring toward retention is intentional — false positives keep more code than ideal but don\'t corrupt structure; false negatives would silently delete real declarations, which is worse.\n\n**OPT-IN**: code is excluded from the default `compress_types` (which is `["json", "logs", "text"]`) because the model is often asked to read code in detail. Add `"code"` to `[compression.conversation.compress_types]` explicitly to engage this mechanism.',
  };
  E['mechanism.logs'] = {
    category: 'Glossary',
    title: 'Mechanism: logs',
    oneliner: 'Two-pass log compressor — dedup adjacent duplicates, then head+tail truncate if still long.',
    description: 'Detects log-shaped content and applies two passes: (1) collapse runs of identical adjacent lines into one with a `[×N]` suffix; (2) if post-dedup line count is still over MaxLines (default 200), keep the first N + last N lines (default 100 each) with a single `…` elision in the middle.',
    why: '`go test`, `npm run build`, `cargo build`, `pytest`, polling/retry loops — these spit out hundreds of nearly-identical progress lines. The dedup pass is what makes test-runner output compress dramatically; head+tail handles outputs where even the distinct lines are too long. Conservative, never alters distinct-line ordering, never modifies content beyond the dedup suffix — safe to apply to any line-oriented output.',
    methodology: '**Detection**: triggered when the content-type sniffer classifies the body as `logs`. Sniffer looks for line-prefix patterns (RFC 3339 timestamps, ISO 8601 dates, log levels like `INFO`/`WARN`/`ERROR`, `[2026-...]`-style brackets).\n\n**Pass 1 — adjacent dedup**: walks lines sequentially. When line[i] == line[i-1], increments a counter; the run collapses to `<line> [×N]` where N is the run length. Distinct lines pass through unchanged. This is what compresses progress bars and retry chatter.\n\n**Pass 2 — head+tail truncate**: if the post-dedup line count exceeds `LogsOptions.MaxLines` (default 200), the output is replaced with the first `Head` + last `Tail` lines (defaults 100 + 100) joined by a single elision marker. This handles outputs where the distinct lines are themselves too numerous (long stack traces, voluminous CI output).\n\n**Trade-offs**: the dedup pass is lossless for the model\'s purposes — knowing a line repeated 50× is equivalent to seeing it 50×. The truncate pass IS lossy on the middle of long outputs, but logs typically have the relevant signal at the start (the operation began) and end (the operation finished or failed) — the middle is usually progress chatter the model already knows the structure of.\n\n**Order matters**: dedup runs first specifically because it can shrink a 1000-line output to <200 lines, avoiding the lossy truncation pass entirely.',
  };
  E['mechanism.text'] = {
    category: 'Glossary',
    title: 'Mechanism: text',
    oneliner: 'Plain-text head+tail truncation — fallback for content not matched by other mechanisms.',
    description: 'The catch-all compressor for content that didn\'t classify as code/json/logs/diff/html. Applies head+tail truncation: keeps the first 40 + last 40 lines for any input over 80 lines, with an elision marker in the middle. Short outputs pass through unchanged.',
    why: 'Catches the long tail — Markdown bodies, README excerpts, narrative descriptions, prose-heavy tool outputs. Smallest contributor to total savings (most prose is short), but ensures NO content type goes uncompressed when it crosses the budget.',
    methodology: '**Detection**: this is the fallback — runs when the content-type sniffer returns `text` (Unknown content with no other mechanism applicable still falls through this path).\n\n**Algorithm**: split body on `\\n` boundaries. If line count ≤ MaxLines (default 80), return body unchanged. Otherwise, keep `Head` lines from the start (default 40) + `Tail` lines from the end (default 40), join them with a single elision marker line (default format: `… N lines elided …` where N is dropped count).\n\n**Why these numbers**: the 80-line trigger preserves typical short outputs (commit messages, single-function diffs, brief narratives) without modification. The 40+40 split preserves the framing of long prose — what the document is about and how it concludes — while dropping the middle that the model can usually infer.\n\n**Trade-off**: lossy on the middle of long inputs. Less suitable for cases where the model needs to read the full document (legal text, contracts, structured prose) — for those workflows, exclude `text` from `compress_types` or raise `min_bytes_to_compress` so the body fits the budget without truncation.',
  };
  E['mechanism.diff'] = {
    category: 'Glossary',
    title: 'Mechanism: diff',
    oneliner: 'Unified-diff context stripper — keeps every change line + ±1 context line, drops the rest.',
    description: 'Strips unified-diff context lines beyond ±1 of each change. Keeps every header (`---`/`+++`/`@@`/index lines), every change line (`+` and `-`), and exactly one context line on each side of every change run. Drops everything else with a single `… N context lines elided` marker between runs.',
    why: '`git diff` and patch tool outputs are dominated by unchanged context — typically 90%+ of the bytes. The model needs the actual changes plus enough context to know where they land; everything beyond that is noise. A 200-line diff with 5 actual changes typically shrinks to ~20 lines (5 changes + 10 context + 5 elision markers) with zero loss of information about what actually changed.',
    methodology: '**Detection**: triggered when the content-type sniffer recognises unified-diff format — leading `diff `/`---`/`+++` headers or `@@` hunk markers.\n\n**Header preservation**: every line matching diff metadata is kept verbatim — file headers (`---`, `+++`), index lines (`index abc..def 100644`), hunk headers (`@@ -10,5 +10,7 @@`). These give the model file context.\n\n**Change preservation**: every line starting with `+` or `-` (excluding the `+++`/`---` header lines, which are matched first) is kept. These ARE the changes — never dropped.\n\n**Context windowing**: for each contiguous run of change lines, exactly one preceding and one trailing context line (` ` prefix in unified diff format) is kept. Context beyond ±1 lines from the nearest change is dropped, replaced by a single `… N context lines elided …` marker between runs.\n\n**Why ±1 specifically**: a single line of context anchors the change to its surroundings (the model sees the function signature above and the next statement below) without paying for redundant context the `@@` header already encodes (line numbers tell the model where in the file we are).\n\n**Lossless on changes**: by construction, every byte of every modification survives. Lossy only on context — the cheap-to-rebuild surroundings.',
  };
  E['mechanism.html'] = {
    category: 'Glossary',
    title: 'Mechanism: html',
    oneliner: 'HTML cruft stripper — removes <script>, <style>, and HTML comments.',
    description: 'Strips `<script>...</script>` and `<style>...</style>` blocks (including their contents) and `<!-- ... -->` comments. Output remains valid HTML-ish enough for the model to read the visible structure without the weight of inline scripts or CSS.',
    why: '`web_fetch` tool_results often pull whole HTML pages where 80%+ of bytes are inline scripts (analytics, framework runtime, hydration data) and CSS (utility classes, design tokens). The model can\'t use them — it needs the visible content and structure. Heavy compression candidate.',
    methodology: '**Detection**: triggered when the content-type sniffer recognises HTML — leading `<!doctype html>` or balanced `<html>`/`<body>` tags, or filename ending in `.html`/`.htm`.\n\n**Algorithm**: three regex-based passes, applied in order:\n\n1. `<script ...>...</script>` — case-insensitive, non-greedy body match (so adjacent `<script>` blocks don\'t get joined). Includes `type="module"` and external `src=...` script tags (the latter has no body but its tag is still cruft).\n2. `<style ...>...</style>` — same pattern, applied to inline CSS.\n3. `<!-- ... -->` — HTML comments, often used for build-tool markers and hydration hints.\n\n**Preserved**: tag attributes (id, class, href, src on non-script tags), text content between tags, structural elements (div, span, header, etc.), inline formatting (b, i, code, etc.), and any external resource references like `<link rel="stylesheet" href="...">` (the link is kept; the linked content was never in-body).\n\n**Trade-off**: the output is no longer guaranteed to render in a browser — empty `<script>` slots may break some pages\' JS lifecycle. But the model isn\'t rendering the page; it\'s reading it. The visible structure + text is what matters.\n\n**What it does NOT do**: it does not minify whitespace, collapse adjacent tags, or rewrite the DOM. Non-cruft content survives byte-for-byte so the model\'s page-comprehension stays intact.',
  };
  E['mechanism.drop'] = {
    category: 'Glossary',
    title: 'Mechanism: drop',
    oneliner: 'Low-importance message replaced by a marker — the only mechanism that loses content the model previously saw.',
    description: 'When per-content-type compression isn\'t enough to hit the target ratio, the budget enforcer ranks remaining messages by an importance score and drops the lowest-scored until the budget is met. Each drop is replaced by a marker block (a small placeholder text) so the conversation flow stays intact for the model, and the dropped content can still be retrieved on-demand via the `search_past_outputs` MCP tool.',
    why: 'Drop is the only mechanism that genuinely reduces context the model has previously seen. Compress mechanisms shrink without losing meaning; drop makes a deliberate "this is the least-important message; I\'ll remove it" tradeoff. High drop counts on a session usually signal that the conversation is rambling or that low-value tool outputs are dominating context — symptoms worth examining.',
    methodology: '**Importance scoring** is a deterministic weighted sum of four `[0,1]` components, computed for every message:\n\n**1. Recency** = `(i+1) / n` where `i` is the message position (0-indexed, so the oldest message gets `1/n` and the newest gets `1.0`). Linear, not exponential — biases toward recent context without hard-cliffing old context.\n\n**2. Reference** = `1.0` if any of the message\'s `tool_use_id`s is referenced by a later `tool_result`, OR any of the message\'s referenced `tool_result`s points to a live `tool_use`. Tool-pair-live messages always get full reference weight regardless of position. `0.0` for messages with no live tool-pairing involvement.\n\n**3. Density** = the fraction of non-whitespace runes in the message text, clamped to `[0,1]`. Whitespace-padded outputs (formatted tables, indented JSON the model doesn\'t need) score lower so the enforcer prefers dropping them over dense narrative content.\n\n**4. Role** = constant per role: `system 1.0`, `user 0.9`, `assistant 0.7`, `tool 0.5`. Tool outputs score lowest because they\'re typically large and the model has already incorporated their information into the conversation flow — dropping them hurts least.\n\n**Composite** = `0.4 × recency + 0.3 × reference + 0.15 × density + 0.15 × role` (default weights, override-able via `[compression.conversation.weights]` in observer config).\n\n**Drop decision**:\n\n1. Sort messages by score ascending — lowest-scored first.\n2. Walk the sorted list, dropping each message that is NOT `Preserved` until total bytes ≤ `target_ratio × original_bytes` (default `target_ratio = 0.85`).\n3. `Preserved` means the message is structurally undroppable — set when `PreserveLastN` pinned it (default last 4 messages, configurable), OR the message is a `system` role, OR it\'s on either side of a live tool-pair (producer with referenced ID, or consumer referencing a live producer).\n4. Consecutive dropped messages collapse into one marker block (rather than N markers) so the conversation flow stays compact.\n\n**Mode interaction**: under `Mode = "cache"`, drop candidates are restricted to the tail half of the conversation so the prefix stays stable for Anthropic prompt caching. Compression (Pass 1, the per-type compressors) still applies to the whole window in cache mode — only the drop pass is restricted.\n\n**Per-event record**: every drop is recorded in `compression_events` with the message\'s `importance_score`. That score is what shows in the Compression tab\'s "Recent compression events" table\'s Importance column — and is the diagnostic for tuning. Persistently low scores on dropped messages mean the threshold is well-calibrated; high scores (≥0.5) on dropped messages signal the enforcer is being too aggressive — raise `target_ratio` to keep more.',
    actionable: 'High importance_score on a dropped event (≥ 0.5) usually means the threshold needs raising — the enforcer is being too aggressive. Tune `[compression.conversation].target_ratio` upward (default 0.85; try 0.9 or 0.95) to keep more.',
  };

  E['chart.compression_over_time'] = {
    category: 'Charts',
    title: 'Compression savings per day chart',
    oneliner: 'Daily tokens-saved (left axis) and bytes-saved (right axis) from the conversation-compression pipeline.',
    description: 'Line chart bucketed by day. Two y-axes because the units differ by a factor of ~4 (tokens ≈ bytes/4). Each point is the per-day sum of saved tokens / saved bytes across all api_turns that hit the proxy that day. Days with no compression activity are filtered out.',
    why: 'A flat-positive trend means the pipeline is paying its way. A spike (positive or negative) on a specific day usually correlates with one outlier session — drill into the Sessions tab on that date to see what happened.',
    source: '/api/timeseries/cost?bucket=day (compression_tokens_saved_est + compression_bytes_saved fields)',
  };

  E['metric.tokens_saved_est'] = {
    category: 'Metrics',
    title: 'Tokens saved (est.)',
    oneliner: 'Estimated input tokens spared by conversation compression. Signed — can be negative.',
    description: 'Computed as saved_bytes ÷ 4 per row, then summed. The ÷4 ratio comes from Anthropic\'s tokenizer averaging ~4 chars per token on typical English/code; for prose-heavy or highly-repetitive content it can drift to 3–5. This is the right ballpark unit for "how much did compression help?" because tokens are what cost dollars; bytes are just what the proxy can see.',
    formula: 'tokens_saved_est = (original_bytes − compressed_bytes) ÷ 4',
    why: 'This is the headline savings number. If you\'re comparing compression effectiveness across models or weeks, compare tokens — bytes can mislead because non-text encoding overhead (escapes, base64) inflates byte counts without changing token counts.',
    related: ['metric.cost_saved_usd_est', 'metric.saved_bytes', 'calc.compression_savings'],
  };

  E['metric.cost_saved_usd_est'] = {
    category: 'Metrics',
    title: 'Dollars saved (est.)',
    oneliner: 'Estimated USD value of the tokens compression spared. Signed.',
    description: 'For each row: tokens_saved_est × the row\'s model input rate (per million tokens, from the cost engine pricing table). Aggregates by sum across rows. Uses the *input* rate (not output, not cache) because compression operates on prompt context — dropped/truncated content would have been billed as fresh input on the upstream side.',
    formula: 'cost_saved_usd_est = Σ_rows (tokens_saved_est × pricing.input ÷ 1,000,000)',
    why: 'The number to put in a budget conversation. It abstracts away byte/token math and cache-state nuance into one currency-shaped number. Treat as ±20% accurate — the bytes-to-tokens ratio is approximate and per-model rates change.',
    related: ['metric.tokens_saved_est', 'calc.cost_math'],
  };

  E['calc.compression_savings'] = {
    category: 'Calculations',
    title: 'Compression savings — full methodology',
    oneliner: 'How tokens-saved and $-saved are derived from the bytes the proxy actually measured.',
    description: 'Per api_turn, the proxy records original_bytes (request body before the pipeline ran) and compressed_bytes (what was actually forwarded upstream). The pipeline does two things: (a) rewrites large tool_result blocks to a truncated form, counted in compressed_count; (b) drops messages that fail the importance scorer, counted in dropped_count and replaced 1:1 with marker messages (marker_count) so the model still sees a placeholder. Per-row saved_bytes = original − compressed (signed; small payloads can go negative when marker overhead exceeds what was dropped). Per-row tokens_saved_est = saved_bytes ÷ 4 (Anthropic tokenizer averages ~4 chars per token; signed). Per-row cost_saved_usd_est = tokens_saved_est × the row\'s model input rate ÷ 1e6 (signed). Aggregates are simple sums across rows.',
    formula: 'saved_bytes = original − compressed   |   tokens_saved_est = saved_bytes ÷ 4   |   cost_saved_usd_est = tokens_saved_est × p.input ÷ 1e6',
    why: 'The proxy can only see bytes. Tokens are what bill you. Dollars are what matter. This calc bridges the three with documented approximations so the savings you see is comparable across models, days, and content types — even though no two of those have exactly the same bytes-per-token ratio.',
    related: ['metric.tokens_saved_est', 'metric.cost_saved_usd_est', 'metric.saved_bytes', 'term.conversation_compression', 'term.marker'],
  };

  E['term.conversation_compression'] = {
    category: 'Glossary',
    title: 'Conversation compression',
    oneliner: 'Pre-forward trimming of API request bodies in the local proxy.',
    description: 'A processing layer between your AI client and the upstream API. For each request, the proxy: (1) parses the messages array; (2) scores each message for importance based on heuristics like recency, tool-call participation, and structured-output presence; (3) rewrites or drops messages below the threshold, replacing drops with marker messages so the conversation flow stays intact; (4) forwards the trimmed body upstream. cache_control breakpoints get re-aligned to the new prefix boundary so prefix caching still works. Configured in [compression.conversation] in observer config; opt-in via enabled = true.',
    why: 'Long-running agent sessions accumulate hundreds of tool_result blocks the model rarely needs to see again. Compression keeps the most-relevant context and trims the rest, so prompt tokens (the largest cost line item) drop without changing what the model can do. The dashboard shows whether it\'s actually paying off — which is what the Compression tab exists for.',
    related: ['term.shell_compression', 'term.marker', 'calc.compression_savings'],
  };

  E['metric.saved_bytes'] = {
    category: 'Metrics',
    title: 'Bytes saved',
    oneliner: 'original_bytes − compressed_bytes (signed; can be negative when marker overhead exceeds what was dropped).',
    description: 'The raw delta the proxy measures. Tokens-saved and $-saved are derived from this. Negative saved_bytes happen on small request bodies where the placeholder markers cost more bytes than the dropped content saved.',
    why: 'Bytes are the source of truth — directly measured, not estimated. But they\'re a poor cost proxy because token counts depend on content type. Use bytes when comparing across the same model/content; use tokens when comparing across models or content types.',
  };
  E['tab.discovery'] = {
    category: 'Tabs',
    title: 'Discovery tab',
    oneliner: 'Wasted-effort signals — same-session stale re-reads and repeated no-change command runs.',
    description: 'Two panels: (1) stale re-reads — files the AI client read more than once in a single session after the content changed between reads; (2) repeated commands — commands the AI ran multiple times with no inputs changing in between. Both are proxies for the model losing track of what it already knew.',
    why: 'These are the two patterns that quietly inflate your token bill. A stale reread costs prompt tokens to re-ingest content the session previously saw; a no-change rerun pays output tokens for a result that hasn\'t changed. High counts on either suggest changing how you cache or scope sessions.',
    related: ['metric.stale_count', 'metric.no_change_reruns', 'term.freshness'],
  };
  E['tab.patterns'] = {
    category: 'Tabs',
    title: 'Patterns tab',
    oneliner: 'Repeatable behaviours the observer noticed across your sessions — the kind you\'d want to write down once so future sessions follow the same habit.',
    description: 'Examples in plain English: "after running `go test`, you usually run `go vet` next" (a command_pair). "When working on auth.ts, you also touch login.tsx" (a cross_tool_file). "You consistently use the project\'s `Make build` target instead of raw go build" (a knowledge_snippet). Each row is one such observation with a confidence score (how sure the observer is it\'s real, not coincidence) and an observation count (how many independent times it was seen).',
    why: 'Patterns are the link between "data the observer collected" and "rules the AI should follow." Run `observer suggest` and the high-confidence patterns get rewritten into CLAUDE.md / AGENTS.md / .cursorrules — instructions your next AI session reads as part of its system prompt. Net effect: new sessions inherit your habits without you re-typing them, and stop reinventing patterns you already use.',
    actionable: 'Sort by Confidence descending. The top of the list is what `observer suggest` will codify next; the bottom is speculation. To turn a high-confidence pattern into a rule for new sessions: run `observer suggest --apply` to write the relevant entries into your CLAUDE.md/AGENTS.md.',
    related: ['col.pattern_type', 'metric.confidence', 'metric.observation_count'],
  };

  // ---------- Filters ----------
  E['filter.window'] = {
    category: 'Filters',
    title: 'Window selector',
    oneliner: 'Time horizon — 7 / 30 / 90 / 365 days from now.',
    description: 'Every loadable view honours this filter via a `days=` query param. Charts bucket by day; tables include all rows whose timestamp falls in [now - days, now].',
  };
  E['filter.tool'] = {
    category: 'Filters',
    title: 'Tool filter',
    oneliner: 'Restrict views to one AI client (claude-code / cursor / codex / etc.). Default: all.',
    description: 'Populated from the per-tool last-seen list. Selecting a tool re-fires whatever tab is active.',
    related: ['term.tool'],
  };
  E['filter.project'] = {
    category: 'Filters',
    title: 'Project filter',
    oneliner: 'Restrict views to sessions/actions whose project root matches.',
    description: 'Project roots are derived from the working directory at session start (with `/.git/...` paths folded back to the working-tree root). Selecting a project re-fires the current tab.',
    related: ['term.project'],
  };
  E['filter.action_type'] = {
    category: 'Filters',
    title: 'Action-type filter (Actions tab)',
    oneliner: 'Scope the actions firehose to one normalised type (read_file, run_command, etc.).',
    description: 'Action types are normalised across all adapters (claude-code, cursor, codex, cline, copilot) so a `read_file` from any tool means the same thing.',
    related: ['term.action'],
  };

  // ---------- Overview KPI tiles ----------
  E['tile.sessions'] = {
    category: 'Tiles',
    title: 'Sessions tile',
    oneliner: 'Total session count in the database (not windowed).',
    description: 'Counts rows in the `sessions` table. The subtitle shows total recorded actions across all sessions. Use the Sessions tab for a windowed view.',
    formula: 'COUNT(*) FROM sessions',
    related: ['term.session', 'tab.sessions'],
  };
  E['tile.api_turns'] = {
    category: 'Tiles',
    title: 'API turns (proxy) tile',
    oneliner: 'Count of HTTP requests captured by the local API proxy.',
    description: 'When you point an AI client at the local proxy via `ANTHROPIC_BASE_URL=http://127.0.0.1:8820` (Claude Code) or `OPENAI_BASE_URL=http://127.0.0.1:8820/v1` (Codex / OpenAI-compatible), the proxy records one row per request in `api_turns`. This is the most accurate token source — it sees the upstream usage envelope directly. Compared to JSONL parsing (next tile) which infers tokens after the fact.',
    formula: 'COUNT(*) FROM api_turns',
    related: ['term.api_turn', 'term.proxy_vs_jsonl'],
  };
  E['tile.token_rows'] = {
    category: 'Tiles',
    title: 'Token rows (jsonl) tile',
    oneliner: 'Count of token-usage events recovered by parsing the AI client\'s session log.',
    description: '"Plentiful but unreliable" — JSONL parsing recovers token data for every session (no proxy needed) but the AI clients echo cumulative usage on every content block, so the cost engine has to deduplicate. See calc.dedup_a1.',
    formula: 'COUNT(*) FROM token_usage',
    related: ['term.token_row', 'calc.dedup_a1', 'term.proxy_vs_jsonl'],
  };
  E['tile.failures_24h'] = {
    category: 'Tiles',
    title: 'Failures (24h) tile',
    oneliner: 'Action failures in the last 24 hours.',
    description: 'Counts actions with success=false in the last 24h. Click into the Discovery tab if non-zero — repeated failures often correlate with no-change command reruns or stale-context reads.',
    formula: 'COUNT(*) FROM actions WHERE success=0 AND timestamp > now-24h',
    related: ['tab.discovery'],
  };

  // ---------- Charts ----------
  E['chart.cost_over_time'] = {
    category: 'Charts',
    title: 'Cost over time chart',
    oneliner: 'Daily token volume, split into the four billable buckets.',
    description: 'Line chart bucketed by day. The four series (Net Input, Cache Read, Cache Write, Output) align with the cost-engine billing buckets. Sums in the meta line above the chart are over the full window.',
    source: '/api/timeseries/cost?bucket=day',
    related: ['metric.net_input', 'metric.cache_read', 'metric.cache_creation', 'metric.output', 'calc.token_math'],
  };
  E['chart.actions_over_time'] = {
    category: 'Charts',
    title: 'Actions over time chart',
    oneliner: 'Daily action count split by total vs failures.',
    description: 'Line chart bucketed by day. "Total" is every recorded action; "Failures" is the subset with success=false.',
    source: '/api/timeseries/actions?bucket=day',
  };
  E['chart.top_models'] = {
    category: 'Charts',
    title: 'Top models (by tokens) chart',
    oneliner: 'Top 8 models in the window, ranked by total tokens consumed.',
    description: 'Stacked bar — Net Input + Cache Read + Output per model. Cache Write omitted to keep the chart legible (most traffic is read-heavy).',
    source: '/api/models',
  };
  E['chart.top_tools'] = {
    category: 'Charts',
    title: 'Top tools chart (actions over time)',
    oneliner: 'Per-day action counts per AI client, stacked. Top-6 tools shown explicitly; the rest roll into "other".',
    description: 'Stacked-area time series. Each band is one AI client (claude-code, cursor, codex, …). Use it to see *when* each tool was active, not just the aggregate ranking — patterns like "cursor only on weekdays" or "claude-code spike when refactoring" pop out here.',
    source: '/api/timeseries/actions?bucket=day (by_tool field)',
    why: 'Aggregate top-N bars hide trends. Time-series shows when work actually happened, when a tool fell out of use, or when a new tool came online.',
  };
  E['chart.token_volume_per_day'] = {
    category: 'Charts',
    title: 'Token volume per day chart (Cost tab)',
    oneliner: 'Stacked bars of daily tokens, split by billing bucket.',
    description: 'Same data as the Overview cost-over-time chart but rendered as bars stacked by bucket. Useful for spotting cache-heavy vs net-input-heavy days at a glance.',
    source: '/api/timeseries/cost?bucket=day',
  };
  E['chart.token_volume_by_model'] = {
    category: 'Charts',
    title: 'Token volume per day, split by model (Cost tab)',
    oneliner: 'Stacked bars of daily tokens with each model as its own series — companion view to the bucket-split chart above.',
    description: 'Same per-day axis as the chart above, but each stack is a model rather than a billing bucket. Each bar\'s height is total tokens (input + output + cache_read + cache_creation summed) attributed to that model on that day. Top-6 models by total tokens get explicit series; everything else rolls up into "other" so the legend stays readable.',
    why: 'The bucket-split chart tells you *what kind* of tokens you used; this one tells you *which model* burned them. You\'ll spot patterns the bucket chart hides — a single expensive model dominating a day, a model migration partway through the window, sub-agents running on cheaper models, an experimental model creeping into traffic. Same source as the cost table — rows are deduplicated proxy-first, JSONL fallback.',
    actionable: 'Spot a day where one model spiked? Click into Sessions for that day to see which projects drove it. If "other" is large, the per-account model fan-out is high — consider consolidating on fewer models. If a small model is unexpectedly dominating, check whether sub-agent dispatching is hitting the right model.',
    source: '/api/timeseries/tokens-by-model?days=N&project=PATH',
    related: ['chart.token_volume_per_day', 'tab.cost', 'metric.reliability'],
  };

  // ---------- Cost-tab columns ----------
  E['col.cost.model']   = { category: 'Columns', title: 'Cost › Model', oneliner: 'Model identifier (claude-opus-4-5, gpt-4o, etc.).', description: 'Comes from the upstream usage envelope (proxy) or the JSONL `message.model` field. Models without pricing data render with reliability=unreliable.' };
  E['col.cost.net_in']  = { category: 'Columns', title: 'Cost › Net In', oneliner: 'Net input tokens — fresh prompt tokens, cache hits subtracted.', description: 'Billed at the model\'s standard input rate.', related: ['metric.net_input', 'calc.token_math'] };
  E['col.cost.cache_r'] = { category: 'Columns', title: 'Cost › Cache R', oneliner: 'Cache-read tokens — prompt tokens served from Anthropic\'s ephemeral cache.', description: 'Billed at the cache-read rate (~10% of standard input rate).', related: ['metric.cache_read'] };
  E['col.cost.cache_w'] = { category: 'Columns', title: 'Cost › Cache W', oneliner: 'Cache-write/creation tokens — new tokens written to the ephemeral cache.', description: 'Billed at 1.25× (5m tier) or 2× (1h tier) of standard input rate. Anthropic only — Codex/OpenAI billing has no equivalent.', related: ['metric.cache_creation'] };
  E['col.cost.output']  = { category: 'Columns', title: 'Cost › Output', oneliner: 'Output tokens — what the model generated.', description: 'Billed at the model\'s output rate (typically 5× input).', related: ['metric.output'] };
  E['col.cost.cost']    = { category: 'Columns', title: 'Cost › Cost', oneliner: 'Computed dollar cost for this row. Always derived from token counts × the model\'s pricing entry — neither Anthropic nor OpenAI returns cost_usd in their API responses.', description: 'For api_turns rows (proxy): tokens × pricing-table[model]. For token_usage rows from OpenCode and Pi (which compute cost client-side and write it into their JSONL): the recorded estimated_cost_usd is used as-is. Every other JSONL adapter goes through the same tokens × pricing path. Pricing-table source-of-truth: docs/pricing-reference.md.', related: ['calc.cost_math', 'term.cost_provenance'] };
  E['col.cost.turns']   = { category: 'Columns', title: 'Cost › Turns', oneliner: 'Number of API turns (proxy) or token-row groups (jsonl) rolled into this row.', description: 'When grouped by model, this is the count of turns attributed to that model in the window.' };
  E['col.cost.source']  = { category: 'Columns', title: 'Cost › Source', oneliner: 'Where the token data underneath this row came from: proxy, jsonl, or mixed.', description: 'proxy = the local API proxy intercepted the upstream request and read the usage envelope directly (one row per HTTP request, no dedup needed). jsonl = recovered by parsing the AI client\'s on-disk session log (works without configuring a base URL but the client echoes cumulative usage on every content block, so the cost engine deduplicates). mixed = both sources contributed rows that survived dedup.', why: 'Tells you whether you\'re seeing what the API actually charged (proxy) or what we inferred from log files (jsonl). For new traffic prefer proxy — it\'s ground truth. JSONL is what gets you historical visibility before the proxy was set up.', actionable: 'Mostly "jsonl" or "mixed"? Engage the proxy with the env var matching your AI client: Claude Code → ANTHROPIC_BASE_URL=http://127.0.0.1:8820; Codex / OpenAI-compatible → OPENAI_BASE_URL=http://127.0.0.1:8820/v1. New rows will go to "proxy". See docs/proxy-routing.md for the full per-client recipes.', related: ['term.proxy_vs_jsonl', 'metric.reliability'] };
  E['col.cost.reliab']  = {
    category: 'Columns',
    title: 'Cost › Reliab.',
    oneliner: 'Trust level for this row\'s tokens — accurate / approximate / unreliable / unknown — plus a "~" suffix when pricing was inferred via family-prefix fallback.',
    description: 'Token-side reliability: accurate = proxy-captured (provider usage envelope verbatim); approximate = JSONL-captured but the client recorded the provider\'s usage envelope intact; unreliable = JSONL streaming-time placeholders (Claude Code, ~10% off output); unknown = no pricing-table entry. Pricing-side: if the value carries a "~", the rate came from a family-prefix fallback (e.g. claude-opus-4-99 inheriting claude-opus-4 family rates) rather than an exact-match table entry — hover the tilde for details. Pricing source-of-truth: docs/pricing-reference.md.',
    why: 'When reconciling against an upstream invoice, the "~" is your first hint that a SKU isn\'t in the baked-in table. Add an explicit entry under [intelligence.pricing.models."<id>"] in config.toml to pin a rate; absent that, family fallback is usually correct but newer SKUs may be priced differently.',
    related: ['metric.reliability', 'term.cost_provenance'],
  };

  // ---------- Sessions-tab columns ----------
  E['col.sessions.id']         = { category: 'Columns', title: 'Sessions › ID', oneliner: 'Stable session identifier (truncated for display).', description: 'Click the row to open the session detail modal.' };
  E['col.sessions.tool']       = { category: 'Columns', title: 'Sessions › Tool', oneliner: 'Which AI client this session ran in.', related: ['term.tool'] };
  E['col.sessions.project']    = { category: 'Columns', title: 'Sessions › Project', oneliner: 'Working-directory root at session start (last two path segments shown; full path on hover).', related: ['term.project'] };
  E['col.sessions.started']    = { category: 'Columns', title: 'Sessions › Started', oneliner: 'Wall-clock timestamp of the first action in the session.' };
  E['col.sessions.actions']    = { category: 'Columns', title: 'Sessions › Actions', oneliner: 'Total recorded actions in the session.', formula: 'COUNT(*) FROM actions WHERE session_id = s.id' };
  E['col.sessions.tokens']     = {
    category: 'Columns',
    title: 'Sessions › Tokens',
    oneliner: 'Total billable tokens for this session (input + output + cache_read + cache_creation).',
    description: 'Sourced from the cost engine\'s GroupBySession rollup with SourceAuto dedup — same numbers the Cost tab shows. When both proxy and JSONL data exist for the same session, proxy wins (billing-grade). Otherwise JSONL fills the gap. Sessions with no API traffic at all (e.g. hook-only Cursor sessions) render "—".',
    why: 'Quick way to spot which sessions burned tokens without opening the detail modal. Pair with Actions to see token-density: a session with 200 actions and 5M tokens ran very different work than one with 10 actions and 5M tokens.',
    formula: 'cost.Engine.Summary(GroupBySession).Tokens.{Input + Output + CacheRead + CacheCreation}',
    related: ['col.sessions.cost', 'tab.cost', 'metric.reliability'],
  };
  E['col.sessions.cost']       = {
    category: 'Columns',
    title: 'Sessions › Cost',
    oneliner: 'Computed dollar cost for this session. A trailing "~" means the figure is approximate (JSONL-derived, not billing-grade).',
    description: 'Same cost engine as the Cost tab. The "~" suffix marks rows whose Reliability tag is anything other than "accurate" — typically because the session went through a non-proxied AI client. Hover the value to see the reliability tag. Sessions whose AI client never produced a token row render "—".',
    why: 'Per-session $ is the unit you care about for "what did this branch of work actually cost?" The dashboard\'s headline cost is windowed; this column is per-session, useful for comparing two sessions on the same project.',
    related: ['col.sessions.tokens', 'col.cost.cost', 'col.cost.reliab', 'term.proxy_vs_jsonl'],
  };
  E['col.sessions.quality']    = { category: 'Columns', title: 'Sessions › Quality', oneliner: 'Composite session quality score (0–1, higher is better). Populated by `observer score`.', related: ['metric.quality_score'] };
  E['col.sessions.errors']     = { category: 'Columns', title: 'Sessions › Errors', oneliner: 'Action failure rate for this session.', formula: 'failures / total_actions', related: ['metric.error_rate'] };
  E['col.sessions.redundancy'] = { category: 'Columns', title: 'Sessions › Redundancy', oneliner: 'Fraction of actions classified as redundant (stale rereads, no-change reruns, etc.).', related: ['metric.redundancy_ratio'] };

  // ---------- Actions-tab columns ----------
  E['col.actions.when']      = { category: 'Columns', title: 'Actions › When', oneliner: 'Wall-clock timestamp of the action.' };
  E['col.actions.tool']      = { category: 'Columns', title: 'Actions › Tool', oneliner: 'AI client that executed the action.' };
  E['col.actions.type']      = { category: 'Columns', title: 'Actions › Type', oneliner: 'Normalised action type (read_file, run_command, web_fetch, ...).' };
  E['col.actions.tool_name'] = { category: 'Columns', title: 'Actions › Tool name', oneliner: 'The raw tool name as the AI client emitted it (before normalisation).' };
  E['col.actions.target']    = { category: 'Columns', title: 'Actions › Target', oneliner: 'File path / command / URL the action operated on.' };
  E['col.actions.session']   = { category: 'Columns', title: 'Actions › Session', oneliner: 'Owning session ID (truncated).' };
  E['col.actions.ok']        = { category: 'Columns', title: 'Actions › OK', oneliner: '✓ for success, ✗ for failure.' };
  E['col.actions.error']     = { category: 'Columns', title: 'Actions › Error', oneliner: 'Truncated error message when success=false.' };

  // ---------- Tools-tab columns ----------
  E['col.tools.tool']         = { category: 'Columns', title: 'Tools › Tool', oneliner: 'AI client name.' };
  E['col.tools.actions']      = { category: 'Columns', title: 'Tools › Actions', oneliner: 'Total actions recorded for this tool in the window.' };
  E['col.tools.failures']     = { category: 'Columns', title: 'Tools › Failures', oneliner: 'Subset of actions with success=false.' };
  E['col.tools.success_rate'] = { category: 'Columns', title: 'Tools › Success rate', oneliner: 'Successful actions as a percentage of total.', formula: '1 - (failures / actions)', related: ['metric.success_rate'] };
  E['col.tools.sessions']     = { category: 'Columns', title: 'Tools › Sessions', oneliner: 'Distinct session count this tool appeared in (in window).' };
  E['col.tools.first_seen']   = { category: 'Columns', title: 'Tools › First seen', oneliner: 'Earliest action timestamp for this tool in the window.' };
  E['col.tools.last_seen']    = { category: 'Columns', title: 'Tools › Last seen', oneliner: 'Most recent action timestamp for this tool in the window.' };

  // ---------- Compression-tab columns ----------
  E['col.compression.model']        = { category: 'Columns', title: 'Compression › Model', oneliner: 'Upstream model ID this row aggregates.' };
  E['col.compression.original']     = { category: 'Columns', title: 'Compression › Original', oneliner: 'Pre-compression conversation payload size in bytes.' };
  E['col.compression.compressed']   = { category: 'Columns', title: 'Compression › Compressed', oneliner: 'Post-compression conversation payload size in bytes (what the proxy actually sent upstream).' };
  E['col.compression.saved']        = { category: 'Columns', title: 'Compression › Saved', oneliner: 'Bytes saved (original − compressed). Negative = compression added overhead, flagged red.', related: ['metric.saved_bytes'] };
  E['col.compression.saved_pct']    = { category: 'Columns', title: 'Compression › Saved %', oneliner: 'Saved bytes as a percentage of original.', formula: '(original - compressed) / original' };
  E['col.compression.turns']        = { category: 'Columns', title: 'Compression › Turns', oneliner: 'Number of API turns this row covers.' };
  E['col.compression.tool_results'] = { category: 'Columns', title: 'Compression › Tool results', oneliner: 'Count of tool_result blocks the pipeline compressed in-place.' };
  E['col.compression.dropped']      = { category: 'Columns', title: 'Compression › Dropped', oneliner: 'Count of blocks dropped entirely (replaced by a marker).', related: ['term.marker'] };
  E['col.compression.markers']      = { category: 'Columns', title: 'Compression › Markers', oneliner: 'Count of compression markers inserted into the conversation.', related: ['term.marker'] };

  // ---------- Discovery-tab columns ----------
  E['col.discover.file']             = { category: 'Columns', title: 'Discovery › File', oneliner: 'File path (truncated). Stale-rereads section.' };
  E['col.discover.reads']            = { category: 'Columns', title: 'Discovery › Reads', oneliner: 'Total reads of this file in the window.' };
  E['col.discover.stale']            = { category: 'Columns', title: 'Discovery › Stale', oneliner: 'Same-session re-reads of a file after it changed.', description: 'The freshness engine tags each read with fresh / stale / missing / unknown by hashing file content. A read is counted as "stale" here only when the SAME session previously read the file and the content changed between the two reads. Cross-session reads are filtered out at query time — a session that has just started has no memory of what a prior session saw, so flagging it as "stale" would imply waste that didn\'t happen.', why: 'Each stale reread is a session reading content it already had in context — that\'s wasted prompt tokens you paid for. High stale counts on hot files suggest the AI client is dropping context too aggressively or you\'re editing the file mid-session.', related: ['metric.stale_count', 'term.freshness', 'calc.freshness_classification'] };
  E['col.discover.wasted']           = {
    category: 'Columns',
    title: 'Discovery › Est. wasted tokens',
    oneliner: 'Estimated prompt tokens spent re-ingesting content the session already had.',
    description: 'Calculated as stale_count × ceil(file_size_bytes / 4). The /4 ratio comes from Claude\'s tokenizer averaging ~4 characters per token on typical source code. When file size isn\'t known (e.g. file deleted before file_state was populated), falls back to 512 tokens per read (assumes a 2 KB excerpt). Treat the absolute number as an upper-bound order-of-magnitude estimate, not exact.',
    formula: 'stale_count × ceil(file_size_bytes / 4)',
    why: 'Translates the stale-count signal into a number you can compare against your actual bill. The Discovery tab\'s ~$ wasted KPI multiplies this by the claude-sonnet-4 input rate ($3/1M) as a representative figure — your real $/M depends on which model is reading.',
    actionable: 'Sort by this column to find the highest-leverage files to pin via cache_control. A 4 KB hot file re-read 30 times is ~30k wasted tokens; pinning it as a cached prompt prefix re-bills those reads at ~$0.10 per million instead of $3 per million.',
  };
  E['col.discover.command']          = { category: 'Columns', title: 'Discovery › Command', oneliner: 'Command string (truncated). Repeated-commands section.' };
  E['col.discover.runs']             = { category: 'Columns', title: 'Discovery › Runs', oneliner: 'Total times this command was run in the window.' };
  E['col.discover.no_change_reruns'] = { category: 'Columns', title: 'Discovery › No-change reruns', oneliner: 'Re-runs of the same command with no relevant inputs changed in between.', description: 'Heuristic — checks whether any file the command\'s output referenced was modified between runs.', related: ['metric.no_change_reruns'] };
  E['col.discover.failed']           = { category: 'Columns', title: 'Discovery › Failed', oneliner: 'How many runs failed (non-zero exit code).' };

  // ---------- Patterns-tab columns ----------
  E['col.patterns.type']         = { category: 'Columns', title: 'Patterns › Type', oneliner: 'Pattern category — knowledge_snippet, command_pair, cross_tool_file, etc.' };
  E['col.patterns.confidence']   = { category: 'Columns', title: 'Patterns › Confidence', oneliner: 'Decay-weighted confidence score (0–1). Older observations weigh less.' };
  E['col.patterns.observations'] = { category: 'Columns', title: 'Patterns › Observations', oneliner: 'Raw count of observations supporting this pattern.' };
  E['col.patterns.data']         = { category: 'Columns', title: 'Patterns › Data', oneliner: 'Pattern payload (shape varies by type) — rendered as key=value pairs.' };
  E['col.pattern_type']          = { category: 'Glossary', title: 'Pattern types', oneliner: 'knowledge_snippet | command_pair | cross_tool_file | failure_correlation | session_summary.', description: '`observer patterns` derives these from session activity. Each type has its own data shape; expect command_pair to carry {first, second}, knowledge_snippet to carry {topic, snippet}, and so on.' };

  // ---------- Metrics ----------
  E['metric.net_input'] = {
    category: 'Metrics',
    title: 'Net Input tokens',
    oneliner: 'Fresh prompt tokens billed at the standard input rate (cache hits subtracted).',
    description: 'When Anthropic prompt-caching is in play, the upstream usage envelope reports total input split across three buckets: net input (fresh), cache read (cache hit), and cache creation (new cache write). "Net Input" is the fresh portion only.',
    formula: 'net_input = usage.input_tokens (already net of cache hits in Anthropic\'s envelope)',
    source: 'api_turns.input_tokens (proxy) | token_usage.input_tokens (jsonl)',
    related: ['metric.cache_read', 'metric.cache_creation', 'calc.token_math'],
  };
  E['metric.cache_read'] = {
    category: 'Metrics',
    title: 'Cache Read tokens',
    oneliner: 'Prompt tokens served from Anthropic\'s ephemeral cache, billed at ~10% of input rate.',
    description: 'When the model can replay cached prompt prefixes, those tokens come at a steep discount. The cost engine treats this as its own line item.',
    source: 'api_turns.cache_read_tokens',
    related: ['metric.cache_creation', 'term.cache_5m_vs_1h'],
  };
  E['metric.cache_creation'] = {
    category: 'Metrics',
    title: 'Cache Write / Creation tokens',
    oneliner: 'Tokens written to the ephemeral cache. Billed at 1.25× (5m tier) or 2× (1h tier) of input rate.',
    description: 'Caching is opt-in via cache_control on prompt blocks. The proxy splits totals into 5m vs 1h tiers (`cache_creation_1h_tokens` is the 1h subset; `cache_creation_tokens − cache_creation_1h_tokens` is the 5m portion).',
    source: 'api_turns.cache_creation_tokens, api_turns.cache_creation_1h_tokens',
    related: ['term.cache_5m_vs_1h', 'metric.cache_read'],
  };
  E['metric.output'] = {
    category: 'Metrics',
    title: 'Output tokens',
    oneliner: 'Tokens the model generated. Billed at the model\'s output rate (typically 5× input).',
    source: 'api_turns.output_tokens | token_usage.output_tokens',
  };
  E['metric.cost_usd'] = {
    category: 'Metrics',
    title: 'Cost (USD)',
    oneliner: 'Estimated dollar cost — what this row of activity would bill you for.',
    description: 'Preference order: (1) the upstream API\'s self-reported cost_usd field if present (the proxy passes this through verbatim — gold standard, reliability=high); (2) tokens × the model\'s pricing entry, summed across the four buckets (net input × input rate, cache_read × cache_read rate, cache_creation_5m × cache_creation rate, cache_creation_1h × cache_creation_1h rate, output × output rate). When the model isn\'t in our pricing table, cost shows $0.0000 and reliability flips to unreliable — token counts are still real, the dollar number just isn\'t computable.',
    why: 'The headline number you\'re here for. Compare against your actual upstream invoice — if there\'s a meaningful gap the most likely causes (in order) are: (1) pricing-table fallback for a SKU we don\'t list — check docs/pricing-reference.md and the "fallback" badge on Reliability; (2) per-turn dedup gaps where some turns went through the proxy and some didn\'t — JSONL gap-fill should kick in for the missing turns; (3) Claude Code JSONL streaming placeholders being slightly off (~10% on output, the unreliable reliability tag). Cost is never sourced from upstream API responses — Anthropic and OpenAI don\'t return cost_usd.',
    actionable: 'Cost suspiciously high vs your invoice? Check the Reliability column — "medium" or below means the dashboard is computing, not reading. Cost looks zero on a window with traffic? Models in that window probably aren\'t in the pricing table.',
    related: ['calc.cost_math', 'metric.reliability', 'col.cost.source'],
  };
  E['metric.turn_count'] = { category: 'Metrics', title: 'Turn count', oneliner: 'Number of API turns rolled into a row.' };
  E['metric.reliability'] = {
    category: 'Metrics',
    title: 'Reliability',
    oneliner: 'How much you should trust the cost number on this row — high / medium / low / unreliable.',
    description: 'high = the upstream API itself reported the cost (proxy captured it directly from the response envelope; this is ground truth). medium = computed by multiplying tokens × the model\'s pricing entry from the cost engine; correct as long as our pricing table is current. low = at least one bucket was estimated (e.g. cache_creation tier split inferred from totals). unreliable = no pricing entry for this model — cost shows $0.0000 but tokens are still counted.',
    why: 'Tells you whether the dollar figure is gospel or back-of-envelope. When budgeting, only "high" is upstream-truthed; "medium" is computed and depends on our pricing being current. If the bulk of a window shows "unreliable", look at unknown_model_count in the cost panel summary — you have a model the cost engine doesn\'t know about.',
    actionable: 'Lots of "unreliable" on a model that should be priced? File an issue or update internal/intelligence/cost/pricing_default.go with the right rates and rebuild.',
    related: ['metric.cost_usd', 'calc.cost_math'],
  };
  E['metric.success_rate']     = { category: 'Metrics', title: 'Success rate', oneliner: 'Fraction of actions that succeeded.', formula: '1 - (failures / total)' };
  E['metric.action_count']     = { category: 'Metrics', title: 'Action count', oneliner: 'Total actions recorded.' };
  E['metric.failure_count']    = { category: 'Metrics', title: 'Failure count', oneliner: 'Actions with success=false.' };
  E['metric.scored_count']     = { category: 'Metrics', title: 'Scored count', oneliner: 'How many sessions in the current filter have been scored by `observer score`.' };
  E['metric.quality_score']    = {
    category: 'Metrics',
    title: 'Quality score',
    oneliner: 'Single 0–1 signal of how well a session went. Higher is better.',
    description: 'Composite produced by `observer score`: blends success rate (more weight), redundancy ratio (penalises stale rereads + no-change reruns), freshness signal (penalises sessions that re-read the same files repeatedly), and a small length normaliser (very short sessions don\'t score well even if perfect — not enough signal). Above 0.7 is solid; below 0.4 something went wrong. Only populated for sessions where `observer score` has been run.',
    why: 'Lets you sort thousands of sessions by "which ones to look at first." Combined with cost_usd, it\'s the prioritisation lens — high cost + low quality = your worst-ROI sessions, look there first.',
    actionable: 'No quality column on the Sessions tab? Run `observer score` from the CLI; it\'ll fill in for any session that doesn\'t have a score yet.',
    related: ['metric.error_rate', 'metric.redundancy_ratio'],
  };
  E['metric.error_rate']       = {
    category: 'Metrics',
    title: 'Error rate',
    oneliner: 'Fraction of a session\'s actions that failed (success=false). 0 = clean, 1 = nothing worked.',
    description: 'Computed at score time as failures / total_actions per session. "Failure" here is the binary success column on each action: a non-zero exit code on a command, an error response from a tool call, an exception during file ops. Hover failures column on the Actions tab for examples.',
    why: 'The cleanest "is this session healthy?" signal. High error rates correlate with no-change reruns (the model retrying the same broken call) and stale rereads (rebuilding context after errors). A session with error_rate > 30% usually means the AI client\'s tool layer was misconfigured or the model couldn\'t recover from an early failure.',
  };
  E['metric.redundancy_ratio'] = {
    category: 'Metrics',
    title: 'Redundancy ratio',
    oneliner: 'Fraction of a session\'s actions that were redundant — stale rereads or no-change reruns.',
    description: 'Computed at score time as (stale_reread_count + no_change_rerun_count) / total_actions. Both numerator components are scoped per-session for the same reason stale_count is.',
    why: 'How much of a session was the AI re-doing work it had already done. Above 0.2 is worth investigating — usually means the session was running long enough that context started rotating out, or the AI was stuck in a loop. Use it together with quality_score to triage which sessions to dig into.',
  };
  E['metric.total_actions']    = { category: 'Metrics', title: 'Total actions (per session)', oneliner: 'Recomputed live as COUNT(*) FROM actions WHERE session_id = s.id (the stored column was historically zero).' };
  E['metric.est_wasted_tokens'] = { category: 'Metrics', title: 'Estimated wasted tokens', oneliner: 'Tokens spent on stale rereads, calculated as stale_count × ceil(file_size / 4).' };
  E['metric.stale_count']     = {
    category: 'Metrics',
    title: 'Stale count',
    oneliner: 'Number of in-session re-reads that hit a changed version of the file. Cross-session reads are NOT counted.',
    description: 'Per-file count of "stale" reads scoped to a single session. The freshness engine classifies a read as stale when the file\'s content hash differs from a previous read — but the stale-count metric only counts those where the previous read also belongs to the same session. A read in session B that lands on a file last touched by session A is treated as a fresh look (it is, from session B\'s perspective), not a stale rediscovery. WHAT THE SYSTEM DOES with this signal: detection and reporting only — the observer surfaces it on the dashboard so YOU can decide what to change. It does not block, cache, or rewrite the read in flight.',
    why: 'Each in-session stale read is a wasted prompt-token cost: the model already had the original content in its context and now has to re-ingest the new version. The Discovery tab\'s ~Tokens wasted KPI is the rough magnitude of that opportunity — fix the underlying habit and you keep those tokens.',
    actionable: 'Different fix depending on whether the rereads are MAIN-THREAD or CROSS-THREAD (see the "Cross-thread" column on the stale-rereads table):\n\n**Main-thread stales** (parent → parent re-read after edit):\n  1. cache_control on the file in your prompt — Anthropic re-bills cache reads at ~10% of input rate, so even when the file changes mid-session, the comparison cost is much lower.\n  2. Shorter sessions, especially when editing the same file repeatedly — start fresh after major refactors.\n  3. Add hot directories to .observerignore so the watcher doesn\'t even record the noise.\n\n**Cross-thread stales** (parent ↔ sub-agent re-read):\n  Pass content via the `Agent` tool\'s `prompt` parameter so the sub-agent doesn\'t re-read what the parent already read. The Agent\'s prompt becomes the sub-agent\'s initial context — embed the relevant file excerpts directly. Different mechanism than caching: this avoids the read entirely rather than discounting it.',
    related: ['term.freshness', 'calc.freshness_classification', 'col.discover.wasted', 'term.sidechain'],
  };
  E['metric.no_change_reruns'] = { category: 'Metrics', title: 'No-change reruns', oneliner: 'Re-runs of a command where no relevant inputs changed between runs.' };
  E['metric.saved_bytes']     = { category: 'Metrics', title: 'Saved bytes', oneliner: 'Bytes saved by compression. Negative = compression made it bigger (flagged red).' };
  E['metric.saved_pct']       = { category: 'Metrics', title: 'Saved %', oneliner: 'Saved bytes as a fraction of the original.' };
  E['metric.confidence']      = {
    category: 'Metrics',
    title: 'Confidence (patterns)',
    oneliner: 'How sure the observer is that this pattern is a real, repeatable behaviour vs a coincidence — 0 to 1.',
    description: 'Each pattern (knowledge_snippet, command_pair, cross_tool_file, …) is assembled from one or more observations across sessions. Confidence is a weighted score that goes UP with more independent observations and stays high when those observations are recent, but DECAYS over time so a pattern that hasn\'t been seen in months gradually drops out. Below ~0.3 the pattern is "we saw it once, take with a grain of salt"; above ~0.7 it\'s "this is the model\'s actual habit."',
    why: 'Patterns drive what gets surfaced as repeatable knowledge — used by `observer suggest` to populate CLAUDE.md / AGENTS.md / .cursorrules. Low-confidence patterns are speculation; high-confidence ones are documented behaviour. Sort by this column to see what\'s most reliably true.',
    actionable: 'Confidence below 0.3 with few observations? Wait for more data. High confidence on a pattern that no longer reflects reality? It\'ll decay out on its own as you stop reproducing it, or you can purge with `observer prune --patterns`.',
    related: ['col.pattern_type', 'metric.observation_count'],
  };
  E['metric.observation_count'] = { category: 'Metrics', title: 'Observation count', oneliner: 'How many independent observations supported this pattern.' };

  // ---------- Calculations ----------
  E['calc.token_math'] = {
    category: 'Calculations',
    title: 'Token math',
    oneliner: 'How Anthropic\'s four-bucket usage envelope composes into the numbers you see.',
    description: 'The Anthropic API returns four numbers per request: input_tokens (the prompt content NOT served from cache — already net), cache_read_input_tokens (prompt content served from the ephemeral cache), cache_creation_input_tokens (prompt content newly written to the cache), and output_tokens (what the model generated). The dashboard treats all four as separate buckets because they bill at very different rates: cache_read ≈ 0.1× input, cache_creation ≈ 1.25× input (5m tier) or 2× input (1h tier), output ≈ 5× input. Total prompt context sent for a turn = input + cache_read + cache_creation. Total tokens billed = those three + output.',
    formula: 'prompt_context = net_input + cache_read + cache_creation     |     total = prompt_context + output',
    why: 'Without the four-bucket split, "input tokens" hides which portion was cached (cheap) vs. fresh (full price). A run with 200k tokens of input could be costing 20k worth of input rate (mostly cached) or 200k worth (mostly fresh) — wildly different bills. The dashboard surfaces all four so you can tell.',
    related: ['metric.net_input', 'metric.cache_read', 'metric.cache_creation', 'metric.output', 'calc.cost_math', 'term.cache_5m_vs_1h'],
  };
  E['calc.cost_math'] = {
    category: 'Calculations',
    title: 'Cost math',
    oneliner: 'How tokens become dollars — the per-bucket pricing × tokens formula.',
    description: 'For each row: cost_usd = (net_input × p.input + cache_read × p.cache_read + cache_creation_5m × p.cache_creation + cache_creation_1h × p.cache_creation_1h + output × p.output) ÷ 1,000,000. All rates are USD per 1M tokens. The 5m vs 1h split for cache_creation: cache_creation_1h is the 1h-tier portion (priced at 2× the 5m rate); the rest (cache_creation_total − cache_creation_1h) is the 5m portion. Pricing comes from internal/intelligence/cost/pricing_default.go (baked-in defaults) plus any user overrides from observer config. When the upstream API itself reports cost_usd in the response envelope (proxy-sourced rows), that value is preferred over the computed one — gold standard.',
    formula: 'cost_usd = Σ_buckets (tokens_in_bucket × rate_per_M ÷ 1e6)',
    why: 'This is the math you can audit against your invoice. If the dashboard\'s reliability=high column matches the actual bill, the proxy is doing its job. If reliability=medium and there\'s a delta, the most likely culprit is stale pricing (rates changed since we last updated the table).',
    related: ['metric.cost_usd', 'metric.reliability', 'term.cache_5m_vs_1h', 'calc.token_math'],
  };
  E['calc.dedup_a1'] = {
    category: 'Calculations',
    title: 'JSONL dedup (audit item A1)',
    oneliner: 'Why we drop duplicate token-usage rows before computing cost.',
    description: 'When an AI client makes one upstream API call that produces N content blocks (e.g. a turn with text + 3 tool calls = 4 blocks), the client writes N JSONL log lines, and each line carries the SAME cumulative usage envelope from the response. If you summed naively, that one API call would count 2-4× in the totals. Two layers of dedup catch this: (1) the Claude Code adapter dedupes on Anthropic\'s `message.id` at write time, so new rows are clean; (2) the cost engine adds defence-in-depth dedup at rollup time on (source_file, model, timestamp-bucketed-to-minute, input, output, cache_read, cache_creation) so any pre-A1 historical rows that slipped through still get collapsed. Migration 007 ran a one-time pass that collapsed ~47% of pre-fix rows in the live install.',
    why: 'Without dedup, JSONL-sourced cost numbers are 2-4× inflated — the kind of bug that makes you think you\'re burning money you\'re not. This calc is why JSONL is usable as a historical source despite being noisy. Proxy-sourced rows don\'t need this because every request maps to one api_turns row.',
    related: ['term.token_row', 'term.proxy_vs_jsonl'],
  };
  E['calc.freshness_classification'] = {
    category: 'Calculations',
    title: 'Freshness classification',
    oneliner: 'How a file read is tagged fresh / stale / missing / etc.',
    description: 'The freshness engine hashes file content at read time and stores the hash in `file_state` keyed by (project, file). On the next read in the same session: if the hash matches → fresh; if it differs (file edited since last read) → stale; if file doesn\'t exist anymore → missing. Cross-session reads start fresh — no comparison happens against another session\'s history.',
    related: ['metric.stale_count', 'term.freshness'],
  };
  E['calc.compression_savings'] = {
    category: 'Calculations',
    title: 'Compression savings',
    oneliner: 'How saved bytes/% are calculated.',
    description: 'Per turn: original_bytes is the JSON-encoded conversation payload before the pipeline ran. compressed_bytes is what the proxy actually forwarded upstream. saved = original − compressed. Negative savings happen when payloads are small enough that marker overhead dominates — flagged red.',
    related: ['metric.saved_bytes', 'metric.saved_pct', 'term.marker'],
  };

  // ---------- Glossary ----------
  E['term.session'] = { category: 'Glossary', title: 'Session', oneliner: 'One continuous AI-coding conversation in a single tool, scoped to a single working directory.', description: 'Each session has a stable ID (claude-code\'s session UUID, codex\'s rollout ID, etc.) and lives in the `sessions` table. All actions, token rows, and api_turns associated with a session reference its ID.' };
  E['term.action']  = { category: 'Glossary', title: 'Action', oneliner: 'One normalised tool call recorded by an adapter.', description: 'Action types are normalised across all adapters: read_file, write_file, edit_file, run_command, search_text, search_files, web_search, web_fetch, mcp_call, user_prompt, task_complete.' };
  E['term.api_turn'] = { category: 'Glossary', title: 'API turn', oneliner: 'One HTTP request captured by the local API proxy.', description: 'Records one row in `api_turns` per request, with the upstream usage envelope intact. The most accurate token source — see term.proxy_vs_jsonl for why.' };
  E['term.cost_provenance'] = {
    category: 'Glossary',
    title: 'How cost is calculated',
    oneliner: 'Cost is always computed locally from token_count × pricing_table[model]. Neither Anthropic nor OpenAI returns cost_usd in API responses, so we never have provider-billed ground truth from the proxy.',
    description: 'The pricing table source of truth lives in docs/pricing-reference.md (Anthropic + OpenAI + Gemini + others). Two adapters — OpenCode and Pi — capture cost client-side from the client\'s own usage envelope and write it into estimated_cost_usd; for those rows the cost engine uses the recorded value as-is. Every other path goes through tokens × rates. When a model isn\'t in the pricing table the engine falls back to the family prefix (claude-opus-4-7 inherits claude-opus-4 family rates); the Cost-tab Reliability column flags fallback rows with a "~" so you know the rate isn\'t exact-match.',
    why: 'Knowing this matters when reconciling against an upstream invoice. Likely causes of a gap (in order): (1) a pricing-table SKU we don\'t have explicitly; (2) per-turn dedup leaving JSONL streaming-placeholder counts in for turns the proxy missed; (3) Anthropic 1h-tier cache write traffic — Anthropic\'s docs price 1h tier at 2× input ($10/M for opus, $6/M for sonnet, etc.) but Claude Code\'s built-in `/usage` command appears to price all cache writes at the 5m rate (1.25× input). When a session uses 1h cache (visible as `ephemeral_1h_input_tokens` in the JSONL), our dashboard will read ~35% higher than `/usage` for the cache-heavy portion. The actual Anthropic invoice should match our number; check the billing portal to confirm.',
    related: ['col.cost.cost', 'col.cost.reliab', 'term.proxy_vs_jsonl'],
  };
  E['term.token_row'] = { category: 'Glossary', title: 'Token row', oneliner: 'One token-usage event recovered by parsing the AI client\'s JSONL session log.', description: 'Plentiful but unreliable — clients echo cumulative usage on every content block, which the cost engine has to dedupe. See calc.dedup_a1.' };
  E['term.tool']    = { category: 'Glossary', title: 'Tool (AI client)', oneliner: 'The AI coding client that produced an action.', description: 'In this dashboard "tool" means the *client* (claude-code, cursor, codex, cline, copilot), not the per-message tool name (read_file, run_command). The latter is "Tool name" on the Actions tab.' };
  E['term.project'] = { category: 'Glossary', title: 'Project', oneliner: 'A working-directory root that owns sessions and actions.', description: 'Derived from the cwd at session start. Paths inside a `.git/worktrees/...` manager directory are folded back to the working-tree root.' };
  E['term.sidechain'] = {
    category: 'Glossary',
    title: 'Sidechain (sub-agent activity)',
    oneliner: 'Tool calls emitted inside a sub-agent runtime — parent session\'s Agent tool spawned them.',
    description: 'When the main agent calls Claude Code\'s `Agent` tool with a `prompt` and `subagent_type`, Claude Code spawns a fresh sub-agent runtime that executes the prompt independently. Every line that runtime emits gets `isSidechain: true` in the JSONL log; the observer\'s adapter copies that flag onto the actions table\'s `is_sidechain` column. Crucially, sub-agents share the parent\'s session_id — they\'re NOT separate sessions. The is_sidechain flag is the only structural marker distinguishing parent-thread work from sub-agent work.',
    why: 'Surfaces "you fanned this work out to a sub-agent" as a first-class signal so you can: (1) see how often you delegate, (2) spot cross-thread redundancy where the parent already-read a file then the sub-agent re-reads it (cost-relevant — the parent could have passed content via Agent\'s `prompt` parameter), and (3) attribute action volume correctly when comparing sessions. The Discovery tab\'s "Cross-thread" column on the stale-rereads table counts exactly this — same file × same session × different is_sidechain on the prior read.',
    actionable: 'Sessions with high sidechain_action_count are doing a lot of fan-out — usually a sign the parent is asking sub-agents for things it could do directly, or vice versa. If you see frequent cross-thread stale rereads, restructure the parent\'s Agent calls to pass content via the `prompt` parameter (the Agent\'s prompt becomes the sub-agent\'s initial context) instead of letting the child re-read files the parent already read.',
    related: ['col.sessions.sidechain', 'metric.stale_count', 'col.discover.stale'],
  };

  E['term.cross_platform_tool_calling'] = {
    category: 'Glossary',
    title: 'Cross-platform tool calling (MCP sharing)',
    oneliner: 'Every connected AI client can read every other client\'s recorded actions through the observer\'s 12-tool MCP server.',
    description: 'When `observer init` registers the MCP server with Claude Code, Cursor, and Codex, each client gains 12 tools that query the observer\'s unified database — not their own scoped slice. Concretely: when Cursor invokes `get_last_test_result`, the answer can be a `go test` run that Claude Code executed an hour earlier. When Codex calls `check_file_freshness("auth.ts")`, the freshness signal reflects every adapter that touched the file. The 12 cross-querying MCP tools: check_file_freshness, get_file_history, get_session_summary, search_past_outputs, get_last_test_result, get_failure_context, get_action_details, check_command_freshness, get_session_recovery_context, get_project_patterns, get_cost_summary, get_redundancy_report. Two layers ship today: (1) read-side via MCP queries — any agent reads any other agent\'s recorded data; (2) write-side via patterns — `observer patterns` derives `cross_tool_file` patterns and `observer suggest` writes them into CLAUDE.md/AGENTS.md/.cursorrules so future sessions inherit. Not yet shipped: synthetic-tool-result injection (replaying agent A\'s tool execution into agent B\'s conversation context) — the MCP query model gives you the result on-demand instead.',
    why: 'Coordinating across multiple AI clients without re-doing work. Run a long test in Claude Code, then ask Cursor in the same project — Cursor\'s MCP query knows the answer instead of re-running. Cross-tool overlap (files touched by ≥2 clients) is the most visible signal that this is working; surfaced on the Discovery tab\'s third panel.',
    actionable: 'Run `observer init` once per AI client to register the MCP server. After that, the cross-platform sharing is automatic — each client\'s MCP-aware tool calls read from the same DB. Look at the Discovery → Cross-tool overlap panel to see which files have multi-client activity (the highest-value targets for `cache_control` since the read cost is shared).',
    related: ['term.tool', 'col.pattern_type'],
  };
  E['term.proxy_vs_jsonl'] = {
    category: 'Glossary',
    title: 'Proxy vs JSONL',
    oneliner: 'Two ways the observer captures what your AI client did. Proxy = ground truth, JSONL = best-effort log parsing.',
    description: 'PROXY: a localhost HTTP server (default 127.0.0.1:8820) that sits between your AI client and the upstream API. Every request flows through it, so the observer sees exact request bodies, response usage envelopes, and timings — one api_turns row per HTTP call, no dedup needed. JSONL: the AI client (Claude Code, Codex, …) writes every conversation to a JSONL log on disk; the observer\'s file watcher parses these into token_usage rows. Plentiful but noisy — the clients echo cumulative usage on every content block of a multi-block response, so a single API call can write 2-4 rows the cost engine has to dedupe.',
    why: 'The proxy is the only way to get exact, real-time data — request/response bodies, conversation-compression savings, exact token counts as Anthropic / OpenAI billed them. (Cost is always computed locally from those tokens; neither provider returns a cost_usd field.) JSONL is how you get historical visibility (sessions before the proxy was wired up, or sessions where the client bypassed the proxy). When both contribute the cost engine deduplicates per-turn — proxy wins for turns it captured, JSONL fills the gaps for turns the proxy missed.',
    actionable: 'New deployment with mostly "jsonl" Source values? Engage the proxy in the shell that launches your AI client. Claude Code: ANTHROPIC_BASE_URL=http://127.0.0.1:8820. Codex / OpenAI-compatible (incl. Cursor in OpenAI mode): OPENAI_BASE_URL=http://127.0.0.1:8820/v1 (the /v1 suffix matters — the proxy routes by path). See docs/proxy-routing.md for per-client recipes.',
    related: ['col.cost.source', 'tile.api_turns', 'tile.token_rows', 'calc.dedup_a1'],
  };
  E['term.cache_5m_vs_1h'] = {
    category: 'Glossary',
    title: 'Cache 5m vs 1h tier',
    oneliner: 'Anthropic\'s prompt cache has two TTLs (5 minutes, 1 hour). The longer-lived tier costs 2× the short one to write.',
    description: 'When you set cache_control on a prompt block, Anthropic stores it in an ephemeral cache. By default the entry expires after ~5 minutes; setting `type: ephemeral, ttl: 3600` extends to 1 hour at 2× the write cost. Cache reads bill the same rate regardless of tier. The proxy records `cache_creation_1h_tokens` as the 1h-tier subset of `cache_creation_tokens`; the difference is the 5m-tier portion. Cost engine applies the right rate to each.',
    why: 'Long-running agents that re-engage every few minutes can save real money on the 1h tier — the 2× write cost is amortised over many cache hits during the hour. Short bursts pay the same write cost and lose the cache before they\'d benefit. As of writing the live data shows zero 1h-tier traffic, so this distinction is groundwork waiting on real usage.',
    related: ['metric.cache_creation', 'metric.cache_read', 'calc.cost_math'],
  };
  E['term.freshness'] = { category: 'Glossary', title: 'Freshness state', oneliner: 'Per-read tag: fresh / stale / missing / modified-elsewhere / unknown.', description: 'Each file read gets one of these tags at write time by hashing the file content. fresh = first read in this session, OR re-read where the content matches the prior read. stale = re-read where the content differs from the prior read in the same session. missing = file no longer exists. modified-elsewhere = file changed between reads but by something other than an observable AI action. unknown = couldn\'t classify (e.g. permission denied at hash time). The stale-rereads metric only counts the stale tag, scoped per session.', why: 'The freshness tag is the dashboard\'s way of distinguishing "the model is doing useful new work" from "the model lost track of what it already knew." Stale reads cost prompt tokens; fresh reads earn them. The per-session scope exists because cross-session "staleness" is meaningless — a brand-new session never saw the previous version.', related: ['calc.freshness_classification', 'metric.stale_count'] };
  E['term.conversation_compression'] = { category: 'Glossary', title: 'Conversation compression', oneliner: 'Pre-forward compression of conversation payloads in the API proxy.', description: 'Compresses or drops large tool_result blocks before forwarding to the upstream API. Replaces dropped content with markers so the model still has a reference. Opt-in via `[compression.conversation].enabled` in observer config.', related: ['term.shell_compression', 'term.marker'] };
  E['term.shell_compression'] = {
    category: 'Glossary',
    title: 'Shell-output compression',
    oneliner: 'Trimming shell command output BEFORE it gets recorded to the AI client\'s context.',
    description: 'Different from conversation compression — runs upstream of recording. When the AI client invokes a Bash hook through the observer, declarative filters (git, go test, docker, kubectl, cargo, pytest, npm, gradle, go build, …) strip routine noise (compilation status banners, dependency-resolution chatter, retry messages) before the output is recorded as the tool_result. Always on for any of the bundled command families; opt-in for new commands via [compression.shell.filters] in observer config.',
    why: 'Shell output is the noisiest source of context bloat — `go test ./...` on a big repo can dump 100k+ tokens of test names you don\'t care about. Filtering at recording time means the AI client never even sees the noise, so it can\'t consume tokens on it later. Conversation compression handles whatever still got through.',
    related: ['term.conversation_compression'],
  };
  E['term.marker']  = {
    category: 'Glossary',
    title: 'Compression marker',
    oneliner: 'A placeholder message that tells the model "something was dropped here" without dumping the original content.',
    description: 'When the conversation-compression pipeline drops a message (importance score below threshold), it doesn\'t just delete the slot — that would shift indexes and confuse tool-call references. Instead it inserts a small marker like `[compressed: tool_result, 12.4 KB → 0 B]` so the model still sees a placeholder at that position. Marker count is what the compression panel surfaces.',
    why: 'Markers are why compression doesn\'t break the conversation. The model reads "there was a tool result here, it was big, I removed it" and proceeds — vs. losing track of where it was. Marker overhead is also why small payloads can show negative savings (the marker bytes exceed what was dropped).',
    related: ['term.conversation_compression', 'metric.saved_bytes'],
  };

  // ---------- Build category groupings for the drawer ----------
  const byCategory = {};
  for (const id in E) {
    const cat = E[id].category || 'Other';
    (byCategory[cat] = byCategory[cat] || []).push(id);
  }
  for (const cat in byCategory) {
    byCategory[cat].sort((a, b) =>
      (E[a].title || a).toLowerCase().localeCompare((E[b].title || b).toLowerCase()));
  }

  return {
    entries: E,
    byCategory: byCategory,
    walkthroughURL: 'docs/dashboard-walkthrough.md',
  };
})();

// ===================================================================
// Tooltip popover — hover any element with [data-help] to see the
// oneliner; click to open the drawer at that entry.
// ===================================================================
(function () {
  const tip = document.createElement('div');
  tip.id = 'help-tip';
  tip.style.display = 'none';
  document.addEventListener('DOMContentLoaded', () => document.body.appendChild(tip));
  // If the script loads after DOMContentLoaded already fired, append now.
  if (document.body) document.body.appendChild(tip);

  function attach() {
    document.body.addEventListener('mouseover', (e) => {
      const el = e.target.closest('[data-help]');
      if (!el) return;
      const id = el.getAttribute('data-help');
      const entry = window.HELP.entries[id];
      if (!entry) return;
      tip.innerHTML =
        '<div class="help-tip-title">' + escapeForTip(entry.title || id) + '</div>' +
        '<div class="help-tip-line">' + escapeForTip(entry.oneliner || '') + '</div>' +
        '<div class="help-tip-hint">click for details</div>';
      const r = el.getBoundingClientRect();
      tip.style.display = 'block';
      // Position below the element; flip above when there isn't room.
      const left = Math.min(window.innerWidth - 320, Math.max(8, r.left));
      let top = r.bottom + 6;
      if (top + 100 > window.innerHeight) top = r.top - tip.offsetHeight - 6;
      tip.style.left = left + 'px';
      tip.style.top = top + 'px';
    });
    document.body.addEventListener('mouseout', (e) => {
      const el = e.target.closest('[data-help]');
      if (!el) return;
      tip.style.display = 'none';
    });
    document.body.addEventListener('click', (e) => {
      const el = e.target.closest('[data-help]');
      if (!el) return;
      // Skip the help-drawer popover for clicks on nav tabs. The tab
      // buttons carry data-help="tab.<name>" so the hover tooltip
      // still works for a quick definition, but the user expects a
      // tab click to do *only* tab navigation — the slide-out drawer
      // popping up on every nav was disruptive.
      if (el.closest('nav') && el.tagName === 'BUTTON' && el.dataset.tab) return;
      const id = el.getAttribute('data-help');
      if (!window.HELP.entries[id]) return;
      tip.style.display = 'none';
      window.openHelp(id);
    }, true);
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', attach);
  } else {
    attach();
  }

  function escapeForTip(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
})();

// ===================================================================
// Help drawer — slide-out from the right. Searchable glossary grouped
// by category. Deep-linkable via #help/<id>.
// ===================================================================
(function () {
  let drawer = null;
  let lastFocus = null;

  function build() {
    if (drawer) return drawer;
    drawer = document.createElement('aside');
    drawer.id = 'help-drawer';
    drawer.setAttribute('role', 'dialog');
    drawer.setAttribute('aria-label', 'Help');
    drawer.innerHTML =
      '<div class="help-drawer-head">' +
        '<input id="help-search" placeholder="Search help…" autocomplete="off">' +
        '<button id="help-close" aria-label="Close help">×</button>' +
      '</div>' +
      '<div class="help-drawer-body" id="help-body"></div>' +
      '<div class="help-drawer-foot">' +
        'Press <kbd>?</kbd> anywhere to open · <kbd>Esc</kbd> to close · ' +
        '<a href="dashboard-walkthrough.md" target="_blank">Open walkthrough doc →</a>' +
      '</div>';
    document.body.appendChild(drawer);
    drawer.querySelector('#help-close').onclick = close;
    drawer.querySelector('#help-search').addEventListener('input', (e) => {
      renderBody(e.target.value);
    });
    return drawer;
  }

  function escapeHtml(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  function renderEntry(id, opts) {
    opts = opts || {};
    const e = window.HELP.entries[id];
    if (!e) return '';
    const head = '<a class="help-entry-title" id="help-entry-' + id + '" href="#help/' + id + '">' +
                 escapeHtml(e.title || id) + '</a>';
    const cat = '<span class="help-entry-cat">' + escapeHtml(e.category || '') + '</span>';
    const one = '<div class="help-entry-line">' + escapeHtml(e.oneliner || '') + '</div>';
    // "Why it matters" gets pride of place — the user-relevance angle is
    // what makes a metric usable, not the technical definition.
    const why = e.why ? '<div class="help-entry-why"><span class="why-label">Why it matters</span><div>' + escapeHtml(e.why) + '</div></div>' : '';
    const desc = e.description ? '<div class="help-entry-desc">' + escapeHtml(e.description) + '</div>' : '';
    const formula = e.formula ? '<div class="help-entry-formula"><span class="muted">formula</span> <code>' + escapeHtml(e.formula) + '</code></div>' : '';
    const source = e.source ? '<div class="help-entry-source"><span class="muted">source</span> <code>' + escapeHtml(e.source) + '</code></div>' : '';
    const example = e.example ? '<div class="help-entry-example"><span class="muted">example</span> ' + escapeHtml(e.example) + '</div>' : '';
    // Methodology is the long-form algorithm explanation. Rendered as a
    // native <details> element so it collapses by default and the user
    // expands explicitly. Supports tiny markdown — **bold**, `code`,
    // \n\n for paragraph breaks, single \n for <br>.
    const methodology = e.methodology
      ? '<details class="help-entry-methodology"><summary>Full methodology · see more</summary>' +
        formatMethodology(e.methodology) +
        '</details>'
      : '';
    const actionable = e.actionable ? '<div class="help-entry-actionable"><span class="muted">what to do</span> ' + escapeHtml(e.actionable) + '</div>' : '';
    const related = (e.related && e.related.length)
      ? '<div class="help-entry-related"><span class="muted">see also</span> ' +
        e.related.map(r => {
          const re = window.HELP.entries[r];
          return '<a href="#help/' + r + '" data-help-jump="' + r + '">' + escapeHtml(re ? re.title : r) + '</a>';
        }).join(', ') + '</div>'
      : '';
    return '<article class="help-entry' + (opts.highlight ? ' highlight' : '') + '">' +
      '<div class="help-entry-head">' + head + cat + '</div>' +
      one + why + desc + methodology + formula + source + example + actionable + related +
      '</article>';
  }

  // Tiny markdown-ish formatter for methodology content. Escapes first
  // (so author's text can't inject markup), then re-introduces a
  // narrow safelist: **bold**, `code`, \n\n → paragraph break,
  // single \n → <br>.
  function formatMethodology(text) {
    let out = String(text || '')
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    out = out.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
    out = out.replace(/`([^`\n]+)`/g, '<code>$1</code>');
    return '<p>' + out
      .split(/\n\n/)
      .map(p => p.replace(/\n/g, '<br>'))
      .join('</p><p>') + '</p>';
  }

  function renderBody(query) {
    const body = drawer.querySelector('#help-body');
    const q = (query || '').trim().toLowerCase();
    const cats = window.HELP.byCategory;
    const order = ['Tabs', 'Tiles', 'Charts', 'Filters', 'Columns', 'Metrics', 'Calculations', 'Glossary'];
    const seen = new Set();
    const matches = (id) => {
      if (!q) return true;
      const e = window.HELP.entries[id];
      const hay = (id + ' ' + (e.title || '') + ' ' + (e.oneliner || '') +
                   ' ' + (e.description || '') + ' ' + (e.formula || '') +
                   ' ' + (e.source || '') + ' ' + (e.example || '')).toLowerCase();
      return hay.indexOf(q) !== -1;
    };
    let html = '';
    for (const cat of order) {
      const ids = (cats[cat] || []).filter(matches);
      if (!ids.length) continue;
      ids.forEach(id => seen.add(id));
      html += '<h2 class="help-cat">' + cat + '</h2>';
      for (const id of ids) html += renderEntry(id);
    }
    // catch any categories not in the canonical order
    for (const cat in cats) {
      if (order.indexOf(cat) !== -1) continue;
      const ids = cats[cat].filter(matches).filter(id => !seen.has(id));
      if (!ids.length) continue;
      html += '<h2 class="help-cat">' + cat + '</h2>';
      for (const id of ids) html += renderEntry(id);
    }
    if (!html) html = '<div class="help-empty">No help entries match "' + escapeHtml(q) + '".</div>';
    body.innerHTML = html;
    body.querySelectorAll('a[data-help-jump]').forEach(a => {
      a.addEventListener('click', (ev) => {
        ev.preventDefault();
        scrollTo(a.getAttribute('data-help-jump'));
      });
    });
  }

  function scrollTo(id) {
    if (!drawer) return;
    const target = drawer.querySelector('#help-entry-' + CSS.escape(id));
    if (!target) return;
    target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    target.parentElement.classList.add('flash');
    setTimeout(() => target.parentElement.classList.remove('flash'), 1400);
    if (location.hash !== '#help/' + id) {
      history.replaceState(null, '', '#help/' + id);
    }
  }

  function open(id) {
    build();
    lastFocus = document.activeElement;
    drawer.classList.add('open');
    drawer.querySelector('#help-search').value = '';
    renderBody('');
    if (id) {
      // wait a tick so renderBody finished
      setTimeout(() => scrollTo(id), 30);
    } else if (location.hash.indexOf('#help/') === 0) {
      const want = location.hash.slice(6);
      setTimeout(() => scrollTo(want), 30);
    }
    drawer.querySelector('#help-search').focus();
  }
  function close() {
    if (!drawer) return;
    drawer.classList.remove('open');
    if (location.hash.indexOf('#help') === 0) history.replaceState(null, '', location.pathname);
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }

  window.openHelp = open;
  window.closeHelp = close;

  document.addEventListener('keydown', (e) => {
    // '?' to open (without typing into an input)
    if (e.key === '?' && !/INPUT|TEXTAREA|SELECT/.test((e.target.tagName || ''))) {
      e.preventDefault();
      if (drawer && drawer.classList.contains('open')) close(); else open();
    }
    if (e.key === 'Escape' && drawer && drawer.classList.contains('open')) {
      e.preventDefault();
      close();
    }
  });

  // Auto-open if URL loads with #help/<id>
  window.addEventListener('load', () => {
    if (location.hash.indexOf('#help/') === 0) {
      const id = location.hash.slice(6);
      open(id);
    }
  });
})();
