import type { DowHourCount } from "@/lib/api";
import { compact } from "@/lib/format";
import { Tooltip } from "@/components/primitives";

const DOWS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

// DowHourHeatmap — 7×24 day-of-week × hour-of-day action-intensity grid (UTC),
// hand-rolled CSS grid (mirrors web/'s DowHourHeatmap idiom). Warmer = busier.
export function DowHourHeatmap({ cells }: { cells: DowHourCount[] }) {
  if (!cells.length) {
    return <div className="grid h-[180px] place-items-center text-[12px] text-fg-3">No activity in window.</div>;
  }
  const grid = new Map<string, number>();
  let max = 1;
  for (const c of cells) {
    grid.set(`${c.dow}-${c.hour}`, c.count);
    if (c.count > max) max = c.count;
  }
  return (
    <div className="overflow-x-auto">
      <div className="inline-grid gap-[2px]" style={{ gridTemplateColumns: "28px repeat(24, 1fr)" }}>
        <div />
        {Array.from({ length: 24 }, (_, h) => (
          <div key={`h${h}`} className="text-center text-[8px] tabular-nums text-fg-4">
            {h % 3 === 0 ? String(h).padStart(2, "0") : ""}
          </div>
        ))}
        {DOWS.map((label, dow) => (
          <Row key={dow} label={label} dow={dow} grid={grid} max={max} />
        ))}
      </div>
    </div>
  );
}

function Row({ label, dow, grid, max }: { label: string; dow: number; grid: Map<string, number>; max: number }) {
  return (
    <>
      <div className="flex items-center text-[9px] text-fg-3">{label}</div>
      {Array.from({ length: 24 }, (_, h) => {
        const n = grid.get(`${dow}-${h}`) ?? 0;
        const t = n / max;
        return (
          <Tooltip key={h} content={`${label} ${String(h).padStart(2, "0")}:00 UTC · ${compact(n)} actions`}>
            <div
              tabIndex={0}
              className="aspect-square min-w-[10px] rounded-[2px] focus:outline-none"
              style={{ background: tint(t) }}
            />
          </Tooltip>
        );
      })}
    </>
  );
}

function tint(t: number): string {
  if (t <= 0) return "var(--bg-3)";
  if (t < 0.25) return "color-mix(in srgb, var(--accent) 25%, var(--bg-3))";
  if (t < 0.5) return "color-mix(in srgb, var(--accent) 50%, transparent)";
  if (t < 0.75) return "color-mix(in srgb, var(--accent) 75%, transparent)";
  return "var(--accent)";
}
