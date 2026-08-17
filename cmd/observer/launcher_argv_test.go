package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestLauncherCmd builds a bare cobra.Command that stands in for a real
// `observer <tool>` launcher: it registers exactly the flag shapes
// splitLauncherArgs needs to exercise (a string flag, a bool flag with the
// pflag-implicit NoOptDefVal, and an int flag whose Set can fail) and
// deliberately does NOT register "resume", so any case involving --resume
// proves the not-registered ⇒ passthrough behavior.
func newTestLauncherCmd() (cmd *cobra.Command, config *string, verify *bool, fromMessage *int) {
	cmd = &cobra.Command{
		Use:   "testcmd [flags]",
		Short: "stand-in launcher for splitLauncherArgs tests",
		RunE:  func(*cobra.Command, []string) error { return nil },
	}
	config = cmd.Flags().String("config", "", "path to config file")
	verify = cmd.Flags().Bool("verify", false, "verify before launching")
	fromMessage = cmd.Flags().Int("from-message", 0, "resume from message index")
	return cmd, config, verify, fromMessage
}

// TestSplitLauncherArgs pins the exact case matrix backlog B6's plan draft
// specifies: every reserved-flag form (space, '=', bare bool), the "--"
// separator, unknown-flag passthrough (including the deliberately
// unregistered --resume), and the two malformed-usage error shapes.
func TestSplitLauncherArgs(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		want       []string
		wantErr    bool
		errContain string
		checkVars  func(t *testing.T, config *string, verify *bool, fromMessage *int)
	}{
		{
			name: "unknown long flag with space value passes through",
			argv: []string{"--model", "sonnet"},
			want: []string{"--model", "sonnet"},
		},
		{
			name: "unknown long flag with eq value passes through as one token",
			argv: []string{"--model=sonnet"},
			want: []string{"--model=sonnet"},
		},
		{
			name: "unknown bare long flag passes through",
			argv: []string{"--yolo"},
			want: []string{"--yolo"},
		},
		{
			name: "unknown flag followed by a dash-prefixed positional",
			argv: []string{"--model", "-sonnet"},
			want: []string{"--model", "-sonnet"},
		},
		{
			name: "positional with a space then an unknown flag pair",
			argv: []string{"fix bug", "--model", "sonnet"},
			want: []string{"fix bug", "--model", "sonnet"},
		},
		{
			name: "bare -- passes everything after it unconditionally, drops --",
			argv: []string{"--", "--print", "hi"},
			want: []string{"--print", "hi"},
		},
		{
			name: "reserved-looking flag after -- is NOT consumed",
			argv: []string{"--", "--config", "x"},
			want: []string{"--config", "x"},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if *config != "" {
					t.Fatalf("config = %q, want empty (default) since -- suppresses reserved matching", *config)
				}
			},
		},
		{
			name: "reserved string flag with space value consumes exactly one token",
			argv: []string{"--config", "/tmp/c.toml", "rest"},
			want: []string{"rest"},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if *config != "/tmp/c.toml" {
					t.Fatalf("config = %q, want /tmp/c.toml", *config)
				}
			},
		},
		{
			name: "reserved string flag with eq value consumes no extra token",
			argv: []string{"--config=/tmp/c.toml"},
			want: []string{},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if *config != "/tmp/c.toml" {
					t.Fatalf("config = %q, want /tmp/c.toml", *config)
				}
			},
		},
		{
			name: "reserved bool flag bare form does not consume the next token",
			argv: []string{"--verify", "rest"},
			want: []string{"rest"},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if !*verify {
					t.Fatalf("verify = false, want true")
				}
			},
		},
		{
			name: "reserved bool flag eq=false form",
			argv: []string{"--verify=false"},
			want: []string{},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if *verify {
					t.Fatalf("verify = true, want false")
				}
			},
		},
		{
			name:       "reserved string flag at end of argv with no value is malformed",
			argv:       []string{"--config"},
			wantErr:    true,
			errContain: "--config",
		},
		{
			name:       "reserved int flag rejects a non-integer value",
			argv:       []string{"--from-message", "abc"},
			wantErr:    true,
			errContain: "--from-message",
		},
		{
			name: "unregistered --resume passes through untouched",
			argv: []string{"--resume", "abc"},
			want: []string{"--resume", "abc"},
		},
		{
			name: "lone dash is a positional",
			argv: []string{"-"},
			want: []string{"-"},
		},
		{
			name: "reserved flag interleaved with unknown flag and positional",
			argv: []string{"--config", "/c.toml", "--model", "sonnet", "positional"},
			want: []string{"--model", "sonnet", "positional"},
			checkVars: func(t *testing.T, config *string, verify *bool, fromMessage *int) {
				if *config != "/c.toml" {
					t.Fatalf("config = %q, want /c.toml", *config)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, config, verify, fromMessage := newTestLauncherCmd()

			got, err := splitLauncherArgs(cmd, tc.argv)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitLauncherArgs(%v) returned nil error, want one mentioning %q", tc.argv, tc.errContain)
				}
				if tc.errContain != "" && !strings.Contains(err.Error(), tc.errContain) {
					t.Fatalf("splitLauncherArgs(%v) error = %q, want it to contain %q", tc.argv, err.Error(), tc.errContain)
				}
				return
			}

			if err != nil {
				t.Fatalf("splitLauncherArgs(%v) unexpected error: %v", tc.argv, err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				// Treat nil and empty-non-nil as equivalent "nothing passed
				// through" results — reflect.DeepEqual would otherwise fail
				// nil vs []string{}.
			} else if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitLauncherArgs(%v) = %#v, want %#v", tc.argv, got, tc.want)
			}

			if tc.checkVars != nil {
				tc.checkVars(t, config, verify, fromMessage)
			}
		})
	}
}

// TestReservedLauncherFlags pins that the reserved set is derived live from
// the command's own FlagSet: every registered long name is present as
// "--name", shorthands (when present) as "-x", and an unregistered name
// (here "resume") is absent so callers know it will fall through to
// passthrough.
func TestReservedLauncherFlags(t *testing.T) {
	cmd, _, _, _ := newTestLauncherCmd()

	reserved := reservedLauncherFlags(cmd.Flags())

	for _, want := range []string{"--config", "--verify", "--from-message"} {
		if _, ok := reserved[want]; !ok {
			t.Errorf("reservedLauncherFlags missing %q", want)
		}
	}
	for _, notWant := range []string{"--resume", "-r"} {
		if _, ok := reserved[notWant]; ok {
			t.Errorf("reservedLauncherFlags unexpectedly contains %q (not registered on the test command)", notWant)
		}
	}
}

// TestSplitFlagEq pins the '='-split contract splitLauncherArgs relies on:
// the name retains its leading dash(es), and hasEq distinguishes "no value
// present" from "value is the empty string".
func TestSplitFlagEq(t *testing.T) {
	cases := []struct {
		tok      string
		wantName string
		wantVal  string
		wantEq   bool
	}{
		{"--config=/tmp/c.toml", "--config", "/tmp/c.toml", true},
		{"--config", "--config", "", false},
		{"-c=/tmp/c.toml", "-c", "/tmp/c.toml", true},
		{"--config=", "--config", "", true},
	}
	for _, tc := range cases {
		name, val, hasEq := splitFlagEq(tc.tok)
		if name != tc.wantName || val != tc.wantVal || hasEq != tc.wantEq {
			t.Errorf("splitFlagEq(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.tok, name, val, hasEq, tc.wantName, tc.wantVal, tc.wantEq)
		}
	}
}

// TestLauncherArgsOrDone covers the launcherArgsOrDone wrapper's three
// outcomes: help shown (done, no error), a parse error (done, error), and
// ordinary success (not done, args returned) — using cobra's real
// InitDefaultHelpFlag registration + Help() output, exactly as a live
// launcher RunE would encounter it.
func TestLauncherArgsOrDone(t *testing.T) {
	t.Run("help via -h stops cleanly and writes help output", func(t *testing.T) {
		cmd, _, _, _ := newTestLauncherCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.InitDefaultHelpFlag()

		args, done, err := launcherArgsOrDone(cmd, []string{"-h"})

		if !done {
			t.Fatalf("done = false, want true")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if args != nil {
			t.Fatalf("args = %#v, want nil", args)
		}
		if buf.Len() == 0 {
			t.Fatalf("expected help output to be written, got empty buffer")
		}
		if !strings.Contains(buf.String(), "testcmd") {
			t.Fatalf("help output = %q, want it to mention the command's Use", buf.String())
		}
	})

	t.Run("help via --help after other tokens still stops cleanly", func(t *testing.T) {
		cmd, _, _, _ := newTestLauncherCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.InitDefaultHelpFlag()

		args, done, err := launcherArgsOrDone(cmd, []string{"--model", "sonnet", "-h"})

		if !done || err != nil {
			t.Fatalf("done, err = %v, %v; want true, nil", done, err)
		}
		if args != nil {
			t.Fatalf("args = %#v, want nil", args)
		}
		if buf.Len() == 0 || !strings.Contains(buf.String(), "testcmd") {
			t.Fatalf("expected non-empty help output mentioning the command's Use, got %q", buf.String())
		}
	})

	t.Run("parse error stops with done=true and the error surfaced", func(t *testing.T) {
		cmd, _, _, _ := newTestLauncherCmd()

		args, done, err := launcherArgsOrDone(cmd, []string{"--config"})

		if !done {
			t.Fatalf("done = false, want true")
		}
		if err == nil {
			t.Fatalf("err = nil, want a malformed-usage error")
		}
		if !strings.Contains(err.Error(), "--config") {
			t.Fatalf("err = %v, want it to mention --config", err)
		}
		if args != nil {
			t.Fatalf("args = %#v, want nil", args)
		}
	})

	t.Run("ordinary success returns args and done=false", func(t *testing.T) {
		cmd, config, _, _ := newTestLauncherCmd()

		args, done, err := launcherArgsOrDone(cmd, []string{"--config", "/tmp/c.toml", "rest"})

		if done {
			t.Fatalf("done = true, want false")
		}
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !reflect.DeepEqual(args, []string{"rest"}) {
			t.Fatalf("args = %#v, want [rest]", args)
		}
		if *config != "/tmp/c.toml" {
			t.Fatalf("config = %q, want /tmp/c.toml", *config)
		}
	})
}

// TestLauncherArgsOrDoneSentinelNotLeaked confirms the internal help
// sentinel never reaches a caller that only checks `err != nil` without
// going through launcherArgsOrDone's translation — a caller inspecting the
// raw splitLauncherArgs error WOULD see it (that's the point of the
// sentinel), but errors.Is must still recognize it correctly.
func TestLauncherArgsOrDoneSentinelNotLeaked(t *testing.T) {
	cmd, _, _, _ := newTestLauncherCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.InitDefaultHelpFlag()

	_, err := splitLauncherArgs(cmd, []string{"-h"})
	if !errors.Is(err, errLauncherHelpShown) {
		t.Fatalf("splitLauncherArgs(-h) error = %v, want errors.Is to match errLauncherHelpShown", err)
	}
}
