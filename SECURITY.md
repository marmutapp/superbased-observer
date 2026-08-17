# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Use GitHub's private vulnerability reporting instead:

1. Go to the [Security tab](https://github.com/superbasedapp/observer/security) on this repository.
2. Click **"Report a vulnerability"**.

This opens a private advisory thread visible only to you and the
maintainers, and lets us collaborate on a fix before anything is
disclosed publicly.

If you're unable to use GitHub's reporting flow, email
**contact@superbased.app** with as much detail as you can provide
(affected version, reproduction steps, impact).

Do not include exploit details, secrets, or sensitive data in a
public issue, discussion, or pull request.

## Supported versions

Observer ships frequent releases. We support the **latest released
version** on a rolling basis. If you're reporting an issue on an
older version, please upgrade and confirm it still reproduces before
reporting — if you can't upgrade, tell us why in the report and we'll
take that into account.

## What to expect

- **Acknowledgement within 72 hours** of a report reaching us through
  either channel above.
- We'll work with you on a **coordinated disclosure** timeline. Our
  default disclosure window is **90 days** from acknowledgement, or
  sooner once a fix ships — extended by mutual agreement if the fix
  needs more time.
- We'll credit you in the advisory and release notes when the fix
  ships, unless you'd prefer to stay anonymous.

## Scope

Observer is a local-first tool. Its real attack surface is narrower
than a hosted product's, but it's still real:

**In scope:**

- The local daemon and dashboard (bound to loopback by default) —
  auth bypass, injection, path traversal, or anything that lets a
  local dashboard request act outside its intended scope.
- The optional API reverse proxy (Anthropic / OpenAI / Gemini
  provider lanes) — request/response handling, header forwarding,
  anything that could leak a credential or mishandle a body.
- Opt-in remote access — device pairing, standing terminal-control
  grants, per-session leases, and the audit trail. Anything that lets
  remote control happen without an explicit grant, or that lets a
  lease outlive its intended scope, is high priority.
- Hooks that execute in the context of an AI coding tool (Claude
  Code, Codex, Cursor, and the rest) — anything that lets hook
  registration or execution do more than the documented capture path.
- The MCP server (stdio subprocess) — tool definitions, path-safety
  gates on file-reading tools, and anything that could be used to
  exfiltrate data the operator didn't ask for.
- Node-side org enrolment: the enrolment exchange, the local key
  material it mints, and the privacy posture around what an enrolled
  node ships versus what it keeps local. (Deployments of the org
  rollup server itself are not part of this repository; report those
  privately through the same channels.)
- The secrets-scrubbing pipeline — cases where scrubbing silently
  fails to redact something it claims to handle.

**Out of scope:**

- Vulnerabilities in the third-party AI coding tools themselves
  (Claude Code, Codex, Cursor, etc.) — please report those upstream.
- Social engineering of maintainers or users.
- Issues that require an already-compromised machine to exploit (at
  that point the local database and config are already accessible to
  the attacker by design — Observer stores everything locally in
  plain SQLite, as documented in `PRIVACY.md`).
- Denial-of-service against your own local instance.

## A note on secrets scrubbing

Observer scrubs known secret patterns (API keys, tokens, common
credential shapes) from what it captures. That scrubbing is
best-effort, not a guarantee — please still review logs, transcripts,
or fixtures before pasting them into a public issue or discussion.
