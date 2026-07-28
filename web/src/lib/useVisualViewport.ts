import { useEffect, useState } from "react";

// The on-screen-keyboard occlusion primitive.
//
// THE THING THAT IS COUNTERINTUITIVE AND COST US A ROUND ALREADY: a virtual
// keyboard does not shrink ANY CSS viewport unit — not `vh`, and not `dvh`
// either. `dvh` is "dynamic" with respect to retractable BROWSER UI (the URL
// bar collapsing on scroll); the CSSWG deliberately excludes the keyboard from
// viewport units so that opening it cannot reflow the whole page. The default
// `interactive-widget=resizes-visual` says exactly that: the VISUAL viewport
// shrinks, the layout viewport and its units do not.
//
// So a panel sized `h-[100dvh]` stays full height when the keyboard opens, the
// bottom of it — the prompt line the user is typing into — sits underneath the
// keyboard, and no ResizeObserver ever fires because nothing resized. That is
// the D7 "hidden input line" defect: it was previously addressed by switching
// `vh` → `dvh`, which changed a real thing but not the one causing the bug.
//
// The visual viewport is the only surface that DOES track the keyboard, so we
// read it directly and drive the panel's height from it. Paired with
// `interactive-widget=resizes-content` in index.html, which fixes the same
// problem natively on Chrome/Android; Safari/iOS ignores that key, and this
// hook is what covers it.

/** VisualViewportBox is the visible area not covered by browser UI or the
 * on-screen keyboard, plus its offset within the layout viewport. */
export interface VisualViewportBox {
  /** Visible height in CSS px. */
  height: number;
  /** The unoccluded height: the tallest this viewport has been at the current
   * width. A keyboard only ever REDUCES the visible height, so the running
   * maximum is what the viewport measures with no keyboard up — without
   * having to ask any engine-specific question about whether one is present.
   * Rotation is excluded by resetting whenever the WIDTH changes, which a
   * keyboard never does.
   *
   * Consumers size to this and translate by the difference, so that opening
   * the keyboard MOVES the panel instead of SHRINKING it. That distinction is
   * load-bearing for a full-screen TUI: shrinking resizes the PTY, and a TUI
   * lays itself out for the rows it is given — Claude Code's composer caps at
   * 2 lines in an 8-row terminal and shows only the tail of what you type,
   * measured 2026-07-28. Panning leaves the row count alone. */
  fullHeight: number;
  /** Distance the visual viewport has been scrolled/pushed down inside the
   * layout viewport. Non-zero on iOS when it scrolls to reveal a focused
   * field; a `position: fixed` element is anchored to the LAYOUT viewport, so
   * it must be translated by this much to stay inside the visible area. */
  offsetTop: number;
}

/**
 * useVisualViewport reports the live visual-viewport box while `enabled`, or
 * null when it should not be consulted.
 *
 * Returns null — meaning "use the stylesheet's own sizing, unchanged" — when
 * disabled, when the API is absent (older browsers, jsdom, SSR), or while the
 * page is pinch-zoomed. Otherwise it reports the visible box on EVERY change,
 * including when no keyboard is up: with none up the value equals what 100dvh
 * would have produced, so reporting it unconditionally costs nothing and
 * removes the need to guess whether a keyboard is present.
 *
 * It deliberately does NOT try to detect the keyboard by comparing against
 * window.innerHeight. Whether the keyboard shrinks innerHeight is an
 * engine-specific behaviour (Chrome/Android does, iOS does not), so that
 * comparison produced a per-browser answer and disabled the hook on exactly
 * one of them.
 *
 * Consumers must treat null as "behave exactly as before", so every desktop
 * path is untouched.
 */
export function useVisualViewport(enabled: boolean): VisualViewportBox | null {
  const [box, setBox] = useState<VisualViewportBox | null>(null);

  useEffect(() => {
    if (!enabled || typeof window === "undefined" || !window.visualViewport) {
      setBox(null);
      return;
    }
    const vv = window.visualViewport;
    let frame = 0;
    // Running maximum of the visible height at the current width — see
    // VisualViewportBox.fullHeight. Reset when the width changes, because that
    // is a rotation (or a desktop window resize), not a keyboard.
    let baseW = vv.width;
    let baseH = vv.height;

    const read = () => {
      frame = 0;
      // Pinch-zoom also shrinks the visual viewport, but there the user wants
      // to pan around a magnified page — resizing the panel under them would
      // fight that. scale ~1 means "not zoomed", the only state we act on.
      if (Math.abs(vv.scale - 1) > 0.01) {
        setBox(null);
        return;
      }
      // vv.height IS the visible height — keyboard, browser chrome and all.
      // Report it unconditionally rather than trying to first DECIDE whether a
      // keyboard is up.
      //
      // The previous version computed `window.innerHeight - vv.height` and
      // required 40px of difference before acting. That silently disabled the
      // whole hook on Chrome/Android, where the keyboard shrinks innerHeight
      // TOO: the difference came out ~0, the hook concluded "no keyboard", and
      // the panel kept its `h-[100dvh]` — which does not shrink for a keyboard
      // — so the terminal's composer sat underneath it. Reported as "only 2
      // lines of what I type are visible".
      //
      // Comparing the two viewports asks a question whose answer differs per
      // engine. Reading the visible height asks one whose answer is the same
      // everywhere, so this needs no per-browser knowledge and no tolerance to
      // tune. With no keyboard up it resolves to the same height 100dvh would
      // have given.
      if (vv.width !== baseW) {
        // Rotation / window resize: the old maximum belongs to the old width.
        baseW = vv.width;
        baseH = vv.height;
      } else if (vv.height > baseH) {
        baseH = vv.height;
      }
      setBox((prev) =>
        prev &&
        prev.height === vv.height &&
        prev.fullHeight === baseH &&
        prev.offsetTop === vv.offsetTop
          ? prev // identical box → same object → no re-render
          : { height: vv.height, fullHeight: baseH, offsetTop: vv.offsetTop },
      );
    };

    // The keyboard animates open, firing a burst of resize events; coalesce
    // them to one read per frame so we resize the PTY once, not per event.
    const schedule = () => {
      if (frame) return;
      frame = window.requestAnimationFrame(read);
    };

    read();
    vv.addEventListener("resize", schedule);
    vv.addEventListener("scroll", schedule);
    return () => {
      if (frame) window.cancelAnimationFrame(frame);
      vv.removeEventListener("resize", schedule);
      vv.removeEventListener("scroll", schedule);
    };
  }, [enabled]);

  return box;
}
