// Package patterns derives project_patterns rows from the observed action
// set (spec §15, §6.2 schema). Five pattern types:
//
//   - hot_file         — paths edited/read more than a baseline threshold.
//   - co_change        — pairs of files that change together within a session.
//   - common_command   — frequently-run shell commands.
//   - edit_test_pair   — "edit X ⇒ run Y" cadences, for predictive test hints.
//   - onboarding_seq   — the first 3 reads of a session (repeated across sessions).
//
// Each pattern gets a confidence in [0, 1] (observation_count / project
// baseline) and a per-type decay_half_life_days so stale hotspots decay
// faster than stable onboarding sequences. Callers drive this via
// `observer patterns` or the Deriver API directly.
package patterns
