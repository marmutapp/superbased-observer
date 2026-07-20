import type { ReactNode } from "react";
import { api, type VendorTelemetry } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { compact, dateTime, num, pct, usd } from "@/lib/format";
import { Card, Empty, ErrorState, PageHeader } from "@/components/ui";
import { Pill, StatCard, StatStripSkeleton, TableSkeleton, ToolGlyphFrame } from "@/components/primitives";

// Maps the rollup vendor id to a tools-registry key (for the provider glyph
// + color). Falls back to "agnostic" for an unknown future vendor.
const VENDOR_TOOL: Record<string, string> = {
  claude_code: "claude-code",
  codex: "codex",
  copilot: "copilot",
};

function humanize(key: string): string {
  return key.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

export function TelemetryPage() {
  const { days } = useFilters();
  const { data, error, loading, reload } = useApi(() => api.telemetry(days), [days]);

  return (
    <>
      <PageHeader
        title="Native telemetry"
        subtitle="Vendor analytics pulled from each provider's own admin console (Claude Code / Codex / Copilot). Org-aggregate, content-free."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <StatStripSkeleton count={3} />
          <Card className="p-4"><TableSkeleton rows={5} /></Card>
        </div>
      ) : !data.configured || data.vendors.length === 0 ? (
        <NotConfigured />
      ) : (
        <Configured vendors={data.vendors} />
      )}
    </>
  );
}

// NotConfigured is the honest empty state for the common case: no native-console
// poller has been wired, so the analytics tables hold no rows. It names the
// exact dependency (an admin org-analytics key per vendor) rather than implying
// data is "coming soon".
function NotConfigured() {
  return (
    <Card className="p-6">
      <h3 className="text-[15px] font-semibold text-fg-0">Native telemetry not configured</h3>
      <p className="mt-2 max-w-2xl text-sm leading-relaxed text-fg-2">
        No native-console poller has populated vendor analytics for this window.
        These metrics come from each provider's own org-analytics API — they are
        distinct from the per-session capture the agents push. To light this up,
        an org admin configures a poller with the matching admin key:
      </p>
      <ul className="mt-4 space-y-2 text-sm text-fg-2">
        <li className="flex items-start gap-2">
          <ToolGlyphFrame tool="claude-code" size={20} />
          <span>
            <b className="text-fg-1">Claude Code</b> — Anthropic Admin Analytics API.
            Surfaces lines accepted/rejected, accept rate, token + cost mix.
          </span>
        </li>
        <li className="flex items-start gap-2">
          <ToolGlyphFrame tool="codex" size={20} />
          <span>
            <b className="text-fg-1">Codex</b> — OpenAI org Usage/Cost API and/or
            ChatGPT-Enterprise workspace analytics. Cost in dollars or credits.
          </span>
        </li>
        <li className="flex items-start gap-2">
          <ToolGlyphFrame tool="copilot" size={20} />
          <span>
            <b className="text-fg-1">GitHub Copilot</b> — Copilot usage-metrics,
            seats, and enhanced-billing APIs. Surfaces seat utilization +
            engagement counts.
          </span>
        </li>
      </ul>
      <p className="mt-4 text-[12px] text-fg-3">
        See <span className="font-mono text-fg-2">docs/native-console-integration-template.md</span>.
        Token, cost, action, tool, and model metrics are already available on the
        other pages from the agents' own capture.
      </p>
    </Card>
  );
}

function Configured({ vendors }: { vendors: VendorTelemetry[] }) {
  const totalUSD = vendors.reduce((a, v) => a + v.cost_usd, 0);
  const totalCredits = vendors.reduce((a, v) => a + (v.credits_cost ?? 0), 0);
  return (
    <div className="space-y-5">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <StatCard label="Vendors reporting" value={num(vendors.length)} />
        <StatCard label="Cost (USD-metered)" value={usd(totalUSD)} accent />
        <StatCard
          label="Credits-metered"
          value={totalCredits > 0 ? `${compact(totalCredits)} cr` : "—"}
          sub={totalCredits > 0 ? "Codex Enterprise (not USD)" : "none"}
        />
      </div>
      {vendors.map((v) => (
        <VendorCard key={v.vendor} v={v} />
      ))}
    </div>
  );
}

function VendorCard({ v }: { v: VendorTelemetry }) {
  return (
    <Card className="space-y-4 p-4">
      <header className="flex items-center gap-3">
        <ToolGlyphFrame tool={VENDOR_TOOL[v.vendor] ?? "agnostic"} size={28} />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <b className="text-[14px] text-fg-0">{v.display_name}</b>
            {(v.surfaces ?? []).map((s) => (
              <Pill key={s} variant="info">
                {s}
              </Pill>
            ))}
          </div>
          <div className="text-[11px] text-fg-3">
            {num(v.days)} day{v.days === 1 ? "" : "s"} with data
            {v.last_pulled_at ? ` · pulled ${dateTime(v.last_pulled_at)}` : ""}
          </div>
        </div>
      </header>

      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
        <Tile label="Cost" value={costValue(v)} sub={costSub(v)} accent />
        {v.tokens ? (
          <Tile
            label="Tokens"
            value={compact(v.tokens.net_input + v.tokens.output)}
            sub={`${compact(v.tokens.cache_read)} cache read`}
          />
        ) : null}
        {v.acceptance ? (
          <Tile
            label="Accept rate"
            value={pct(v.acceptance.accept_rate)}
            sub={`${num(v.acceptance.accepted)} / ${num(v.acceptance.accepted + v.acceptance.rejected)} edits`}
          />
        ) : null}
        {v.seats ? (
          <Tile
            label="Seat utilization"
            value={pct(v.seats.utilization)}
            sub={`${num(v.seats.active)} of ${num(v.seats.total)} seats active`}
          />
        ) : null}
      </div>

      {v.engagement && v.engagement.length > 0 ? (
        <div>
          <div className="mb-2 text-[10px] font-semibold uppercase tracking-wide text-fg-3">
            Engagement
          </div>
          <div className="grid grid-cols-2 gap-x-6 gap-y-1.5 sm:grid-cols-3">
            {v.engagement.map((e) => (
              <div key={e.key} className="flex items-baseline justify-between gap-2 text-[12.5px]">
                <span className="truncate text-fg-2">{humanize(e.key)}</span>
                <span className="font-mono text-fg-1">{compact(e.count)}</span>
              </div>
            ))}
          </div>
        </div>
      ) : null}

      {!v.tokens && !v.acceptance && !v.seats && (!v.engagement || v.engagement.length === 0) && v.cost_usd === 0 && !(v.credits_cost && v.credits_cost > 0) ? (
        <Empty message="No metrics reported for this vendor in the window." />
      ) : null}
    </Card>
  );
}

// costValue renders the headline cost honoring the unit trap: USD where the
// vendor metered in dollars, credits where it metered in credits, an em-dash
// when no cost was reported.
function costValue(v: VendorTelemetry): string {
  if (v.cost_unit === "credits") return `${compact(v.credits_cost ?? 0)} cr`;
  if (v.cost_usd > 0 || v.cost_unit === "usd" || v.cost_unit === "mixed") return usd(v.cost_usd);
  return "—";
}

function costSub(v: VendorTelemetry): ReactNode {
  if (v.cost_unit === "mixed" && v.credits_cost) return `+ ${compact(v.credits_cost)} credits`;
  if (v.cost_unit === "usd") return "USD-metered";
  if (v.cost_unit === "credits") return "credits (not USD)";
  return "no cost reported";
}

function Tile({
  label,
  value,
  sub,
  accent,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  accent?: boolean;
}) {
  return (
    <div
      className={
        accent
          ? "rounded-2 border border-accent/40 bg-bg-2 px-3 py-2.5"
          : "rounded-2 border border-line-2 bg-bg-2 px-3 py-2.5"
      }
    >
      <div className="text-[10px] uppercase tracking-wide text-fg-3">{label}</div>
      <div className="mt-1 font-mono text-[17px] leading-tight text-fg-0">{value}</div>
      {sub ? <div className="mt-0.5 text-[11px] text-fg-3">{sub}</div> : null}
    </div>
  );
}
