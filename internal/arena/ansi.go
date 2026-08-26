package arena

import "regexp"

// stripANSIRe matches ANSI escape sequences (CSI + OSC + two-byte
// escapes) so deterministic consumers — judge prompts, stored excerpts —
// see clean text.
var stripANSIRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-Z\\-_])`)
