// Package scrub redacts secrets (Bearer tokens, API keys, AWS keys,
// connection-string passwords, env-var assignments) from tool inputs before
// they reach storage. See spec §8.
//
// Scrubbing runs at the adapter boundary — original unscrubbed data is never
// written to disk.
package scrub
