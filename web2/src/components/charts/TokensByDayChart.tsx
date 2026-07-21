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
import type { DayBuckets } from "@/lib/api";
import { compact, shortDate } from "@/lib/format";
import { CHART_AXIS, CHART_GRID } from "./common";
import { ChartTooltip } from "./ChartTooltip";
import { ChartLegend } from "./ChartLegend";

// TokensByDayChart — stacked area of daily token volume by billing bucket.
const BUCKETS = [
  { key: "net_input", name: "Net input", color: "var(--tok-net)" },
  { key: "cache_write", name: "Cache write", color: "var(--tok-write)" },
  { key: "cache_read", name: "Cache read", color: "var(--tok-read)" },
  { key: "output", name: "Output", color: "var(--tok-out)" },
] as const;

export function TokensByDayChart({ data }: { data: DayBuckets[] }) {
  if (!data.length) {
    return <div className="grid h-[240px] place-items-center text-[12px] text-fg-3">No token activity in window.</div>;
  }
  return (
    <ResponsiveContainer width="100%" height={240}>
      <AreaChart data={data} margin={{ top: 8, right: 12, left: 0, bottom: 0 }}>
        <Legend verticalAlign="top" align="left" height={26} content={<ChartLegend />} />
        <defs>
          {BUCKETS.map((b) => (
            <linearGradient key={b.key} id={`tok-${b.key}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={b.color} stopOpacity={0.7} />
              <stop offset="100%" stopColor={b.color} stopOpacity={0.05} />
            </linearGradient>
          ))}
        </defs>
        <CartesianGrid {...CHART_GRID} />
        <XAxis dataKey="date" tickFormatter={shortDate} {...CHART_AXIS} minTickGap={24} />
        <YAxis tickFormatter={compact} {...CHART_AXIS} />
        <Tooltip
          cursor={{ stroke: "var(--line-3)" }}
          content={<ChartTooltip labelFormatter={shortDate} formatItem={(n, v) => `${n}: ${compact(v)}`} />}
        />
        {BUCKETS.map((b) => (
          <Area
            key={b.key}
            type="monotone"
            dataKey={b.key}
            name={b.name}
            stackId="1"
            stroke={b.color}
            fill={`url(#tok-${b.key})`}
            strokeWidth={1.4}
          />
        ))}
      </AreaChart>
    </ResponsiveContainer>
  );
}
