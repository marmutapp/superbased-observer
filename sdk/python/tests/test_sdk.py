"""Unit tests for the SuperBased Python SDK.

Runs with stdlib only (no OpenTelemetry required): the module imports otel
lazily inside init(), so everything here — the admission port default, the
content-attach helpers, the off-switch, truncation, and the env-flag parser —
is exercised without the heavy dependency. pytest-compatible; also runnable
directly with ``python3 tests/test_sdk.py``.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import superbased as s  # noqa: E402


class FakeSpan:
    """Duck-typed span recording set_attribute calls (no otel needed)."""

    def __init__(self):
        self.attrs = {}

    def set_attribute(self, key, value):
        self.attrs[key] = value


def _set_capture(enabled, max_chars=s.DEFAULT_MAX_CONTENT_CHARS):
    # init() would set these, but it needs otel; drive the module state directly.
    s._CAPTURE_CONTENT = enabled
    s._MAX_CONTENT_CHARS = max_chars


def test_admission_endpoint_defaults_to_8081():
    assert s.DEFAULT_ADMISSION_ENDPOINT == "http://127.0.0.1:8081/api/obs/admission/check"


def test_content_attached_by_default():
    _set_capture(True)
    sp = FakeSpan()
    s.set_content(sp, prompt="hello", response="world")
    assert sp.attrs["input.value"] == "hello"
    assert sp.attrs["output.value"] == "world"


def test_set_usage_attaches_content_and_usage():
    _set_capture(True)
    sp = FakeSpan()
    s.set_usage(sp, input_tokens=10, output_tokens=2, prompt="p", response="r")
    assert sp.attrs["gen_ai.usage.input_tokens"] == 10
    assert sp.attrs["input.value"] == "p"
    assert sp.attrs["output.value"] == "r"


def test_off_switch_suppresses_content_keeps_usage():
    _set_capture(False)
    sp = FakeSpan()
    s.set_usage(sp, input_tokens=5, prompt="secret", response="secret")
    assert sp.attrs["gen_ai.usage.input_tokens"] == 5
    assert "input.value" not in sp.attrs
    assert "output.value" not in sp.attrs


def test_tool_args_result_map_to_value_keys():
    _set_capture(True)
    sp = FakeSpan()
    s.set_content(sp, tool_args='{"q":1}', tool_result="ok")
    assert sp.attrs["input.value"] == '{"q":1}'
    assert sp.attrs["output.value"] == "ok"


def test_truncation_clips_to_cap():
    _set_capture(True, max_chars=5)
    sp = FakeSpan()
    s.set_content(sp, prompt="abcdefghij")
    assert sp.attrs["input.value"] == "abcde…[truncated]"
    _set_capture(True)  # restore


def test_env_flag_parsing():
    for raw in ("0", "false", "no", "off", ""):
        os.environ["OBSERVER_CAPTURE_CONTENT"] = raw
        assert s._env_flag("OBSERVER_CAPTURE_CONTENT", True) is False
    for raw in ("1", "true", "yes", "on"):
        os.environ["OBSERVER_CAPTURE_CONTENT"] = raw
        assert s._env_flag("OBSERVER_CAPTURE_CONTENT", False) is True
    del os.environ["OBSERVER_CAPTURE_CONTENT"]
    assert s._env_flag("OBSERVER_CAPTURE_CONTENT", True) is True
    assert s._env_flag("OBSERVER_CAPTURE_CONTENT", False) is False


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"ok   {name}")
            except AssertionError as exc:
                failures += 1
                print(f"FAIL {name}: {exc}")
    print(f"\n{'PASS' if failures == 0 else 'FAIL'} — {failures} failure(s)")
    sys.exit(1 if failures else 0)
