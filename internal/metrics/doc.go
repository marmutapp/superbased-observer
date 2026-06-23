// Package metrics renders SuperBased Observer state as Prometheus text-format
// exposition for `/metrics` scraping. Zero dependency on the Prometheus
// client library — we emit a small, fixed set of gauges derived from
// [diag.Snapshot], [cost.Engine.Summary], and the session_pid_bridge table.
// See spec §25 (forward work — Prometheus metrics export).
package metrics
