import type { LegendProps } from "recharts";

// MIRROR of web/src/components/charts/ChartLegend.tsx — shared Recharts
// <Legend content={...} /> renderer: a horizontal row of [dot] [label] pills.
export function ChartLegend(props: LegendProps) {
  const payload = props.payload ?? [];
  if (!payload.length) return null;
  return (
    <ul className="flex flex-wrap items-center gap-x-3 gap-y-1 px-1 pb-1 text-[10.5px] text-fg-2">
      {payload.map((p, i) => (
        <li key={`${p.value}-${i}`} className="flex items-center gap-1.5">
          <span
            aria-hidden
            className="h-2 w-2 rounded-pill"
            style={{ background: p.color ?? "var(--fg-3)" }}
          />
          <span>{p.value}</span>
        </li>
      ))}
    </ul>
  );
}
