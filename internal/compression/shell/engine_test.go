package shell

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLookupPrecedence(t *testing.T) {
	e := NewEngine()
	generic := FilterSpec{Command: "git", Description: "generic"}
	specific := FilterSpec{Command: "git", Subcommands: []string{"status"}, Description: "status"}
	other := FilterSpec{Command: "git", Subcommands: []string{"diff"}, Description: "diff"}
	e.AddSpec(generic)
	e.AddSpec(specific)
	e.AddSpec(other)

	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"git", "status", "-s"}, "status"},
		{[]string{"git", "diff"}, "diff"},
		{[]string{"git", "log"}, "generic"},
		{[]string{"/usr/bin/git", "status"}, "status"},
		{[]string{"go", "test"}, ""},
		{[]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			got := e.Lookup(tc.argv)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil || got.Description != tc.want {
				t.Fatalf("want %q, got %+v", tc.want, got)
			}
		})
	}
}

func TestAddSpecReplacesOnSameKey(t *testing.T) {
	e := NewEngine()
	e.AddSpec(FilterSpec{Command: "git", Description: "first"})
	e.AddSpec(FilterSpec{Command: "git", Description: "second"})
	specs := e.Specs()
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	if specs[0].Description != "second" {
		t.Fatalf("expected replacement, got %+v", specs[0])
	}
}

func TestLoadFSSingleSpecShape(t *testing.T) {
	fsys := singleFS("git.toml", `
command = "git"
description = "strip git hints"
[[rule]]
type = "line_drop"
pattern = '^hint:'
`)
	e := NewEngine()
	if err := e.LoadFS(fsys); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	specs := e.Specs()
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %d", len(specs))
	}
	if specs[0].Command != "git" || len(specs[0].Rules) != 1 {
		t.Fatalf("unexpected spec: %+v", specs[0])
	}
}

func TestLoadFSMultiSpecShape(t *testing.T) {
	fsys := singleFS("bundle.toml", `
[[filter]]
command = "git"
[[filter.rule]]
type = "squash_whitespace"

[[filter]]
command = "go"
subcommands = ["test"]
[[filter.rule]]
type = "line_drop"
pattern = '^\s*$'
`)
	e := NewEngine()
	if err := e.LoadFS(fsys); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if len(e.Specs()) != 2 {
		t.Fatalf("want 2 specs, got %d", len(e.Specs()))
	}
}

func TestLoadFSRejectsInvalidSpec(t *testing.T) {
	fsys := singleFS("bad.toml", `
command = "git"
[[rule]]
type = "line_drop"
# missing pattern
`)
	e := NewEngine()
	if err := e.LoadFS(fsys); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadFSRejectsBadRegex(t *testing.T) {
	fsys := singleFS("bad.toml", `
command = "git"
[[rule]]
type = "line_drop"
pattern = "[unterminated"
`)
	e := NewEngine()
	if err := e.LoadFS(fsys); err == nil {
		t.Fatal("expected regex compile error")
	}
}

func TestLoadFSIgnoresNonToml(t *testing.T) {
	fsys := singleFS("README.md", "not a filter")
	e := NewEngine()
	if err := e.LoadFS(fsys); err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if len(e.Specs()) != 0 {
		t.Fatal("should not have loaded anything")
	}
}

func TestLoadDirMissingDirOK(t *testing.T) {
	e := NewEngine()
	if err := e.LoadDir(filepath.Join(os.TempDir(), "sb-shell-nonexistent-xyz")); err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
}

func TestLoadDirUserOverrides(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "git.toml"), `
command = "git"
description = "user override"
[[rule]]
type = "squash_whitespace"
`)
	e := NewEngine()
	e.AddSpec(FilterSpec{Command: "git", Description: "baked-in"})
	if err := e.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	specs := e.Specs()
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %d", len(specs))
	}
	if specs[0].Description != "user override" {
		t.Fatalf("override not applied: %+v", specs[0])
	}
}

func TestLoadDefaultsEmbedded(t *testing.T) {
	// Just confirms the embed compiled and the placeholder parses cleanly.
	// No specs are registered by the current default bundle.
	e := NewEngine()
	if err := e.LoadDefaults(); err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
}

func TestHookRegistryAndExecution(t *testing.T) {
	e := NewEngine()
	state := &stateful{}
	e.RegisterHook("count", func() Rule {
		return &countingRule{state: state}
	})
	e.AddSpec(FilterSpec{
		Command: "count-cmd",
		Rules:   []RuleSpec{{Type: "hook", Hook: "count"}},
	})

	var out bytes.Buffer
	s, err := e.Filter([]string{"count-cmd"}, &out)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	io.WriteString(s, "a\nb\nc\n")
	s.Close()

	if state.calls != 3 {
		t.Fatalf("want 3 hook Push calls, got %d", state.calls)
	}
	if state.final != 1 {
		t.Fatalf("want 1 hook Flush call, got %d", state.final)
	}
	if !strings.Contains(out.String(), "a") {
		t.Fatalf("hook rule should have emitted lines, got %q", out.String())
	}
}

func TestUnknownHookReturnsError(t *testing.T) {
	e := NewEngine()
	e.AddSpec(FilterSpec{
		Command: "x",
		Rules:   []RuleSpec{{Type: "hook", Hook: "missing"}},
	})
	if _, err := e.Filter([]string{"x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected hook-not-registered error")
	}
}

func TestHookFactoryIsolatesState(t *testing.T) {
	e := NewEngine()
	e.RegisterHook("counter", func() Rule {
		return &lineCounter{}
	})
	e.AddSpec(FilterSpec{
		Command: "x",
		Rules:   []RuleSpec{{Type: "hook", Hook: "counter"}},
	})
	var b1, b2 bytes.Buffer
	s1, _ := e.Filter([]string{"x"}, &b1)
	s2, _ := e.Filter([]string{"x"}, &b2)
	io.WriteString(s1, "one\ntwo\n")
	io.WriteString(s2, "a\n")
	s1.Close()
	s2.Close()
	if got := strings.TrimSpace(b1.String()); got != "1\n2" {
		t.Fatalf("s1 got %q", b1.String())
	}
	if got := strings.TrimSpace(b2.String()); got != "1" {
		t.Fatalf("s2 got %q", b2.String())
	}
}

// --- helpers --------------------------------------------------------------

type stateful struct {
	calls int
	final int
}

type countingRule struct {
	state *stateful
}

func (r *countingRule) Push(line string, emit func(string)) {
	r.state.calls++
	emit(line)
}
func (r *countingRule) Flush(emit func(string)) { r.state.final++ }

type lineCounter struct{ n int }

func (r *lineCounter) Push(_ string, emit func(string)) {
	r.n++
	emit(fmt.Sprintf("%d", r.n))
}
func (r *lineCounter) Flush(emit func(string)) {}

// singleFS returns a one-file fs.FS usable by Engine.LoadFS.
func singleFS(name, contents string) fs.FS {
	return fstest.MapFS{name: {Data: []byte(contents)}}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
