import type { HourCount } from "@/lib/api";
import { compact } from "@/lib/format";
import { Tooltip } from "@/components/primitives";

// HourBars — 24-bucket "when activity happens" histogram (UTC), hand-rolled so
// each bar carries an hour label + tooltip. Adapted from web/'s HourBars to the
// org wire shape (action count, not cost). Intensity scales with the count.
export function HourBars({ buckets }: { buckets: HourCount[] }) {
  const filled = Array.from(
    { length: 24 },
    (_, h) => buckets.find((b) => b.hour === h) ?? { hour: h, count: 0 },
  );
  const max = Math.max(1, ...filled.map((b) => b.count));

  return (
    <div className="flex h-[180px] items-end gap-[2px] px-1">
      {filled.map((b) => {
        const intensity = b.count / max;
        const h = Math.max(2, intensity * 150);
        return (
          <div key={b.hour} className="group flex flex-1 flex-col items-center gap-1">
            <Tooltip content={`${pad(b.hour)}:00 UTC · ${compact(b.count)} actions`}>
              <div
                tabIndex={0}
                className="w-full cursor-help rounded-sm transition-opacity group-hover:opacity-90 focus:outline-none"
                style={{ height: `${h}px`, background: hourTint(intensity) }}
              />
            </Tooltip>
            <span className="text-[9px] tabular-nums text-fg-3">{pad(b.hour)}</span>
          </div>
        );
      })}
    </div>
  );
}

function hourTint(t: number): string {
  if (t <= 0) return "var(--bg-4)";
  if (t < 0.2) return "color-mix(in srgb, var(--tok-net) 35%, var(--bg-4))";
  if (t < 0.4) return "color-mix(in srgb, var(--tok-net) 60%, transparent)";
  if (t < 0.6) return "color-mix(in srgb, var(--accent) 70%, transparent)";
  if (t < 0.8) return "color-mix(in srgb, var(--warn) 75%, transparent)";
  return "var(--warn)";
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}
