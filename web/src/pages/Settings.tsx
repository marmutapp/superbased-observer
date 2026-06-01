import { lazy, Suspense, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import clsx from "clsx";
import { ChartShell, PageHeader, Pill, SlideOver, StatCard, Tooltip } from "@/components/primitives";
import { SECTION_SPECS } from "./settings/sectionSpecs";
import { StructuredConfigSection } from "./settings/StructuredConfigSection";
import { AntigravityHelperCard } from "./settings/AntigravityHelperCard";
import { EnrolmentSection } from "./settings/EnrolmentSection";
import { ChartState } from "@/components/ChartState";
import {
  BoltIcon,
  CalendarIcon,
  ClockIcon,
  CoinsIcon,
  CompassIcon,
  CompressIcon,
  DatabaseIcon,
  EyeIcon,
  SearchIcon,
  SparklesIcon,
} from "@/components/icons";

// TomlView is only used by the read-only config viewers below.
// Lazy-import so the hand-rolled lexer + serializer ships in its
// own chunk and Settings's first paint stays slim.
const TomlView = lazy(() =>
  import("@/components/TomlView").then((m) => ({ default: m.TomlView })),
);
import { fetchJSON } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { fmtInt, fmtUSD } from "@/lib/format";
import type {
  BackfillJob,
  BackfillJobsListResponse,
  BackfillRunResponse,
  BackfillStatusResponse,
  ConfigResponse,
  CostPricing,
  ModelPricing,
  PricingDefaultsResponse,
} from "@/lib/types";
import { costPricingToConfig } from "@/lib/types";

type SectionId =
  | "pricing"
  | "backfill"
  | "enrolment"
  | "observer"
  | "watcher"
  | "freshness"
  | "retention"
  | "hooks"
  | "proxy"
  | "compression"
  | "intelligence"
  | "antigravity";

type SectionDef = {
  id: SectionId;
  label: string;
  group: "edit" | "config";
  // hot = takes effect immediately on save (cost-engine reload).
  // soft = consumed lazily by the next job, no restart needed.
  // restart = daemon must restart to bind the new value.
  status: "hot" | "soft" | "restart";
  // Inline glyph rendered in the SectionNav row + page header to
  // match design/page-settings.jsx — every section carries a single
  // identifying icon so the nav reads at a glance.
  icon: ReactNode;
  about: {
    summary: string;
    whenModified: string;
    behavior: string;
  };
};

const SECTIONS: SectionDef[] = [
  {
    id: "pricing",
    label: "Pricing",
    group: "edit",
    status: "hot",
    icon: <CoinsIcon size={13} />,
    about: {
      summary:
        "Per-model pricing overrides. The cost engine ships with baked-in defaults for every common Anthropic / OpenAI / xAI model; overrides let you correct a contract rate or add a new SKU before observer's defaults catch up.",
      whenModified:
        "Custom contract rates, or a new SKU lands before observer's defaults catch up. Hot-reloaded — no restart needed.",
      behavior:
        "Save writes ~/.observer/config.toml + a .bak of the prior version. The cost engine swaps the active pricing table atomically on save — no restart, no daemon bounce.",
    },
  },
  {
    id: "backfill",
    label: "Backfill",
    group: "edit",
    status: "soft",
    icon: <DatabaseIcon size={13} />,
    about: {
      summary:
        "Run schema-fill jobs against your DB. Useful after a schema upgrade or when a new derived column lands and needs back-population.",
      whenModified:
        "After a schema migration, or when a v1.x.x release adds new derived columns.",
      behavior:
        "Spawns a subprocess that streams output back to the dashboard via 3-second polling. Idempotent; safe to re-run.",
    },
  },
  {
    id: "enrolment",
    label: "Enrolment",
    group: "edit",
    status: "soft",
    icon: <OrgSectionIcon />,
    about: {
      summary:
        "Teams & Org Visibility. When enrolled, this agent shares content-free activity rollups (counts, costs, timings, paths — never prompt text or tool output) with your organisation's Observer server. View exactly what was last shared, or unenrol.",
      whenModified:
        "Joining or leaving an organisation. Enrol with `observer enroll <org-url> <token>`; unenrol from here or with `observer unenroll`.",
      behavior:
        "Unenrol deletes the local enrolment + keychain credentials immediately; a running daemon's push loop stops within one interval. Nothing already shared with the server is deleted.",
    },
  },
  {
    id: "intelligence",
    label: "Intelligence",
    group: "edit",
    status: "restart",
    icon: <SparklesIcon size={13} />,
    about: {
      summary:
        "Summary model, monthly budget cap, code-graph integration. These power the Analysis tab's headline KPIs and the MCP `get_session_summary` tool.",
      whenModified:
        "Switching to a cheaper Haiku for summaries, raising/lowering the budget cap, or enabling/disabling the code-graph backend.",
      behavior:
        "Save writes config.toml; consumers bind the value at daemon startup, so a `observer serve` restart is required for the new value to take effect.",
    },
  },
  {
    id: "observer",
    label: "Observer",
    group: "config",
    status: "restart",
    icon: <EyeIcon size={13} />,
    about: {
      summary: "Top-level observer settings: watch paths, freshness, retention, hooks, antigravity. Read-only view; edit the source TOML to change.",
      whenModified: "Adding/removing a watched root, tweaking retention, etc.",
      behavior: "Restart required — consumers bind at startup.",
    },
  },
  {
    id: "watcher",
    label: "Watcher",
    group: "config",
    status: "restart",
    icon: <SearchIcon size={13} />,
    about: {
      summary: "Filesystem watcher — watch_paths, ignore_globs. Defines what observer scans for new session files.",
      whenModified: "Onboarding a new AI client, or moving session files to a non-default location.",
      behavior: "Restart required.",
    },
  },
  {
    id: "freshness",
    label: "Freshness",
    group: "config",
    status: "restart",
    icon: <ClockIcon size={13} />,
    about: {
      summary: "Freshness classifier — how observer scores whether a file read is stale vs fresh.",
      whenModified: "Tuning the staleness threshold or hashing rules.",
      behavior: "Restart required.",
    },
  },
  {
    id: "retention",
    label: "Retention",
    group: "config",
    status: "restart",
    icon: <CalendarIcon size={13} />,
    about: {
      summary: "How long observer keeps each table's data. Trims the DB on schedule.",
      whenModified: "Disk pressure or compliance retention windows.",
      behavior: "Restart required.",
    },
  },
  {
    id: "hooks",
    label: "Hooks",
    group: "config",
    status: "restart",
    icon: <BoltIcon size={13} />,
    about: {
      summary: "Per-tool hook configuration (Claude Code, Codex, Cursor, etc).",
      whenModified: "Adding a new tool integration or fixing a broken envelope.",
      behavior: "Restart required.",
    },
  },
  {
    id: "proxy",
    label: "Proxy",
    group: "config",
    status: "restart",
    icon: <CompassIcon size={13} />,
    about: {
      summary: "API proxy port + compression knobs.",
      whenModified: "Changing the proxy port or compression toggles.",
      behavior: "Restart required — proxy is bound at startup.",
    },
  },
  {
    id: "compression",
    label: "Compression",
    group: "config",
    status: "restart",
    icon: <CompressIcon size={13} />,
    about: {
      summary: "Per-mechanism compression configuration — drop / dedup / stash thresholds.",
      whenModified: "Tuning the compression pipeline.",
      behavior: "Restart required.",
    },
  },
  {
    id: "antigravity",
    label: "Antigravity",
    group: "config",
    status: "restart",
    icon: <AntigravitySectionIcon />,
    about: {
      summary: "Antigravity (Google) adapter config — bridge ports, decrypt keys, etc.",
      whenModified: "Onboarding the Antigravity adapter.",
      behavior: "Restart required.",
    },
  },
];

const STATUS_LABEL: Record<SectionDef["status"], string> = {
  hot: "hot",
  soft: "soft",
  restart: "restart",
};

const STATUS_CLASS: Record<SectionDef["status"], string> = {
  hot: "border-success/40 bg-success-soft text-success",
  soft: "border-line-3 bg-bg-3 text-fg-2",
  restart: "border-danger/40 bg-danger-soft text-danger",
};

function sectionAt(id: SectionId): SectionDef {
  return SECTIONS.find((s) => s.id === id)!;
}

// Small inline glyph for the Antigravity section — mirrors the
// per-tool antigravity ToolGlyph (upward triangle floating above a
// baseline) at the section-nav size. Sized to match the other 13px
// icon set rather than reach into ToolGlyph + its tinted frame.
function AntigravitySectionIcon() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 24 24"
      stroke="currentColor"
      fill="none"
      aria-hidden
    >
      <path d="M12 5 L18 14 L6 14 Z" fill="currentColor" stroke="none" />
      <line
        x1="5"
        y1="18"
        x2="19"
        y2="18"
        strokeWidth="1.6"
        strokeLinecap="round"
        opacity="0.5"
      />
    </svg>
  );
}

// Small inline glyph for the Enrolment (Teams) section — two stacked
// people, sized to match the 13px section-nav icon set.
function OrgSectionIcon() {
  return (
    <svg width="13" height="13" viewBox="0 0 16 16" stroke="currentColor" fill="none" aria-hidden>
      <circle cx="6" cy="5" r="2.2" strokeWidth="1.3" />
      <path d="M2 13a4 4 0 0 1 8 0" strokeWidth="1.3" strokeLinecap="round" />
      <path d="M10.5 3.2a2.2 2.2 0 0 1 0 3.6M11 9.2a4 4 0 0 1 3 3.8" strokeWidth="1.3" strokeLinecap="round" />
    </svg>
  );
}

export function SettingsPage() {
  const [active, setActive] = useState<SectionId>("pricing");
  const [helpOpen, setHelpOpen] = useState(true);
  const config = useApi<ConfigResponse>("/api/config");
  const def = sectionAt(active);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <SettingsHeader
        configPath={config.data?.config_path ?? ""}
        helpOpen={helpOpen}
        onToggleHelp={() => setHelpOpen((o) => !o)}
      />
      <div
        className={clsx(
          "grid min-h-0 flex-1 gap-0",
          helpOpen
            ? "grid-cols-[220px_minmax(0,1fr)_300px]"
            : "grid-cols-[220px_minmax(0,1fr)]",
        )}
      >
        <aside className="border-r border-line-1 bg-bg-1 p-4">
          <SectionNav active={active} onChange={setActive} />
          {config.data && (
            <div className="mt-6 rounded-2 border border-line-1 bg-bg-2 px-3 py-2 text-[10.5px] text-fg-3">
              <div className="mb-0.5 font-semibold uppercase tracking-[0.06em] text-fg-3">
                Config file
              </div>
              <Tooltip
                content={<span className="break-all font-mono">{config.data.config_path}</span>}
                maxWidth={420}
                disabled={!config.data.config_path}
              >
                <div
                  tabIndex={config.data.config_path ? 0 : undefined}
                  className="cursor-help break-all font-mono text-fg-2 focus:outline-none"
                >
                  {config.data.config_path || "(no file — ephemeral)"}
                </div>
              </Tooltip>
            </div>
          )}
        </aside>

        <main className="min-w-0 overflow-y-auto p-6">
          {active === "pricing" && (
            <PricingSection
              config={config.data}
              loading={config.loading}
              error={config.error}
              onReload={config.reload}
            />
          )}
          {active === "backfill" && <BackfillSection />}
          {active === "enrolment" && <EnrolmentSection />}
          {active === "intelligence" && (
            <IntelligenceSection
              config={config.data}
              loading={config.loading}
              error={config.error}
              onReload={config.reload}
            />
          )}
          {active !== "pricing" &&
            active !== "backfill" &&
            active !== "enrolment" &&
            active !== "intelligence" && (
              <SectionView
                section={active}
                config={config.data}
                loading={config.loading}
                error={config.error}
              />
            )}
        </main>

        {helpOpen && <AboutSectionRail def={def} />}
      </div>
    </div>
  );
}

function SettingsHeader({
  configPath,
  helpOpen,
  onToggleHelp,
}: {
  configPath: string;
  helpOpen: boolean;
  onToggleHelp: () => void;
}) {
  return (
    <div className="border-b border-line-1 bg-bg-1 px-6 py-4">
      <PageHeader
        title="Settings"
        helpId="tab.settings"
        sub={
          <>
            View and edit the live <code className="font-mono text-fg-2">config.toml</code>.
            Pricing hot-reloads — saves swap the cost engine in place.
            Every other section saves the file (prior version preserved
            at <code className="font-mono text-fg-2">.bak</code>) and
            surfaces a "restart daemon" banner — consumers bind at
            startup so the new value takes effect on the next
            <code className="font-mono text-fg-2"> observer start</code>.
            {configPath && (
              <>
                {" · "}
                <Tooltip content={<span className="break-all font-mono">{configPath}</span>} maxWidth={420}>
                  <span tabIndex={0} className="cursor-help font-mono text-fg-2 focus:outline-none">
                    {configPath}
                  </span>
                </Tooltip>
              </>
            )}
          </>
        }
        right={
          <button
            type="button"
            onClick={onToggleHelp}
            className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] text-fg-2 hover:bg-bg-3 hover:text-fg-0"
          >
            {helpOpen ? "Hide help" : "Show help"}
          </button>
        }
      />
    </div>
  );
}

function AboutSectionRail({ def }: { def: SectionDef }) {
  return (
    <aside className="overflow-y-auto border-l border-line-1 bg-bg-1 px-4 py-5">
      <div className="mb-3 flex items-center gap-2">
        <h2 className="text-[12.5px] font-semibold text-fg-1">
          About this section
        </h2>
        <span
          className={clsx(
            "rounded-pill border px-1.5 py-px text-[9.5px] font-medium uppercase tracking-[0.04em]",
            STATUS_CLASS[def.status],
          )}
        >
          {STATUS_LABEL[def.status]}
        </span>
      </div>
      <AboutBlock label={def.label.toUpperCase()} body={def.about.summary} />
      <AboutBlock label="WHEN MODIFIED" body={def.about.whenModified} />
      <AboutBlock label="HOT-RELOAD BEHAVIOR" body={def.about.behavior} />
    </aside>
  );
}

function AboutBlock({ label, body }: { label: string; body: string }) {
  return (
    <div className="mb-4">
      <div className="mb-1 text-[9.5px] font-semibold uppercase tracking-[0.08em] text-fg-3">
        {label}
      </div>
      <p className="text-[11.5px] leading-snug text-fg-2">{body}</p>
    </div>
  );
}

function SectionNav({
  active,
  onChange,
}: {
  active: SectionId;
  onChange: (s: SectionId) => void;
}) {
  const groups: { id: string; label: string; items: SectionDef[] }[] = [
    {
      id: "edit",
      label: "Edit",
      items: SECTIONS.filter((s) => s.group === "edit"),
    },
    {
      id: "config",
      label: "Config (read-only)",
      items: SECTIONS.filter((s) => s.group === "config"),
    },
  ];
  return (
    <nav className="flex flex-col gap-4">
      {groups.map((g) => (
        <div key={g.id}>
          <div className="mb-1.5 px-1 text-[10px] font-semibold uppercase tracking-[0.08em] text-fg-3">
            {g.label}
          </div>
          <div className="flex flex-col gap-0.5">
            {g.items.map((s) => (
              <button
                key={s.id}
                type="button"
                onClick={() => onChange(s.id)}
                className={clsx(
                  "flex items-center gap-2 rounded-2 px-2 py-1.5 text-left text-[12px] transition-colors",
                  active === s.id
                    ? "bg-bg-3 text-fg-0"
                    : "text-fg-2 hover:bg-bg-2 hover:text-fg-1",
                )}
              >
                <span
                  className={clsx(
                    "grid h-5 w-5 shrink-0 place-items-center rounded-1",
                    active === s.id
                      ? "bg-bg-4 text-accent"
                      : "bg-bg-2 text-fg-3",
                  )}
                  aria-hidden
                >
                  {s.icon}
                </span>
                <span className="min-w-0 flex-1 truncate">{s.label}</span>
                <span
                  className={clsx(
                    "shrink-0 rounded-pill border px-1.5 py-px text-[9.5px] font-medium uppercase tracking-[0.04em]",
                    STATUS_CLASS[s.status],
                  )}
                >
                  {STATUS_LABEL[s.status]}
                </span>
              </button>
            ))}
          </div>
        </div>
      ))}
    </nav>
  );
}

// ============================================================ Pricing

function PricingSection({
  config,
  loading,
  error,
  onReload,
}: {
  config: ConfigResponse | null;
  loading: boolean;
  error: Error | null;
  onReload: () => void;
}) {
  const defaults = useApi<PricingDefaultsResponse>(
    "/api/config/pricing/defaults",
  );
  const [overrides, setOverrides] = useState<
    Record<string, ModelPricing>
  >({});
  const [showDefaults, setShowDefaults] = useState(false);
  const [save, setSave] = useState<{
    state: "idle" | "saving" | "ok" | "err";
    message?: string;
  }>({ state: "idle" });

  // Seed local edits from server config when it loads.
  useEffect(() => {
    if (config?.config?.Intelligence?.Pricing?.Models) {
      setOverrides(config.config.Intelligence.Pricing.Models);
    } else {
      setOverrides({});
    }
  }, [config]);

  const modelKeys = useMemo(
    () => Object.keys(overrides).sort(),
    [overrides],
  );
  const defaultKeys = useMemo(
    () => Object.keys(defaults.data?.defaults ?? {}).sort(),
    [defaults.data],
  );

  async function saveAll() {
    if (!config) return;
    setSave({ state: "saving" });
    try {
      const res = await fetchJSON<{ saved: boolean }>(
        "/api/config/pricing",
        undefined,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ models: overrides }),
        },
      );
      if (!res.saved) throw new Error("server did not confirm save");
      setSave({ state: "ok", message: "Saved · cost engine reloaded" });
      onReload();
    } catch (e) {
      setSave({
        state: "err",
        message: e instanceof Error ? e.message : String(e),
      });
    }
  }

  function addOverride(modelKey: string) {
    setOverrides((prev) => {
      if (prev[modelKey]) return prev;
      const def = defaults.data?.defaults[modelKey];
      return {
        ...prev,
        [modelKey]: def ? costPricingToConfig(def) : blankPricing(),
      };
    });
  }

  function removeOverride(modelKey: string) {
    setOverrides((prev) => {
      const next = { ...prev };
      delete next[modelKey];
      return next;
    });
  }

  function updateRate(modelKey: string, field: keyof ModelPricing, val: string) {
    const n = Number(val);
    if (!Number.isFinite(n) || n < 0) return;
    setOverrides((prev) => ({
      ...prev,
      [modelKey]: { ...prev[modelKey], [field]: n },
    }));
  }

  return (
    <ChartShell
      title="Pricing overrides"
      sub="Per-million-token rates that shadow the baked-in defaults. Save triggers an in-place cost engine reload — Cost / Analysis / Session-detail pages reflect the new rates on next query (no daemon restart)."
      right={
        <div className="flex items-center gap-2 text-[11px]">
          {save.state === "ok" && (
            <span className="text-success">{save.message}</span>
          )}
          {save.state === "err" && (
            <Tooltip content={save.message} maxWidth={360}>
              <span tabIndex={0} className="cursor-help text-danger focus:outline-none">
                Save failed
              </span>
            </Tooltip>
          )}
          <Tooltip content="Jump to baked-in defaults">
            <button
              type="button"
              onClick={() => setShowDefaults((v) => !v)}
              className="rounded-pill border border-line-2 bg-bg-3 px-2 py-0.5 text-[10.5px] text-fg-2 hover:bg-bg-4"
            >
              Baked-in defaults · {defaultKeys.length}
            </button>
          </Tooltip>
          <Tooltip content="Pick a default model and add a pricing override">
            <button
              type="button"
              onClick={() => setShowDefaults(true)}
              className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 text-[11px] text-fg-1 hover:bg-bg-3"
            >
              + Add override
            </button>
          </Tooltip>
          <button
            type="button"
            onClick={saveAll}
            disabled={save.state === "saving" || loading || !config}
            className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1 text-[11px] font-medium text-accent disabled:opacity-40"
          >
            {save.state === "saving" ? "Saving…" : "Save pricing"}
          </button>
        </div>
      }
    >
      <ChartState
        loading={loading && !config}
        error={error}
        empty={false}
        height={120}
      >
        {modelKeys.length === 0 ? (
          <div className="rounded-2 border border-dashed border-line-2 bg-bg-3/40 px-4 py-3 text-[12px] text-fg-3">
            No overrides defined. The cost engine is using its baked-in
            defaults (
            <button
              type="button"
              onClick={() => setShowDefaults((v) => !v)}
              className="text-accent hover:text-accent-strong"
            >
              {showDefaults ? "hide" : "show"} {defaultKeys.length} models
            </button>
            ). Add an override below to shadow a default rate.
          </div>
        ) : (
          <PricingTable
            models={overrides}
            modelKeys={modelKeys}
            onChange={updateRate}
            onRemove={removeOverride}
          />
        )}

        <details
          className="mt-4 rounded-2 border border-line-1 bg-bg-2"
          open={showDefaults}
          onToggle={(e) =>
            setShowDefaults((e.target as HTMLDetailsElement).open)
          }
        >
          <summary className="cursor-pointer px-3 py-2 text-[11.5px] text-fg-2">
            Baked-in defaults ·{" "}
            <span className="font-mono">{defaultKeys.length}</span> models
          </summary>
          <div className="border-t border-line-1 p-3">
            {defaults.loading && (
              <div className="text-[11px] text-fg-3">loading…</div>
            )}
            {defaults.data && (
              <DefaultsTable
                defaults={defaults.data.defaults}
                onAdd={addOverride}
                existing={new Set(modelKeys)}
              />
            )}
          </div>
        </details>
      </ChartState>
    </ChartShell>
  );
}

function blankPricing(): ModelPricing {
  return {
    Input: 0,
    Output: 0,
    CacheRead: 0,
    CacheCreation: 0,
    CacheCreation1h: 0,
    LongContextThreshold: 0,
    LongContextInput: 0,
    LongContextOutput: 0,
    LongContextCacheRead: 0,
    LongContextCacheCreation: 0,
    LongContextCacheCreation1h: 0,
  };
}

const PRICING_FIELDS: { key: keyof ModelPricing; label: string }[] = [
  { key: "Input", label: "Input" },
  { key: "Output", label: "Output" },
  { key: "CacheRead", label: "Cache R" },
  { key: "CacheCreation", label: "Cache W (5m)" },
  { key: "CacheCreation1h", label: "Cache W (1h)" },
];

function PricingTable({
  models,
  modelKeys,
  onChange,
  onRemove,
}: {
  models: Record<string, ModelPricing>;
  modelKeys: string[];
  onChange: (key: string, field: keyof ModelPricing, val: string) => void;
  onRemove: (key: string) => void;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[820px] text-left text-[11.5px]">
        <thead className="text-[10px] uppercase tracking-[0.06em] text-fg-3">
          <tr className="border-b border-line-2">
            <th className="py-1.5 pl-2 font-medium">Model</th>
            {PRICING_FIELDS.map((f) => (
              <Tooltip key={f.key} content="$ per million tokens">
                <th tabIndex={0} className="cursor-help py-1.5 text-right font-medium focus:outline-none">
                  {f.label}
                </th>
              </Tooltip>
            ))}
            <th className="py-1.5 pl-3 font-medium" />
          </tr>
        </thead>
        <tbody>
          {modelKeys.map((k) => (
            <tr
              key={k}
              className="border-b border-line-1 last:border-b-0 hover:bg-bg-3/40"
            >
              <td className="py-1.5 pl-2 font-mono text-fg-1">{k}</td>
              {PRICING_FIELDS.map((f) => (
                <td key={f.key} className="py-1.5">
                  <input
                    type="number"
                    step="0.01"
                    min="0"
                    value={models[k][f.key]}
                    onChange={(e) => onChange(k, f.key, e.target.value)}
                    className="ml-auto block h-7 w-[88px] rounded-1 border border-line-2 bg-bg-2 px-2 text-right font-mono text-[11px] text-fg-1 focus:border-accent focus:outline-none"
                  />
                </td>
              ))}
              <td className="py-1.5 pl-3">
                <Tooltip content="Remove override (revert to default)">
                  <button
                    type="button"
                    onClick={() => onRemove(k)}
                    className="text-[10.5px] text-fg-3 hover:text-danger"
                  >
                    Reset
                  </button>
                </Tooltip>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function DefaultsTable({
  defaults,
  existing,
  onAdd,
}: {
  defaults: Record<string, CostPricing>;
  existing: Set<string>;
  onAdd: (key: string) => void;
}) {
  const [query, setQuery] = useState("");
  const keys = useMemo(() => {
    const q = query.trim().toLowerCase();
    const all = Object.keys(defaults).sort();
    return q ? all.filter((k) => k.toLowerCase().includes(q)) : all;
  }, [defaults, query]);
  return (
    <>
      <input
        type="search"
        placeholder={`Filter ${Object.keys(defaults).length} models…`}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        className="mb-2 h-7 w-full rounded-2 border border-line-2 bg-bg-2 px-2 text-[11px] text-fg-1 placeholder:text-fg-4 focus:border-accent focus:outline-none"
      />
      <div className="max-h-[300px] overflow-y-auto">
        <table className="w-full text-left text-[11px]">
          <thead className="sticky top-0 bg-bg-2 text-[10px] uppercase tracking-[0.06em] text-fg-3">
            <tr className="border-b border-line-2">
              <th className="py-1 pl-2 font-medium">Model</th>
              <th className="py-1 text-right font-medium">Input</th>
              <th className="py-1 text-right font-medium">Output</th>
              <th className="py-1 text-right font-medium">Cache R</th>
              <th className="py-1 text-right font-medium">Cache W</th>
              <th className="py-1 pl-3 font-medium" />
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => {
              const p = defaults[k];
              const exists = existing.has(k);
              return (
                <tr
                  key={k}
                  className="border-b border-line-1 last:border-b-0"
                >
                  <td className="py-1 pl-2 font-mono text-fg-1">{k}</td>
                  <td className="py-1 text-right font-mono text-fg-2 tabular-nums">
                    {fmtUSD(p.input, true)}
                  </td>
                  <td className="py-1 text-right font-mono text-fg-2 tabular-nums">
                    {fmtUSD(p.output, true)}
                  </td>
                  <td className="py-1 text-right font-mono text-fg-2 tabular-nums">
                    {fmtUSD(p.cache_read, true)}
                  </td>
                  <td className="py-1 text-right font-mono text-fg-2 tabular-nums">
                    {fmtUSD(p.cache_creation, true)}
                  </td>
                  <td className="py-1 pl-3">
                    <button
                      type="button"
                      onClick={() => onAdd(k)}
                      disabled={exists}
                      className="rounded-1 border border-line-2 bg-bg-3 px-2 py-0.5 text-[10px] text-fg-2 hover:text-fg-0 disabled:opacity-30"
                    >
                      {exists ? "Already overridden" : "Override"}
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}

// ============================================================ Backfill

function BackfillSection() {
  const status = useApi<BackfillStatusResponse>("/api/backfill/status");
  const [jobs, setJobs] = useState<Record<string, BackfillJob>>({});
  // trackerOpen drives the progress dialog. Auto-opens on Run / Run All so
  // the operator sees live output instead of guessing from per-row pills;
  // closing it doesn't cancel the jobs (they run in the observer process
  // and continue accumulating output for the next reopen).
  const [trackerOpen, setTrackerOpen] = useState(false);

  // Restore jobs on mount: the registry is process-local in the
  // observer, so navigating away from Settings then back lost the
  // running indicator pre-fix even though the subprocess was still
  // alive. /api/backfill/jobs returns every job newest-first; collapse
  // to one per mode (the freshest) to match the UI's single-job-per-mode
  // model. The poll useEffect below picks up any restored status="running"
  // entries automatically.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const list = await fetchJSON<BackfillJobsListResponse>(
          "/api/backfill/jobs",
        );
        if (cancelled || !list?.jobs?.length) return;
        const byMode: Record<string, BackfillJob> = {};
        for (const j of list.jobs) {
          if (!byMode[j.mode]) byMode[j.mode] = j;
        }
        setJobs((prev) => ({ ...byMode, ...prev }));
      } catch {
        // soft-fail — list is best-effort UX restoration
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  async function run(mode: string) {
    setTrackerOpen(true);
    try {
      const res = await fetchJSON<BackfillRunResponse>(
        "/api/backfill/run",
        undefined,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ mode }),
        },
      );
      setJobs((prev) => ({
        ...prev,
        [mode]: {
          id: res.job_id,
          mode: res.mode,
          status: res.status,
          started_at: res.started_at,
          output: "",
        },
      }));
    } catch (e) {
      setJobs((prev) => ({
        ...prev,
        [mode]: {
          id: "",
          mode,
          status: "failed",
          started_at: new Date().toISOString(),
          output: "",
          error: e instanceof Error ? e.message : String(e),
        },
      }));
    }
  }

  // Poll every running job every 3s.
  useEffect(() => {
    const running = Object.values(jobs).filter(
      (j) => j.status === "running" && j.id,
    );
    if (running.length === 0) return;
    const id = setInterval(async () => {
      for (const j of running) {
        try {
          const fresh = await fetchJSON<BackfillJob>(
            `/api/backfill/jobs/${j.id}`,
          );
          setJobs((prev) => ({ ...prev, [j.mode]: fresh }));
        } catch {
          // ignore — next tick retries
        }
      }
    }, 3000);
    return () => clearInterval(id);
  }, [jobs]);

  const anyRunning = Object.values(jobs).some((j) => j.status === "running");

  async function runAll() {
    if (!status.data) return;
    setTrackerOpen(true);
    for (const m of status.data.modes) {
      const j = jobs[m.mode];
      if (j?.status === "running") continue;
      await run(m.mode);
    }
  }

  const jobList = Object.values(jobs);
  const runningCount = jobList.filter((j) => j.status === "running").length;
  const doneCount = jobList.filter((j) => j.status === "done").length;
  const failedCount = jobList.filter((j) => j.status === "failed").length;

  return (
    <ChartShell
      title="Backfill jobs"
      sub="Each mode re-fills a column or row class added in a later migration. SQL-checkable modes show a candidate count; file-walking modes mark `-1` (only the run itself can count). Jobs spawn the observer CLI subprocess with the current config path."
      right={
        status.data && status.data.modes.length > 0 ? (
          <Tooltip content="Fire every mode in sequence">
            <button
              type="button"
              onClick={runAll}
              disabled={anyRunning}
              className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1 text-[11px] font-medium text-accent disabled:opacity-40"
            >
              {anyRunning ? "Running…" : "Run all"}
            </button>
          </Tooltip>
        ) : null
      }
    >
      {jobList.length > 0 && !trackerOpen && (
        <div className="mb-3 flex items-center gap-2 rounded-2 border border-line-2 bg-bg-2 px-3 py-2 text-[11.5px]">
          <span className="text-fg-2">
            {runningCount > 0 && (
              <>
                <b className="text-accent">{runningCount} running</b>
                {(doneCount > 0 || failedCount > 0) && " · "}
              </>
            )}
            {doneCount > 0 && (
              <>
                <span className="text-success">{doneCount} done</span>
                {failedCount > 0 && " · "}
              </>
            )}
            {failedCount > 0 && (
              <span className="text-danger">{failedCount} failed</span>
            )}
          </span>
          <button
            type="button"
            onClick={() => setTrackerOpen(true)}
            className="ml-auto rounded-2 border border-line-3 bg-bg-3 px-2.5 py-0.5 text-[11px] text-fg-1 hover:border-accent hover:text-accent"
          >
            View progress
          </button>
        </div>
      )}

      <ChartState
        loading={status.loading && !status.data}
        error={status.error}
        empty={!status.data?.modes.length}
        emptyHint="No backfill modes registered."
        height={160}
      >
        {status.data && (
          <ul className="space-y-2">
            {status.data.modes.map((m) => {
              const job = jobs[m.mode];
              return (
                <li
                  key={m.mode}
                  className="rounded-2 border border-line-1 bg-bg-2 px-3 py-2"
                >
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-mono text-[12px] text-fg-1">
                          {m.mode}
                        </span>
                        <span className="font-mono text-[10.5px] text-fg-3">
                          {m.flag}
                        </span>
                        <CandidatesPill
                          count={m.candidates}
                          note={m.candidates_note}
                        />
                        {job && <JobStatusPill status={job.status} />}
                      </div>
                      <p className="mt-0.5 text-[11px] text-fg-2">
                        {m.description}
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() => run(m.mode)}
                      disabled={job?.status === "running"}
                      className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1 text-[11px] font-medium text-accent disabled:opacity-40"
                    >
                      {job?.status === "running" ? "Running…" : "Run"}
                    </button>
                  </div>

                  {job && (job.status === "done" || job.status === "failed") && (
                    <details className="mt-2 rounded-1 border border-line-1 bg-bg-1">
                      <summary className="cursor-pointer px-2 py-1 text-[11px] text-fg-3">
                        Output · exit {job.exit_code ?? "?"} ·{" "}
                        {job.output.length.toLocaleString()}B
                        {job.error && (
                          <span className="ml-2 text-danger">{job.error}</span>
                        )}
                      </summary>
                      <pre className="m-0 max-h-[200px] overflow-auto whitespace-pre-wrap break-all px-2 py-1.5 font-mono text-[11px] text-fg-2">
                        {job.output || "(no output captured)"}
                      </pre>
                    </details>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </ChartState>
      <BackfillTrackerDialog
        open={trackerOpen}
        onClose={() => setTrackerOpen(false)}
        jobs={jobList}
        runningCount={runningCount}
        doneCount={doneCount}
        failedCount={failedCount}
      />
    </ChartShell>
  );
}

// BackfillTrackerDialog — progress surface that auto-opens on Run / Run
// All. Lists every job (running + done + failed) with streaming output
// for running jobs (polled by the parent every 3s; the job's Output
// field accumulates chunks as the subprocess writes them). Closing the
// dialog does NOT cancel jobs — they continue in the observer process
// and the banner reopens it on demand.
function BackfillTrackerDialog({
  open,
  onClose,
  jobs,
  runningCount,
  doneCount,
  failedCount,
}: {
  open: boolean;
  onClose: () => void;
  jobs: BackfillJob[];
  runningCount: number;
  doneCount: number;
  failedCount: number;
}) {
  return (
    <SlideOver
      open={open}
      onClose={onClose}
      title="Backfill progress"
      subtitle={
        <span>
          {runningCount > 0 && (
            <>
              <b className="text-accent">{runningCount} running</b>
              {(doneCount > 0 || failedCount > 0) && " · "}
            </>
          )}
          {doneCount > 0 && (
            <>
              <span className="text-success">{doneCount} done</span>
              {failedCount > 0 && " · "}
            </>
          )}
          {failedCount > 0 && (
            <span className="text-danger">{failedCount} failed</span>
          )}
          {jobs.length === 0 && "no jobs yet — click Run to kick one off"}
        </span>
      }
      width={720}
    >
      <div className="p-4">
        {jobs.length === 0 ? (
          <div className="rounded-2 border border-dashed border-line-2 px-4 py-6 text-center text-[12px] text-fg-3">
            No backfill jobs have been started in this session.
          </div>
        ) : (
          <ul className="space-y-3">
            {jobs.map((j) => (
              <BackfillTrackerRow key={j.mode + ":" + j.id} job={j} />
            ))}
          </ul>
        )}
      </div>
    </SlideOver>
  );
}

function BackfillTrackerRow({ job }: { job: BackfillJob }) {
  // Pin the scrollable output to the bottom on each render while the
  // job is running so the operator sees the latest chunks land
  // without manually scrolling. Stops once the job finishes so they
  // can scroll up to inspect.
  const preRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    if (job.status !== "running") return;
    const el = preRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [job.output, job.status]);

  return (
    <li className="rounded-2 border border-line-1 bg-bg-2">
      <div className="flex items-center justify-between gap-3 border-b border-line-1 px-3 py-2">
        <div className="flex items-center gap-2">
          <span className="font-mono text-[12px] text-fg-1">{job.mode}</span>
          <JobStatusPill status={job.status} />
          {job.exit_code != null && job.status !== "running" && (
            <span className="font-mono text-[10.5px] text-fg-3">
              exit {job.exit_code}
            </span>
          )}
        </div>
        <span className="font-mono text-[10.5px] text-fg-3">
          {job.output.length.toLocaleString()}B
        </span>
      </div>
      {job.error && (
        <div className="border-b border-line-1 px-3 py-1.5 text-[11px] text-danger">
          {job.error}
        </div>
      )}
      <pre
        ref={preRef}
        className="m-0 max-h-[280px] overflow-auto whitespace-pre-wrap break-all px-3 py-2 font-mono text-[11px] text-fg-2"
      >
        {job.output || (job.status === "running" ? "(starting…)" : "(no output)")}
      </pre>
    </li>
  );
}

function CandidatesPill({
  count,
  note,
}: {
  count: number;
  note?: string;
}) {
  if (count < 0) {
    return (
      <Pill title={note}>
        scan needed
      </Pill>
    );
  }
  if (count === 0) {
    return <Pill variant="success">all filled</Pill>;
  }
  return (
    <Pill variant={count > 1000 ? "warn" : "info"}>
      {fmtInt(count)} candidates
    </Pill>
  );
}

function JobStatusPill({ status }: { status: string }) {
  switch (status) {
    case "running":
      return <Pill variant="accent">running</Pill>;
    case "done":
      return <Pill variant="success">done</Pill>;
    case "failed":
      return <Pill variant="danger">failed</Pill>;
    default:
      return <Pill>{status}</Pill>;
  }
}

// ============================================================ Intelligence

function IntelligenceSection({
  config,
  loading,
  error,
  onReload,
}: {
  config: ConfigResponse | null;
  loading: boolean;
  error: Error | null;
  onReload: () => void;
}) {
  const intel = config?.config?.Intelligence;
  const [summaryModel, setSummaryModel] = useState("");
  const [apiKeyEnv, setApiKeyEnv] = useState("");
  const [monthlyBudget, setMonthlyBudget] = useState(0);
  const [codeGraphEnabled, setCodeGraphEnabled] = useState(false);
  const [save, setSave] = useState<{
    state: "idle" | "saving" | "ok" | "err";
    message?: string;
  }>({ state: "idle" });

  useEffect(() => {
    if (!intel) return;
    setSummaryModel(intel.SummaryModel ?? "");
    setApiKeyEnv(intel.APIKeyEnv ?? "");
    setMonthlyBudget(intel.MonthlyBudgetUSD ?? 0);
    setCodeGraphEnabled(intel.CodeGraph?.Enabled ?? false);
  }, [intel]);

  async function saveSection() {
    setSave({ state: "saving" });
    try {
      const res = await fetchJSON<{ saved: boolean; restart_required: boolean }>(
        "/api/config/section/intelligence",
        undefined,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            SummaryModel: summaryModel,
            APIKeyEnv: apiKeyEnv,
            MonthlyBudgetUSD: monthlyBudget,
            CodeGraph: { Enabled: codeGraphEnabled },
          }),
        },
      );
      if (!res.saved) throw new Error("server did not confirm save");
      setSave({
        state: "ok",
        message: res.restart_required
          ? "Saved · restart daemon to apply"
          : "Saved",
      });
      onReload();
    } catch (e) {
      setSave({
        state: "err",
        message: e instanceof Error ? e.message : String(e),
      });
    }
  }

  return (
    <ChartShell
      title="Intelligence"
      sub="Summary model + monthly budget + code-graph toggle. Saving writes config.toml and surfaces a Restart-required banner — these consumers bind config at startup, unlike pricing."
      right={
        <div className="flex items-center gap-2 text-[11px]">
          {save.state === "ok" && (
            <span className="text-success">{save.message}</span>
          )}
          {save.state === "err" && (
            <Tooltip content={save.message} maxWidth={360}>
              <span tabIndex={0} className="cursor-help text-danger focus:outline-none">
                Save failed
              </span>
            </Tooltip>
          )}
          <button
            type="button"
            onClick={saveSection}
            disabled={save.state === "saving" || loading || !intel}
            className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1 text-[11px] font-medium text-accent disabled:opacity-40"
          >
            {save.state === "saving" ? "Saving…" : "Save section"}
          </button>
        </div>
      }
    >
      <ChartState
        loading={loading && !config}
        error={error}
        empty={!intel}
        emptyHint="Intelligence section unavailable."
        height={200}
      >
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <Field label="Summary model" hint="Used by `observer summarize` for session summaries.">
            <input
              type="text"
              value={summaryModel}
              onChange={(e) => setSummaryModel(e.target.value)}
              placeholder="claude-haiku-4-5-20251001"
              className="h-8 w-full rounded-2 border border-line-2 bg-bg-2 px-2 font-mono text-[12px] text-fg-1 focus:border-accent focus:outline-none"
            />
          </Field>
          <Field label="API key env" hint="Env var name to read the summary API key from.">
            <input
              type="text"
              value={apiKeyEnv}
              onChange={(e) => setApiKeyEnv(e.target.value)}
              placeholder="ANTHROPIC_API_KEY"
              className="h-8 w-full rounded-2 border border-line-2 bg-bg-2 px-2 font-mono text-[12px] text-fg-1 focus:border-accent focus:outline-none"
            />
          </Field>
          <Field label="Monthly budget (USD)" hint="Drives the MTD budget bar on the Analysis tab.">
            <input
              type="number"
              min="0"
              step="10"
              value={monthlyBudget}
              onChange={(e) => setMonthlyBudget(Number(e.target.value) || 0)}
              className="h-8 w-full rounded-2 border border-line-2 bg-bg-2 px-2 font-mono text-[12px] text-fg-1 focus:border-accent focus:outline-none"
            />
            <div className="mt-1 text-[11px] text-fg-3">
              Current: <strong>{fmtUSD(monthlyBudget)}</strong>
            </div>
          </Field>
          <Field
            label="Code graph"
            hint="Enable codebase-memory-mcp queries for richer MCP responses."
          >
            <label className="flex items-center gap-2 text-[12px] text-fg-1">
              <input
                type="checkbox"
                checked={codeGraphEnabled}
                onChange={(e) => setCodeGraphEnabled(e.target.checked)}
                className="h-3.5 w-3.5 accent-current"
              />
              {codeGraphEnabled ? "Enabled" : "Disabled"}
            </label>
          </Field>
        </div>
      </ChartState>
    </ChartShell>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <div className="mb-1 text-[10.5px] font-semibold uppercase tracking-[0.06em] text-fg-3">
        {label}
      </div>
      {children}
      {hint && <div className="mt-1 text-[11px] text-fg-3">{hint}</div>}
    </div>
  );
}

// ============================================================ SectionView
//
// Per-section router. Sections with a registered structured spec
// (`./settings/sectionSpecs`) render as a form (legacy parity);
// everything else falls back to the read-only TOML viewer.

function SectionView({
  section,
  config,
  loading,
  error,
}: {
  section: SectionId;
  config: ConfigResponse | null;
  loading: boolean;
  error: Error | null;
}) {
  const spec = SECTION_SPECS[section];
  if (spec) {
    return (
      <StructuredConfigSection
        spec={spec}
        config={config}
        footer={
          section === "antigravity" ? <AntigravityHelperCard /> : undefined
        }
      />
    );
  }
  return (
    <ConfigViewerSection
      section={section}
      config={config}
      loading={loading}
      error={error}
    />
  );
}

// ============================================================ Read-only

function ConfigViewerSection({
  section,
  config,
  loading,
  error,
}: {
  section: SectionId;
  config: ConfigResponse | null;
  loading: boolean;
  error: Error | null;
}) {
  const data = sectionData(section, config?.config);

  return (
    <ChartShell
      title={SECTIONS.find((s) => s.id === section)?.label ?? section}
      sub="Read-only view of the running config. Edit the source TOML at the path on the left, then restart the daemon — or wire a save endpoint here in a future phase."
    >
      <ChartState
        loading={loading && !config}
        error={error}
        empty={!data}
        emptyHint="Section not present in config."
        height={160}
      >
        {data != null ? (
          <div className="space-y-3">
            <StatCard
              label="Keys"
              value={fmtInt(countKeys(data))}
              sub="Rendered as TOML; the running daemon binds at startup"
            />
            <Suspense
              fallback={
                <pre className="m-0 max-h-[440px] overflow-auto whitespace-pre rounded-2 border border-line-1 bg-bg-1 px-3 py-2 font-mono text-[11.5px] text-fg-3">
                  loading…
                </pre>
              }
            >
              <TomlView data={data} />
            </Suspense>
          </div>
        ) : null}
      </ChartState>
    </ChartShell>
  );
}

function sectionData(section: SectionId, cfg?: unknown): unknown {
  if (!cfg || typeof cfg !== "object") return null;
  const c = cfg as Record<string, unknown>;
  switch (section) {
    case "observer":
      return slim(c.Observer, ["Watch", "Freshness", "Retention", "Hooks", "Antigravity"]);
    case "watcher":
      return readSub(c.Observer, "Watch");
    case "freshness":
      return readSub(c.Observer, "Freshness");
    case "retention":
      return readSub(c.Observer, "Retention");
    case "hooks":
      return readSub(c.Observer, "Hooks");
    case "antigravity":
      return readSub(c.Observer, "Antigravity");
    case "proxy":
      return c.Proxy ?? null;
    case "compression":
      return c.Compression ?? null;
    default:
      return null;
  }
}

function slim(obj: unknown, exclude: string[]): unknown {
  if (!obj || typeof obj !== "object") return obj;
  const o = obj as Record<string, unknown>;
  const out: Record<string, unknown> = {};
  for (const k of Object.keys(o)) {
    if (!exclude.includes(k)) out[k] = o[k];
  }
  return out;
}

function readSub(parent: unknown, key: string): unknown {
  if (!parent || typeof parent !== "object") return null;
  const p = parent as Record<string, unknown>;
  return p[key] ?? null;
}

function countKeys(v: unknown): number {
  if (v == null || typeof v !== "object") return 0;
  if (Array.isArray(v)) return v.length;
  return Object.keys(v as Record<string, unknown>).length;
}
