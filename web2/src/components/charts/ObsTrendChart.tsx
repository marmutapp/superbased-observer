import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { ObsDayPoint } from "@/lib/api";
import { compact, shortDate } from "@/lib/format";
import { CHART_AXIS, CHART_GRID } from "./common";
import { ChartTooltip } from "./ChartTooltip";

// ObsTrendChart — area of daily trace volume (left) over the window. A simple,
// content-free volume trend over the obs_summaries aggregate.
export function ObsTrendChart({ data }: { data: ObsDayPoint[] }) {
  if (!data.length) {
    return (
      <div className="grid h-[240px] place-items-center text-[12px] text-fg-3">
        No trajectories in window.
      </div>
    );
  }
  return (
    <ResponsiveContainer width="100%" height={240}>
      <AreaChart data={data} margin={{ top: 8, right: 12, bottom: 0, left: 4 }}>
        <defs>
          <linearGradient id="obsTrace" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--tok-net)" stopOpacity={0.35} />
            <stop offset="100%" stopColor="var(--tok-net)" stopOpacity={0.02} />
          </linearGradient>
        </defs>
        <CartesianGrid {...CHART_GRID} />
        <XAxis dataKey="date" tickFormatter={shortDate} {...CHART_AXIS} />
        <YAxis tickFormatter={compact} width={44} {...CHART_AXIS} />
        <Tooltip content={<ChartTooltip labelFormatter={shortDate} formatItem={(n, v) => `${n}: ${compact(v)}`} />} />
        <Area
          type="monotone"
          dataKey="traces"
          name="Traces"
          stroke="var(--tok-net)"
          fill="url(#obsTrace)"
          strokeWidth={2}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}
