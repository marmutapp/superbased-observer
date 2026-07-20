import { useMemo, useState } from "react";
import { Responsive, WidthProvider } from "react-grid-layout";
import type { Layout, Layouts } from "react-grid-layout";
import "react-grid-layout/css/styles.css";
import "react-resizable/css/styles.css";
import type { ReactNode } from "react";
import type { WorkspaceLayouts } from "./WorkspaceGrid";

// WorkspaceGridInner — the react-grid-layout half of the Terminal Workspace,
// split into its own module so RGL (vendor-grid chunk) loads lazily with the
// grid, never in the eager bundle.
//
// Grid model (design §3.2): 12/8/4/1 columns at lg/md/sm/xs, 40 px rows,
// vertical auto-compaction (the operator's "auto-compact into expandable
// grid"), drag by the tile header only (draggableHandle), resize from the
// south/east edges. Drag + resize are disabled at xs (<640 px — touch phones
// get a vertical stack; those gestures are hostile on touch).

const ResponsiveGridLayout = WidthProvider(Responsive);

const BREAKPOINTS = { lg: 1200, md: 900, sm: 640, xs: 0 };
const COLS = { lg: 12, md: 8, sm: 4, xs: 1 };

export default function WorkspaceGridInner({
  docked,
  layouts,
  onLayoutsChange,
  renderTile,
  readOnly,
}: {
  docked: string[];
  layouts: WorkspaceLayouts;
  onLayoutsChange: (all: WorkspaceLayouts) => void;
  renderTile: (token: string) => ReactNode;
  readOnly: boolean;
}) {
  const [breakpoint, setBreakpoint] = useState("lg");
  const interactive = breakpoint !== "xs" && !readOnly;

  // Build COMPLETE layouts: every docked token gets an item in EVERY
  // breakpoint (saved position when present, a synthesized slot otherwise).
  // Deliberately NO data-grid on the children — in RGL 1.x a child data-grid
  // takes precedence over the layouts prop during synchronization, which
  // would snap loaded/dragged positions back to defaults (review HIGH).
  // Stale items for undocked tokens are filtered; the huge synthetic y is
  // normalized by vertical compaction on first render.
  const rglLayouts = useMemo<Layouts>(() => {
    const out: Layouts = {};
    for (const bp of Object.keys(COLS) as (keyof typeof COLS)[]) {
      const cols = COLS[bp];
      const existing = (layouts[bp] ?? []).filter((it) => docked.includes(it.i));
      const known = new Set(existing.map((it) => it.i));
      const w = Math.min(6, cols);
      const synth = docked
        .filter((t) => !known.has(t))
        .map((t, k) => ({
          i: t,
          x: (k * w) % cols,
          y: 100000 + k,
          w,
          h: 10,
          minW: Math.min(3, cols),
          minH: 5,
        }));
      out[bp] = [...existing, ...synth];
    }
    return out;
  }, [docked, layouts]);

  return (
    <ResponsiveGridLayout
      className="ws-grid"
      breakpoints={BREAKPOINTS}
      cols={COLS}
      rowHeight={40}
      margin={[10, 10]}
      layouts={rglLayouts}
      compactType="vertical"
      draggableHandle=".ws-tile-drag"
      isDraggable={interactive}
      isResizable={interactive}
      resizeHandles={["se", "e", "s"]}
      onBreakpointChange={(bp: string) => setBreakpoint(bp)}
      onLayoutChange={(_current: Layout[], all: Layouts) =>
        onLayoutsChange(all as WorkspaceLayouts)
      }
    >
      {docked.map((token) => (
        <div key={token}>{renderTile(token)}</div>
      ))}
    </ResponsiveGridLayout>
  );
}
