"""Minimal SuperBased Observer SDK example.

Run a local Observer with [observability] enabled, then:

    pip install -e sdk/python
    python sdk/python/examples/quickstart.py

It emits one trace (an agent span containing an LLM span and a tool span) to
the OTLP receiver at http://127.0.0.1:4318/v1/traces. Open the dashboard's
Trajectories page to see it.
"""

import superbased


def main() -> None:
    superbased.init(session_id="quickstart-1", user="demo")

    with superbased.span("agent.run", kind=superbased.AGENT):
        with superbased.llm_span("chat", model="gpt-4o", provider="openai") as s:
            # ... your real model call goes here ...
            superbased.set_usage(
                s,
                input_tokens=1200,
                output_tokens=64,
                response_id="chatcmpl-demo-123",
            )
        with superbased.span("web_search", kind=superbased.TOOL):
            pass  # ... your tool call ...

    superbased.shutdown()  # flush before exit
    print("sent one trace to Observer — check /trajectories")


if __name__ == "__main__":
    main()
