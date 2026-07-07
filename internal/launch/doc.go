// Package launch builds and executes best-effort "open a terminal
// running this AI tool" plans for the dashboard launch button
// (usability arc P4.6 / review row L2b).
//
// The package is the spec's pure-package shape: Plan is a pure
// table-driven walk over an Environment value the caller resolves
// (Detect on the real host, literals in tests), and Spawn is the one
// I/O seam, stubbed by the dashboard handler's tests.
//
// Honesty contract: a Spec ALWAYS carries the copy-paste Command, and
// spawning is best-effort on top — when no mechanism exists (headless
// host, no interop) Plan returns an empty Argv with the reason, and
// Spawn failures surface to the caller instead of being swallowed.
// The button never fakes success.
//
// Process-creation guard: every Argv element is either a hardcoded
// literal, a LookPath-resolved well-known binary name, the daemon's
// own executable path, or the WSL distro name from the daemon's
// environment. The only user-controlled input is the tool name,
// validated against the hardcoded allow-list in innerFor.
package launch
