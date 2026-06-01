import clsx from "clsx";

const OPTIONS = [7, 30, 90] as const;

// WindowSelect picks the trailing window (days) for the rollups.
export function WindowSelect({ value, onChange }: { value: number; onChange: (d: number) => void }) {
  return (
    <div className="inline-flex overflow-hidden rounded border border-line text-xs">
      {OPTIONS.map((d) => (
        <button
          key={d}
          onClick={() => onChange(d)}
          className={clsx(
            "px-2.5 py-1 transition",
            d === value ? "bg-accent/20 text-accent" : "text-muted hover:bg-surface2",
          )}
        >
          {d}d
        </button>
      ))}
    </div>
  );
}
