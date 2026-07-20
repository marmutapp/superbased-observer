import clsx from "clsx";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { Sparkline } from "./Sparkline";
import { HelpInd } from "@/components/HelpInd";

// MIRROR of web/src/components/primitives/StatCard.tsx — the standard headline
// KPI tile: label + big value + optional unit / sub / delta / sparkline, with
// a faint accent radial wash. Delta coloring is cost-aware (UP = danger,
// DOWN = success) because these tiles measure cost/volume.

export type StatCardProps = {
  label: string;
  value: ReactNode;
  unit?: string;
  sub?: ReactNode;
  // Signed fraction: 0.12 = +12%. UP renders danger-red, DOWN success-green.
  delta?: number;
  deltaLabel?: string;
  deltaPrior?: ReactNode;
  spark?: number[];
  sparkColor?: string;
  warn?: boolean;
  accent?: boolean;
  className?: string;
  helpId?: string;
  icon?: ReactNode;
  cornerPill?: ReactNode;
  linkTo?: string;
  loading?: boolean;
  children?: ReactNode;
};

export function StatCard(props: StatCardProps) {
  const { linkTo, className, accent, warn, spark, sparkColor, loading, ...rest } =
    props;
  const wrapperClass = clsx(
    "relative flex min-h-[104px] flex-col gap-1 overflow-hidden rounded-3 border bg-bg-2 px-4 py-3 transition-colors",
    accent
      ? "border-accent/40"
      : warn
        ? "border-warn/30"
        : "border-line-2 hover:border-line-3",
    linkTo && "hover:bg-bg-3",
    className,
  );
  const overlayGradient = accent
    ? "radial-gradient(circle at 100% 0%, var(--accent-soft), transparent 62%)"
    : warn
      ? "radial-gradient(circle at 100% 0%, var(--warn-soft), transparent 62%)"
      : "radial-gradient(circle at 100% 0%, color-mix(in srgb, var(--accent-soft) 35%, transparent), transparent 70%)";
  const inner = (
    <>
      <span
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{ background: overlayGradient }}
      />
      <div
        className={clsx(
          "relative flex min-h-0 flex-1 flex-col gap-1",
          loading && "animate-pulse",
        )}
      >
        <Body {...rest} loading={loading} />
      </div>
      {spark && spark.length >= 2 && (
        <div
          aria-hidden
          className="pointer-events-none absolute bottom-2.5 right-3 opacity-[0.85]"
        >
          <Sparkline data={spark} color={sparkColor} width={80} height={28} />
        </div>
      )}
    </>
  );
  if (linkTo) {
    return (
      <Link to={linkTo} className={wrapperClass}>
        {inner}
      </Link>
    );
  }
  return <div className={wrapperClass}>{inner}</div>;
}

function Body({
  label,
  value,
  unit,
  sub,
  delta,
  deltaLabel,
  deltaPrior,
  helpId,
  icon,
  cornerPill,
  loading,
  children,
}: Omit<
  StatCardProps,
  "linkTo" | "className" | "warn" | "accent" | "spark" | "sparkColor"
>) {
  const deltaColor =
    delta == null || !Number.isFinite(delta)
      ? "text-fg-3"
      : delta > 0
        ? "text-danger"
        : delta < 0
          ? "text-success"
          : "text-fg-3";
  return (
    <>
      <div className="flex items-start justify-between gap-2">
        <div className="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
          {icon && <span className="text-fg-3">{icon}</span>}
          {label}
          {helpId && <HelpInd id={helpId} />}
          {loading && (
            <span
              aria-label="loading"
              className="ml-0.5 inline-block h-1 w-1 animate-pulse rounded-full bg-accent"
            />
          )}
        </div>
        {cornerPill && <div className="shrink-0">{cornerPill}</div>}
      </div>
      <div className="mt-1 flex items-baseline gap-1.5 text-fg-0">
        <span className="text-[34px] font-bold leading-[1.05] tracking-[-0.025em]">
          {value}
        </span>
        {unit && (
          <span className="text-[16px] font-medium text-fg-2">{unit}</span>
        )}
      </div>
      {(sub || delta != null || deltaPrior != null) && (
        <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 pr-[88px] text-[11px] text-fg-3">
          {delta != null && Number.isFinite(delta) && (
            <span
              className={clsx(
                "inline-flex items-center gap-0.5 font-semibold",
                deltaColor,
              )}
            >
              {delta > 0 ? "↑" : delta < 0 ? "↓" : "·"}
              {(Math.abs(delta) * 100).toFixed(1)}%
            </span>
          )}
          {deltaPrior != null ? (
            <span>{deltaPrior}</span>
          ) : (
            deltaLabel && <span>{deltaLabel}</span>
          )}
          {sub && <span>{sub}</span>}
        </div>
      )}
      {children}
    </>
  );
}
