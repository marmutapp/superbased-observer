import clsx from "clsx";
import type { ReactNode } from "react";

// MIRROR of web/src/components/primitives/ChartShell.tsx — titled card frame
// every chart sits inside, so headers/padding/borders stay consistent.
export function ChartShell({
  title,
  sub,
  right,
  className,
  bodyClassName,
  children,
}: {
  title: ReactNode;
  sub?: ReactNode;
  right?: ReactNode;
  className?: string;
  bodyClassName?: string;
  children: ReactNode;
}) {
  return (
    <section
      className={clsx(
        "flex flex-col gap-3 rounded-3 border border-line-2 bg-bg-2 p-4",
        className,
      )}
    >
      <header className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-[13px] font-semibold text-fg-0">{title}</h3>
          {sub && <p className="mt-0.5 text-[11px] text-fg-3">{sub}</p>}
        </div>
        {right && <div className="shrink-0">{right}</div>}
      </header>
      <div className={clsx("min-h-0 flex-1", bodyClassName)}>{children}</div>
    </section>
  );
}
