// Display formatters shared across the org dashboard.

export function usd(n: number): string {
  if (n >= 1000) return `$${n.toLocaleString("en-US", { maximumFractionDigits: 0 })}`;
  if (n >= 1) return `$${n.toFixed(2)}`;
  return `$${n.toFixed(4)}`;
}

export function num(n: number): string {
  return n.toLocaleString("en-US");
}

export function pct(ratio: number): string {
  return `${(ratio * 100).toFixed(0)}%`;
}

// pct1 is a one-decimal percentage (for deltas / cache hit rates).
export function pct1(ratio: number): string {
  return `${(ratio * 100).toFixed(1)}%`;
}

// compact abbreviates large counts: 1234 → 1.2k, 3_400_000 → 3.4M.
export function compact(n: number): string {
  const abs = Math.abs(n);
  if (abs >= 1e9) return `${(n / 1e9).toFixed(1)}B`;
  if (abs >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (abs >= 1e3) return `${(n / 1e3).toFixed(1)}k`;
  return String(n);
}

// ms formats a millisecond duration: 850 → 850ms, 2400 → 2.4s.
export function ms(n: number): string {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}s`;
  return `${Math.round(n)}ms`;
}

export function shortDate(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

export function dateTime(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// projectName trims a project_root to its last path segment for compact labels.
export function projectName(root: string): string {
  const parts = root.replace(/\/+$/, "").split("/");
  return parts[parts.length - 1] || root;
}
