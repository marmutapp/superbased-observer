// Pure formatting helpers — no React, no DOM. Reused across cards,
// tables, tooltips. Locale is locked to en-US for now since the
// current dashboard is en-US only.

const compactFmt = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

const intFmt = new Intl.NumberFormat("en-US");

const currencyFmt = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 2,
});

const currencyPreciseFmt = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 4,
  maximumFractionDigits: 4,
});

export function fmtInt(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return intFmt.format(Math.round(n));
}

export function fmtCompact(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return compactFmt.format(n);
}

export function fmtUSD(n: number | null | undefined, precise = false): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return (precise ? currencyPreciseFmt : currencyFmt).format(n);
}

export function fmtPct(
  n: number | null | undefined,
  digits = 1,
  fromFraction = true,
): string {
  if (n == null || !Number.isFinite(n)) return "—";
  const v = fromFraction ? n * 100 : n;
  return `${v.toFixed(digits)}%`;
}

export function fmtBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export function fmtDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = s / 60;
  if (m < 60) return `${m.toFixed(1)}m`;
  const h = m / 60;
  return `${h.toFixed(1)}h`;
}

// fmtClock renders an RFC3339/ISO timestamp as a compact local wall-clock
// time for table cells — e.g. "Jun 25, 14:32:10" (24h, locale month). Used
// where a row needs the ACTUAL capture time, not just elapsed/duration.
// Returns "—" for empty/unparseable input.
export function fmtClock(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}
