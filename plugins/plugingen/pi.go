package main

import (
	"strings"
)

// ---------------------------------------------------------------------
// Pi (earendil-works) — coverage wave C, plan §7 row "pi".
//
// THE DECISION: no extension package is built. This surface ships a
// README explaining that Pi needs no plugin, because every artifact one
// could build here would declare something observer cannot serve.
//
// The §7 verdict sketched "a `pi install`-able npm/git package containing
// a TS extension that fires our hook receiver on pi.on(...)". Three pieces
// of evidence, checked in this repository rather than assumed, say that
// package would be a facade:
//
//  1. **There is no hook receiver to fire.** `observer hook <tool>`
//     dispatches on exactly four tools — claude-code, cursor, codex and
//     hermes (cmd/observer/hook.go). `observer hook pi` is not a command;
//     an extension calling it would shell out to an error. Adding a
//     receiver is an adapter change (cmd/observer + internal/adapter),
//     not a packaging change, and it would need its own grounding pass.
//     internal/integration records HookMechanism None for pi.
//  2. **Capture is already complete without one.** internal/adapter/pi
//     parses Pi's own session JSONL under ~/.pi/agent/sessions —
//     HandoffCapability.Transcript is TranscriptFull for pi, and the
//     watcher needs no cooperation from Pi to read it. A hook receiver
//     would re-report events the transcript already carries.
//  3. **Pi has no MCP client, by design.** Its own Philosophy page states
//     "No MCP. Build CLI tools with READMEs (see Skills), or build an
//     extension that adds MCP support." So the MCP entry every other
//     surface in this repository publishes has no target here, and
//     internal/integration records MCP: nil for pi.
//
// What IS worth telling a Pi user is therefore: the watcher already has
// you; the `observer pi` launcher adds wire-accurate token counts through
// the documented custom-provider route (RouteProviderJSON, live-verified
// 2026-06-27, cmd/observer/pi.go); and observer's CLI is a CLI tool, which
// is exactly the extension mechanism Pi's philosophy points you at.
//
// This is a listing, not a stub: if `observer hook pi` ever exists, the
// package the §7 verdict describes becomes buildable and this file is
// where that decision gets revisited.
// ---------------------------------------------------------------------

// piSurfaceDir is this surface's directory in the in-tree layout.
const piSurfaceDir = "pi"

func piReadme() string {
	var b strings.Builder
	b.WriteString(generatedBanner)
	b.WriteString(`
# SuperBased Observer — Pi: no plugin needed

Local-first token, cost and cache observability for
[Pi](https://pi.dev/) — with **nothing to install into Pi**.

Every other artifact in this repository is a wiring layer: a manifest or a
config block that declares observer's MCP server inside some tool. Pi needs
none of them, and this page explains why rather than shipping a package that
would do nothing.

**` + binaryPrereqSentence + `** — that is the
only prerequisite, and it is the same binary every other surface here points
at:

` + "```bash" + `
npm i -g @superbased/observer     # or: pipx install superbased-observer
observer start                    # run the local daemon
` + "```" + `

## Install

There is no Pi package to install. Start the observer daemon (above) and use
Pi normally — observer's watcher reads Pi's own session transcripts under
` + "`~/.pi/agent/sessions`" + ` as they are written.

For wire-accurate token counts, launch Pi through observer:

` + "```bash" + `
observer pi                       # or: observer pi --resume <session-id>
` + "```" + `

That writes an ` + "`" + `observer` + "`" + ` provider into
` + "`~/.pi/agent/models.json`" + ` — Pi's own documented custom-provider
mechanism, with a ` + "`baseUrl`" + ` pointing at the local proxy and an API key
field that names an environment variable rather than storing a secret — and
runs ` + "`pi --provider observer`" + `. Token counts then come from the wire
instead of a self-report. Pi's built-in providers ignore
` + "`OPENAI_BASE_URL`" + `, which is why the launcher writes a provider instead
of setting an environment variable.

## What it wires

Nothing. That is the point of this page. Three reasons, each checkable:

| Artifact one could ship | Why it is not shipped |
|---|---|
| An MCP server entry | Pi has **no MCP client**. Its own Philosophy page says: "No MCP. Build CLI tools with READMEs (see Skills), or build an extension that adds MCP support." An MCP entry would have nothing to read it. |
| A Pi extension forwarding lifecycle events to an observer hook | There is **no hook to forward to**. ` + "`observer hook`" + ` accepts claude-code, cursor, codex and hermes; ` + "`observer hook pi`" + ` is not a command. An extension calling it would shell out to an error. |
| An extension duplicating capture | Capture is **already complete**. Observer's Pi adapter reads Pi's full session transcript directly, with no cooperation from Pi required. An extension would re-report what the watcher already has. |

Pi's own answer to "how do I extend this" is *build a CLI tool with a
README*. Observer is a CLI tool: ` + "`observer predict`" + `,
` + "`observer cost`" + `, ` + "`observer sessions`" + ` and the rest are available
to Pi through its ordinary shell access, no wiring required.

Captured data lands in the local ` + "`~/.observer/observer.db`" + `.

## If that changes

A Pi extension is a real, documented artifact — a TypeScript module exporting
` + "`(pi: ExtensionAPI) => {…}`" + `, distributed as a "Pi Package" (a
` + "`pi`" + ` key in ` + "`package.json`" + `) and installed with
` + "`pi install npm:<pkg>`" + ` or ` + "`pi install git:<url>`" + `. The moment
observer grows a Pi hook receiver, that package becomes worth building. Until
then, publishing one would be shipping a shell.

## Double-wiring

Not possible here: there is nothing to install, so nothing can be installed
twice. ` + "`observer init`" + ` writes no Pi config either — the
` + "`observer pi`" + ` launcher writes the provider entry, idempotently, and
only when you use it.

## Links

- Docs: ` + homepage + `
- Source: https://github.com/superbasedapp/observer
- License: ` + license + `
`)
	return b.String()
}
