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
