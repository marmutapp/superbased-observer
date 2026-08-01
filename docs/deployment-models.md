# Deployment models — the two planes

SuperBased serves **two distinct deployment models**. They share one
binary and a lot of plumbing, but they answer different questions for different
people. Pick the one that matches your goal; you can run either, both, or
neither.

> **TL;DR.** *Plane B* is about **your own coding assistant** and (optionally)
> rolling that up to your team/org. *Plane A* is about **an LLM application you
> operate for end-users**, observed and guarded at the admin level. If you came
> here to see what Claude Code / Codex / Cursor cost you, you want Plane B.

---

## Plane B — Coding-agent, and Teams/Org rollup

**Who:** a developer (and, optionally, their team/org admin).

**What:** SuperBased captures your own AI **coding assistant's** activity —
messages, sessions, projects, per-model/per-tool token usage and cost — from
Claude Code, Codex, Cursor, Cline, Copilot, and the rest. You see it on the
**node dashboard** (`http://localhost:8081`). With Teams enabled, each
developer's node **pushes** an opt-in slice to an org server, and the admin sees
the org-wide rollup on the **admin dashboard** (`web2`, served by
`observer-org`).

**Surfaces:**
- Node dashboard (this repo's `web/`): Overview, Sessions, Actions, Cost, Cache,
  Routing, Compression, Security (the `guard` egress-policy layer), etc.
- Admin dashboard (`web2`): org-wide Overview, People, Teams, Tools, Models,
  Sessions, Routing, Security, Report, Audit.
- Config: `[org_client]` / `[org_client.share]`; guardrails on the coding
  agent's own tool calls live under `[guard]`.
- Identity: the **developer** (`user_email`, SAML/SCIM-pinned server-side).

**Start here:** [`teams-getting-started.md`](teams-getting-started.md),
[`teams-architecture.md`](teams-architecture.md), [`guard.md`](guard.md).

---

## Plane A — General observability of a hosted LLM app

**Who:** an admin/operator running an LLM-based **application** whose
**end-user** requests route through SuperBased.

**What:** SuperBased observes that application — traces and spans, usage metrics,
per-end-user cost — and can **guard** it at the input: budget/session limits and
content filtering (an "admission" gate) judged by a **local LLM (Ollama), a
custom-remote-hosted LLM, or a major gateway/provider LLM**. The end-users are
customers of the hosted app, *not* the coding agent's developer.

**Surfaces:**
- Capture: point any OpenTelemetry-instrumented app/agent at the node's local
  OTLP endpoint (`[observability] enabled = true`), or route it through the
  proxy. Traces land in the node-local `obs_*` tables.
- **Viewing is on the admin dashboard** (`web2`, the **Trajectories** nav
  group): trajectories, analytics, evals, cost, alerts — over data each node
  **pushes** under its own opt-in tiers (T1–T5). *The node/developer dashboard
  deliberately does not carry these surfaces* — see "Why the node dashboard is
  Plane-B-only" below.
- Guardrails: the **admission** input gate — `[observability.admission]` +
  `[observability.admission.budget]` (per-**end-user** $ caps). Managed via
  `observer obs admission …` and `observer eval …`; there is no per-end-user
  budgeting in `[guard]` (that's Plane B).
- Identity: the **end-user** of the hosted app (`obs_traces.user`, from OTel
  `enduser.id` / `sbo.user` / the SDK `user=` / the proxy `X-Superbased-User`
  header). This is PII and rides the org wire only under
  `[org_client.share].full_content` / `admin_managed`.

**Start here:** [`observability.md`](observability.md),
[`admission-setup.md`](admission-setup.md), [`guardrails.md`](guardrails.md).

---

## Why the node dashboard is Plane-B-only

The node/developer dashboard (`http://localhost:8081`) is the **developer's**
local view of their own coding-assistant activity — it is intentionally
**Plane B only**. General-observability surfaces (Trajectories, Evals,
Admission) were removed from it (2026-07-06); their home is the **admin
dashboard** (`web2`), which is where an admin operating a hosted LLM app views
that app's observability. A node still *captures and pushes* obs data when
`[observability]` is enabled — it just doesn't render the obs UI locally. This
keeps the two planes legible: the local dashboard is for the developer, the org
dashboard is for the admin.

**One deliberate exception (2026-07-12): the Egress page.** The G22 egress
audit log (`obs_egress_decisions`) is node-local and never pushed — there is
no org tier for it — so an admin-dashboard page would have nothing to read.
Its read-only surface therefore lives on the node dashboard (`/egress`),
alongside the `observer obs egress` CLI. The rule stands for every surface
whose data *is* pushed: those render on web2 only.

See the plane-separation audit
[`audits/plane-separation-audit-2026-07-06.md`](audits/plane-separation-audit-2026-07-06.md)
for the full boundary analysis.

## Shared plumbing (neither plane, or both)

The reverse **proxy** (`:8820`), OTLP **receiver**, capture watcher, cost
engine, and compression are shared infrastructure that both planes lean on;
they branch on **capability** (e.g. an `X-Superbased-User` header, obs being
enabled), never on which plane you "are". Purely local dev-productivity features
— cache tracking, cost prediction, routing, code intelligence — belong to
neither org plane and never leave the node.
