import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Sidebar } from "@/components/Sidebar";
import { TopBar } from "@/components/TopBar";
import { HelpDrawer } from "@/components/HelpDrawer";
import { CommandPalette } from "@/components/CommandPalette";

// Org dashboard shell — mirrors the primary dashboard's frame: a fixed-width
// token-styled Sidebar, a sticky TopBar (breadcrumb + global window + refresh
// + help + theme toggle), and a scrolling content column constrained to a
// readable max width.
//
// Owns the Help drawer wiring (mirror of web/'s App.tsx): the `?` key toggles
// it when no input is focused, and a single delegated click listener opens it
// scrolled to whichever HelpInd (data-help-id) was clicked.
export function Layout({ children }: { children: ReactNode }) {
  const [helpOpen, setHelpOpen] = useState(false);
  const [helpId, setHelpId] = useState<string | null>(null);
  const [paletteOpen, setPaletteOpen] = useState(false);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      // ⌘K / Ctrl-K toggles the palette even from an input (VSCode/Linear-style).
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setPaletteOpen((o) => !o);
        return;
      }
      const t = e.target as HTMLElement | null;
      const tag = (t?.tagName || "").toLowerCase();
      const isInput =
        tag === "input" || tag === "textarea" || t?.isContentEditable;
      if (isInput) return;
      if (e.key === "?") {
        e.preventDefault();
        setHelpOpen((o) => !o);
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // Clicking any HelpInd (data-help-id) opens the drawer scrolled to that
  // entry. One delegated listener keeps the indicator lightweight and avoids
  // prop-drilling.
  useEffect(() => {
    function onClick(e: MouseEvent) {
      const el = (e.target as HTMLElement | null)?.closest<HTMLElement>(
        "[data-help-id]",
      );
      if (!el) return;
      const id = el.getAttribute("data-help-id");
      if (id) {
        setHelpId(id);
        setHelpOpen(true);
      }
    }
    document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, []);

  const openHelp = useCallback(() => {
    setHelpId(null);
    setHelpOpen(true);
  }, []);

  return (
    <div className="flex h-full bg-bg-0">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar onHelp={openHelp} />
        <main className="flex-1 overflow-y-auto px-6 py-5">
          <div className="mx-auto max-w-6xl">{children}</div>
        </main>
      </div>
      <HelpDrawer
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        initialId={helpId}
      />
      <CommandPalette open={paletteOpen} onClose={() => setPaletteOpen(false)} />
    </div>
  );
}
