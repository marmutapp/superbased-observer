// MIRROR of web/src/components/charts/common.ts — shared Recharts axis/grid
// styling so every chart in the org dashboard matches the primary one.

export const CHART_AXIS = {
  stroke: "var(--line-3)",
  tick: { fill: "var(--fg-3)", fontSize: 10 },
  tickLine: false,
  axisLine: false,
} as const;

export const CHART_GRID = {
  stroke: "var(--line-1)",
  vertical: false,
} as const;
