// Package service is the pure-logic layer behind `observer service`:
// it renders the systemd unit that supervises `observer start` and
// constructs the systemctl/journalctl argv the command executes.
//
// Boundary: rendering + argv only. Systemd detection, unit-file writes,
// and process execution live in cmd/observer/service.go. See
// docs/teams-getting-started.md and docs/daemon-restart-runbook.md (the
// daemon IS the proxy on :8820 — restarting the service while a route
// is active drops that session).
package service
