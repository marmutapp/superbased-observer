// Package admission is the PURE input-admission engine for the obs
// subsystem's co-resident-custom-app deployment (admission spec,
// docs/plans/input-admission-guardrail-implementation-spec-2026-07-05.md).
//
// It evaluates an incoming end-user request against an admin-authored policy
// via a layered pipeline — cheap deterministic pre-filters first, an optional
// LLM judge only for the ambiguous middle — and returns an allow/flag/ask/deny
// verdict. It ships observe-first and reuses the obs eval plane's judge-hosting
// abstraction (local / provider / aggregator / private).
//
// Purity (obs plan §11, admission spec §2): this package imports no
// database/sql, net/http, or fsnotify — pinned by imports_test.go. Persistence
// lives in internal/obs/store, the judge network call is the host's (reached
// only through the injected JudgeClient interface), and config is translated
// into a PolicySpec AT THE BOUNDARY (Compile) so this package never imports
// internal/config. Downstream logic branches on the canonical Decision/Type
// vocabulary, never on source identity (CLAUDE.md rule #3).
//
// The one seam is Evaluate (pipeline.go): every enforcement point (SDK
// admit(), proxy pre-forward) calls it and gets a plain Result. The judge's
// types never leak past the boundary.
package admission
