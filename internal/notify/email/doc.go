// Package email is the SuperBased Observer email notification channel: a
// reusable SMTP composer + delivery client that rides the EXISTING alert
// evaluators (node-side [observability.alerts], org-server budget alerts, and
// org-server obs-alert rules) as an ADDITIONAL delivery target alongside their
// webhooks.
//
// # Scope and network posture
//
// This package performs outbound network I/O (SMTP), so — like the guard cloud
// webhooks and the org server's webhook dispatch — it is reached ONLY from
// explicitly-configured, opt-in alert paths and from the org server process.
// It is NEVER called from the observer watcher, hooks, or any hot capture path
// (CLAUDE.md "no network calls in the observer/watcher"). Every send is gated
// by [email].enabled (default false) AND a per-consumer opt-in, so a default
// install stays egress-free.
//
// # Fail-soft
//
// Delivery is best-effort by contract. The high-level Notifier.Send never
// returns an error and never panics: a composition or SMTP failure logs a
// warning at most and the caller (an alert evaluator) proceeds unaffected. An
// email failure must never block or fail evaluation.
//
// # Compose vs send
//
// Composition (Compose, building a Message from a payload) is deliberately
// separated from delivery (Sender.Send). Compose is pure and reusable — the
// same seam will back the future scheduled report digests (gap-register G13).
//
// # Security
//
// The resolved SMTP password is kept out of every diagnostic surface:
// Config.String and Config.Redacted never reveal it, and credentials are never
// logged. Config.Resolve reads the password from an environment variable
// (PasswordEnv, preferred) or a secret file (PasswordFile) so no secret need
// live in a config file; a direct Password field is supported for convenience
// but is redacted from any dump/diagnostic.
//
// # Standard library only
//
// Delivery uses net/smtp with a small custom smtp.Auth (LOGIN, and a
// TLS-aware PLAIN) — no third-party dependency and no CGO. STARTTLS, implicit
// TLS, AUTH PLAIN/LOGIN, multiple recipients, plaintext+HTML multipart, and a
// context deadline are all supported.
package email
