import { useMemo } from "react";
import {
  Area,
  AreaChart,
  CartesianGrid,
  Legend,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { ToolDayCount } from "@/lib/api";
import { compact, shortDate } from "@/lib/format";
import { toolMeta } from "@/lib/tools";
import { CHART_AXIS, CHART_GRID } from "./common";
import { ChartTooltip } from "./ChartTooltip";
import { ChartLegend } from "./ChartLegend";

// ActionsByToolChart — stacked area of daily action counts by tool. Pivots the
// flat (date, tool, count) rows into wide per-date rows, one Area per tool.
export function ActionsByToolChart({ data }: { data: ToolDayCount[] }) {
  const { rows, tools } = useMemo(() => pivot(data), [data]);
  if (!rows.length) {
    return <div className="grid h-[240px] place-items-center text-[12px] text-fg-3">No actions in window.</div>;
  }
  return (
    <ResponsiveContainer width="100%" height={240}>
      <AreaChart data={rows} margin={{ top: 8, right: 12, left: 0, bottom: 0 }}>
        <Legend verticalAlign="top" align="left" height={26} content={<ChartLegend />} />
        <CartesianGrid {...CHART_GRID} />
        <XAxis dataKey="date" tickFormatter={shortDate} {...CHART_AXIS} minTickGap={24} />
        <YAxis tickFormatter={compact} {...CHART_AXIS} />
        <Tooltip
          cursor={{ stroke: "var(--line-3)" }}
          content={<ChartTooltip labelFormatter={shortDate} formatItem={(n, v) => `${n}: ${compact(v)}`} />}
        />
        {tools.map((t) => (
          <Area
            key={t}
            type="monotone"
            dataKey={t}
            name={toolMeta(t).label}
            stackId="1"
            stroke={toolMeta(t).colorVar}
            fill={toolMeta(t).colorVar}
            fillOpacity={0.25}
            strokeWidth={1.4}
          />
        ))}
      </AreaChart>
    </ResponsiveContainer>
  );
}

function pivot(data: ToolDayCount[]): { rows: Record<string, number | string>[]; tools: string[] } {
  const byDate = new Map<string, Record<string, number | string>>();
  const toolTotals = new Map<string, number>();
  for (const r of data) {
    let row = byDate.get(r.date);
    if (!row) {
      row = { date: r.date };
      byDate.set(r.date, row);
    }
    row[r.tool] = ((row[r.tool] as number) ?? 0) + r.count;
    toolTotals.set(r.tool, (toolTotals.get(r.tool) ?? 0) + r.count);
  }
  // Top 8 tools by total; rest folded into "other".
  const ranked = [...toolTotals.entries()].sort((a, b) => b[1] - a[1]).map(([t]) => t);
  const tools = ranked.slice(0, 8);
  const rows = [...byDate.values()].sort((a, b) => String(a.date).localeCompare(String(b.date)));
  return { rows, tools };
}
