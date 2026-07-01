import type { ReactNode } from "react";
import clsx from "clsx";

// Small self-contained primitives for the org dashboard (mirrors the agent
// dashboard's look without sharing its component tree).

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div className={clsx("rounded-lg border border-line bg-surface p-4", className)}>{children}</div>
  );
}

export function Stat({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <Card>
      <div className="text-[11px] uppercase tracking-wide text-faint">{label}</div>
      <div className="mt-1 text-2xl font-semibold text-fg">{value}</div>
      {sub != null && <div className="mt-0.5 text-xs text-muted">{sub}</div>}
    </Card>
  );
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="flex h-40 items-center justify-center gap-3 text-sm text-muted">
      <span className="inline-block h-3 w-3 animate-spin rounded-full border border-line border-t-accent" />
      {label}
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <Card className="border-bad/40">
      <div className="text-sm text-bad">{message}</div>
      {onRetry && (
        <button
          onClick={onRetry}
          className="mt-3 rounded border border-line px-3 py-1 text-xs text-fg hover:bg-surface2"
        >
          Retry
        </button>
      )}
    </Card>
  );
}

export function Empty({ message }: { message: string }) {
  return <div className="py-12 text-center text-sm text-faint">{message}</div>;
}

export function Badge({ children, tone = "muted" }: { children: ReactNode; tone?: "muted" | "good" | "warn" | "bad" | "accent" }) {
  const tones: Record<string, string> = {
    muted: "border-line text-muted",
    good: "border-good/40 text-good",
    warn: "border-warn/40 text-warn",
    bad: "border-bad/40 text-bad",
    accent: "border-accent/40 text-accent",
  };
  return (
    <span className={clsx("inline-flex items-center rounded border px-1.5 py-0.5 text-[11px]", tones[tone])}>
      {children}
    </span>
  );
}

export function Button({
  children,
  onClick,
  variant = "default",
  type = "button",
  disabled,
  title,
}: {
  children: ReactNode;
  onClick?: () => void;
  variant?: "default" | "primary" | "danger";
  type?: "button" | "submit";
  disabled?: boolean;
  title?: string;
}) {
  const variants: Record<string, string> = {
    default: "border-line text-fg hover:bg-surface2",
    primary: "border-accent bg-accent/15 text-accent hover:bg-accent/25",
    danger: "border-bad/50 text-bad hover:bg-bad/10",
  };
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={clsx(
        "rounded border px-3 py-1.5 text-xs font-medium transition disabled:cursor-not-allowed disabled:opacity-40",
        variants[variant],
      )}
    >
      {children}
    </button>
  );
}

export function PageHeader({ title, subtitle, right }: { title: string; subtitle?: string; right?: ReactNode }) {
  return (
    <div className="mb-5 flex items-start justify-between gap-4">
      <div>
        <h1 className="text-lg font-semibold text-fg">{title}</h1>
        {subtitle && <p className="mt-0.5 text-sm text-muted">{subtitle}</p>}
      </div>
      {right}
    </div>
  );
}
