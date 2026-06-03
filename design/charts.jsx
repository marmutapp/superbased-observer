/* ============================================================
   SVG chart primitives — hand-rolled, no chart lib.
   All charts use the same color tokens from tokens.css.
   ============================================================ */
const { useMemo, useState, useRef, useEffect } = React;

// ---- shared helpers ----
function nice(max) {
  if (max <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(max)));
  const n = max / p;
  if (n <= 1) return p;
  if (n <= 2) return 2 * p;
  if (n <= 5) return 5 * p;
  return 10 * p;
}
function smooth(points) {
  // simple monotone cubic-ish bezier
  if (points.length < 2) return points.map(p => `${p[0]},${p[1]}`).join(" ");
  let d = `M ${points[0][0]},${points[0][1]}`;
  for (let i = 1; i < points.length; i++) {
    const p0 = points[i-1], p1 = points[i];
    const dx = (p1[0] - p0[0]) * 0.4;
    d += ` C ${p0[0]+dx},${p0[1]} ${p1[0]-dx},${p1[1]} ${p1[0]},${p1[1]}`;
  }
  return d;
}

// ============================================================
// AreaChart — stacked or single area
// ============================================================
function AreaChart({ data, series, height = 220, gradient = true, smoothLine = true, yFmt = v => v, showLegend = true }) {
  // data: [{date, ...keys}], series: [{key, label, color}]
  const ref = useRef(null);
  const [w, setW] = useState(800);
  const [hover, setHover] = useState(null);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(es => setW(es[0].contentRect.width));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  const padL = 44, padR = 12, padT = 8, padB = 24;
  const innerW = Math.max(50, w - padL - padR);
  const innerH = height - padT - padB;
  const n = data.length;
  const xStep = n > 1 ? innerW / (n - 1) : innerW;
  // stacked sums
  const stacks = data.map(d => {
    let acc = 0;
    return series.map(s => { const v = d[s.key] || 0; const o = { y0: acc, y1: acc + v }; acc += v; return o; });
  });
  const maxY = Math.max(0.001, ...stacks.map(s => s[s.length-1].y1));
  const niceMax = nice(maxY);
  const yScale = v => padT + innerH - (v / niceMax) * innerH;
  const yTicks = [0, niceMax * 0.25, niceMax * 0.5, niceMax * 0.75, niceMax];
  const xAt = i => padL + i * xStep;

  return (
    <div ref={ref} style={{ width: "100%" }}>
      {showLegend && (
        <div className="chart-legend">
          {series.map(s => (
            <div className="item" key={s.key}>
              <span className="sw" style={{ background: s.color }}></span>
              {s.label}
            </div>
          ))}
        </div>
      )}
      <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none"
        onMouseLeave={() => setHover(null)}
      >
        <defs>
          {series.map((s, i) => (
            <linearGradient key={i} id={`grad-${s.key}-${i}`} x1="0" x2="0" y1="0" y2="1">
              <stop offset="0%" stopColor={s.color} stopOpacity="0.55" />
              <stop offset="100%" stopColor={s.color} stopOpacity="0.05" />
            </linearGradient>
          ))}
        </defs>
        {/* grid */}
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={padL} x2={w - padR} y1={yScale(t)} y2={yScale(t)} stroke="var(--line-1)" strokeDasharray={i === 0 ? "" : "2 4"} />
            <text x={padL - 6} y={yScale(t) + 3} fontSize="9" fill="var(--fg-3)" textAnchor="end" fontFamily="var(--font-mono)">{yFmt(t)}</text>
          </g>
        ))}
        {/* x labels — first, middle, last */}
        {[0, Math.floor(n/4), Math.floor(n/2), Math.floor(3*n/4), n-1].filter((v,i,a) => a.indexOf(v) === i).map((i) => (
          <text key={i} x={xAt(i)} y={height - 8} fontSize="9" fill="var(--fg-3)" textAnchor="middle" fontFamily="var(--font-mono)">
            {data[i] && data[i].date ? data[i].date.slice(5) : ""}
          </text>
        ))}
        {/* stacks — from top series to bottom for clean overlap */}
        {series.map((s, si) => {
          const pts = stacks.map((st, i) => [xAt(i), yScale(st[si].y1)]);
          const ptsBottom = stacks.map((st, i) => [xAt(i), yScale(st[si].y0)]).reverse();
          const all = [...pts, ...ptsBottom];
          const pathTop = smoothLine ? smooth(pts) : "M " + pts.map(p => p.join(",")).join(" L ");
          const pathArea = (smoothLine ? smooth(pts) : "M " + pts.map(p => p.join(",")).join(" L "))
            + " L " + ptsBottom.map(p => p.join(",")).join(" L ") + " Z";
          return (
            <g key={s.key}>
              <path d={pathArea} fill={gradient ? `url(#grad-${s.key}-${si})` : s.color} opacity={gradient ? 1 : 0.4} />
              <path d={pathTop} fill="none" stroke={s.color} strokeWidth="1.5" />
            </g>
          );
        })}
        {/* hover crosshair */}
        {hover != null && (
          <g>
            <line x1={xAt(hover)} x2={xAt(hover)} y1={padT} y2={padT+innerH} stroke="var(--line-3)" strokeDasharray="2 3" />
            <circle cx={xAt(hover)} cy={yScale(stacks[hover][stacks[hover].length-1].y1)} r="3" fill="#fff" />
          </g>
        )}
        {/* hover hitareas */}
        {data.map((d, i) => (
          <rect key={i} x={xAt(i) - xStep/2} y={padT} width={xStep} height={innerH} fill="transparent"
            onMouseEnter={() => setHover(i)} />
        ))}
      </svg>
      {hover != null && data[hover] && (
        <div style={{ fontSize: 11, color: "var(--fg-3)", marginTop: 4, fontFamily: "var(--font-mono)" }}>
          {data[hover].date} · {series.map(s => `${s.label.toLowerCase()} ${yFmt(data[hover][s.key]||0)}`).join(" · ")}
        </div>
      )}
    </div>
  );
}

// ============================================================
// StackedBar — vertical stacked bars
// ============================================================
function StackedBar({ data, series, height = 220, yFmt = v => v, showLegend = true, barGap = 2 }) {
  const ref = useRef(null);
  const [w, setW] = useState(800);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(es => setW(es[0].contentRect.width));
    ro.observe(ref.current); return () => ro.disconnect();
  }, []);
  const padL = 44, padR = 12, padT = 8, padB = 24;
  const innerW = Math.max(50, w - padL - padR);
  const innerH = height - padT - padB;
  const n = data.length;
  const barW = Math.max(1, innerW / n - barGap);
  const stacks = data.map(d => series.reduce((acc, s) => acc + (d[s.key] || 0), 0));
  const maxY = Math.max(0.001, ...stacks);
  const niceMax = nice(maxY);
  const yScale = v => padT + innerH - (v / niceMax) * innerH;
  const yTicks = [0, niceMax * 0.25, niceMax * 0.5, niceMax * 0.75, niceMax];

  return (
    <div ref={ref} style={{ width: "100%" }}>
      {showLegend && (
        <div className="chart-legend">
          {series.map(s => (
            <div className="item" key={s.key}>
              <span className="sw" style={{ background: s.color }}></span>{s.label}
            </div>
          ))}
        </div>
      )}
      <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`}>
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={padL} x2={w-padR} y1={yScale(t)} y2={yScale(t)} stroke="var(--line-1)" strokeDasharray={i===0?"":"2 4"} />
            <text x={padL-6} y={yScale(t)+3} fontSize="9" fill="var(--fg-3)" textAnchor="end" fontFamily="var(--font-mono)">{yFmt(t)}</text>
          </g>
        ))}
        {data.map((d, i) => {
          const xb = padL + i * (innerW / n) + barGap/2;
          let acc = 0;
          return (
            <g key={i}>
              {series.map((s, si) => {
                const v = d[s.key] || 0;
                const h = (v / niceMax) * innerH;
                const y = yScale(acc + v);
                acc += v;
                return <rect key={si} x={xb} y={y} width={barW} height={h} fill={s.color} rx="1" />;
              })}
            </g>
          );
        })}
        {[0, Math.floor(n/4), Math.floor(n/2), Math.floor(3*n/4), n-1].filter((v,i,a)=>a.indexOf(v)===i).map(i => (
          <text key={i} x={padL + i * (innerW/n) + barW/2 + barGap/2} y={height-8} fontSize="9" fill="var(--fg-3)" textAnchor="middle" fontFamily="var(--font-mono)">
            {data[i] && data[i].date ? data[i].date.slice(5) : ""}
          </text>
        ))}
      </svg>
    </div>
  );
}

// ============================================================
// HBar — horizontal bar (top-N)
// ============================================================
function HBarChart({ data, labelKey = "label", valueKey = "value", color = "var(--accent)", maxBars = 10, fmt = v => v }) {
  const rows = data.slice(0, maxBars);
  const max = Math.max(0.001, ...rows.map(r => r[valueKey]));
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {rows.map((r, i) => {
        const w = (r[valueKey] / max) * 100;
        const col = typeof color === "function" ? color(r, i) : color;
        return (
          <div key={i} style={{ display: "grid", gridTemplateColumns: "160px 1fr 80px", gap: 10, alignItems: "center" }}>
            <div className="path-mono" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", color: "var(--fg-1)" }}>{r[labelKey]}</div>
            <div style={{ height: 18, background: "var(--bg-3)", borderRadius: 3, overflow: "hidden", position: "relative" }}>
              <div style={{ width: `${w}%`, height: "100%", background: col, borderRadius: 3, transition: "width 0.4s var(--ease)" }}/>
            </div>
            <div className="num" style={{ textAlign: "right", fontFamily: "var(--font-mono)", fontSize: 12, color: "var(--fg-1)" }}>{fmt(r[valueKey])}</div>
          </div>
        );
      })}
    </div>
  );
}

// ============================================================
// Donut — proportion ring with center label
// ============================================================
function Donut({ data, size = 140, thickness = 18, centerLabel, centerValue, centerSub }) {
  const total = data.reduce((s, d) => s + d.value, 0) || 1;
  const cx = size/2, cy = size/2, r = (size - thickness)/2;
  const c = 2 * Math.PI * r;
  let off = 0;
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 16 }}>
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`}>
        <circle cx={cx} cy={cy} r={r} stroke="var(--bg-3)" strokeWidth={thickness} fill="none" />
        {data.map((d, i) => {
          const frac = d.value / total;
          const dash = frac * c;
          const el = <circle key={i} cx={cx} cy={cy} r={r}
            stroke={d.color} strokeWidth={thickness} fill="none"
            strokeDasharray={`${dash} ${c - dash}`}
            strokeDashoffset={-off}
            transform={`rotate(-90 ${cx} ${cy})`}
            strokeLinecap="butt"
          />;
          off += dash;
          return el;
        })}
        {(centerLabel || centerValue) && (
          <g>
            {centerValue && <text x={cx} y={cy} textAnchor="middle" dominantBaseline="middle" fontSize="20" fontWeight="700" fill="var(--fg-0)" fontFamily="var(--font-display)">{centerValue}</text>}
            {centerLabel && <text x={cx} y={cy + 14} textAnchor="middle" dominantBaseline="middle" fontSize="9" fill="var(--fg-3)" letterSpacing="0.08em">{centerLabel}</text>}
          </g>
        )}
      </svg>
      <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: 4 }}>
        {data.map((d, i) => (
          <div key={i} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12 }}>
            <span style={{ width: 8, height: 8, borderRadius: 2, background: d.color }}/>
            <span style={{ flex: 1, color: "var(--fg-1)" }}>{d.label}</span>
            <span className="num" style={{ fontFamily: "var(--font-mono)", color: "var(--fg-2)", fontSize: 11 }}>
              {((d.value/total)*100).toFixed(1)}%
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

// ============================================================
// Sparkline — inline trend
// ============================================================
function Sparkline({ data, color = "var(--accent)", w = 80, h = 28, fill = true }) {
  const max = Math.max(0.001, ...data);
  const min = Math.min(0, ...data);
  const range = max - min || 1;
  const step = data.length > 1 ? w / (data.length - 1) : w;
  const pts = data.map((v, i) => [i * step, h - ((v - min) / range) * h]);
  const line = "M " + pts.map(p => p.join(",")).join(" L ");
  const area = line + ` L ${(data.length-1)*step},${h} L 0,${h} Z`;
  return (
    <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`}>
      {fill && <path d={area} fill={color} opacity="0.15" />}
      <path d={line} stroke={color} strokeWidth="1.5" fill="none" />
    </svg>
  );
}

// ============================================================
// Heatmap — hour-of-day × day-of-week (24 × 7)
// ============================================================
function Heatmap({ data, w = 600, h = 160, color = "var(--accent)" }) {
  // data: 24-element array (hours). We'll synthesize 7 days of variance from a baseline.
  const cells = [];
  const max = Math.max(...data) || 1;
  for (let dow = 0; dow < 7; dow++) {
    for (let h = 0; h < 24; h++) {
      const base = data[h];
      // synthesize day-of-week pattern: weekends light, mid-week heavy
      const factor = [0.4, 1.1, 1.15, 1.2, 1.0, 0.85, 0.35][dow];
      cells.push({ dow, h, v: base * factor });
    }
  }
  const cw = (w - 36) / 24;
  const ch = (h - 30) / 7;
  return (
    <svg width="100%" height={h} viewBox={`0 0 ${w} ${h}`}>
      {["Mon","Tue","Wed","Thu","Fri","Sat","Sun"].map((d, i) => (
        <text key={i} x={28} y={i*ch + ch*0.65 + 14} textAnchor="end" fontSize="9" fill="var(--fg-3)" fontFamily="var(--font-mono)">{d}</text>
      ))}
      {[0,4,8,12,16,20].map(hr => (
        <text key={hr} x={36 + hr*cw + cw/2} y={h-8} textAnchor="middle" fontSize="9" fill="var(--fg-3)" fontFamily="var(--font-mono)">
          {String(hr).padStart(2,"0")}
        </text>
      ))}
      {cells.map((c, i) => {
        const o = Math.max(0.04, Math.min(1, c.v / max));
        return <rect key={i} x={36 + c.h*cw + 1} y={c.dow*ch + 14 + 1} width={cw - 2} height={ch - 2} fill={color} opacity={o} rx="2" />;
      })}
    </svg>
  );
}

// ============================================================
// Gauge — half-ring with a value 0-1
// ============================================================
function Gauge({ value, label, size = 120, color = "var(--success)" }) {
  const cx = size/2, cy = size/2, r = (size-18)/2;
  const c = Math.PI * r; // half
  const off = c * (1 - value);
  return (
    <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 4 }}>
      <svg width={size} height={size/2 + 14} viewBox={`0 0 ${size} ${size/2+14}`}>
        <path d={`M ${cx-r} ${cy} A ${r} ${r} 0 0 1 ${cx+r} ${cy}`} stroke="var(--bg-4)" strokeWidth="10" fill="none" strokeLinecap="round" />
        <path d={`M ${cx-r} ${cy} A ${r} ${r} 0 0 1 ${cx+r} ${cy}`} stroke={color} strokeWidth="10" fill="none" strokeLinecap="round"
          strokeDasharray={c} strokeDashoffset={off} />
        <text x={cx} y={cy-2} textAnchor="middle" fontSize="22" fontWeight="700" fill="var(--fg-0)" fontFamily="var(--font-display)">
          {(value*100).toFixed(1)}%
        </text>
      </svg>
      <div style={{ fontSize: 10, textTransform: "uppercase", letterSpacing: "0.08em", color: "var(--fg-3)", fontWeight: 600 }}>{label}</div>
    </div>
  );
}

// ============================================================
// HBarLine — single horizontal bar line (inline in tables)
// ============================================================
function InlineBar({ value, max, color = "var(--accent)", width = 50 }) {
  const w = Math.min(100, (value/max) * 100);
  return (
    <span style={{ display: "inline-block", width, height: 4, background: "var(--bg-4)", borderRadius: 2, verticalAlign: "middle", overflow: "hidden" }}>
      <span style={{ display: "block", width: `${w}%`, height: "100%", background: color }}/>
    </span>
  );
}

// ============================================================
// VerticalBars — simple bar chart (no stack)
// ============================================================
function BarChart({ data, valueKey = "value", labelKey = "date", height = 200, color = "var(--accent)", yFmt = v => v }) {
  const ref = useRef(null);
  const [w, setW] = useState(800);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(es => setW(es[0].contentRect.width));
    ro.observe(ref.current); return () => ro.disconnect();
  }, []);
  const padL = 44, padR = 12, padT = 8, padB = 24;
  const innerW = Math.max(50, w - padL - padR);
  const innerH = height - padT - padB;
  const n = data.length;
  const slot = innerW / n;
  const barW = Math.max(2, slot - 4);
  const max = Math.max(0.001, ...data.map(d => d[valueKey]));
  const niceMax = nice(max);
  const yScale = v => padT + innerH - (v / niceMax) * innerH;
  const yTicks = [0, niceMax * 0.5, niceMax];
  return (
    <div ref={ref} style={{ width: "100%" }}>
      <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`}>
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={padL} x2={w-padR} y1={yScale(t)} y2={yScale(t)} stroke="var(--line-1)" strokeDasharray={i===0?"":"2 4"} />
            <text x={padL-6} y={yScale(t)+3} fontSize="9" fill="var(--fg-3)" textAnchor="end" fontFamily="var(--font-mono)">{yFmt(t)}</text>
          </g>
        ))}
        {data.map((d, i) => {
          const v = d[valueKey];
          const h = (v / niceMax) * innerH;
          return <rect key={i} x={padL + i*slot + 2} y={yScale(v)} width={barW} height={h} fill={color} rx="2" opacity="0.85" />;
        })}
      </svg>
    </div>
  );
}

// ============================================================
// LineChart — pure line
// ============================================================
function LineChart({ data, valueKey = "value", height = 220, color = "var(--success)", yFmt = v => v }) {
  const ref = useRef(null);
  const [w, setW] = useState(800);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(es => setW(es[0].contentRect.width));
    ro.observe(ref.current); return () => ro.disconnect();
  }, []);
  const padL = 50, padR = 12, padT = 8, padB = 24;
  const innerW = Math.max(50, w - padL - padR);
  const innerH = height - padT - padB;
  const n = data.length;
  const xStep = n > 1 ? innerW / (n - 1) : innerW;
  const max = Math.max(0.001, ...data.map(d => d[valueKey]));
  const niceMax = nice(max);
  const yScale = v => padT + innerH - (v / niceMax) * innerH;
  const pts = data.map((d, i) => [padL + i*xStep, yScale(d[valueKey])]);
  const path = smooth(pts);
  const area = path + ` L ${padL + (n-1)*xStep},${padT+innerH} L ${padL},${padT+innerH} Z`;
  const yTicks = [0, niceMax*0.25, niceMax*0.5, niceMax*0.75, niceMax];
  return (
    <div ref={ref}>
      <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`}>
        <defs>
          <linearGradient id="line-grad" x1="0" x2="0" y1="0" y2="1">
            <stop offset="0%" stopColor={color} stopOpacity="0.3" />
            <stop offset="100%" stopColor={color} stopOpacity="0.02" />
          </linearGradient>
        </defs>
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={padL} x2={w-padR} y1={yScale(t)} y2={yScale(t)} stroke="var(--line-1)" strokeDasharray={i===0?"":"2 4"} />
            <text x={padL-6} y={yScale(t)+3} fontSize="9" fill="var(--fg-3)" textAnchor="end" fontFamily="var(--font-mono)">{yFmt(t)}</text>
          </g>
        ))}
        <path d={area} fill="url(#line-grad)" />
        <path d={path} stroke={color} strokeWidth="2" fill="none" />
        {pts.map((p, i) => <circle key={i} cx={p[0]} cy={p[1]} r="2.5" fill={color} stroke="var(--bg-2)" strokeWidth="1.5" />)}
        {[0, Math.floor(n/2), n-1].map(i => data[i] && (
          <text key={i} x={pts[i][0]} y={height-8} fontSize="9" fill="var(--fg-3)" textAnchor="middle" fontFamily="var(--font-mono)">
            {data[i].date ? data[i].date.slice(5) : i}
          </text>
        ))}
      </svg>
    </div>
  );
}

Object.assign(window, { AreaChart, StackedBar, HBarChart, Donut, Sparkline, Heatmap, Gauge, InlineBar, BarChart, LineChart });
