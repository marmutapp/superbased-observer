// Package policy implements the guard layer's pure policy engine: the
// rule model, command/path analysis, and the single evaluation seam
// that turns a normalized agent action (Event) into a Verdict
// (allow / flag / ask / deny).
//
// Spec: docs/plans/guard-layer-implementation-spec-2026-06-10.md
// (§3.4 evaluation model, §4 policy model, §5 rule catalog, §17
// module invariants). Approved shapes: docs/plans/
// guard-g1-design-note-2026-06-10.md.
//
// # Purity contract
//
// This package is PURE in the cachetrack/freshness sense (spec §17.1):
// no SQL, no HTTP, no fsnotify, no os/exec, no filesystem access, and
// no imports from any other observer subsystem. Everything the engine
// needs arrives through Config (injected at construction — e.g. Home,
// because the package never calls os.UserHomeDir) or through the Event
// itself (resolved at the boundary by the caller — e.g. Cwd,
// ProjectRoot, Capabilities, Dialect). The invariant is enforced by
// TestPackageImports_Bounded, which also forbids path/filepath: path
// semantics here are deliberately OS-agnostic (see paths.go) so that
// the same Event yields the same Verdict regardless of which OS the
// daemon happens to run on (a Linux daemon evaluating a Windows
// client's command via the WSL bridge must agree with a Windows-native
// daemon).
//
// # The one seam
//
// Engine.Evaluate(Event) Verdict is the single entry point (spec
// §17.2). Hot paths (hook, proxy, ingest) hold only Event, Verdict and
// Capabilities values; rule types never leak past this package.
// Evaluate is deterministic and contains no recover(): the Q2
// fail-open/fail-closed wrapper belongs to the guard composition layer
// (internal/guard, G3) where the [guard] strict setting lives.
//
// # No source-identity branching
//
// Event.Tool exists for REPORTING only. Client differences are
// resolved into Capabilities and Dialect at the boundary; nothing in
// this package branches on tool identity (spec §17.3, enforced by
// TestNoToolIdentityBranching).
//
// # Table-driven rules
//
// Built-in rules are ordered table rows (rules_destructive.go,
// rules_boundary.go), each with a stable public ID from the spec §5
// catalog. The engine is a generic walker: safe patterns are evaluated
// BEFORE destructive matches, per command unit, and that ordering is
// owned by the engine itself (applyRule) so an individual rule author
// cannot forget it — the dcg lesson from the market sweep. A rule ID
// may span multiple table rows when the catalog itself splits decision
// by sub-shape (e.g. R-152 read vs write); validation keeps same-ID
// rows consistent in category and severity.
//
// # Command analysis
//
// shellparse.go owns semantic command parsing for POSIX-style shells:
// quote-aware tokenization, command-unit splitting, wrapper stripping
// (sudo, env, timeout, xargs, ...), recursive unwrap of `sh -c`-style
// payloads (≤5 levels), command substitution extraction, heredoc
// capture, and interpreter one-liner payload extraction. psparse.go
// owns the PowerShell and cmd.exe dialects (alias resolution,
// prefix-matched parameters, EncodedCommand decoding). Both produce
// the same Command shape so rules match on capability-style facts
// (recursive delete, force push, ...) rather than dialect-specific
// syntax. Parsing is analysis only — nothing is ever expanded,
// resolved against the filesystem, or executed.
package policy
