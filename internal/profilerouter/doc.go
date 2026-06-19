// Package profilerouter routes proxy traffic to per-profile
// compressor instances with session stickiness (usability arc §2.3
// decision 3, Track R).
//
// The router owns three pieces of state, all behind one mutex:
//
//   - an instance cache keyed by profile name, valid for the current
//     assignment version — instances are built lazily on first sight
//     of a profile and reused for every later session (the
//     "one immutable Pipeline per resolved profile" rule);
//   - a bounded sticky map from session ID to the instance the
//     session first resolved — a session keeps its instance for its
//     lifetime so stateful compression machinery (rolling summaries,
//     read caches) never sees a mid-session settings flip;
//   - an assignment version, bumped by Update, so profile or
//     assignment edits apply to NEW sessions without a daemon restart
//     while existing sessions stay pinned.
//
// The package is pure routing logic: what a "compressor" is, how a
// profile name resolves from a provider, and how an instance is built
// are all injected by the caller (cmd/observer wires these to
// internal/config resolution and conversation.Pipeline construction).
// No I/O, no config import, no compression import — per the module
// boundary rules in CLAUDE.md.
package profilerouter
