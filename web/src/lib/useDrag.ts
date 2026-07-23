import { useCallback, useRef } from "react";

// Shared pointer-drag primitive. One owner for the drag gesture math used by
// both the floating terminal dock (LaunchDock.useDockDrag, an offset model) and
// the FloatingPanel primitive (an absolute-rect model). The hook only tracks a
// single pointer gesture and reports the cumulative delta from pointer-down;
// each consumer owns its own clamp/persist policy on top.

export type DragDelta = { dx: number; dy: number };

/**
 * usePointerDrag returns pointer handlers that track ONE drag gesture using
 * pointer capture (so the gesture survives the cursor leaving the element).
 * onMove receives the cumulative delta from the pointer-down point; onEnd fires
 * once with the final delta.
 *
 * Pointer hygiene: only the PRIMARY (left) button starts a gesture; the
 * gesture is pinned to the pointerId that started it (so a second touch can't
 * hijack an in-flight drag); and it is finalized through ONE cleanup that runs
 * on pointerup, pointercancel, AND lostpointercapture — clearing start.current
 * either way, so a cancelled/interrupted drag can never leave the gesture armed
 * for a later stray pointermove to resume from a stale origin.
 */
export function usePointerDrag(opts: {
  onStart?: (e: React.PointerEvent) => void;
  onMove: (delta: DragDelta, e: React.PointerEvent) => void;
  onEnd?: (delta: DragDelta) => void;
}): {
  onPointerDown: (e: React.PointerEvent) => void;
  onPointerMove: (e: React.PointerEvent) => void;
  onPointerUp: (e: React.PointerEvent) => void;
  onPointerCancel: (e: React.PointerEvent) => void;
  onLostPointerCapture: (e: React.PointerEvent) => void;
} {
  const start = useRef<{ x: number; y: number; pointerId: number } | null>(null);
  const last = useRef<DragDelta>({ dx: 0, dy: 0 });
  // Refs so the returned handlers stay stable while calling the latest callbacks.
  const cbs = useRef(opts);
  cbs.current = opts;

  const onPointerDown = useCallback((e: React.PointerEvent) => {
    // Primary (left) button only — a middle/right/aux press must not start a
    // move/resize. Ignore a second down while a gesture is already tracking.
    if (e.button !== 0 || start.current) return;
    (e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId);
    start.current = { x: e.clientX, y: e.clientY, pointerId: e.pointerId };
    last.current = { dx: 0, dy: 0 };
    cbs.current.onStart?.(e);
  }, []);

  const onPointerMove = useCallback((e: React.PointerEvent) => {
    const s = start.current;
    if (!s || e.pointerId !== s.pointerId) return; // ignore other pointers
    const delta = { dx: e.clientX - s.x, dy: e.clientY - s.y };
    last.current = delta;
    cbs.current.onMove(delta, e);
  }, []);

  // Single finalizer for pointerup / pointercancel / lostpointercapture: clear
  // the gesture and fire onEnd exactly once for the owning pointer.
  const finish = useCallback((e: React.PointerEvent) => {
    const s = start.current;
    if (!s || e.pointerId !== s.pointerId) return;
    start.current = null;
    try {
      (e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId);
    } catch {
      /* capture already released/lost — nothing to do */
    }
    cbs.current.onEnd?.(last.current);
  }, []);

  return {
    onPointerDown,
    onPointerMove,
    onPointerUp: finish,
    onPointerCancel: finish,
    onLostPointerCapture: finish,
  };
}

export type Rect = { x: number; y: number; w: number; h: number };

/**
 * clampRectToViewport bounds an absolute rect so it stays within the viewport
 * with the given margin, keeping at least `keepVisible` px of the title bar row
 * reachable on every edge. Width/height are capped to the viewport first, then
 * the origin is bounded. Pure.
 */
export function clampRectToViewport(rect: Rect, margin = 8, keepVisible = 120): Rect {
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const w = Math.min(rect.w, vw - margin * 2);
  const h = Math.min(rect.h, vh - margin * 2);
  // Keep a strip of the panel reachable: never let it slide fully off any edge.
  const minX = margin - (w - keepVisible);
  const maxX = vw - margin - keepVisible;
  const minY = margin;
  const maxY = vh - margin - keepVisible;
  return {
    x: Math.min(Math.max(rect.x, minX), Math.max(minX, maxX)),
    y: Math.min(Math.max(rect.y, minY), Math.max(minY, maxY)),
    w,
    h,
  };
}
