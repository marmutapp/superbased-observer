import { Component, type ReactNode } from "react";

// RouteErrorBoundary catches render errors below the app shell so a crashing
// page can't blank the whole org dashboard — the sidebar, topbar, and every
// other route stay alive. App.tsx keys this boundary on the route pathname, so
// navigating away from a crashed page automatically resets it. The recovery
// actions are full page loads on purpose: after a render crash, remounting from
// scratch is the only state we can vouch for. Mirrors web/'s ErrorBoundary
// (without the arcade mascot, which web2 doesn't ship).
export class RouteErrorBoundary extends Component<
  { children: ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    if (!this.state.error) return this.props.children;
    const message = this.state.error.message || String(this.state.error);
    return (
      <div className="flex h-full items-center justify-center p-12">
        <div className="flex max-w-md flex-col items-center text-center">
          <span className="rounded-2 border border-line-2 bg-bg-2 px-2.5 py-1 font-mono text-[10.5px] tracking-[0.12em] text-fg-3">
            ▸ RENDER ERROR
          </span>
          <p className="mt-3 text-[13px] text-fg-2">
            Something broke while rendering this page. The rest of the dashboard
            is fine.
          </p>
          <p className="mt-2 max-h-24 overflow-y-auto rounded-2 bg-bg-2 px-3 py-2 font-mono text-[11px] text-fg-3">
            {message}
          </p>
          <div className="mt-4 flex items-center gap-3">
            <button
              type="button"
              onClick={() => window.location.reload()}
              className="rounded-2 border border-accent/40 bg-accent-soft px-3 py-1.5 text-[11.5px] font-semibold text-accent hover:opacity-90"
            >
              Reload
            </button>
            <a href="/" className="text-[12px] font-medium text-accent hover:underline">
              Back to Overview
            </a>
          </div>
        </div>
      </div>
    );
  }
}
