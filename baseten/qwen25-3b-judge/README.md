# qwen25-3b-judge — Baseten Truss deployment

A [Baseten](https://baseten.co) deployment of **Qwen 2.5 3B Instruct**
(`Qwen/Qwen2.5-3B-Instruct`) served through vLLM's OpenAI-compatible server, so
it exposes `/v1/chat/completions`. Its purpose is to be an **opt-in** judge-LLM
provider for SuperBased Observer's observability judge binding
(`[observability.judge]`). It is **never** the default judge — the default
posture stays OpenRouter free-models-only.

This is a Baseten **Custom Server** (`docker_server`) Truss: there is no
`model.py`; the vLLM image *is* the server. See
[Baseten custom-server docs](https://docs.baseten.co/truss/guides/custom-server)
and the [vLLM example](https://docs.baseten.co/examples/vllm).

## Layout

```
baseten/qwen25-3b-judge/
├── config.yaml     # the whole deployment: image, start command, GPU, endpoints
├── model/          # placeholder (unused for a docker_server truss)
└── README.md       # this file
```

## Prerequisites

- A Baseten account with a **WORKSPACE_MANAGE_ALL** or **personal**-scoped API
  key. A deploy is a GraphQL model-create mutation; read-only / invoke-only keys
  get `UNAUTHORIZED_ACCESS` and cannot deploy.
- `truss` installed (`pip install truss`; a venv is fine).
- The key configured for truss (either `truss login`, or `~/.trussrc`):

  ```ini
  [baseten]
  remote_provider = baseten
  api_key = <YOUR_BASETEN_API_KEY>
  remote_url = https://app.baseten.co
  ```

## Deploy

```bash
cd baseten/qwen25-3b-judge
truss push --promote --wait
```

`--promote` publishes and promotes to the **production** environment, which
gives the sync URL Observer wants:

```
https://model-<MODEL_ID>.api.baseten.co/environments/production/sync/v1/chat/completions
```

Capture `<MODEL_ID>` from the push output (or `scripts/demo-baseten.sh status`).

## Budget — scale-to-zero (important)

The GPU is the cost. Two safeguards keep idle cost ~0:

1. **Scale-to-zero.** Set the production deployment's autoscaling floor to
   `min_replica = 0` so Baseten sleeps it when idle and bills no GPU minutes:

   ```bash
   curl -X PATCH \
     "https://api.baseten.co/v1/models/<MODEL_ID>/deployments/production/autoscaling_settings" \
     -H "Authorization: Api-Key $BASETEN_API_KEY" \
     -H 'Content-Type: application/json' \
     --data '{"min_replica":0,"max_replica":1,"scale_down_delay":120}'
   ```

   `scripts/demo-baseten.sh up` applies this for you.

2. **Deactivate when done.** `scripts/demo-baseten.sh down` deactivates the
   deployment outright — it then consumes no compute and won't wake on a
   request. This is the surest "off".

**GPU choice:** `L4` (24 GB) — Baseten's own reference accelerator for Qwen 2.5
3B, and the smallest adequate GPU (fp16 weights ~6 GB + KV cache). Do not bump
to A100/H100 for a 3B model.

## Wire it into Observer

Copy `examples/baseten-judge/observer.judge.baseten.toml` into your
`~/.observer/config.toml`, replace `<BASETEN_MODEL_ID>`, and export
`BASETEN_API_KEY`. Full operator reference: `docs/baseten-judge.md`.
