import { Area, AreaChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { CostPoint } from "@/lib/api";
import { shortDate, usd } from "@/lib/format";
import { Empty } from "./ui";

// CostChart renders a daily spend area series. Content-free (cost only).
export function CostChart({ data, height = 220 }: { data: CostPoint[]; height?: number }) {
  if (!data.length) return <Empty message="No spend in this window." />;
  return (
    <ResponsiveContainer width="100%" height={height}>
      <AreaChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
        <defs>
          <linearGradient id="costFill" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#3b82f6" stopOpacity={0.35} />
            <stop offset="100%" stopColor="#3b82f6" stopOpacity={0} />
          </linearGradient>
        </defs>
        <XAxis
          dataKey="date"
          tickFormatter={shortDate}
          tick={{ fill: "#9aa3b2", fontSize: 11 }}
          axisLine={{ stroke: "#262b33" }}
          tickLine={false}
          minTickGap={24}
        />
        <YAxis
          tickFormatter={(v: number) => usd(v)}
          tick={{ fill: "#9aa3b2", fontSize: 11 }}
          axisLine={false}
          tickLine={false}
          width={56}
        />
        <Tooltip
          contentStyle={{
            background: "#14171c",
            border: "1px solid #262b33",
            borderRadius: 8,
            color: "#e6e9ef",
            fontSize: 12,
          }}
          labelFormatter={(l: string) => shortDate(l)}
          formatter={(v: number) => [usd(v), "Spend"]}
        />
        <Area type="monotone" dataKey="cost_usd" stroke="#3b82f6" fill="url(#costFill)" strokeWidth={2} />
      </AreaChart>
    </ResponsiveContainer>
  );
}
