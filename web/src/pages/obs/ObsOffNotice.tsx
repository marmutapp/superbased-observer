import type { ReactNode } from "react";
import { ChartShell } from "@/components/primitives";

// ObsOffNotice is the honest "subsystem disabled" state shared by the
// Trajectories + Evals list pages: when [observability] is off the endpoints
// aren't served, so a fetch error means "feature off", not "error". It names
// the exact config key + next step (the operator-honesty steer) and renders
// through ChartShell so it reads like every other panel on the platform.
export function ObsOffNotice({ children }: { children: ReactNode }) {
  return (
    <ChartShell title="Observability subsystem is off">
      <div className="max-w-2xl text-[13px] leading-relaxed text-fg-2">{children}</div>
    </ChartShell>
  );
}
