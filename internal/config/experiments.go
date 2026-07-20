package config

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"
)

// This file implements profile EXPERIMENTS — productized A/B runs
// over Track-R profiles (usability arc P6.4 / review §8.2; the §2.4
// recipe-content-refresh methodology turned into a user feature).
//
// An experiment names a traffic class and two profiles. While it
// runs, sessions in that class are split deterministically between
// the control and candidate arms by a salted hash of the session ID —
// no restarts, no manual assignment flips, both arms live
// simultaneously under identical ambient conditions. Reporting
// recomputes each session's arm from the same hash, so no arm state
// is ever persisted (and nothing new enters the org-push surface).
//
// Experiments are ADVISORY measurement tooling, and resolution-only:
// they pick which PROFILE a session resolves, exactly as an
// assignment would. The enablement split is untouched — the master
// conversation switch stays the one gate, and a project that turned
// compression off stays off regardless of any experiment.
//
// Pure logic: no I/O, no clocks beyond parsing the stored timestamps.

// ExperimentConfig is one [[experiments]] entry in config.toml.
// Definitions are immutable once started (stop, then start a new one)
// — mutating arms mid-run would scramble the hash-recomputed arm
// attribution the report relies on.
type ExperimentConfig struct {
	// Name identifies the experiment (profile-name rules: lowercase
	// alnum + dashes). Salts the arm hash, so two experiments split
	// sessions independently.
	Name string `toml:"name" json:"name"`
	// Class is the traffic class the experiment owns while running:
	// "anthropic" / "openai" (provider classes) or "tool:<name>"
	// (pidbridge-resolved tool, R2). An ACTIVE experiment wins over
	// the assignment table AND project [profiles] overlays for its
	// class — an explicit, time-bounded operator action beats layered
	// configuration; stopping restores them.
	Class string `toml:"class" json:"class"`
	// Control / Candidate are the two arm profiles.
	Control   string `toml:"control" json:"control"`
	Candidate string `toml:"candidate" json:"candidate"`
	// StartedAt / StoppedAt are RFC3339 UTC. Empty StoppedAt = running.
	StartedAt string `toml:"started_at" json:"started_at"`
	StoppedAt string `toml:"stopped_at,omitempty" json:"stopped_at,omitempty"`
	// Note is free-form operator context ("A4: does cline need its
	// own profile?").
	Note string `toml:"note,omitempty" json:"note,omitempty"`
}

// Running reports whether the experiment is active.
func (e ExperimentConfig) Running() bool { return e.StartedAt != "" && e.StoppedAt == "" }

// experimentNameRx mirrors the profile-name allow-list.
var experimentNameRx = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidateExperiment checks a definition before it lands in config:
// name shape, class shape, distinct arms. Profile existence is the
// caller's job (the ProfileStore knows user profiles; this package
// only knows built-ins).
func ValidateExperiment(e ExperimentConfig) error {
	if !experimentNameRx.MatchString(e.Name) {
		return fmt.Errorf("config: invalid experiment name %q (lowercase letters, digits, dashes; max 64)", e.Name)
	}
	if err := ValidateExperimentClass(e.Class); err != nil {
		return err
	}
	if e.Control == "" || e.Candidate == "" {
		return fmt.Errorf("config: experiment %q needs both a control and a candidate profile", e.Name)
	}
	if e.Control == e.Candidate {
		return fmt.Errorf("config: experiment %q arms must differ (both %q)", e.Name, e.Control)
	}
	return nil
}

// ValidateExperimentClass accepts "anthropic", "openai", or
// "tool:<name>".
func ValidateExperimentClass(class string) error {
	if class == "anthropic" || class == "openai" {
		return nil
	}
	if name, ok := strings.CutPrefix(class, "tool:"); ok && name != "" {
		return nil
	}
	return fmt.Errorf("config: invalid experiment class %q (anthropic, openai, or tool:<name>)", class)
}

// ActiveExperimentFor returns the first RUNNING experiment matching
// the traffic class, or nil. Tool-class experiments match the
// pidbridge-resolved tool; provider-class experiments match the
// provider. First match wins — the start path refuses overlapping
// running experiments per class, so order is moot in practice.
func ActiveExperimentFor(experiments []ExperimentConfig, provider, tool string) *ExperimentConfig {
	for i := range experiments {
		e := &experiments[i]
		if !e.Running() {
			continue
		}
		if name, ok := strings.CutPrefix(e.Class, "tool:"); ok {
			if tool != "" && tool == name {
				return e
			}
			continue
		}
		if e.Class == provider {
			return e
		}
	}
	return nil
}

// ExperimentArm deterministically assigns a session to an arm:
// FNV-1a over (experiment name NUL session id), avalanched, low bit
// picks. The same (experiment, session) always lands on the same arm
// — the report recomputes membership with this exact function instead
// of persisting arm state anywhere.
//
// The murmur3-style finalizer matters: a raw FNV-1a low bit is a pure
// parity function of the input bits (XOR folds through the odd-prime
// multiply), so two experiment names with equal bit-parity would
// split every session identically — the finalizer's shifts diffuse
// every input bit into bit 0.
func ExperimentArm(e ExperimentConfig, sessionID string) (profile, arm string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(e.Name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionID))
	x := h.Sum32()
	x ^= x >> 16
	x *= 0x85ebca6b
	x ^= x >> 13
	x *= 0xc2b2ae35
	x ^= x >> 16
	if x&1 == 0 {
		return e.Control, "control"
	}
	return e.Candidate, "candidate"
}

// ResolveProfileNameForSession is ResolveProfileName with the
// experiment tier on top: an active experiment matching the class
// claims the session and returns its hash-assigned arm. Sessions the
// extractor couldn't identify (empty sessionID — the per-request
// path) never enter experiments: they resolve per the plain table so
// behavior stays deterministic without stickiness.
func ResolveProfileNameForSession(pc ProfilesConfig, experiments []ExperimentConfig, provider, tool, sessionID string) (profile, experimentName, arm string) {
	if sessionID != "" {
		if e := ActiveExperimentFor(experiments, provider, tool); e != nil {
			p, a := ExperimentArm(*e, sessionID)
			return p, e.Name, a
		}
	}
	return ResolveProfileName(pc, provider, tool), "", ""
}

// ExperimentWindow parses the experiment's [start, end] reporting
// window; a running experiment ends at now.
func ExperimentWindow(e ExperimentConfig, now time.Time) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339, e.StartedAt)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("config: experiment %q started_at: %w", e.Name, err)
	}
	end := now
	if e.StoppedAt != "" {
		end, err = time.Parse(time.RFC3339, e.StoppedAt)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("config: experiment %q stopped_at: %w", e.Name, err)
		}
	}
	return start.UTC(), end.UTC(), nil
}
