import { Fragment, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  ChartShell,
  HeroStat,
  PageHeader,
  Pill,
  SegmentedControl,
  StatCard,
  Tooltip,
} from "@/components/primitives";
import { HelpInd, TitleWithHelp } from "@/components/HelpInd";
import { CopyOnClick } from "@/components/CopyOnClick";
import {
  BoltIcon,
  CoinsIcon,
  CompressIcon,
  DatabaseIcon,
  DropletIcon,
} from "@/components/icons";
import {
  CompressionSavingsChart,
  SavingsByMechanismDonut,
  type SavingsUnit,
} from "@/components/charts";
import { ChartState } from "@/components/ChartState";
import { ExperimentsCard } from "@/components/ExperimentsCard";
import { Pagination } from "@/components/DataTable";
import { useFilters, windowDays } from "@/lib/filters";
import { useApi } from "@/lib/useApi";
import {
  fmtBytes,
  fmtCompact,
  fmtInt,
  fmtPct,
  fmtUSD,
} from "@/lib/format";
import type {
  CompactionEventsResponse,
  CompressionByModelResponse,
  CompressionEventsResponse,
  CompressionRetrieval,
  CompressionRollingCost,
  CompressionTimeseries,
  SetupClaude,
  SetupCodex,
} from "@/lib/types";

const EVENTS_LIMIT = 25;

export function CompressionPage() {
  const { win, tool, project } = useFilters();
  const days = windowDays(win);
  const daysParam = days === "all" ? 36500 : days;
  const projectParam = project === "all" ? undefined : project;
  const toolParam = tool === "all" ? undefined : tool;

  const [unit, setUnit] = useState<SavingsUnit>("usd");
  const [page, setPage] = useState(1);

  const setupClaude = useApi<SetupClaude>("/api/setup/claude");
  const setupCodex = useApi<SetupCodex>("/api/setup/codex");
  const timeseries = useApi<CompressionTimeseries>(
    "/api/compression/timeseries",
    { days: daysParam, bucket: "day", tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const events = useApi<CompressionEventsResponse>(
    "/api/compression/events",
    { days: daysParam, page, limit: EVENTS_LIMIT, tool: toolParam, project: projectParam },
    [win, page, tool, project],
  );
  const retrieval = useApi<CompressionRetrieval>(
    "/api/compression/retrieval",
    { days: Math.min(365, Number(daysParam)), tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const compaction = useApi<CompactionEventsResponse>(
    "/api/compaction/events",
    { days: Math.min(365, Number(daysParam)), tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const rolling = useApi<CompressionRollingCost>(
    "/api/compression/rolling-cost",
    { days: Math.min(365, Number(daysParam)), tool: toolParam, project: projectParam },
    [win, tool, project],
  );
  const byModel = useApi<CompressionByModelResponse>(
    "/api/compression/by-model",
    { days: daysParam, tool: toolParam, project: projectParam },
    [win, tool, project],
  );

  const totals = useMemo(
    () => deriveTotals(timeseries.data),
    [timeseries.data],
  );

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Compression"
        sub="How many tokens, dollars, and bytes the proxy saved by trimming conversation context before forwarding upstream. KPIs, daily savings trajectory, savings-by-mechanism donut, recent events, and beta surfaces (CCR retrieve rate, compaction events, rolling-summarisation net delta)."
        helpId="tab.compression"
      />
      <SetupBanner
        claude={setupClaude.data}
        codex={setupCodex.data}
        onChanged={() => {
          setupClaude.reload();
          setupCodex.reload();
        }}
      />

      {/* Design 1.22: HeroStat for Total savings (~2/5 of the row at
          xl, full width below xl) + 4 smaller StatCards on the right.
          The hero's sub-line carries the events + bytes + token
          breakdown so the four right-side tiles can keep their
          narrower focus. */}
      <div className="grid grid-cols-1 gap-3 xl:grid-cols-[1.4fr_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)]">
        <HeroStat
          label="Total compression savings"
          icon={<CompressIcon />}
          loading={timeseries.loading}
          value={fmtUSD(totals.usd)}
          sub={
            <>
              across {fmtInt(totals.events)} compression events ·{" "}
              {fmtBytes(totals.bytes)} trimmed from upstream payloads · ~
              {fmtCompact(totals.bytes / 4)} tokens
            </>
          }
          spark={totals.sparkUsd}
          sparkColor="var(--success)"
          variant="accent"
        />
        <StatCard
          label="Tokens saved"
          icon={<DatabaseIcon />}
          loading={timeseries.loading}
          value={fmtCompact(totals.bytes / 4)}
          sub="≈ bytes ÷ 4 (Claude tokenizer)"
          spark={totals.sparkBytes}
          sparkColor="var(--tok-net)"
        />
        <StatCard
          label="Dollars saved"
          icon={<CoinsIcon />}
          loading={timeseries.loading}
          value={fmtUSD(totals.usd)}
          sub="priced at row's model input rate"
          spark={totals.sparkUsd}
          sparkColor="var(--success)"
        />
        <StatCard
          label="Bytes saved"
          icon={<DropletIcon />}
          loading={timeseries.loading}
          value={fmtBytes(totals.bytes)}
          sub={`across ${fmtInt(totals.days)} active days`}
          spark={totals.sparkBytes}
          sparkColor="var(--tok-read)"
        />
        <StatCard
          label="Turns compressed"
          icon={<BoltIcon />}
          loading={timeseries.loading}
          value={fmtInt(totals.events)}
          sub={
            totals.topMech
              ? `top mech: ${totals.topMech}`
              : "events across the window"
          }
          spark={totals.sparkEvents}
          sparkColor="var(--accent)"
        />
      </div>

      {/* Profile experiments (P6.4) — productized A/B over Track-R
          profiles; reports recompute arms from the session hash. */}
      <ExperimentsCard />

      {/* Savings: per-day stack + by-mechanism donut, side-by-side */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[1.5fr_1fr]">
        <ChartShell
          title={<TitleWithHelp text="Savings per day" helpId="chart.compression_over_time" />}
          sub={`${describeUnit(unit)}. Mechanisms: json / code / logs / text / diff / html / drop / tools / stash / read_cache / rolling_summary.`}
          right={
            <SegmentedControl<SavingsUnit>
              options={[
                { value: "usd", label: "$" },
                { value: "tokens", label: "Tokens" },
                { value: "bytes", label: "Bytes" },
              ]}
              value={unit}
              onChange={setUnit}
            />
          }
        >
          <ChartState
            loading={timeseries.loading && !timeseries.data}
            error={timeseries.error}
            empty={!timeseries.data?.series.length}
            emptyHint="No compression events in window. The proxy compresses request bodies on the way out — make sure Claude Code is routed through the proxy (see Setup banner above)."
            height={260}
          >
            {timeseries.data && (
              <CompressionSavingsChart
                data={timeseries.data.series}
                unit={unit}
              />
            )}
          </ChartState>
        </ChartShell>

        <ChartShell
          title={<TitleWithHelp text="Savings by mechanism" helpId="chart.compression_by_mechanism" />}
          sub={`Rolled-up share of ${describeUnit(unit).toLowerCase()} per mechanism · ${win}`}
        >
          <ChartState
            loading={timeseries.loading && !timeseries.data}
            error={timeseries.error}
            empty={!timeseries.data?.series.length}
            emptyHint="No compression activity to break down."
            height={260}
          >
            {timeseries.data && (
              <SavingsByMechanismDonut
                data={timeseries.data}
                unit={unit}
              />
            )}
          </ChartState>
        </ChartShell>
      </div>

      {/* Per-model breakdown */}
      <ChartShell
        title={<TitleWithHelp text="Per-model breakdown" helpId="chart.compression.by_model" />}
        sub="Compression savings rolled up per model × mechanism. $ is estimated by pricing saved bytes at the model's input rate (4 bytes/token)."
      >
        <ChartState
          loading={byModel.loading && !byModel.data}
          error={byModel.error}
          empty={!byModel.loading && !byModel.data?.rows.length}
          emptyHint="No per-model compression activity in window."
          height={180}
        >
          {byModel.data && <CompressionByModelTable rows={byModel.data.rows} />}
        </ChartState>
      </ChartShell>

      {/* Recent events table */}
      <ChartShell
        title="Recent compression events"
        sub={`Latest events with per-row mechanism, savings, importance, message slot. ${events.data?.total ?? 0} total in window.`}
      >
        <ChartState
          loading={events.loading && !events.data}
          error={events.error}
          empty={!events.loading && !events.data?.rows.length}
          emptyHint="No compression events recorded."
          height={200}
        >
          {events.data && <CompressionEventsTable rows={events.data.rows} />}
        </ChartState>
        {events.data && (
          <Pagination
            page={events.data.page}
            limit={events.data.limit}
            total={events.data.total}
            onPage={setPage}
            loading={events.loading}
          />
        )}
      </ChartShell>

      {/* CCR retrieve rate */}
      <ChartShell
        title={
          <span className="flex items-center gap-2">
            Reversibility — CCR retrieve rate
            <BetaTag>gpb</BetaTag>
          </span>
        }
        sub="Is stash-and-retrieve paying off? Stashed bodies that the model later retrieves (or that FTS5 search hits) signal the disk-offload mechanism is earning its keep."
      >
        <ChartState
          loading={retrieval.loading && !retrieval.data}
          error={retrieval.error}
          empty={!retrieval.data || retrieval.data.total_stashes === 0}
          emptyHint="No stashes recorded in window. CCR activates on tool_result bodies above the importance threshold."
          height={200}
        >
          {retrieval.data && <RetrievalPanel data={retrieval.data} />}
        </ChartState>
      </ChartShell>

      {/* Compaction events */}
      <ChartShell
        title={
          <span className="flex items-center gap-2">
            Compaction events
            <BetaTag>d23</BetaTag>
          </span>
        }
        sub="Post-compact recovery — when Claude Code's /compact fires, the proxy injects ghost-file snapshots so the next turn knows what was already loaded."
      >
        <ChartState
          loading={compaction.loading && !compaction.data}
          error={compaction.error}
          empty={!compaction.data || compaction.data.count === 0}
          emptyHint="No /compact events in window."
          height={200}
        >
          {compaction.data && <CompactionPanel data={compaction.data} />}
        </ChartState>
      </ChartShell>

      {/* Rolling summarisation net cost */}
      <ChartShell
        title={
          <span className="flex items-center gap-2">
            Rolling-summarisation net cost
            <BetaTag>d20</BetaTag>
          </span>
        }
        sub="Anthropic Haiku summary calls vs. the cache_creation savings they unlock on subsequent turns. Positive = paying off."
      >
        <ChartState
          loading={rolling.loading && !rolling.data}
          error={rolling.error}
          empty={!rolling.data || rolling.data.summary_calls === 0}
          emptyHint="No rolling-summary calls in window."
          height={140}
        >
          {rolling.data && <RollingPanel data={rolling.data} />}
        </ChartState>
      </ChartShell>
    </div>
  );
}

// --------------------------------------------------------------- helpers

function deriveTotals(ts?: CompressionTimeseries | null) {
  if (!ts) {
    return {
      usd: 0,
      bytes: 0,
      events: 0,
      days: 0,
      topMech: "",
      topMechUSD: 0,
      sparkUsd: [] as number[],
      sparkBytes: [] as number[],
      sparkEvents: [] as number[],
    };
  }
  let usd = 0,
    bytes = 0,
    events = 0,
    days = 0;
  const byMech: Record<string, number> = {};
  const sparkUsd: number[] = [];
  const sparkBytes: number[] = [];
  const sparkEvents: number[] = [];
  for (const p of ts.series) {
    if (p.total_saved_usd_est > 0 || p.total_saved_bytes > 0 || p.total_count > 0)
      days++;
    usd += p.total_saved_usd_est;
    bytes += p.total_saved_bytes;
    events += p.total_count;
    sparkUsd.push(p.total_saved_usd_est);
    sparkBytes.push(p.total_saved_bytes);
    sparkEvents.push(p.total_count);
    for (const [m, s] of Object.entries(p.by_mechanism)) {
      byMech[m] = (byMech[m] ?? 0) + s.saved_usd_est;
    }
  }
  let topMech = "",
    topMechUSD = 0;
  for (const [m, v] of Object.entries(byMech)) {
    if (v > topMechUSD) {
      topMech = m;
      topMechUSD = v;
    }
  }
  return {
    usd,
    bytes,
    events,
    days,
    topMech,
    topMechUSD,
    sparkUsd,
    sparkBytes,
    sparkEvents,
  };
}

function describeUnit(u: SavingsUnit): string {
  switch (u) {
    case "usd":
      return "Dollars saved";
    case "tokens":
      return "Token estimate (saved_bytes ÷ 4)";
    case "bytes":
      return "Bytes saved";
  }
}

// --------------------------------------------------------------- Setup

function SetupBanner({
  claude,
  codex,
  onChanged,
}: {
  claude: SetupClaude | null;
  codex: SetupCodex | null;
  onChanged: () => void;
}) {
  const [expanded, setExpanded] = useState<"claude" | "codex" | null>(null);
  if (!claude && !codex) return null;
  const claudeOK =
    claude?.status === "oauth_ready" || claude?.status === "api_key_ready";
  const codexOK = codex?.status === "routed_to_observer";
  const allOK = claudeOK && codexOK;
  const intent = allOK ? "success" : "warn";
  const intentBorder =
    intent === "success" ? "border-success/30" : "border-warn/30";
  const intentBg = intent === "success" ? "bg-success-soft" : "bg-warn-soft";
  const intentFg = intent === "success" ? "text-success" : "text-warn";
  return (
    <div
      className={`rounded-3 border text-[11.5px] ${intentBorder} ${intentBg}`}
    >
      <div className="flex items-center gap-3 px-4 py-2">
        <span className={`font-semibold ${intentFg}`}>Proxy</span>
        <StatusPill
          label="Claude"
          status={claude?.status ?? "unknown"}
          ok={claudeOK}
          active={expanded === "claude"}
          onClick={() =>
            setExpanded((cur) => (cur === "claude" ? null : "claude"))
          }
        />
        <StatusPill
          label="Codex"
          status={codex?.status ?? "unknown"}
          ok={codexOK}
          active={expanded === "codex"}
          onClick={() =>
            setExpanded((cur) => (cur === "codex" ? null : "codex"))
          }
        />
        {claude && claude.proxy_port > 0 && (
          <span className="text-fg-3">port {claude.proxy_port}</span>
        )}
        <div className="flex-1" />
        {!allOK && (
          <Link
            to="/settings?section=compression"
            className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 font-semibold text-fg-1 hover:bg-bg-3"
          >
            Configure now
          </Link>
        )}
      </div>
      {expanded === "claude" && claude && (
        <>
          <ExpandedDetail
            rows={[
              ["Status", claude.status],
              ["Proxy port", claude.proxy_port ? String(claude.proxy_port) : "—"],
              ["Proxy URL", claude.proxy_url || "—"],
              ["Credentials path", claude.credentials_path || "—"],
              ["OAuth credentials", claude.has_oauth_credentials ? "yes" : "no"],
              [
                "Claude binary",
                claude.claude_binary_found
                  ? claude.claude_binary_path || "found"
                  : "not installed",
              ],
              ["Launcher command", claude.launcher_command || "—"],
              [
                "Durable route",
                claude.routed_to_observer
                  ? `routed to this observer (${claude.routed_base_url})`
                  : claude.routed_base_url
                    ? `set to ${claude.routed_base_url}`
                    : "(not set)",
              ],
              ["Settings file", claude.settings_path || "—"],
            ]}
          />
          <RouteAction
            endpoint="/api/setup/claude"
            routed={claude.routed_to_observer}
            wouldRegister={claude.would_register}
            conflictError={claude.would_register_error}
            configPath={claude.settings_path || "~/.claude/settings.json"}
            writeSummary={`Writes "env": { "ANTHROPIC_BASE_URL": "${claude.proxy_url}" } into the file below. Claude Code picks it up on its next session — every session then routes through the proxy (exact tokens, compression, cache tracking) with no wrapper command.`}
            routedNote="New Claude Code sessions route through this observer. Undo: remove the env entry from settings.json, or run `observer uninstall --claude-code`."
            onChanged={onChanged}
          />
        </>
      )}
      {expanded === "codex" && codex && (
        <>
          <ExpandedDetail
            rows={[
              ["Status", codex.status],
              ["Config path", codex.config_path || "—"],
              ["Config exists", codex.config_exists ? "yes" : "no"],
              ["Proxy port", codex.proxy_port ? String(codex.proxy_port) : "—"],
              ["Desired base URL", codex.desired_base_url || "—"],
              [
                "Current base URL",
                codex.current_base_url || "(unset)",
              ],
              [
                "Desired model provider",
                codex.desired_model_provider || "—",
              ],
              [
                "Reserved openai block",
                codex.has_reserved_openai_block ? "yes" : "no",
              ],
              ...(codex.auth_mode
                ? [["Auth mode", codex.auth_mode] as const]
                : []),
              ...(codex.would_register_error
                ? [["Register error", codex.would_register_error] as const]
                : []),
            ]}
          />
          <RouteAction
            endpoint="/api/setup/codex"
            routed={codex.status === "routed_to_observer"}
            wouldRegister={codex.would_register}
            conflictError={codex.would_register_error}
            configPath={codex.config_path || "~/.codex/config.toml"}
            writeSummary={`Adds an "${codex.desired_model_provider}" model provider with base_url ${codex.desired_base_url} to the file below and points codex at it. Codex picks it up on its next run — sessions route through the proxy with no wrapper command.`}
            routedNote="Codex routes through this observer. Undo: remove the provider from config.toml, or run `observer uninstall --codex`."
            onChanged={onChanged}
          />
        </>
      )}
    </div>
  );
}

// RouteAction — the L1 one-click durable-routing control (usability
// arc P1.5/P1.6). Explicit-consent flow: the button never writes on
// first click; it expands a preview of the exact file change, and only
// the confirm click POSTs. A 409 conflict (a base URL the user set
// deliberately) surfaces the server's explanation and requires a
// separate force confirmation.
function RouteAction({
  endpoint,
  routed,
  wouldRegister,
  conflictError,
  configPath,
  writeSummary,
  routedNote,
  onChanged,
}: {
  endpoint: string;
  routed: boolean;
  wouldRegister: boolean;
  conflictError?: string;
  configPath: string;
  writeSummary: string;
  routedNote: string;
  onChanged: () => void;
}) {
  const [phase, setPhase] = useState<
    "idle" | "confirm" | "forceConfirm" | "working"
  >("idle");
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);

  async function run(force: boolean) {
    setPhase("working");
    setErr(null);
    try {
      const res = await fetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ force }),
      });
      const out = (await res.json().catch(() => null)) as
        | { error?: string; already_set?: boolean }
        | null;
      if (res.status === 409) {
        setErr(out?.error ?? "conflict — an existing value blocks the write");
        setPhase("forceConfirm");
        return;
      }
      if (!res.ok) {
        throw new Error(out?.error || `HTTP ${res.status}`);
      }
      setDone(
        out?.already_set
          ? "Already routed — nothing to change."
          : "Routed. New sessions go through the proxy.",
      );
      setPhase("idle");
      onChanged();
    } catch (e: unknown) {
      setErr(e instanceof Error ? e.message : String(e));
      setPhase("idle");
    }
  }

  return (
    <div className="border-t border-line-1 px-4 py-3 text-[11.5px]">
      {routed ? (
        <p className="m-0 text-fg-3">
          <span className="font-semibold text-success">Routed.</span>{" "}
          {routedNote}
        </p>
      ) : (
        <div className="space-y-2">
          {phase === "idle" && (
            <div className="flex flex-wrap items-center gap-3">
              <button
                type="button"
                onClick={() => setPhase("confirm")}
                className="rounded-2 bg-accent px-2.5 py-1 font-semibold text-accent-on transition-opacity hover:opacity-90"
              >
                Route through the observer proxy…
              </button>
              {!wouldRegister && conflictError && (
                <span className="text-warn">{conflictError}</span>
              )}
              {done && <span className="text-success">{done}</span>}
              {err && <span className="text-danger">{err}</span>}
            </div>
          )}
          {(phase === "confirm" || phase === "forceConfirm") && (
            <div className="rounded-2 border border-line-2 bg-bg-2 p-3">
              <p className="m-0 text-fg-2">{writeSummary}</p>
              <p className="m-0 mt-1 font-mono text-fg-3">{configPath}</p>
              {phase === "forceConfirm" && err && (
                <p className="m-0 mt-2 text-warn">
                  {err} — overwriting replaces a value you (or another
                  tool) set deliberately.
                </p>
              )}
              <div className="mt-2 flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => run(phase === "forceConfirm")}
                  className={`rounded-2 px-2.5 py-1 font-semibold ${
                    phase === "forceConfirm"
                      ? "bg-warn text-bg-0"
                      : "bg-accent text-accent-on"
                  } transition-opacity hover:opacity-90`}
                >
                  {phase === "forceConfirm"
                    ? "Force overwrite"
                    : "Write it"}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setPhase("idle");
                    setErr(null);
                  }}
                  className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-fg-2 hover:bg-bg-3"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
          {phase === "working" && <span className="text-fg-3">Writing…</span>}
        </div>
      )}
    </div>
  );
}

function StatusPill({
  label,
  status,
  ok,
  active,
  onClick,
}: {
  label: string;
  status: string;
  ok: boolean;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <Tooltip content={`Show ${label} setup detail`}>
    <button
      type="button"
      onClick={onClick}
      className={`inline-flex items-center gap-1.5 rounded-pill border px-2 py-0.5 text-[10.5px] transition-colors ${
        active
          ? "border-accent/60 bg-bg-2"
          : "border-line-2 bg-bg-2/60 hover:border-line-3"
      }`}
    >
      <span className="font-semibold text-fg-2">{label}:</span>
      <span className={ok ? "font-mono text-success" : "font-mono text-warn"}>
        {status}
      </span>
      <span aria-hidden className="text-fg-4">
        {active ? "▴" : "▾"}
      </span>
    </button>
    </Tooltip>
  );
}

function ExpandedDetail({
  rows,
}: {
  rows: readonly (readonly [string, string])[];
}) {
  if (rows.length === 0) return null;
  return (
    <dl className="grid grid-cols-[140px_minmax(0,1fr)] gap-x-3 gap-y-1 border-t border-line-1 px-4 py-2 text-[11px]">
      {rows.map(([k, v]) => (
        <Fragment key={k}>
          <dt className="text-fg-3">{k}</dt>
          <dd className="min-w-0 break-words font-mono text-fg-1">{v}</dd>
        </Fragment>
      ))}
    </dl>
  );
}

// --------------------------------------------------------------- Events

function CompressionEventsTable({
  rows,
}: {
  rows: CompressionEventsResponse["rows"];
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[1080px] text-left text-[11.5px]">
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="border-b border-line-2">
            <th className="py-1.5 pl-2 font-medium">When</th>
            <th className="py-1.5 font-medium">Mech</th>
            <th className="py-1.5 font-medium">Model<HelpInd id="column.compression.model" /></th>
            <th className="py-1.5 text-right font-medium">Original<HelpInd id="column.compression.original" /></th>
            <th className="py-1.5 text-right font-medium">Compressed<HelpInd id="column.compression.compressed" /></th>
            <th className="py-1.5 text-right font-medium">Saved<HelpInd id="column.compression.saved" /></th>
            <th className="py-1.5 text-right font-medium">Save %<HelpInd id="column.compression.saved_pct" /></th>
            <th className="py-1.5 text-right font-medium">$ saved<HelpInd id="column.compression.saved" /></th>
            <th className="py-1.5 text-right font-medium">Slot</th>
            <th className="py-1.5 text-right font-medium">Importance</th>
            <th className="py-1.5 font-medium">Session</th>
            <th className="py-1.5 pl-3 font-medium">Source</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const savePct =
              r.original_bytes > 0 ? r.saved_bytes / r.original_bytes : 0;
            return (
              <tr
                key={r.id}
                className="border-b border-line-1 last:border-b-0 hover:bg-bg-3/40"
              >
                <Tooltip content={r.timestamp}>
                  <td tabIndex={0} className="cursor-help py-1.5 pl-2 text-fg-2 focus:outline-none">
                    {relativeTime(r.timestamp)}
                  </td>
                </Tooltip>
                <td className="py-1.5 font-mono text-fg-1">{r.mechanism}</td>
                <td className="py-1.5 font-mono text-fg-2">
                  {r.model || "—"}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtBytes(r.original_bytes)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtBytes(r.compressed_bytes)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-1">
                  {fmtBytes(r.saved_bytes)}
                </td>
                <td className="py-1.5">
                  <div className="ml-auto flex max-w-[140px] items-center justify-end gap-2">
                    <div className="h-1.5 w-[80px] overflow-hidden rounded-pill bg-bg-3">
                      <span
                        className="block h-full"
                        style={{
                          width: `${Math.max(0, Math.min(100, savePct * 100))}%`,
                          background:
                            savePct >= 0.5
                              ? "var(--success)"
                              : savePct >= 0.2
                                ? "var(--info)"
                                : savePct < 0
                                  ? "var(--danger)"
                                  : "var(--fg-3)",
                        }}
                      />
                    </div>
                    <span
                      className={
                        "tabular-nums " +
                        (savePct >= 0.5
                          ? "text-success"
                          : savePct >= 0.2
                            ? "text-fg-1"
                            : savePct < 0
                              ? "text-danger"
                              : "text-fg-3")
                      }
                    >
                      {fmtPct(savePct)}
                    </span>
                  </div>
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-0">
                  {r.saved_usd_est > 0 ? fmtUSD(r.saved_usd_est) : "—"}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-3">
                  {r.msg_index >= 0 ? r.msg_index : "—"}
                </td>
                <td className="py-1.5 text-right tabular-nums">
                  {r.importance_score > 0 ? (
                    <Tooltip content={`importance_score = ${r.importance_score.toFixed(3)}`}>
                      <span
                        tabIndex={0}
                        className={`cursor-help focus:outline-none ${
                          r.importance_score >= 0.7
                            ? "text-success"
                            : r.importance_score >= 0.4
                              ? "text-fg-1"
                              : "text-fg-3"
                        }`}
                      >
                        {r.importance_score.toFixed(2)}
                      </span>
                    </Tooltip>
                  ) : (
                    <span className="text-fg-4">—</span>
                  )}
                </td>
                <td className="py-1.5">
                  {r.session_id ? (
                    <CopyOnClick
                      value={r.session_id}
                      className="font-mono text-[11px] text-fg-2"
                    >
                      {r.session_id.slice(0, 8)}…
                    </CopyOnClick>
                  ) : (
                    <span className="text-fg-4">—</span>
                  )}
                </td>
                <td className="py-1.5 pl-3">
                  {r.is_subagent_runtime ? (
                    <Pill variant="accent">subagent</Pill>
                  ) : (
                    <Pill>main</Pill>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// --------------------------------------------------------------- Retrieval

function RetrievalPanel({ data }: { data: CompressionRetrieval }) {
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard label="Total stashes" value={fmtInt(data.total_stashes)} />
        <StatCard
          label="Retrievals"
          value={fmtInt(data.stash_retrievals)}
          sub={
            data.total_stashes > 0
              ? fmtPct(data.stash_retrievals / data.total_stashes)
              : undefined
          }
        />
        <StatCard
          label="Retrieve rate"
          value={fmtPct(data.retrieve_rate)}
          sub="% retrieves per stash"
          accent={data.retrieve_rate > 0.5}
        />
        <StatCard
          label="Search hits"
          value={fmtInt(data.search_hits)}
          sub="FTS5 lookups"
        />
      </div>

      {(data.top_retrieved_shas.length > 0 ||
        data.top_searched_actions.length > 0) && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          {data.top_retrieved_shas.length > 0 && (
            <div className="rounded-2 border border-line-1 bg-bg-3/40 p-3">
              <div className="mb-2 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
                Top retrieved SHAs
              </div>
              <ul className="space-y-1 text-[11px]">
                {data.top_retrieved_shas.slice(0, 8).map((s) => (
                  <li
                    key={s.sha}
                    className="flex items-baseline justify-between gap-2"
                  >
                    <Tooltip content={<span className="break-all font-mono">{s.sha}</span>} maxWidth={360}>
                      <span tabIndex={0} className="cursor-help truncate font-mono text-fg-1 focus:outline-none">
                        {s.sha.slice(0, 12)}…
                      </span>
                    </Tooltip>
                    <span className="tabular-nums text-fg-2">
                      {fmtInt(s.count)}×
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {data.top_searched_actions.length > 0 && (
            <div className="rounded-2 border border-line-1 bg-bg-3/40 p-3">
              <div className="mb-2 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
                Top searched actions
              </div>
              <ul className="space-y-1 text-[11px]">
                {data.top_searched_actions.slice(0, 8).map((a) => (
                  <li
                    key={a.action_id}
                    className="flex items-baseline justify-between gap-2"
                  >
                    <span className="font-mono text-fg-1">
                      #{a.action_id}
                    </span>
                    <span className="tabular-nums text-fg-2">
                      {fmtInt(a.count)} hits
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// --------------------------------------------------------------- Compaction

function CompactionPanel({ data }: { data: CompactionEventsResponse }) {
  const rejectRate =
    data.count > 0 ? 1 - data.injections_fired / data.count : 0;
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatCard label="/compact events" value={fmtInt(data.count)} />
        <StatCard
          label="Sessions affected"
          value={fmtInt(data.sessions_affected)}
        />
        <StatCard
          label="Injections fired"
          value={fmtInt(data.injections_fired)}
          sub={
            data.count > 0
              ? `${fmtPct(data.injections_fired / data.count)} of events`
              : undefined
          }
        />
        <StatCard
          label="Reject rate"
          value={fmtPct(rejectRate)}
          warn={rejectRate > 0.2}
          sub="injection unavailable or skipped"
        />
      </div>

      {data.events.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[700px] text-left text-[11.5px]">
            <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
              <tr className="border-b border-line-2">
                <th className="py-1.5 pl-2 font-medium">When</th>
                <th className="py-1.5 font-medium">Tool</th>
                <th className="py-1.5 font-medium">Session</th>
                <th className="py-1.5 text-right font-medium">Pre-actions</th>
                <th className="py-1.5 text-right font-medium">Ghost files</th>
                <th className="py-1.5 text-right font-medium">File snapshot</th>
                <th className="py-1.5 pl-3 font-medium">Injected</th>
              </tr>
            </thead>
            <tbody>
              {data.events.slice(0, 12).map((e) => (
                <tr
                  key={e.id}
                  className="border-b border-line-1 last:border-b-0 hover:bg-bg-3/40"
                >
                  <Tooltip content={e.timestamp}>
                    <td tabIndex={0} className="cursor-help py-1.5 pl-2 text-fg-2 focus:outline-none">
                      {relativeTime(e.timestamp)}
                    </td>
                  </Tooltip>
                  <td className="py-1.5 font-mono text-fg-2">{e.tool}</td>
                  <td className="py-1.5 font-mono text-fg-2">
                    {e.session_id.slice(0, 8)}…
                  </td>
                  <td className="py-1.5 text-right tabular-nums text-fg-1">
                    {fmtInt(e.pre_action_count)}
                  </td>
                  <td className="py-1.5 text-right tabular-nums text-fg-2">
                    {fmtInt(e.ghost_files_after_count)}
                  </td>
                  <td className="py-1.5 text-right tabular-nums text-fg-2">
                    {fmtInt(e.file_snapshot_count)}
                  </td>
                  <td className="py-1.5 pl-3">
                    {e.injected_at ? (
                      <Pill variant="success">yes</Pill>
                    ) : (
                      <Pill variant="danger">no</Pill>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// --------------------------------------------------------------- Rolling

function RollingPanel({ data }: { data: CompressionRollingCost }) {
  const positive = data.net_delta_usd > 0;
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
      <StatCard
        label="Summary calls"
        value={fmtInt(data.summary_calls)}
        sub={`${fmtCompact(data.summary_input_tokens)} in · ${fmtCompact(data.summary_output_tokens)} out`}
      />
      <StatCard
        label="Summary cost"
        value={fmtUSD(data.summary_cost_usd)}
        sub="Haiku spend"
      />
      <StatCard
        label="Savings unlocked"
        value={fmtUSD(data.rolling_savings_cost_usd_est)}
        sub={`${fmtCompact(data.rolling_savings_tokens_est)} cache_creation tokens`}
      />
      <StatCard
        label="Net delta"
        value={fmtUSD(data.net_delta_usd)}
        accent={positive}
        warn={!positive}
        sub={positive ? "paying off" : "losing money"}
      />
    </div>
  );
}

// --------------------------------------------------------------- utils

// BetaTag — small green capsule next to a section title that flags
// the underlying protocol / draft ID (gpb / d23 / d20). Matches the
// design's section-status chip styling.
function CompressionByModelTable({
  rows,
}: {
  rows: CompressionByModelResponse["rows"];
}) {
  return (
    <div className="overflow-x-auto rounded-2 border border-line-1">
      <table className="w-full min-w-[820px] text-left text-[11.5px]">
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="border-b border-line-2">
            <th className="py-1.5 pl-3 font-medium">Model</th>
            <th className="py-1.5 font-medium">Mechanism</th>
            <th className="py-1.5 text-right font-medium">Events</th>
            <th className="py-1.5 text-right font-medium">Original</th>
            <th className="py-1.5 text-right font-medium">Compressed</th>
            <th className="py-1.5 text-right font-medium">Saved</th>
            <th className="py-1.5 text-right font-medium">Save %</th>
            <th className="py-1.5 pr-3 text-right font-medium">$ saved (est)</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => {
            const savePct =
              r.original_bytes > 0
                ? (r.saved_bytes / r.original_bytes) * 100
                : 0;
            return (
              <tr
                key={`${r.model}|${r.mechanism}|${i}`}
                className={
                  "border-b border-line-1 last:border-b-0 " +
                  (i % 2 === 1 ? "bg-bg-3/30" : "")
                }
              >
                <td className="py-1.5 pl-3 font-mono text-fg-1">{r.model}</td>
                <td className="py-1.5">
                  <Pill>{r.mechanism}</Pill>
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtInt(r.events)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtBytes(r.original_bytes)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-2">
                  {fmtBytes(r.compressed_bytes)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-0">
                  {fmtBytes(r.saved_bytes)}
                </td>
                <td className="py-1.5 text-right tabular-nums text-fg-1">
                  {savePct.toFixed(1)}%
                </td>
                <td className="py-1.5 pr-3 text-right tabular-nums text-fg-0">
                  {r.saved_usd_est > 0 ? (
                    <span className="font-semibold">
                      {fmtUSD(r.saved_usd_est)}
                    </span>
                  ) : (
                    <span className="text-fg-4">—</span>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function BetaTag({ children }: { children: React.ReactNode }) {
  return (
    <span className="rounded-pill border border-success/40 bg-success-soft px-1.5 py-px font-mono text-[9.5px] font-medium uppercase tracking-[0.04em] text-success">
      {children}
    </span>
  );
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "—";
  const ms = Date.now() - t;
  if (ms < 0) return "future";
  const s = ms / 1000;
  if (s < 60) return `${Math.round(s)}s ago`;
  const m = s / 60;
  if (m < 60) return `${Math.round(m)}m ago`;
  const h = m / 60;
  if (h < 24) return `${Math.round(h)}h ago`;
  const d = h / 24;
  if (d < 14) return `${Math.round(d)}d ago`;
  return `${Math.round(d / 7)}w ago`;
}
