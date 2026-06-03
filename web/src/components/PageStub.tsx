import type { ReactNode } from "react";

export function PageStub({
  title,
  phase,
  children,
}: {
  title: string;
  phase: string;
  children?: ReactNode;
}) {
  return (
    <div className="p-6">
      <div className="mb-1 flex items-baseline gap-3">
        <h1 className="text-[22px] font-semibold tracking-tight text-fg-0">
          {title}
        </h1>
        <span className="rounded-pill border border-line-2 bg-bg-2 px-2 py-0.5 text-[10px] uppercase tracking-wide text-fg-3">
          {phase}
        </span>
      </div>
      <p className="mb-5 text-[12px] text-fg-3">
        Placeholder. Real content lands in {phase}.
      </p>
      {children && <div className="space-y-4">{children}</div>}
    </div>
  );
}
