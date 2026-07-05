import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { ModelSlice } from "@/lib/api";
import { compact, usd } from "@/lib/format";
import { CHART_AXIS, CHART_GRID } from "./common";
import { ChartTooltip } from "./ChartTooltip";

// ModelBar — horizontal cost-by-model bar. Mirrors the primary dashboard's
// per-model breakdown idiom; content-free (model label + cost + tokens).
export function ModelBar({ models }: { models: ModelSlice[] }) {
  if (!models.length) {
    return (
      <div className="grid h-[200px] place-items-center text-[12px] text-fg-3">
        No model spend in window.
      </div>
    );
  }
  const data = models.slice(0, 8);
  const height = Math.max(160, data.length * 34);
  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={data} layout="vertical" margin={{ top: 4, right: 12, bottom: 4, left: 8 }}>
        <CartesianGrid {...CHART_GRID} horizontal={false} vertical />
        <XAxis type="number" tickFormatter={(v: number) => usd(v)} {...CHART_AXIS} />
        <YAxis
          type="category"
          dataKey="model"
          width={130}
          {...CHART_AXIS}
          tick={{ fill: "var(--fg-2)", fontSize: 10 }}
          tickFormatter={(s: string) => (s.length > 20 ? s.slice(0, 19) + "…" : s)}
        />
        <Tooltip
          cursor={{ fill: "var(--bg-3)", opacity: 0.4 }}
          content={
            <ChartTooltip
              formatItem={(_n, v) => `Spend: ${usd(v)}`}
              extra={(row) =>
                row.tokens != null ? `${compact(Number(row.tokens))} tokens` : null
              }
            />
          }
        />
        <Bar dataKey="cost_usd" name="Spend" fill="var(--accent)" radius={[0, 3, 3, 0]} />
      </BarChart>
    </ResponsiveContainer>
  );
}
