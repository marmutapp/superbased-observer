// Package remotecfg is the ONE owner of the remote-access arm/disarm/rotate
// transaction (dashboard-management-surface plan §B): mint-and-hash the pairing
// secret, pin a loopback backend, mutate + validate + persist [remote], and
// roll back the secret on any failure. It is called identically from the
// `observer remote enable|disable|rotate` CLI (a thin shell) and the dashboard
// `/api/remote/enable|disable|rotate` handlers, so arming is byte-identical by
// construction.
//
// It imports ONLY internal/config + internal/remoteauth (pinned by
// imports_test.go): no net/http, no dashboard, no cmd. BuildController stays in
// cmd (the return-type import direction would otherwise cycle) — under the
// restart-required model the dashboard never constructs a controller at
// runtime, so nothing outside cmd needs it.
package remotecfg
