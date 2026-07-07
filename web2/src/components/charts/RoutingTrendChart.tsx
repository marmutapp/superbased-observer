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
import type { RoutingDayPoint } from "@/lib/api";
import { num, shortDate } from "@/lib/format";
import { CHART_AXIS, CHART_GRID } from "./common";
import { ChartTooltip } from "./ChartTooltip";
import { ChartLegend } from "./ChartLegend";

// RoutingTrendChart — stacked area of daily routing decisions split into the
// subset actually applied (rewritten) vs advisory (counted but not applied).
// Advisory is derived as decisions − applied per day.
type Row = { date: string; applied: number; advisory: number };

export function RoutingTrendChart({ data }: { data: RoutingDayPoint[] }) {
  if (!data.length) {
    return (
      <div className="grid h-[240px] place-items-center text-[12px] text-fg-3">
        No routing decisions in window.
      </div>
    );
  }
  const rows: Row[] = data.map((d) => ({
    date: d.date,
    applied: d.applied,
    advisory: Math.max(0, d.decisions - d.applied),
  }));
  const series = [
    { key: "applied", name: "Applied", color: "var(--tok-out)" },
    { key: "advisory", name: "Advisory", color: "var(--tok-net)" },
  ] as const;
  return (
    <ResponsiveContainer width="100%" height={240}>
      <AreaChart data={rows} margin={{ top: 8, right: 12, left: 0, bottom: 0 }}>
        <Legend verticalAlign="top" align="left" height={26} content={<ChartLegend />} />
        <defs>
          {series.map((s) => (
            <linearGradient key={s.key} id={`routing-${s.key}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={s.color} stopOpacity={0.7} />
              <stop offset="100%" stopColor={s.color} stopOpacity={0.05} />
            </linearGradient>
          ))}
        </defs>
        <CartesianGrid {...CHART_GRID} />
        <XAxis dataKey="date" tickFormatter={shortDate} {...CHART_AXIS} minTickGap={24} />
        <YAxis tickFormatter={num} {...CHART_AXIS} allowDecimals={false} />
        <Tooltip
          cursor={{ stroke: "var(--line-3)" }}
          content={<ChartTooltip labelFormatter={shortDate} formatItem={(n, v) => `${n}: ${num(v)}`} />}
        />
        {series.map((s) => (
          <Area
            key={s.key}
            type="monotone"
            dataKey={s.key}
            name={s.name}
            stackId="1"
            stroke={s.color}
            fill={`url(#routing-${s.key})`}
            strokeWidth={1.4}
          />
        ))}
      </AreaChart>
    </ResponsiveContainer>
  );
}
