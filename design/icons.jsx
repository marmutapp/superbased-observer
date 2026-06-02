/* ============================================================
   Icon set — minimal, 1.5px strokes, currentColor, 16px viewBox.
   Lucide-style geometry, hand-drawn to avoid runtime dep.
   ============================================================ */
const I = (path, viewBox = "0 0 24 24") => ({ d: path, vb: viewBox });

const ICONS = {
  // navigation
  overview:    "M3 13h8V3H3v10zm0 8h8v-6H3v6zm10 0h8V11h-8v10zm0-18v6h8V3h-8z",
  sessions:    "M4 4h16v4H4zm0 6h16v4H4zm0 6h10v4H4z",
  actions:     "M3 6h18M3 12h18M3 18h12",
  cost:        "M12 1v22M17 5H9.5a3.5 3.5 0 000 7h5a3.5 3.5 0 010 7H6",
  analysis:    "M3 3v18h18M7 14l4-4 4 4 6-6",
  tools:       "M14.7 6.3a4 4 0 005.4 5.4l-9.4 9.4-5.4-5.4 9.4-9.4zM4 20l3-3",
  compression: "M9 3v18M15 3v18M3 9h18M3 15h18",
  discovery:   "M11 11a8 8 0 1 0 0-0.01M21 21l-4.35-4.35",
  patterns:    "M4 5l4 14M16 5l4 14M10 12h4",
  settings:    "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zm9-3a9 9 0 0 1-.1 1.3l2 1.6-2 3.4-2.3-1a9 9 0 0 1-2.3 1.3l-.4 2.4h-4l-.4-2.4a9 9 0 0 1-2.3-1.3l-2.3 1-2-3.4 2-1.6A9 9 0 0 1 3 12a9 9 0 0 1 .1-1.3l-2-1.6 2-3.4 2.3 1A9 9 0 0 1 7.7 5.4L8.1 3h4l.4 2.4a9 9 0 0 1 2.3 1.3l2.3-1 2 3.4-2 1.6A9 9 0 0 1 21 12z",

  // generic ui
  search:    "M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.35-4.35",
  filter:    "M3 4h18l-7 8v6l-4 2v-8L3 4z",
  refresh:   "M3 12a9 9 0 0 1 15-6.7L21 8M21 3v5h-5M21 12a9 9 0 0 1-15 6.7L3 16M3 21v-5h5",
  download:  "M12 3v12m-5-5l5 5 5-5M4 21h16",
  export:    "M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5-5 5 5M12 5v12",
  help:      "M10 9a2 2 0 1 1 4 .5c0 1-1 1.5-2 2.5M12 16v.01M12 22a10 10 0 1 1 0-20 10 10 0 0 1 0 20z",
  close:     "M6 6l12 12M18 6L6 18",
  chevron_r: "M9 6l6 6-6 6",
  chevron_l: "M15 6l-6 6 6 6",
  chevron_d: "M6 9l6 6 6-6",
  chevron_u: "M6 15l6-6 6 6",
  external: "M14 3h7v7M21 3l-9 9M19 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h6",
  copy:     "M9 3h11a1 1 0 0 1 1 1v11M5 7h11a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1z",
  warn:     "M12 2L2 22h20L12 2zm0 8v5m0 3v.01",
  check:    "M5 12l5 5L20 7",
  x:        "M6 6l12 12M18 6L6 18",
  info:     "M12 16v-4M12 8v.01M12 22a10 10 0 1 1 0-20 10 10 0 0 1 0 20z",
  spark:    "M12 2l2.4 7.4H22l-6.2 4.5 2.4 7.4L12 17l-6.2 4.3 2.4-7.4L2 9.4h7.6L12 2z",
  bolt:     "M13 2L3 14h8l-1 8 10-12h-8l1-8z",
  pulse:    "M3 12h4l3-9 4 18 3-9h4",
  layers:   "M12 2l9 5-9 5-9-5 9-5zM3 12l9 5 9-5M3 17l9 5 9-5",
  cube:     "M12 2l9 5v10l-9 5-9-5V7l9-5zM12 12L3 7M12 12l9-5M12 12v10",
  shield:   "M12 2l8 4v6c0 5-4 9-8 10-4-1-8-5-8-10V6l8-4z",
  zap:      "M13 2L3 14h8l-1 8 10-12h-8l1-8z",
  flame:    "M12 2s4 4 4 8-4 4-4 8c0 0-6-2-6-8 0-3 3-5 3-5s0 2 1 3c1-3 2-6 2-6z",
  ai:       "M12 3a9 9 0 1 0 .01 0M12 7v10M7 12h10M9 9l6 6M15 9l-6 6",
  eye:      "M2 12s4-8 10-8 10 8 10 8-4 8-10 8S2 12 2 12zm10 3a3 3 0 1 0 0-6 3 3 0 0 0 0 6z",
  lightbulb:"M9 21h6M10 17h4M12 3a6 6 0 0 0-4 10c1 1 1 2 1 3v1h6v-1c0-1 0-2 1-3a6 6 0 0 0-4-10z",
  flag:     "M4 21V4l8 3 8-3v11l-8 3-8-3z",
  trend_up: "M3 17l6-6 4 4 7-8M14 7h7v7",
  trend_dn: "M3 7l6 6 4-4 7 8M14 17h7v-7",
  database: "M12 2c5 0 9 1.5 9 3.5S17 9 12 9 3 7.5 3 5.5 7 2 12 2zm0 7c-5 0-9-1.5-9-3.5V12c0 2 4 3.5 9 3.5s9-1.5 9-3.5V5.5c0 2-4 3.5-9 3.5zm0 6c-5 0-9-1.5-9-3.5v6c0 2 4 3.5 9 3.5s9-1.5 9-3.5v-6c0 2-4 3.5-9 3.5z",
  folder:   "M3 6a2 2 0 0 1 2-2h4l2 3h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6z",
  calendar: "M3 9h18M5 4h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2zM8 2v4M16 2v4",
  clock:    "M12 7v5l3 2M12 22a10 10 0 1 1 0-20 10 10 0 0 1 0 20z",
  user:     "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21a8 8 0 0 1 16 0",
  cpu:      "M5 5h14v14H5zM9 9h6v6H9zM2 9h3M2 15h3M19 9h3M19 15h3M9 2v3M15 2v3M9 19v3M15 19v3",
  link:     "M10 14l4-4M8 6h-2a4 4 0 0 0 0 8h2M16 18h2a4 4 0 0 0 0-8h-2",
  terminal: "M4 17l5-5-5-5M12 19h8",
  arrow_r:  "M5 12h14M13 5l7 7-7 7",
  arrow_l:  "M19 12H5M11 5l-7 7 7 7",
  plus:     "M12 5v14M5 12h14",
  minus:    "M5 12h14",
  pin:      "M12 2v9M5 11l7 11 7-11",
  play:     "M5 3l14 9-14 9V3z",
  pause:    "M6 4h4v16H6zM14 4h4v16h-4z",
  arrow_up_right: "M7 17l10-10M7 7h10v10",
  dots_v:   "M12 4v.01M12 12v.01M12 20v.01",
  dots_h:   "M4 12h.01M12 12h.01M20 12h.01",
  command:  "M6 6h12v12H6z M3 6h3v12H3zM18 6h3v12h-3z",
  network:  "M5 5a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM19 5a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM12 13a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM7 8l4 6M17 8l-4 6",
  shrink:   "M9 9H4V4M15 9h5V4M9 15H4v5M15 15h5v5",
  expand:   "M4 8V4h4M20 8V4h-4M4 16v4h4M20 16v4h-4",
  list:     "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  scale:    "M12 2l8 4-8 4-8-4 8-4zM4 6v6l8 4 8-4V6M4 12v6l8 4 8-4v-6",
  alert:    "M12 9v4M12 17v.01M10.3 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z",
  spark2:   "M12 2v7m0 6v7M2 12h7m6 0h7M4.93 4.93l4.95 4.95m4.24 4.24l4.95 4.95M4.93 19.07l4.95-4.95m4.24-4.24l4.95-4.95",
  beaker:   "M9 3h6v6l5 10a2 2 0 01-1.7 3H5.7A2 2 0 014 19l5-10V3z",
  sun:      "M12 4V2M12 22v-2M4 12H2M22 12h-2M5.6 5.6L4.2 4.2M19.8 19.8l-1.4-1.4M5.6 18.4l-1.4 1.4M19.8 4.2l-1.4 1.4M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10z",
  moon:     "M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z",
  monitor:  "M3 5h18v12H3zM8 21h8M12 17v4",
};

function Icon({ name, size = 14, stroke = 1.5, className = "", style = {} }) {
  const p = ICONS[name];
  if (!p) return null;
  return (
    <svg
      width={size} height={size} viewBox="0 0 24 24"
      fill="none" stroke="currentColor"
      strokeWidth={stroke} strokeLinecap="round" strokeLinejoin="round"
      className={className} style={style}
    >
      <path d={p} />
    </svg>
  );
}

window.Icon = Icon;
window.ICONS = ICONS;
