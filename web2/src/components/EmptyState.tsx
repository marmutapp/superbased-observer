import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { Inbox, type LucideIcon } from "lucide-react";
import clsx from "clsx";
import { HelpInd } from "@/components/HelpInd";

// EmptyState — the org dashboard's shared teaching empty state (G11). Replaces
// the grab-bag of bespoke per-page "No data." lines and one-off NotConfigured
// cards with one component that TEACHES: what the surface shows, the exact
// dependency to configure to get data, and a way to reach the deeper help
// (help drawer via `helpId`, an in-app link via `to`, and/or a docs pointer).
//
// Intentionally token-styled to match Card/StatCard so it reads as part of the
// system in both light and dark. Keep the copy honest — name the real missing
// dependency (a node-side opt-in, a poller key, a proxy route), never imply
// "coming soon".
export type EmptyStateProps = {
  // Short headline — the affirmative "what would be here".
  title: string;
  // One or two sentences: what this surface shows + what to configure.
  body?: ReactNode;
  // Optional numbered/plain teaching steps.
  steps?: ReactNode[];
  icon?: LucideIcon;
  // Opens the help drawer scrolled to this registry entry (see help.ts).
  helpId?: string;
  // In-app call-to-action (e.g. "/invite" to enroll an agent).
  to?: string;
  toLabel?: string;
  // Docs pointer rendered as a monospace hint (the org server does not serve
  // docs, so this is a path string, not a link — matches existing convention).
  docHint?: string;
  // Extra content (e.g. a per-vendor list) rendered under the body.
  children?: ReactNode;
  // "card" (default) draws the bordered panel; "inline" is a compact
  // borderless variant for inside an existing Card/ChartShell.
  variant?: "card" | "inline";
  className?: string;
};

export function EmptyState({
  title,
  body,
  steps,
  icon: Icon = Inbox,
  helpId,
  to,
  toLabel = "Get started",
  docHint,
  children,
  variant = "card",
  className,
}: EmptyStateProps) {
  return (
    <div
      className={clsx(
        variant === "card"
          ? "rounded-3 border border-line-2 bg-bg-1 p-6"
          : "py-8 text-center",
        className,
      )}
    >
      <div
        className={clsx(
          "flex gap-3",
          variant === "inline" && "flex-col items-center",
        )}
      >
        <span
          className={clsx(
            "grid h-9 w-9 shrink-0 place-items-center rounded-2 border border-line-2 bg-bg-2 text-fg-3",
            variant === "inline" && "mx-auto",
          )}
          aria-hidden
        >
          <Icon size={16} />
        </span>
        <div className={clsx("min-w-0", variant === "inline" && "text-center")}>
          <h3 className="flex items-center gap-1 text-[14px] font-semibold text-fg-0">
            {title}
            {helpId && <HelpInd id={helpId} />}
          </h3>
          {body && (
            <p className="mt-1.5 max-w-2xl text-[13px] leading-relaxed text-fg-2">
              {body}
            </p>
          )}
          {steps && steps.length > 0 && (
            <ol
              className={clsx(
                "mt-3 space-y-1.5 text-[12.5px] text-fg-2",
                variant === "inline" && "inline-block text-left",
              )}
            >
              {steps.map((s, i) => (
                <li key={i} className="flex items-baseline gap-2">
                  <span className="font-mono text-[10.5px] text-fg-4">
                    {i + 1}
                  </span>
                  <span>{s}</span>
                </li>
              ))}
            </ol>
          )}
          {children && <div className="mt-3">{children}</div>}
          {(to || docHint) && (
            <div
              className={clsx(
                "mt-4 flex flex-wrap items-center gap-3",
                variant === "inline" && "justify-center",
              )}
            >
              {to && (
                <Link
                  to={to}
                  className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1.5 text-[12px] font-semibold text-accent hover:opacity-90"
                >
                  {toLabel}
                </Link>
              )}
              {docHint && (
                <span className="text-[11.5px] text-fg-4">
                  Docs: <code className="font-mono text-fg-3">{docHint}</code>
                </span>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
