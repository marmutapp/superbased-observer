// Pure formatters used across the sidebar trees.
//
// Lives in its own module so the unit suite can exercise them
// without a vscode runtime shim.

export function formatUSD(n: number): string {
  if (!Number.isFinite(n)) return '$—';
  if (Math.abs(n) >= 1000) {
    return `$${n.toLocaleString('en-US', { maximumFractionDigits: 0 })}`;
  }
  return `$${n.toFixed(2)}`;
}

export function formatPct(n: number): string {
  if (!Number.isFinite(n)) return '—';
  const sign = n >= 0 ? '+' : '';
  return `${sign}${n.toFixed(1)}%`;
}

export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n === 0) return '0';
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  const remM = m % 60;
  return remM > 0 ? `${h}h${remM}m` : `${h}h`;
}

export function basename(p: string): string {
  if (!p) return p;
  const cleaned = p.replace(/[\\/]+$/, '');
  const idx = Math.max(cleaned.lastIndexOf('/'), cleaned.lastIndexOf('\\'));
  return idx >= 0 ? cleaned.slice(idx + 1) : cleaned;
}
