#!/usr/bin/env python3.12
"""Production-shaped Plane-A chat companion for the Azure/org demo.

Open WebUI alone only hits the LLM proxy (api_turns). Org Trajectories /
message bodies require the hosted app to ALSO export OpenInference/OTLP spans
(with content) to Observer — the same dual-rail pattern documented in
docs/observability.md and validated 2026-06-28.

This companion:
  1. Calls OpenAI-compatible chat through Observer's proxy (/up/openrouter)
  2. Emits AGENT+LLM spans (with prompt/response bodies) to Observer OTLP
  3. Uses a stable session.id so Trajectories + org share can roll them up

Usage (local, after mux + share flags):

  set -a; source .env; set +a
  python3 scripts/demo/plane-a-chat-companion.py \\
    --prompt "Explain Observer Plane A in one sentence."

Env:
  OPENROUTER_TOKEN / OPENAI_API_KEY — bearer for OpenRouter via proxy
  SUPERBASED_OTLP_ENDPOINT — default http://127.0.0.1:4318/v1/traces
  OBSERVER_PROXY_BASE — default http://127.0.0.1:8820/up/openrouter/api/v1
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

# Prefer the in-repo SDK without requiring an install.
_REPO = Path(__file__).resolve().parents[2]
_SDK = _REPO / "sdk" / "python"
if _SDK.is_dir():
    sys.path.insert(0, str(_SDK))

import superbased  # noqa: E402


def _chat(
    base: str, key: str, model: str, prompt: str, max_tokens: int = 0, timeout: float = 120.0
) -> dict:
    url = base.rstrip("/") + "/chat/completions"
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0.2,
    }
    if max_tokens > 0:
        # OpenRouter reserves credit for the model's full completion window
        # when max_tokens is absent — a near-zero balance then 402s even
        # though the actual turn would cost fractions of a cent.
        payload["max_tokens"] = max_tokens
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        url,
        data=body,
        headers={
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode())


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--prompt", default="Say hello from the Plane A demo in one short sentence.")
    ap.add_argument("--model", default="openai/gpt-4o-mini")
    ap.add_argument("--max-tokens", type=int, default=512)
    ap.add_argument("--session", default="")
    ap.add_argument("--user", default="demo-enduser@example.com")
    ap.add_argument(
        "--proxy-base",
        default=os.environ.get("OBSERVER_PROXY_BASE", "http://127.0.0.1:8820/up/openrouter/api/v1"),
    )
    ap.add_argument(
        "--otlp",
        default=os.environ.get("SUPERBASED_OTLP_ENDPOINT", "http://127.0.0.1:4318/v1/traces"),
    )
    args = ap.parse_args()

    key = (
        os.environ.get("OPENROUTER_TOKEN")
        or os.environ.get("OPENAI_API_KEY")
        or os.environ.get("OR_TOKEN")
        or ""
    ).strip()
    if not key:
        print("missing OPENROUTER_TOKEN / OPENAI_API_KEY", file=sys.stderr)
        return 2

    session_id = args.session.strip() or ("plane-a-demo-" + time.strftime("%Y%m%d-%H%M%S"))
    request_id = "req_" + uuid.uuid4().hex[:16]

    superbased.init(
        endpoint=args.otlp,
        session_id=session_id,
        user=args.user,
        service_name="azure-plane-a-chat-demo",
        capture_content=True,
    )

    reply = ""
    usage_in = usage_out = 0
    response_id = ""
    err = None
    with superbased.span("chat.turn", kind=superbased.AGENT):
        with superbased.llm_span(
            "chat.completions",
            model=args.model,
            provider="openrouter",
            prompt=args.prompt,
        ) as sp:
            try:
                data = _chat(args.proxy_base, key, args.model, args.prompt, args.max_tokens)
                choice = (data.get("choices") or [{}])[0]
                reply = ((choice.get("message") or {}).get("content")) or ""
                u = data.get("usage") or {}
                usage_in = int(u.get("prompt_tokens") or u.get("input_tokens") or 0)
                usage_out = int(u.get("completion_tokens") or u.get("output_tokens") or 0)
                response_id = str(data.get("id") or "")
                superbased.set_usage(
                    sp,
                    input_tokens=usage_in,
                    output_tokens=usage_out,
                    response_id=response_id or None,
                    request_id=request_id,
                    response=reply,
                )
            except urllib.error.HTTPError as e:
                err = e.read().decode(errors="replace")[:800]
                sp.record_exception(e)
                raise
            except Exception as e:  # noqa: BLE001
                err = str(e)
                sp.record_exception(e)
                raise

    superbased.shutdown()
    print(
        json.dumps(
            {
                "ok": err is None,
                "session_id": session_id,
                "request_id": request_id,
                "response_id": response_id,
                "model": args.model,
                "input_tokens": usage_in,
                "output_tokens": usage_out,
                "reply_preview": (reply or "")[:240],
                "error": err,
            },
            indent=2,
        )
    )
    return 0 if err is None else 1


if __name__ == "__main__":
    raise SystemExit(main())
