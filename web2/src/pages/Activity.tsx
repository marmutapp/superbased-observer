import { useState } from "react";
import { api } from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { useFilters } from "@/lib/filters";
import { ErrorState, PageHeader } from "@/components/ui";
import { ChartShell, ChartSkeleton } from "@/components/primitives";
import { ActionsByToolChart } from "@/components/charts/ActionsByToolChart";
import { TokensByDayChart } from "@/components/charts/TokensByDayChart";
import { HourBars } from "@/components/charts/HourBars";
import { DowHourHeatmap } from "@/components/charts/DowHourHeatmap";

export function ActivityPage() {
  const { days } = useFilters();
  // §6c-3 global tool filter — surfaced here (Activity is fully tool-filterable:
  // every metric on the page rides the spend/ev/actions substrate that carries
  // the tool dimension). Empty = whole org.
  const [tool, setTool] = useState("");
  const { data, error, loading, reload } = useApi(() => api.activity(days, tool || undefined), [days, tool]);
  const tools = useApi(() => api.tools(days), [days]);

  const filter = (
    <label className="inline-flex items-center gap-1.5 text-[12px] text-fg-3">
      Tool
      <select
        value={tool}
        onChange={(e) => setTool(e.target.value)}
        className="rounded-2 border border-line-2 bg-bg-2 px-2 py-1 text-[12px] text-fg-1 focus:border-accent focus:outline-none"
      >
        <option value="">All</option>
        {(tools.data?.tools ?? []).map((t) => (
          <option key={t.tool} value={t.tool}>
            {t.tool}
          </option>
        ))}
      </select>
    </label>
  );

  return (
    <>
      <PageHeader
        title="Activity"
        subtitle="When and how the org works — actions, tokens, and time-of-day rhythm (UTC)."
        right={filter}
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <div className="space-y-5">
          <ChartShell title="Actions by day" sub="Stacked by AI tool"><ChartSkeleton /></ChartShell>
          <ChartShell title="Tokens by day" sub="Stacked by billing bucket"><ChartSkeleton /></ChartShell>
        </div>
      ) : (
        <div className="space-y-5">
          <ChartShell title="Actions by day" sub="Stacked by AI tool">
            <ActionsByToolChart data={data.tool_by_day} />
          </ChartShell>

          <ChartShell title="Tokens by day" sub="Stacked by billing bucket">
            <TokensByDayChart data={data.tokens_by_day} />
          </ChartShell>

          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <ChartShell title="Activity by hour" sub="Actions by hour of day (UTC)">
              <HourBars buckets={data.hour_of_day} />
            </ChartShell>
            <ChartShell title="Weekly rhythm" sub="Day of week × hour (UTC)">
              <DowHourHeatmap cells={data.dow_hour} />
            </ChartShell>
          </div>
        </div>
      )}
    </>
  );
}
