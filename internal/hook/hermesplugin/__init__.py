"""SuperBased Observer — Hermes Agent plugin bridge.

Fires `observer hook hermes <event>` as a fire-and-forget subprocess on each
relevant Hermes lifecycle hook so the observer daemon can ingest Hermes
activity in real time.

Design constraints:

- 0.5 s subprocess timeout (TIMEOUT_SECONDS below). The Hermes plugin loader
  catches all exceptions raised by callbacks (hermes_cli/plugins.py line
  1565) but a hung subprocess could still introduce perceptible latency
  inside the host CLI; the explicit timeout caps it.
- Exceptions are caught and ignored — observer being absent / down / slow
  is NOT an error condition. The hook returns immediately and Hermes
  continues normally.
- stdout from the subprocess is captured but ignored.

Environment overrides (read at plugin load time):
- OBSERVER_BIN     Path to the observer binary. `observer init --hermes`
                   substitutes the running binary's absolute path into
                   the default value below at install time so PATH
                   manipulation isn't required. Setting OBSERVER_BIN in
                   the operator's environment still wins — useful when
                   the agent and observer live under different `bin/`
                   paths.
- OBSERVER_CONFIG  Path to observer config.toml. Defaults to empty (the
                   observer falls back to its standard discovery). Set
                   this if the agent and observer run under different
                   home directories.

Hooks registered (subset of hermes_cli/plugins.py VALID_HOOKS — see
docs/hermes-adapter-plan.md §17.1.F for the rationale on each pick):

- on_session_start  — captures session UUID, model, source, CWD.
- on_session_end    — captures end_reason and ended_at.
- post_tool_call    — captures the tool name, args, result, duration_ms.
                      Fires AFTER the tool returns so the result is
                      available; the assistant message that initiated
                      the call lands in the SQLite backfill path
                      separately.
- post_api_request  — captures upstream API usage (input/output/cache/
                      reasoning tokens) per call. The plan originally
                      targeted post_llm_call but reality (§17.1.F.1)
                      showed that hook carries no usage payload at all;
                      post_api_request is the right capture surface.
- subagent_stop     — captures sub-agent completion (duration, status,
                      summary) for cross-thread analytics.
"""

import json
import os
import subprocess
import time

OBSERVER_BIN = os.environ.get("OBSERVER_BIN", "{{OBSERVER_BIN_DEFAULT}}")
OBSERVER_CONFIG = os.environ.get("OBSERVER_CONFIG", "")
TIMEOUT_SECONDS = 0.5


def _fire(event_name, payload):
    """Run `observer hook hermes <event>` with payload on stdin. Fire-and-forget."""
    cmd = [OBSERVER_BIN, "hook", "hermes", event_name]
    if OBSERVER_CONFIG:
        cmd.extend(["--config", OBSERVER_CONFIG])
    try:
        subprocess.run(
            cmd,
            input=json.dumps(payload),
            capture_output=True,
            text=True,
            timeout=TIMEOUT_SECONDS,
            check=False,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError, ValueError):
        # ValueError covers json.dumps failures on non-serialisable payload
        # parts (rare but possible for tool args that wrap third-party
        # objects); the rest cover subprocess + env failures. Observer being
        # absent / slow / mis-configured MUST NEVER block the host Hermes
        # tool, so every exception class lands in the same swallow.
        pass


def _safe_json(value):
    """Return value if it's already JSON-shaped, else stringify it."""
    if value is None:
        return None
    if isinstance(value, (dict, list, str, int, float, bool)):
        return value
    return str(value)


def register(ctx):
    """Hermes plugin entry point.

    Called once by the plugin loader at agent startup. Registers callbacks
    for the lifecycle hooks observer ingests.
    """

    def on_session_start(session_id=None, model=None, source=None, **kwargs):
        if not session_id:
            return
        _fire("session_start", {
            "event": "session_start",
            "session_id": session_id,
            "model": model or "",
            "source": source or "",
            "cwd": os.getcwd(),
            "started_at": time.time(),
            "telemetry_schema_version": kwargs.get("telemetry_schema_version", ""),
        })

    def on_session_end(session_id=None, end_reason=None, **kwargs):
        if not session_id:
            return
        _fire("session_end", {
            "event": "session_end",
            "session_id": session_id,
            "end_reason": end_reason or "",
            # cwd is REQUIRED — observer's store layer drops events with
            # empty ProjectRoot (`if e.ProjectRoot == "" { continue }`)
            # to avoid an FK violation on the project upsert. The Hermes
            # on_session_end hook doesn't surface cwd in its kwargs, so
            # we capture os.getcwd() at end-time. Validation 2026-06-06
            # caught session_end rows being silently dropped pre-fix.
            "cwd": os.getcwd(),
            "ended_at": time.time(),
            "telemetry_schema_version": kwargs.get("telemetry_schema_version", ""),
        })

    def post_tool_call(session_id=None, tool_name=None, args=None, result=None,
                       duration_ms=0, tool_call_id=None, task_id=None, **kwargs):
        if not session_id or not tool_name:
            return
        # result can be a dict (structured per §17.1 C) or a string. Coerce
        # to a string for the Go side, which parses with json.Unmarshal.
        if isinstance(result, (dict, list)):
            result_str = json.dumps(result)
        elif result is None:
            result_str = ""
        else:
            result_str = str(result)
        _fire("tool_call", {
            "event": "tool_call",
            "session_id": session_id,
            "task_id": task_id or "",
            "tool_call_id": tool_call_id or "",
            "tool_name": tool_name,
            "args": _safe_json(args) or {},
            "result": result_str,
            "duration_ms": int(duration_ms) if duration_ms is not None else 0,
            "cwd": os.getcwd(),
            "timestamp": time.time(),
            "telemetry_schema_version": kwargs.get("telemetry_schema_version", ""),
        })

    def post_api_request(session_id=None, model=None, provider=None,
                         usage=None, api_call_count=0, finish_reason=None,
                         api_duration=0, **kwargs):
        if not session_id or not usage:
            return
        _fire("api_request", {
            "event": "api_request",
            "session_id": session_id,
            "model": model or "",
            "provider": provider or "",
            "usage": _safe_json(usage) or {},
            "api_call_count": int(api_call_count) if api_call_count is not None else 0,
            "finish_reason": finish_reason or "",
            "api_duration": float(api_duration) if api_duration is not None else 0,
            # cwd is REQUIRED for the same reason as on_session_end —
            # observer's store layer needs a non-empty ProjectRoot to
            # upsert the owning project before the token row can land.
            # Validation 2026-06-06 caught api_request token rows being
            # silently dropped pre-fix even though the usage block was
            # populated.
            "cwd": os.getcwd(),
            "timestamp": time.time(),
            "telemetry_schema_version": kwargs.get("telemetry_schema_version", ""),
        })

    def subagent_stop(parent_session_id=None, child_summary=None,
                      child_status=None, duration_ms=0, **kwargs):
        if not parent_session_id:
            return
        _fire("subagent_stop", {
            "event": "subagent_stop",
            "session_id": parent_session_id,
            "child_summary": child_summary or "",
            "child_status": child_status or "",
            "duration_ms": int(duration_ms) if duration_ms is not None else 0,
            # cwd required (same reason as the other event callbacks).
            "cwd": os.getcwd(),
            "timestamp": time.time(),
            "telemetry_schema_version": kwargs.get("telemetry_schema_version", ""),
        })

    ctx.register_hook("on_session_start", on_session_start)
    ctx.register_hook("on_session_end", on_session_end)
    ctx.register_hook("post_tool_call", post_tool_call)
    ctx.register_hook("post_api_request", post_api_request)
    ctx.register_hook("subagent_stop", subagent_stop)
