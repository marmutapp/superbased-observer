#!/usr/bin/env python3.12
"""Synthetic scenario seeder for the Plane-A observability client demo.

Generates realistic-looking but 100% FICTIONAL agent-trace traffic (fake
companies acme-corp/northwind/initech, fake users, fake SaaS support content)
and ships it straight to Observer's OTLP receiver at
``http://127.0.0.1:4318/v1/traces`` so the org dashboard's Trajectories /
Analytics / Evals / End-user-spend pages have something worth demoing.

This does NOT touch the node DB directly — every row this script produces
enters through the same wire a real instrumented app would use. It also does
NOT edit any Go code; it's pure client-side traffic generation.

Why raw OpenTelemetry instead of the ``superbased`` convenience SDK
(``sdk/python/superbased``, see ``scripts/demo/plane-a-chat-companion.py``
for the SDK's normal usage pattern)? The SDK's ``span()``/``llm_span()``
helpers always stamp "now" as start/end time via
``start_as_current_span()``. To spread ~60 traces realistically over the
past N days (so the dashboard's time-series charts aren't one vertical
spike), spans need explicit ``start_time``/``end_time`` in nanoseconds,
which only the underlying OTel SDK's ``tracer.start_span(..., start_time=,
context=)`` exposes. The SDK is a thin wrapper over exactly this API (its
own docstring says so), and it's importable without installing anything
(the repo root is on ``sys.path`` via the same trick the companion script
uses) — this script reuses it only for the OpenInference span-kind
constants (LLM/TOOL/RETRIEVER/AGENT/...) so both scripts speak the same
vocabulary; span creation itself goes straight to ``opentelemetry-sdk`` +
``opentelemetry-exporter-otlp-proto-http`` (both already present in this
environment), matching the attribute keys documented in
``internal/obs/ingest/ingest.go`` exactly:

  session   -> session.id
  end-user  -> enduser.id
  tenant    -> sbo.tenant
  model     -> gen_ai.request.model / llm.model_name
  provider  -> gen_ai.system / llm.provider
  tokens    -> gen_ai.usage.input_tokens / gen_ai.usage.output_tokens
  cost      -> llm.cost.total
  request   -> gen_ai.request.id
  response  -> gen_ai.response.id
  tool name -> tool.name
  kind      -> openinference.span.kind (LLM/TOOL/RETRIEVER/AGENT/...)

session.id / enduser.id / sbo.tenant are set as SPAN attributes on every
span of a trace (not just resource attributes) because the ingester
coalesces session/user/tenant across all spans in a trace, span-level
values winning first — this is what lets one process seed many different
(user, tenant, session) triples without restarting the exporter per user.

Usage:

  python3.12 scripts/demo/plane-a-scenario-seeder.py \\
      --days 7 --traces 60 --seed 20260813

No secrets are read or embedded in this file; it makes no calls to any real
LLM provider — all token counts, costs, and content are synthesized.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import random
import socket
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Optional

# Prefer the in-repo SDK without requiring an install (same trick as
# plane-a-chat-companion.py) — used here only for its span-kind constants.
_REPO = Path(__file__).resolve().parents[2]
_SDK = _REPO / "sdk" / "python"
if _SDK.is_dir():
    sys.path.insert(0, str(_SDK))

import superbased  # noqa: E402 — span-kind constants (LLM/TOOL/RETRIEVER/AGENT)

from opentelemetry import trace  # noqa: E402
from opentelemetry.exporter.otlp.proto.http.trace_exporter import (  # noqa: E402
    OTLPSpanExporter,
)
from opentelemetry.sdk.resources import Resource  # noqa: E402
from opentelemetry.sdk.trace import TracerProvider  # noqa: E402
from opentelemetry.sdk.trace.export import BatchSpanProcessor  # noqa: E402
from opentelemetry.trace import Status, StatusCode  # noqa: E402

DEFAULT_OTLP_ENDPOINT = "http://127.0.0.1:4318/v1/traces"

# ---------------------------------------------------------------------------
# Fictional demo fixtures — obviously fake companies/emails, no real PII.
# ---------------------------------------------------------------------------

# 2 tenants per the demo brief. luis@initech.dev is folded into "acme" as a
# fictional contractor engaged by Acme Corp, keeping the tenant *set* at
# exactly {acme, northwind} while still giving us a 5th distinct end-user.
USERS = [
    ("priya@acme-corp.com", "acme"),
    ("chen@acme-corp.com", "acme"),
    ("sam@northwind.io", "northwind"),
    ("fatima@northwind.io", "northwind"),
    ("luis@initech.dev", "acme"),
]

# gen_ai.system / llm.provider + pricing (USD per 1M tokens) for cost.total.
MODELS = [
    {
        "model": "nvidia/nemotron-3.5-lightning:free",
        "provider": "openrouter",
        "price_in": 0.0,
        "price_out": 0.0,
        "weight": 0.70,
    },
    {
        "model": "openai/gpt-4o-mini",
        "provider": "openai",
        "price_in": 0.15,
        "price_out": 0.60,
        "weight": 0.20,
    },
    {
        "model": "anthropic/claude-sonnet-5",
        "provider": "anthropic",
        "price_in": 3.00,
        "price_out": 15.00,
        "weight": 0.10,
    },
]

PRODUCT = "Lumen Workspace"

QA_PAIRS = [
    (
        f"How do I reset my {PRODUCT} password?",
        "Go to Settings > Security > Reset Password, then check your inbox "
        "for the reset link (valid for 30 minutes).",
    ),
    (
        "Why is my dashboard showing stale data?",
        "Dashboards refresh every 15 minutes by default; you can force a "
        "refresh from the sync icon in the top-right corner.",
    ),
    (
        "Can I export my usage report to CSV?",
        "Yes — open Reports > Usage, click Export, and choose CSV or XLSX.",
    ),
    (
        "My teammate can't see the billing page.",
        "Billing visibility requires the Admin or Billing Manager role; you "
        "can grant it under Settings > Team > Roles.",
    ),
    (
        f"How do I connect {PRODUCT} to Slack?",
        "Go to Integrations > Slack, click Connect, and authorize the "
        "workspace you want notifications sent to.",
    ),
    (
        "What happens if I exceed my seat limit?",
        "New invites are queued until you free up a seat or upgrade your "
        "plan under Settings > Billing > Plan.",
    ),
    (
        "Is there an API rate limit?",
        "Yes — 600 requests/min per API key on the Growth plan. Enterprise "
        "plans can request a higher limit from support.",
    ),
    (
        "How do I enable SSO for our org?",
        "SSO (SAML) is available on the Enterprise plan — go to Settings > "
        "Security > SSO and upload your IdP metadata.",
    ),
    (
        "Can I recover a deleted project?",
        "Deleted projects are recoverable for 14 days from Settings > "
        "Trash; after that they're purged permanently.",
    ),
    (
        "Why did my scheduled report not send?",
        "Scheduled reports pause automatically if the recipient list is "
        "empty or the report owner's account was deactivated.",
    ),
    (
        f"Does {PRODUCT} support two-factor authentication?",
        "Yes — enable it under Settings > Security > Two-Factor "
        "Authentication; we support TOTP apps and SMS backup codes.",
    ),
    (
        "How long is data retained after I cancel?",
        "Account data is retained for 30 days after cancellation, then "
        "permanently deleted; you can export everything before that window closes.",
    ),
]

# Hour-of-day sampling weights (0-23) — biased toward business hours so
# traces cluster realistically instead of spreading uniformly.
HOUR_WEIGHTS = [
    1, 1, 1, 1, 1, 1,  # 00-05 overnight
    2, 3, 6, 9, 10, 10,  # 06-11 morning ramp
    8, 9, 10, 10, 9, 8,  # 12-17 afternoon
    6, 4, 3, 2, 1, 1,  # 18-23 evening tail
]

ERROR_REASONS = [
    "upstream request timed out after 30000ms",
    "provider returned 503 (model overloaded)",
    "connection reset while streaming completion",
]


# ---------------------------------------------------------------------------
# OTLP readiness + plumbing
# ---------------------------------------------------------------------------


def wait_for_otlp(url: str, attempts: int = 10, delay: float = 15.0) -> None:
    """Poll the OTLP endpoint until it accepts connections.

    The node daemon may restart mid-run (another agent reconfiguring it) —
    any HTTP response (even a 4xx from a malformed probe body) proves the
    receiver is alive; only connection-level failures are retried.
    """
    for attempt in range(1, attempts + 1):
        req = urllib.request.Request(
            url,
            data=b"",  # a zero-length body is a valid (empty) OTLP export request
            headers={"Content-Type": "application/x-protobuf"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                resp.read()
            print(f"[seeder] OTLP endpoint ready ({url}), attempt {attempt}", file=sys.stderr)
            return
        except urllib.error.HTTPError:
            # Any HTTP status means the receiver answered.
            print(f"[seeder] OTLP endpoint ready ({url}), attempt {attempt}", file=sys.stderr)
            return
        except (urllib.error.URLError, socket.timeout, ConnectionError, OSError) as exc:
            print(
                f"[seeder] OTLP endpoint not reachable yet (attempt {attempt}/{attempts}): {exc}",
                file=sys.stderr,
            )
            if attempt == attempts:
                raise SystemExit(
                    f"OTLP endpoint {url} never became reachable after {attempts} attempts"
                )
            time.sleep(delay)


def build_provider(endpoint: str) -> TracerProvider:
    resource = Resource.create(
        {
            "service.name": "plane-a-demo-seeder",
            "sbo.sdk": "superbased-python-scenario-seeder",
        }
    )
    provider = TracerProvider(resource=resource)
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint)))
    return provider


# ---------------------------------------------------------------------------
# Timestamp helpers (all nanoseconds — the OTel SDK's unit for
# start_time/end_time)
# ---------------------------------------------------------------------------


def random_ts_ns(rng: random.Random, days: int) -> int:
    """A random timestamp within the past `days`, hour-biased for clustering."""
    day_offset = rng.randint(0, max(days - 1, 0))
    base = dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=day_offset)
    hour = rng.choices(range(24), weights=HOUR_WEIGHTS, k=1)[0]
    minute = rng.randint(0, 59)
    second = rng.randint(0, 59)
    micro = rng.randint(0, 999_999)
    ts = base.replace(hour=hour, minute=minute, second=second, microsecond=micro)
    # replace() can push a day-offset-0 timestamp past "now" (a random hour
    # later today) — traces in the future sort above live data and pollute
    # time-bucketed charts, so clamp by shifting one day back.
    now = dt.datetime.now(dt.timezone.utc)
    if ts > now:
        ts -= dt.timedelta(days=1)
    return int(ts.timestamp() * 1e9)


def ms(n: float) -> int:
    return int(n * 1_000_000)  # milliseconds -> nanoseconds


# ---------------------------------------------------------------------------
# Trace generation
# ---------------------------------------------------------------------------


def pick_model(rng: random.Random) -> dict:
    return rng.choices(MODELS, weights=[m["weight"] for m in MODELS], k=1)[0]


def llm_cost(model: dict, input_tokens: int, output_tokens: int) -> float:
    return round(
        (input_tokens / 1_000_000) * model["price_in"]
        + (output_tokens / 1_000_000) * model["price_out"],
        6,
    )


def start_child(
    tracer,
    parent_span,
    name: str,
    kind: str,
    start_ns: int,
    attributes: dict,
):
    ctx = trace.set_span_in_context(parent_span) if parent_span is not None else None
    attrs = {"openinference.span.kind": kind}
    attrs.update(attributes)
    return tracer.start_span(name, context=ctx, attributes=attrs, start_time=start_ns)


def finish(sp, end_ns: int, error: Optional[str] = None) -> None:
    if error:
        sp.set_status(Status(StatusCode.ERROR, error))
        sp.add_event(
            "exception",
            {"exception.type": "TimeoutError", "exception.message": error},
            timestamp=end_ns,
        )
    else:
        sp.set_status(Status(StatusCode.OK))
    sp.end(end_time=end_ns)


def gen_support_bot_trace(
    tracer,
    rng: random.Random,
    start_ns: int,
    session_id: str,
    user: str,
    tenant: str,
    is_error: bool,
) -> tuple[int, int]:
    """AGENT chat.turn -> RETRIEVER doc_search -> LLM chat.completions.

    Returns (span_count, end_ns).
    """
    question, answer = rng.choice(QA_PAIRS)
    request_id = "req_" + uuid.uuid4().hex[:16]

    root = start_child(
        tracer,
        None,
        "chat.turn",
        superbased.AGENT,
        start_ns,
        {"session.id": session_id, "enduser.id": user, "sbo.tenant": tenant},
    )

    retr_dur = ms(rng.uniform(50, 300))
    retr_start = start_ns
    retr_end = retr_start + retr_dur
    retr = start_child(
        tracer,
        root,
        "doc_search",
        superbased.RETRIEVER,
        retr_start,
        {
            "session.id": session_id,
            "enduser.id": user,
            "sbo.tenant": tenant,
            "tool.name": "doc-search",
            "input.value": json.dumps({"query": question}),
            "output.value": json.dumps(
                {"docs": [f"kb/{PRODUCT.lower().replace(' ', '-')}-faq-{rng.randint(1, 40)}"]}
            ),
        },
    )
    finish(retr, retr_end)

    model = pick_model(rng)
    llm_start = retr_end
    llm_dur = ms(rng.uniform(600, 4000))
    llm_end = llm_start + llm_dur
    input_tokens = rng.randint(300, 3000)
    output_tokens = rng.randint(100, 800) if not is_error else 0

    llm_attrs = {
        "session.id": session_id,
        "enduser.id": user,
        "sbo.tenant": tenant,
        "gen_ai.request.model": model["model"],
        "llm.model_name": model["model"],
        "gen_ai.system": model["provider"],
        "llm.provider": model["provider"],
        "gen_ai.usage.input_tokens": input_tokens,
        "gen_ai.request.id": request_id,
        "input.value": json.dumps(
            [{"role": "system", "content": f"You are the {PRODUCT} support assistant."},
             {"role": "user", "content": question}]
        ),
    }
    if not is_error:
        llm_attrs["gen_ai.usage.output_tokens"] = output_tokens
        llm_attrs["gen_ai.response.id"] = "gen-" + uuid.uuid4().hex[:20]
        llm_attrs["llm.cost.total"] = llm_cost(model, input_tokens, output_tokens)
        llm_attrs["output.value"] = json.dumps([{"role": "assistant", "content": answer}])

    llm = start_child(tracer, root, "chat.completions", superbased.LLM, llm_start, llm_attrs)
    finish(llm, llm_end, error=rng.choice(ERROR_REASONS) if is_error else None)

    finish(root, llm_end, error=("agent turn failed" if is_error else None))
    return 3, llm_end


def gen_orders_agent_trace(
    tracer,
    rng: random.Random,
    start_ns: int,
    session_id: str,
    user: str,
    tenant: str,
    is_error: bool,
) -> tuple[int, int]:
    """AGENT chat.turn -> TOOL lookup_order -> TOOL refund_policy_search -> LLM synthesis."""
    order_id = f"{tenant.upper()}-{rng.randint(10000, 99999)}"
    request_id = "req_" + uuid.uuid4().hex[:16]

    root = start_child(
        tracer,
        None,
        "chat.turn",
        superbased.AGENT,
        start_ns,
        {"session.id": session_id, "enduser.id": user, "sbo.tenant": tenant},
    )

    lookup_dur = ms(rng.uniform(80, 400))
    lookup_start = start_ns
    lookup_end = lookup_start + lookup_dur
    order_status = rng.choice(["shipped", "processing", "delivered", "delayed"])
    lookup = start_child(
        tracer,
        root,
        "lookup_order",
        superbased.TOOL,
        lookup_start,
        {
            "session.id": session_id,
            "enduser.id": user,
            "sbo.tenant": tenant,
            "tool.name": "lookup_order",
            "input.value": json.dumps({"order_id": order_id}),
            "output.value": json.dumps(
                {
                    "order_id": order_id,
                    "status": order_status,
                    "carrier": rng.choice(["FastFreight", "NorthLine Courier", "AcmeShip"]),
                    "eta_days": rng.randint(1, 6),
                }
            ),
        },
    )
    finish(lookup, lookup_end)

    policy_dur = ms(rng.uniform(80, 400))
    policy_start = lookup_end
    policy_end = policy_start + policy_dur
    policy = start_child(
        tracer,
        root,
        "refund_policy_search",
        superbased.TOOL,
        policy_start,
        {
            "session.id": session_id,
            "enduser.id": user,
            "sbo.tenant": tenant,
            "tool.name": "refund_policy_search",
            "input.value": json.dumps({"query": "refund window for damaged goods"}),
            "output.value": json.dumps(
                {
                    "policy": "Damaged-goods refunds are accepted within 30 days of "
                    "delivery with photo evidence.",
                    "source": "policy-doc-14",
                }
            ),
        },
    )
    finish(policy, policy_end)

    model = pick_model(rng)
    llm_start = policy_end
    llm_dur = ms(rng.uniform(900, 8000))
    llm_end = llm_start + llm_dur
    input_tokens = rng.randint(400, 2000)
    output_tokens = rng.randint(150, 600) if not is_error else 0

    synth_prompt = (
        f"Customer asked about order {order_id}. Order status: {order_status}. "
        "Refund policy: damaged goods accepted within 30 days with photo evidence. "
        "Draft a helpful reply."
    )
    synth_answer = (
        f"Your order {order_id} is currently {order_status}. If it arrives "
        "damaged, you're covered for a refund within 30 days as long as you "
        "include photo evidence."
    )

    llm_attrs = {
        "session.id": session_id,
        "enduser.id": user,
        "sbo.tenant": tenant,
        "gen_ai.request.model": model["model"],
        "llm.model_name": model["model"],
        "gen_ai.system": model["provider"],
        "llm.provider": model["provider"],
        "gen_ai.usage.input_tokens": input_tokens,
        "gen_ai.request.id": request_id,
        "input.value": json.dumps(
            [{"role": "system", "content": "You are the orders support agent."},
             {"role": "user", "content": synth_prompt}]
        ),
    }
    if not is_error:
        llm_attrs["gen_ai.usage.output_tokens"] = output_tokens
        llm_attrs["gen_ai.response.id"] = "gen-" + uuid.uuid4().hex[:20]
        llm_attrs["llm.cost.total"] = llm_cost(model, input_tokens, output_tokens)
        llm_attrs["output.value"] = json.dumps([{"role": "assistant", "content": synth_answer}])

    llm = start_child(tracer, root, "chat.completions", superbased.LLM, llm_start, llm_attrs)
    finish(llm, llm_end, error=rng.choice(ERROR_REASONS) if is_error else None)

    finish(root, llm_end, error=("agent turn failed" if is_error else None))
    return 4, llm_end


# ---------------------------------------------------------------------------
# Plan: decide, up front, every trace's (type, session, user, tenant, ts,
# is_error) so the error rate and multi-turn sessions land exactly right.
# ---------------------------------------------------------------------------


def build_plan(rng: random.Random, total: int, days: int) -> list[dict]:
    plan: list[dict] = []

    # A couple of long multi-turn sessions (item 4) — skipped if the caller
    # requested so few traces that reserving 2*(5-6) turns would blow the
    # budget (keeps this safe for small --traces smoke runs).
    multi_turn_sessions = 2 if total >= 16 else 0
    for _ in range(multi_turn_sessions):
        turns = rng.choice([5, 6])
        user, tenant = rng.choice(USERS)
        session_id = "sess-" + uuid.uuid4().hex[:12]
        trace_type = rng.choice(["support-bot", "orders-agent"])
        base_ts = random_ts_ns(rng, days)
        ts = base_ts
        for _ in range(turns):
            plan.append(
                {
                    "type": trace_type,
                    "session_id": session_id,
                    "user": user,
                    "tenant": tenant,
                    "ts": ts,
                }
            )
            # Sequential turns minutes apart within the same session.
            ts += ms(rng.uniform(2 * 60_000, 12 * 60_000))

    remaining = max(total - len(plan), 0)
    for _ in range(remaining):
        trace_type = "support-bot" if rng.random() < 0.55 else "orders-agent"
        user, tenant = rng.choice(USERS)
        plan.append(
            {
                "type": trace_type,
                "session_id": "sess-" + uuid.uuid4().hex[:12],
                "user": user,
                "tenant": tenant,
                "ts": random_ts_ns(rng, days),
            }
        )

    # ~8% error rate, spread across the whole plan (at least one if we have
    # any traces at all).
    error_count = max(1, round(len(plan) * 0.08)) if plan else 0
    error_indices = set(rng.sample(range(len(plan)), min(error_count, len(plan))))
    for i, job in enumerate(plan):
        job["is_error"] = i in error_indices

    plan.sort(key=lambda j: j["ts"])
    return plan


def run_plan(tracer, rng: random.Random, plan: list[dict]) -> dict:
    stats = {
        "support-bot": {"traces": 0, "spans": 0, "errors": 0},
        "orders-agent": {"traces": 0, "spans": 0, "errors": 0},
        "multi_turn_sessions": set(),
        "sessions_seen": set(),
    }
    session_counts: dict[str, int] = {}
    for job in plan:
        session_counts[job["session_id"]] = session_counts.get(job["session_id"], 0) + 1

    for job in plan:
        if job["type"] == "support-bot":
            n_spans, _ = gen_support_bot_trace(
                tracer,
                rng,
                job["ts"],
                job["session_id"],
                job["user"],
                job["tenant"],
                job["is_error"],
            )
        else:
            n_spans, _ = gen_orders_agent_trace(
                tracer,
                rng,
                job["ts"],
                job["session_id"],
                job["user"],
                job["tenant"],
                job["is_error"],
            )
        bucket = stats[job["type"]]
        bucket["traces"] += 1
        bucket["spans"] += n_spans
        if job["is_error"]:
            bucket["errors"] += 1
        stats["sessions_seen"].add(job["session_id"])
        if session_counts[job["session_id"]] > 1:
            stats["multi_turn_sessions"].add(job["session_id"])

    return stats


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--days", type=int, default=7, help="spread traces over the past N days")
    ap.add_argument("--traces", type=int, default=60, help="total number of traces to seed")
    ap.add_argument("--seed", type=int, default=20260813, help="RNG seed for reproducibility")
    ap.add_argument(
        "--otlp",
        default=os.environ.get("SUPERBASED_OTLP_ENDPOINT", DEFAULT_OTLP_ENDPOINT),
        help="OTLP HTTP traces endpoint",
    )
    ap.add_argument("--wait-attempts", type=int, default=10)
    ap.add_argument("--wait-delay", type=float, default=15.0)
    args = ap.parse_args()

    wait_for_otlp(args.otlp, attempts=args.wait_attempts, delay=args.wait_delay)

    rng = random.Random(args.seed)
    provider = build_provider(args.otlp)
    tracer = provider.get_tracer("plane-a-scenario-seeder")

    plan = build_plan(rng, args.traces, args.days)
    stats = run_plan(tracer, rng, plan)

    provider.force_flush(timeout_millis=30_000)
    provider.shutdown()

    total_traces = stats["support-bot"]["traces"] + stats["orders-agent"]["traces"]
    total_spans = stats["support-bot"]["spans"] + stats["orders-agent"]["spans"]
    total_errors = stats["support-bot"]["errors"] + stats["orders-agent"]["errors"]

    report = {
        "otlp_endpoint": args.otlp,
        "days": args.days,
        "seed": args.seed,
        "total_traces": total_traces,
        "total_spans": total_spans,
        "total_errors": total_errors,
        "error_rate": round(total_errors / total_traces, 4) if total_traces else 0.0,
        "by_scenario": {
            "support-bot": stats["support-bot"],
            "orders-agent": stats["orders-agent"],
        },
        "distinct_sessions": len(stats["sessions_seen"]),
        "multi_turn_sessions": sorted(stats["multi_turn_sessions"]),
    }
    print(json.dumps(report, indent=2, default=list))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
