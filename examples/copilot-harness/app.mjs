// Acme Copilot — a minimal "employee copilot" harness for demonstrating
// SuperBased Observer's Plane-A governance (admission + egress routing + trace
// capture) end to end.
//
// What it does on every employee message:
//   1. ADMISSION  — POST the message to Observer's admission gate
//                   (/api/obs/admission/check). A local Ollama judge adjudicates
//                   the natural-language criteria; deterministic layers handle
//                   topics/jailbreak/prefilter. Large messages are chunked and
//                   reduced strictest-wins.
//   2. ANSWER     — if admitted, call the model THROUGH the Observer proxy
//                   (:8820 /up/ollama/v1) so the turn is captured in api_turns
//                   (exact tokens + cost) and any egress routing rule can apply.
//                   The end-user id rides the X-Superbased-User header for the
//                   per-user budget gate.
//   3. TRACE      — (optional, ENABLE_OTEL=1) emit an OpenTelemetry LLM span to
//                   Observer's OTLP receiver (:4318) so the request shows up in
//                   the web2 Trajectories view.
//
// Core path has ZERO npm dependencies — `node app.mjs` just works on Node 18+.
// Tracing is opt-in and uses @opentelemetry/* (see package.json); if it isn't
// installed the app logs one line and runs without it.

import http from "node:http";

// ---------------------------------------------------------------------------
// Config (all overridable by environment)
// ---------------------------------------------------------------------------
const CFG = {
  port: intEnv("PORT", 8090),
  // Observer node admission API (rides the dashboard listener on :8081).
  admissionEndpoint: env("ADMISSION_ENDPOINT", "http://127.0.0.1:8081/api/obs/admission/check"),
  // OpenAI-compatible base URL. Point it at the Observer proxy's /up/ollama/
  // prefix so answers are captured in api_turns AND egress routing can apply.
  // Set PROXY_BASE=http://127.0.0.1:11434/v1 to bypass the proxy (no capture).
  proxyBase: env("PROXY_BASE", "http://127.0.0.1:8820/up/ollama/v1"),
  answerModel: env("ANSWER_MODEL", "qwen2.5:1.5b-instruct"), // set to your `ollama list` tag
  // Admission chunking (mirrors the reference deployment).
  chunkBytes: intEnv("CHUNK_BYTES", 3500),
  chunkOverlap: intEnv("CHUNK_OVERLAP", 200),
  answerTruncate: intEnv("ANSWER_TRUNCATE", 3000),
  admitTimeoutMs: intEnv("ADMIT_TIMEOUT_MS", 45000), // CPU Ollama judge is slow
  answerTimeoutMs: intEnv("ANSWER_TIMEOUT_MS", 120000),
  enableOtel: boolEnv("ENABLE_OTEL", false),
  otlpEndpoint: env("SUPERBASED_OTLP_ENDPOINT", "http://127.0.0.1:4318/v1/traces"),
  serviceName: env("SERVICE_NAME", "acme-copilot"),
};

function env(k, d) { return process.env[k] ?? d; }
function intEnv(k, d) { const v = parseInt(process.env[k] ?? "", 10); return Number.isFinite(v) ? v : d; }
function boolEnv(k, d) { const v = process.env[k]; if (v == null) return d; return !["0", "false", "no", "off", ""].includes(v.trim().toLowerCase()); }

// ---------------------------------------------------------------------------
// Optional OpenTelemetry tracing (opt-in; degrades gracefully)
// ---------------------------------------------------------------------------
let otel = null; // { tracer, SpanStatusCode }
if (CFG.enableOtel) {
  try {
    const api = await import("@opentelemetry/api");
    const { OTLPTraceExporter } = await import("@opentelemetry/exporter-trace-otlp-http");
    const { Resource } = await import("@opentelemetry/resources");
    const { BatchSpanProcessor, NodeTracerProvider } = await import("@opentelemetry/sdk-trace-node");
    const provider = new NodeTracerProvider({
      resource: new Resource({ "service.name": CFG.serviceName, "sbo.sdk": "acme-copilot-harness" }),
    });
    provider.addSpanProcessor(new BatchSpanProcessor(new OTLPTraceExporter({ url: CFG.otlpEndpoint })));
    provider.register();
    otel = { tracer: api.trace.getTracer("acme-copilot"), api, provider };
    log("otel", `tracing ON → ${CFG.otlpEndpoint}`);
  } catch (e) {
    log("otel", `tracing requested but @opentelemetry/* not installed (${e.code || e.message}); continuing without traces. Run \`npm install\` to enable.`);
  }
}

// ---------------------------------------------------------------------------
// Admission
// ---------------------------------------------------------------------------
const RANK = { deny: 3, ask: 2, flag: 1, allow: 0 };

// Split on whitespace boundaries into ~chunkBytes windows with overlap.
function chunk(text) {
  if (Buffer.byteLength(text, "utf8") <= CFG.chunkBytes) return [text];
  const out = [];
  let i = 0;
  while (i < text.length) {
    let end = Math.min(text.length, i + CFG.chunkBytes);
    if (end < text.length) {
      const nl = text.lastIndexOf(" ", end);
      if (nl > i + CFG.chunkBytes / 2) end = nl;
    }
    out.push(text.slice(i, end));
    if (end >= text.length) break;
    i = Math.max(end - CFG.chunkOverlap, i + 1);
  }
  return out;
}

async function admitOne(message, { user, session, requestId }) {
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), CFG.admitTimeoutMs);
  try {
    const resp = await fetch(CFG.admissionEndpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: message, user: user || "", session: session || "", request_id: requestId || "" }),
      signal: ctrl.signal,
    });
    if (!resp.ok) return failOpen(`admission ${resp.status}`);
    const d = await resp.json();
    return {
      allowed: d.allowed !== false,
      decision: d.decision || "allow",
      severity: d.severity || "",
      criterion: d.criterion || "",
      reason: d.reason || "",
      mode: d.mode || "",
      enforceDecision: d.enforce_decision || "",
      degraded: d.degraded || "",
      latencyMs: d.latency_ms || 0,
    };
  } catch (e) {
    // Fail OPEN — a down/unreachable Observer never blocks the app.
    return failOpen(e.name === "AbortError" ? "admission timeout" : (e.code || e.message));
  } finally { clearTimeout(t); }
}

function failOpen(why) {
  return { allowed: true, decision: "allow", severity: "", criterion: "", reason: "", mode: "", enforceDecision: "", degraded: `client-failopen:${why}` };
}

// Admit a (possibly large) message chunk-by-chunk, strictest verdict wins.
async function admit(message, ctx) {
  const parts = chunk(message);
  let worst = null;
  for (let i = 0; i < parts.length; i++) {
    const v = await admitOne(parts[i], { ...ctx, requestId: `${ctx.requestId}-c${i}` });
    v.chunkIndex = i;
    v.chunkCount = parts.length;
    if (!worst || RANK[v.decision] > RANK[worst.decision]) worst = v;
    // Short-circuit: a deny can't be beaten.
    if (v.decision === "deny") break;
  }
  return worst;
}

// ---------------------------------------------------------------------------
// Answer (through the Observer proxy → Ollama)
// ---------------------------------------------------------------------------
async function answer(message, { user }) {
  const prompt = message.length > CFG.answerTruncate
    ? message.slice(0, CFG.answerTruncate) + "\n\n[message truncated for answering; full content was policy-checked]"
    : message;

  const body = {
    model: CFG.answerModel,
    messages: [
      { role: "system", content: "You are Acme Copilot, an internal assistant for Acme Cloud employees. Be concise and helpful." },
      { role: "user", content: prompt },
    ],
    stream: false,
  };

  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), CFG.answerTimeoutMs);
  const started = Date.now();
  try {
    const resp = await fetch(`${CFG.proxyBase}/chat/completions`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Superbased-User": user || "anonymous", // per-user budget attribution
      },
      body: JSON.stringify(body),
      signal: ctrl.signal,
    });
    const text = await resp.text();
    if (!resp.ok) return { ok: false, error: `answer upstream ${resp.status}: ${text.slice(0, 500)}` };
    const d = JSON.parse(text);
    return {
      ok: true,
      content: d.choices?.[0]?.message?.content ?? "(no content)",
      responseId: d.id || "",
      usage: d.usage || {},
      latencyMs: Date.now() - started,
      prompt,
    };
  } catch (e) {
    return { ok: false, error: e.name === "AbortError" ? "answer timeout" : (e.code || e.message) };
  } finally { clearTimeout(t); }
}

// Emit one LLM span so the turn shows up in Trajectories (dedups into the
// proxy's api_turn by response id).
function emitSpan({ user, session, model, prompt, response, usage, responseId }) {
  if (!otel) return;
  const span = otel.tracer.startSpan("chat", {
    attributes: {
      "openinference.span.kind": "LLM",
      "gen_ai.request.model": model,
      "llm.model_name": model,
      "gen_ai.system": "ollama",
      "llm.provider": "ollama",
      "session.id": session || "",
      "sbo.user": user || "",
      "enduser.id": user || "",
      "gen_ai.usage.input_tokens": usage.prompt_tokens || 0,
      "gen_ai.usage.output_tokens": usage.completion_tokens || 0,
      "gen_ai.response.id": responseId || "",
      "input.value": (prompt || "").slice(0, 8000),
      "output.value": (response || "").slice(0, 8000),
    },
  });
  span.setStatus({ code: otel.api.SpanStatusCode.OK });
  span.end();
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------
let reqCounter = 0;

const server = http.createServer(async (req, res) => {
  try {
    if (req.method === "GET" && (req.url === "/" || req.url === "/index.html")) {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(PAGE);
      return;
    }
    if (req.method === "GET" && req.url === "/config") {
      return json(res, 200, {
        answerModel: CFG.answerModel,
        proxyBase: CFG.proxyBase,
        admissionEndpoint: CFG.admissionEndpoint,
        otel: !!otel,
      });
    }
    if (req.method === "POST" && req.url === "/chat") {
      const bodyText = await readBody(req);
      const { message, user, session } = JSON.parse(bodyText || "{}");
      if (!message || !message.trim()) return json(res, 400, { error: "empty message" });
      const requestId = `req-${Date.now()}-${++reqCounter}`;

      // 1) Admission
      const verdict = await admit(message, { user, session, requestId });

      // In enforce mode a real block sets allowed=false. In observe mode the
      // app still sees allowed=true, but enforceDecision previews the block.
      const blocked = verdict.allowed === false;
      if (blocked) {
        return json(res, 200, {
          blocked: true,
          verdict,
          answer: userFacingBlock(verdict),
        });
      }

      // 2) Answer through the proxy
      const a = await answer(message, { user });
      if (!a.ok) return json(res, 200, { blocked: false, verdict, answer: `⚠️ ${a.error}`, error: a.error });

      // 3) Trace
      emitSpan({
        user, session, model: CFG.answerModel, prompt: a.prompt, response: a.content,
        usage: a.usage, responseId: a.responseId,
      });

      return json(res, 200, {
        blocked: false,
        verdict,
        answer: a.content,
        usage: a.usage,
        latencyMs: a.latencyMs,
      });
    }
    res.writeHead(404); res.end("not found");
  } catch (e) {
    json(res, 500, { error: String(e && e.message || e) });
  }
});

function userFacingBlock(v) {
  const base = v.reason || "This request was blocked by the usage policy.";
  if (v.decision === "ask") return `🟡 I need a bit more detail to help within policy. ${base}`;
  return `⛔ ${base}`;
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let b = ""; req.on("data", (c) => (b += c)); req.on("end", () => resolve(b)); req.on("error", reject);
  });
}
function json(res, code, obj) { res.writeHead(code, { "Content-Type": "application/json" }); res.end(JSON.stringify(obj)); }
function log(tag, msg) { console.log(`[${new Date().toISOString()}] [${tag}] ${msg}`); }

server.listen(CFG.port, "127.0.0.1", () => {
  log("boot", `Acme Copilot on http://127.0.0.1:${CFG.port}`);
  log("boot", `admission → ${CFG.admissionEndpoint}`);
  log("boot", `answers   → ${CFG.proxyBase}  (model: ${CFG.answerModel})`);
  log("boot", `otel      → ${otel ? CFG.otlpEndpoint : "off (set ENABLE_OTEL=1 + npm install to enable)"}`);
});

// ---------------------------------------------------------------------------
// Minimal single-page UI
// ---------------------------------------------------------------------------
const PAGE = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Acme Copilot</title><style>
:root{color-scheme:light dark}
*{box-sizing:border-box}body{font:15px/1.5 system-ui,sans-serif;margin:0;background:#0e1116;color:#e6e9ef}
.wrap{max-width:760px;margin:0 auto;padding:16px;display:flex;flex-direction:column;height:100vh}
h1{font-size:17px;margin:4px 0 12px}h1 small{color:#8b95a5;font-weight:400}
#log{flex:1;overflow:auto;display:flex;flex-direction:column;gap:10px;padding-bottom:8px}
.msg{padding:10px 12px;border-radius:10px;max-width:85%;white-space:pre-wrap;word-wrap:break-word}
.me{align-self:flex-end;background:#1f6feb;color:#fff}
.bot{align-self:flex-start;background:#1b2029;border:1px solid #2a313c}
.badge{display:inline-block;font-size:11px;padding:2px 7px;border-radius:20px;margin-bottom:6px;font-weight:600}
.allow{background:#1a3d2b;color:#5ce08a}.flag{background:#4a3a12;color:#f0c860}
.ask{background:#4a3a12;color:#ffd166}.deny{background:#4a1e1e;color:#ff8080}
.meta{font-size:11px;color:#8b95a5;margin-top:6px}
form{display:flex;gap:8px;padding-top:10px;border-top:1px solid #2a313c}
select,input,button{font:inherit;padding:9px 11px;border-radius:8px;border:1px solid #2a313c;background:#161b22;color:#e6e9ef}
input{flex:1}button{background:#238636;border-color:#238636;color:#fff;cursor:pointer;font-weight:600}
button:disabled{opacity:.5}
</style></head><body><div class="wrap">
<h1>🛡️ Acme Copilot <small id="cfg"></small></h1>
<div id="log"></div>
<form id="f">
<select id="user" title="end-user id (per-user budget)"><option>alice</option><option>bob</option><option>carol</option></select>
<input id="m" placeholder="Ask Acme Copilot… (try a poem, or 'competitor pricing')" autocomplete="off">
<button id="send">Send</button>
</form></div>
<script>
const log=document.getElementById('log'),f=document.getElementById('f'),m=document.getElementById('m'),send=document.getElementById('send'),userSel=document.getElementById('user');
const session='s-'+Math.random().toString(36).slice(2,8);
fetch('/config').then(r=>r.json()).then(c=>{document.getElementById('cfg').textContent='· model: '+c.answerModel+(c.otel?' · traces on':'');});
function add(cls,html){const d=document.createElement('div');d.className='msg '+cls;d.innerHTML=html;log.appendChild(d);log.scrollTop=log.scrollHeight;return d;}
function esc(s){return (s||'').replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]));}
f.addEventListener('submit',async e=>{
  e.preventDefault();const text=m.value.trim();if(!text)return;
  add('me',esc(text));m.value='';send.disabled=true;
  const thinking=add('bot','<span class="meta">…checking policy & answering…</span>');
  try{
    const r=await fetch('/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:text,user:userSel.value,session})});
    const d=await r.json();const v=d.verdict||{};
    const dec=(v.decision||'allow');
    let badge='<span class="badge '+dec+'">'+dec.toUpperCase()+(v.criterion?(' · '+esc(v.criterion)):'')+'</span><br>';
    let meta='';
    if(v.mode) meta+='mode='+esc(v.mode);
    if(v.enforceDecision && v.enforceDecision!==dec) meta+=' · enforce would: '+esc(v.enforceDecision);
    if(d.usage&&d.usage.total_tokens) meta+=' · '+d.usage.total_tokens+' tok';
    if(d.latencyMs) meta+=' · '+d.latencyMs+'ms';
    if(v.degraded) meta+=' · '+esc(v.degraded);
    thinking.innerHTML=badge+esc(d.answer)+(meta?('<div class="meta">'+meta+'</div>'):'');
  }catch(err){thinking.innerHTML='<span class="badge deny">ERROR</span><br>'+esc(String(err));}
  send.disabled=false;m.focus();
});
</script></body></html>`;
