import {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import clsx from "clsx";
import { postSessionTags, fetchTagRollup } from "@/lib/api";
import {
  CLASSIFY_REMOTE_BLOCKED_MSG,
  canClassifySessions,
} from "@/lib/remote";
import type { TagRollup } from "@/lib/types";
import { TagPill } from "@/components/TagPill";
import { useCompanionRegistry } from "@/components/primitives/companion";

// TagEditor — the popover that adds/removes a session's tags, plus the
// favorite star that shares its POST endpoint.
// docs/plans/session-classification-tags-plan-2026-07-31.md §0/§5.
//
// Normalization is mirrored here ONLY as a live preview so the operator can
// see what they'll actually get before committing; the server remains the
// authority and its post-mutation reply always overwrites local state.

export const MAX_TAGS_PER_SESSION = 16;
export const MAX_TAG_LENGTH = 40;

// Starter vocabulary (§0). Suggestions only — never enforced, and shown
// only while the operator has no vocabulary of their own.
export const STARTER_TAGS = [
  "experiment",
  "ui-ux",
  "backend",
  "database",
  "networking",
  "junk",
  "exploratory",
  "learning",
  "benchmark",
];

// TAG_CHAR mirrors the server's post-normalization charset check exactly
// (internal/store/sessiontags.go::NormalizeTag → [a-z0-9._-]).
const TAG_CHAR = /^[a-z0-9._-]$/;

// normalizeTag applies ONLY the transforms the server also applies
// (internal/store/sessiontags.go::NormalizeTag): trim, lowercase, collapse each
// internal whitespace run to a single `-`, then trim leading/trailing `-`.
//
// It deliberately does NOT strip disallowed characters and does NOT truncate.
// The server REJECTS such input rather than reshaping it — `café`, `C++` and a
// 41-character tag are all errors there — so a preview that silently produced
// `caf`, `c` and a 40-char stem would promise a tag the operator never asked
// for and, worse, split one intended label across two vocabulary entries.
// Pair every call with tagInputError to decide whether the result is
// submittable at all.
export function normalizeTag(raw: string): string {
  return raw
    .trim()
    .toLowerCase()
    .replace(/\s+/g, "-")
    .replace(/^-+|-+$/g, "");
}

// utf8Length counts the UTF-8 BYTES of a string, because the server's 40-char
// bound is `len(tag)` in Go — bytes, not code points. For a tag that passes the
// charset check the two are identical (ASCII); the distinction only matters
// while reporting how far over the limit a rejected multi-byte input is.
function utf8Length(s: string): number {
  if (typeof TextEncoder !== "undefined") return new TextEncoder().encode(s).length;
  let n = 0;
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0;
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
  }
  return n;
}

// tagInputError validates a NORMALIZED tag against the same three rules the
// server applies, in the same order (empty → length → charset), and returns an
// operator-facing message naming the exact offending characters or limit.
// Returns null when the tag would be accepted.
export function tagInputError(normalized: string): string | null {
  if (normalized === "") {
    return "Nothing usable left after normalization — a tag needs at least one of a-z, 0-9, dot, underscore or dash.";
  }
  const bytes = utf8Length(normalized);
  if (bytes > MAX_TAG_LENGTH) {
    return `Too long — the limit is ${MAX_TAG_LENGTH} characters (this is ${bytes}).`;
  }
  const bad = Array.from(
    new Set(Array.from(normalized).filter((c) => !TAG_CHAR.test(c))),
  );
  if (bad.length > 0) {
    return `Can't use ${bad.map((c) => `“${c}”`).join(" ")} — tags may contain only a-z, 0-9, dot, underscore and dash.`;
  }
  return null;
}

// FavoriteStar — the per-session bookmark toggle. Purely presentational:
// the caller owns the optimistic update + POST so the same control works
// from a table cell and from the detail panel header.
//
// The remote gate is resolved HERE rather than at each call site so no consumer
// can forget it (the classification POST is Execute-class and unreachable from
// a paired device — see canClassifySessions). The star still RENDERS remotely,
// showing whether the session is favorited; it just can't be toggled.
export function FavoriteStar({
  favorite,
  onToggle,
  size = 14,
  className,
}: {
  favorite: boolean;
  onToggle: () => void;
  size?: number;
  className?: string;
}) {
  const blocked = !canClassifySessions();
  return (
    <button
      type="button"
      aria-pressed={favorite}
      aria-label={favorite ? "Remove from favorites" : "Mark as favorite"}
      disabled={blocked}
      title={
        blocked
          ? CLASSIFY_REMOTE_BLOCKED_MSG
          : favorite
            ? "Favorited — click to unstar"
            : "Mark as favorite"
      }
      onClick={(e) => {
        e.stopPropagation();
        if (blocked) return;
        onToggle();
      }}
      className={clsx(
        "inline-grid place-items-center rounded-1 p-0.5 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent-ring)]",
        favorite ? "text-warn hover:text-warn/80" : "text-fg-4 hover:text-fg-2",
        blocked && "cursor-not-allowed opacity-60 hover:text-inherit",
        className,
      )}
    >
      <svg
        width={size}
        height={size}
        viewBox="0 0 16 16"
        fill={favorite ? "currentColor" : "none"}
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
        aria-hidden
      >
        <path d="M8 1.8 10 6l4.4.5-3.3 3 .9 4.4L8 11.7 4 13.9l.9-4.4-3.3-3L6 6z" />
      </svg>
    </button>
  );
}

// TagEditor renders its own trigger button and an anchored popover. It
// portals to <body> (the Sessions table's overflow-x-auto wrapper would
// otherwise clip it) and registers the portal root with the terminal
// companion registry, exactly like the ContextMenu primitive.
export function TagEditor({
  sessionId,
  tags,
  onTagsChange,
  label,
  className,
  onError,
}: {
  sessionId: string;
  tags: string[];
  // onTagsChange receives the optimistic value, then the server's truth (or
  // the pre-mutation value on failure). The owner is the single source of
  // rendered state; this component holds no copy of it.
  onTagsChange: (next: string[]) => void;
  label?: string;
  className?: string;
  onError?: (message: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState("");
  const [vocab, setVocab] = useState<TagRollup[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Execute-class POST → unreachable from a paired remote device.
  const blocked = !canClassifySessions();
  // busyRef is the SINGLE-FLIGHT latch. `busy` state drives the disabled
  // styling, but two affordances fired in the same tick (Enter while a
  // suggestion click is already dispatching) would both read the pre-render
  // `busy === false`; the ref flips synchronously so the second one is
  // refused. Without it two concurrent mutations each capture the same stale
  // `tags` prop and the later response silently reverts the earlier edit.
  const busyRef = useRef(false);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [anchor, setAnchor] = useState<{ left: number; top: number } | null>(
    null,
  );
  const companion = useCompanionRegistry();

  // Vocabulary is fetched once PER OPEN — fresh enough that a tag added
  // from another row shows up, cheap enough that typing never refetches.
  useEffect(() => {
    if (!open) return;
    const ac = new AbortController();
    fetchTagRollup(ac.signal)
      .then((r) => setVocab(r.tags ?? []))
      .catch(() => {
        // A failed vocabulary load must not block free entry: the input
        // still works, we just show the starter suggestions instead.
        if (!ac.signal.aborted) setVocab([]);
      });
    return () => ac.abort();
  }, [open, sessionId]);

  useEffect(() => {
    if (!open) return;
    setDraft("");
    setErr(null);
    requestAnimationFrame(() => inputRef.current?.focus());
  }, [open]);

  // Register the portal root so focusing the input under the expanded
  // terminal's focusin trap reads as legitimate coexistence.
  useEffect(() => {
    const el = panelRef.current;
    if (!open || !el || !companion) return;
    return companion.register(el);
  }, [open, companion]);

  // Anchor under the trigger, clamped into the viewport.
  useLayoutEffect(() => {
    if (!open) return;
    const t = triggerRef.current?.getBoundingClientRect();
    const p = panelRef.current?.getBoundingClientRect();
    if (!t) return;
    const m = 8;
    const w = p?.width ?? 300;
    const h = p?.height ?? 280;
    let left = t.left;
    let top = t.bottom + 4;
    if (left + w > window.innerWidth - m) left = Math.max(m, window.innerWidth - m - w);
    if (top + h > window.innerHeight - m) top = Math.max(m, t.top - 4 - h);
    setAnchor({ left, top });
  }, [open, vocab, tags.length]);

  // Close on outside pointer-down / Escape / scroll, mirroring ContextMenu.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (panelRef.current?.contains(target)) return;
      if (triggerRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    const onScroll = (e: Event) => {
      if (panelRef.current?.contains(e.target as Node)) return;
      setOpen(false);
    };
    document.addEventListener("pointerdown", onDown, true);
    document.addEventListener("keydown", onKey, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onScroll);
    return () => {
      document.removeEventListener("pointerdown", onDown, true);
      document.removeEventListener("keydown", onKey, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onScroll);
    };
  }, [open]);

  const preview = normalizeTag(draft);
  // previewError is null while the box is empty (nothing to complain about
  // yet) and otherwise carries the server's own verdict, computed locally.
  const previewError = draft.trim() === "" ? null : tagInputError(preview);
  const atCap = tags.length >= MAX_TAGS_PER_SESSION;
  const duplicate = previewError === null && tags.includes(preview);

  // Suggestions: the operator's own vocabulary, minus what this session
  // already carries, filtered by whatever they're typing. Falls back to the
  // starter list only while the vocabulary is genuinely empty (§0).
  const suggestions = useMemo(() => {
    const own = (vocab ?? []).map((v) => v.tag);
    const pool = own.length > 0 ? own : vocab === null ? [] : STARTER_TAGS;
    const q = preview;
    return pool
      .filter((t) => !tags.includes(t))
      .filter((t) => (q ? t.includes(q) : true))
      .slice(0, 40);
  }, [vocab, tags, preview]);

  const usingStarters = (vocab?.length ?? 0) === 0 && vocab !== null;

  // mutate is SINGLE-FLIGHT: a second call while one is in flight is refused
  // outright rather than queued. Both the optimistic value and the revert value
  // are derived from the `tags` prop captured at call time, so two overlapping
  // mutations would compute their optimistic sets from the SAME pre-mutation
  // list and whichever response landed last would win — dropping the other
  // edit. Refusing is the simpler correct option: the UI is already fully
  // disabled for the (sub-second) duration, so the second click is a mis-click,
  // not a lost intent.
  async function mutate(add: string[], remove: string[]) {
    if (blocked) {
      setErr(CLASSIFY_REMOTE_BLOCKED_MSG);
      onError?.(CLASSIFY_REMOTE_BLOCKED_MSG);
      return;
    }
    if (busyRef.current) return;
    busyRef.current = true;
    const before = tags;
    const optimistic = [
      ...before.filter((t) => !remove.includes(t)),
      ...add.filter((t) => !before.includes(t)),
    ].sort();
    setBusy(true);
    setErr(null);
    onTagsChange(optimistic);
    try {
      const r = await postSessionTags(sessionId, { add, remove });
      onTagsChange(r.tags ?? []);
    } catch (e) {
      // Revert to the pre-mutation value — an optimistic edit that the
      // server rejected must not linger as if it stuck.
      onTagsChange(before);
      const msg = e instanceof Error ? e.message : String(e);
      setErr(msg);
      onError?.(msg);
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }

  function addDraft(value?: string) {
    if (blocked || busyRef.current) return;
    const t = normalizeTag(value ?? draft);
    // Validate against the SERVER's rules and refuse to submit rather than
    // silently sending something it will reject (or, worse, quietly reshaping
    // the operator's input into a different tag).
    const problem = tagInputError(t);
    if (problem) {
      setErr(problem);
      return;
    }
    if (tags.includes(t)) {
      setDraft("");
      return;
    }
    if (atCap) {
      setErr(`Limit is ${MAX_TAGS_PER_SESSION} tags per session.`);
      return;
    }
    setDraft("");
    void mutate([t], []);
  }

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        disabled={blocked}
        title={blocked ? CLASSIFY_REMOTE_BLOCKED_MSG : "Edit this session's tags"}
        onClick={(e) => {
          e.stopPropagation();
          if (blocked) return;
          setOpen((o) => !o);
        }}
        className={clsx(
          "inline-flex items-center gap-1 rounded-2 border px-1.5 py-0.5 text-[10.5px] transition-colors",
          open
            ? "border-accent bg-accent-soft text-accent"
            : "border-line-2 bg-bg-2 text-fg-3 hover:bg-bg-3 hover:text-fg-1",
          blocked && "cursor-not-allowed opacity-50 hover:bg-bg-2 hover:text-fg-3",
          className,
        )}
      >
        {label ?? "+ tag"}
      </button>

      {open &&
        createPortal(
          <div
            ref={panelRef}
            role="dialog"
            aria-label="Edit tags"
            onClick={(e) => e.stopPropagation()}
            style={{
              position: "fixed",
              left: anchor?.left ?? -9999,
              top: anchor?.top ?? -9999,
              zIndex: 200,
              width: 300,
              visibility: anchor ? "visible" : "hidden",
            }}
            className="overflow-hidden rounded-3 border border-line-2 bg-bg-1 shadow-drawer"
          >
            <div className="flex items-baseline justify-between gap-2 border-b border-line-1 bg-bg-2/60 px-3 py-1.5">
              <span className="text-[10px] font-semibold uppercase tracking-[0.06em] text-fg-3">
                Tags
              </span>
              <span className="font-mono text-[10px] text-fg-4">
                {tags.length}/{MAX_TAGS_PER_SESSION}
              </span>
            </div>

            {tags.length > 0 && (
              // pointer-events-none while a mutation is in flight: the remove
              // × is a mutation affordance and must be inert under the
              // single-flight rule. Neutralising the whole row (rather than
              // dropping each ×) keeps the layout stable. mutate()'s busyRef
              // latch is still the hard backstop.
              <div
                className={clsx(
                  "flex flex-wrap gap-1 border-b border-line-1 px-3 py-2",
                  busy && "pointer-events-none opacity-60",
                )}
              >
                {tags.map((t) => (
                  <TagPill
                    key={t}
                    tag={t}
                    onRemove={(tag) => void mutate([], [tag])}
                  />
                ))}
              </div>
            )}

            <div className="border-b border-line-1 px-3 py-2">
              <input
                ref={inputRef}
                type="text"
                value={draft}
                // No maxLength clamp on the raw box: the length RULE is
                // reported by tagInputError against the normalized value, so
                // over-long input produces a named error instead of a silent
                // truncation the operator never sees.
                placeholder={atCap ? "tag limit reached" : "new tag…"}
                disabled={atCap || busy}
                aria-invalid={previewError !== null}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    addDraft();
                  }
                }}
                className="h-7 w-full appearance-none rounded-2 border border-line-2 bg-bg-1 px-2 font-mono text-[11px] text-fg-1 placeholder:text-fg-4 focus:border-accent focus:outline-none disabled:opacity-50"
              />
              {draft.trim() !== "" && (
                <p className="mt-1 flex items-center gap-1 text-[10px] text-fg-3">
                  {previewError !== null ? (
                    // The server would REJECT this input, so say exactly why
                    // and submit nothing — never quietly strip or truncate it
                    // into a different tag.
                    <span className="text-danger">{previewError}</span>
                  ) : (
                    <>
                      <span>saves as</span>
                      <code className="rounded-1 border border-line-2 bg-bg-2 px-1 font-mono text-[10px] text-fg-1">
                        {preview}
                      </code>
                      {duplicate && (
                        <span className="text-fg-4">· already applied</span>
                      )}
                    </>
                  )}
                </p>
              )}
            </div>

            <div className="max-h-[220px] overflow-y-auto py-1">
              {usingStarters && (
                <p className="px-3 pb-1 pt-1 text-[10px] text-fg-4">
                  No tags yet — start with one of these:
                </p>
              )}
              {suggestions.length === 0 ? (
                <p className="px-3 py-2 text-[11px] text-fg-3">
                  {previewError !== null
                    ? "Fix the tag above before it can be created."
                    : preview
                      ? "No matching tag — press Enter to create it."
                      : "No other tags yet."}
                </p>
              ) : (
                suggestions.map((t) => {
                  const count = vocab?.find((v) => v.tag === t)?.sessions;
                  return (
                    <button
                      key={t}
                      type="button"
                      disabled={atCap || busy}
                      onClick={() => addDraft(t)}
                      className="flex w-full items-center gap-2 px-2.5 py-1 text-left transition-colors hover:bg-bg-2 disabled:opacity-40"
                    >
                      <TagPill tag={t} />
                      {count != null && (
                        <span className="ml-auto shrink-0 font-mono text-[10px] text-fg-4">
                          {count}
                        </span>
                      )}
                    </button>
                  );
                })
              )}
            </div>

            {err && (
              <p className="border-t border-line-1 px-3 py-1.5 text-[10.5px] text-danger">
                {err}
              </p>
            )}
          </div>,
          document.body,
        )}
    </>
  );
}
