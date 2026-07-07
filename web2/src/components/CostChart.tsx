import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { CostPoint } from "@/lib/api";
import { shortDate, usd } from "@/lib/format";
import { CHART_AXIS, CHART_GRID } from "./charts/common";
import { ChartTooltip } from "./charts/ChartTooltip";
import { Empty } from "./ui";

// CostChart renders a daily spend area series. Content-free (cost only).
// Themed via the design tokens (mirror of web/'s chart styling) so it flips
// on light/dark without hardcoded hex.
export function CostChart({
  data,
  height = 220,
}: {
  data: CostPoint[];
  height?: number;
}) {
  if (!data.length) return <Empty message="No spend in this window." />;
  return (
    <ResponsiveContainer width="100%" height={height}>
      <AreaChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
        <defs>
          <linearGradient id="costFill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.4} />
            <stop offset="100%" stopColor="var(--accent)" stopOpacity={0} />
          </linearGradient>
        </defs>
        <CartesianGrid {...CHART_GRID} />
        <XAxis
          dataKey="date"
          tickFormatter={shortDate}
          {...CHART_AXIS}
          minTickGap={24}
        />
        <YAxis
          tickFormatter={(v: number) => usd(v)}
          {...CHART_AXIS}
          width={56}
        />
        <Tooltip
          content={
            <ChartTooltip
              labelFormatter={shortDate}
              formatItem={(_name, value) => `Spend: ${usd(value)}`}
            />
          }
          cursor={{ stroke: "var(--line-3)" }}
        />
        <Area
          type="monotone"
          dataKey="cost_usd"
          name="Spend"
          stroke="var(--accent)"
          fill="url(#costFill)"
          strokeWidth={2}
        />
      </AreaChart>
    </ResponsiveContainer>
  );
}
