// Package opencode implements a snapshot-style adapter for OpenCode desktop
// state persisted in opencode.db. The adapter treats assistant-message
// message.data.tokens as the authoritative token source, because OpenCode's
// step-finish parts duplicate that bundle exactly while older session-level
// aggregate columns can be stale or zero.
package opencode
