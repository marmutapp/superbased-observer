// Package machineid computes a best-effort, org-salted machine fingerprint
// used by Enterprise-Managed Tenancy to bind one managed node to one machine
// (Arc 4 P6a, docs/plans/org-admin-comprehensive-control-plane-2026-08-19.md
// §9).
//
// It is deliberately EVIDENCE, not PREVENTION. The fingerprint is derived from
// the most stable OS source available on the running host, but that host is
// controlled by the very developer the managed plane observes: the raw source
// can be edited (e.g. /etc/machine-id), a fresh VM or container can be spun up,
// or the binary patched. Under WSL2 the Linux source identifies the DISTRO, not
// the Windows host, so a second distro reads as a second machine. Cloned images
// and bare containers may share or lack a stable source entirely. The product
// therefore treats a machine identity as a dedup anchor and a tamper-evidence
// signal (a changed or colliding identity is surfaced to the org admin), while
// true prevention comes from OS/device ownership + managed-settings/MDM, per the
// §5 legal/ownership gate and the native-console template.
//
// The identity is salted with the org id and one-way hashed, so the same
// machine enrolling in two different orgs yields two unrelated identities (no
// cross-org machine correlation) and the org never learns the raw OS id.
//
// Module boundary (CLAUDE.md #1): the pure core — source ordering, salting, and
// hashing — carries no I/O. The OS reads (files, hostname, platform tools) live
// behind package-level func vars with OS defaults, overridable in tests.
package machineid
