package livesym

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureFile writes content to a path under t.TempDir() and returns
// the absolute path. The extension chooses the parser language.
func fixtureFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestParse_Go pins the Go pattern set: functions, methods on both
// value + pointer receivers, struct types, interfaces, generic types.
func TestParse_Go(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "x.go", `package main

import "fmt"

func handleClick(e int) {
	fmt.Println("click")
}

func (e *Editor) handleKeyDown(k int) {
	_ = k
}

func (e Editor) Render() string {
	return ""
}

type Editor struct {
	x int
}

type Renderer interface {
	Render() string
}

type Alias = Editor
`)
	syms, unsupported, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if unsupported {
		t.Errorf(".go should be supported")
	}
	want := map[string]string{
		"handleClick":   "function",
		"handleKeyDown": "method",
		"Render":        "method",
		"Editor":        "type",
		"Renderer":      "interface",
		"Alias":         "type",
	}
	got := map[string]string{}
	for _, s := range syms {
		got[s.Name] = s.Kind
		if s.Language != "go" {
			t.Errorf("Language: got %q, want go for %q", s.Language, s.Name)
		}
		if s.EndLine != -1 {
			t.Errorf("EndLine: got %d, want -1 for %q", s.EndLine, s.Name)
		}
		if s.StartLine <= 0 {
			t.Errorf("StartLine non-positive for %q: %d", s.Name, s.StartLine)
		}
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %q: got kind %q, want %q (all syms: %v)", name, got[name], kind, got)
		}
	}
}

// TestParse_TypeScript pins the TS/TSX pattern set.
func TestParse_TypeScript(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "Editor.tsx", `import React from "react";

export function handleClick(e: MouseEvent) {
}

export async function handleKeyDown(k: KeyboardEvent) {
}

export class Editor extends React.Component {
}

interface Props {
  x: number;
}

export type State = { selected: boolean };
`)
	syms, unsupported, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if unsupported {
		t.Errorf(".tsx should be supported")
	}
	want := map[string]string{
		"handleClick":   "function",
		"handleKeyDown": "function",
		"Editor":        "class",
		"Props":         "interface",
		"State":         "type",
	}
	got := map[string]string{}
	for _, s := range syms {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %q: got kind %q, want %q (all syms: %v)", name, got[name], kind, got)
		}
	}
}

// TestParse_Python pins def + async def + class.
func TestParse_Python(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "x.py", `import os

def handle_click(e):
    pass

async def handle_async(e):
    pass

class Editor:
    pass

class Renderer(Base):
    pass
`)
	syms, unsupported, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if unsupported {
		t.Errorf(".py should be supported")
	}
	want := map[string]string{
		"handle_click": "function",
		"handle_async": "function",
		"Editor":       "class",
		"Renderer":     "class",
	}
	got := map[string]string{}
	for _, s := range syms {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %q: got kind %q, want %q", name, got[name], kind)
		}
	}
}

// TestParse_JavaScript: function + class but no interface or type.
func TestParse_JavaScript(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "x.js", `function add(a, b) { return a + b; }
class Foo {}
export async function bar() {}
`)
	syms, _, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"add": "function",
		"Foo": "class",
		"bar": "function",
	}
	got := map[string]string{}
	for _, s := range syms {
		got[s.Name] = s.Kind
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("symbol %q: got %q, want %q", name, got[name], kind)
		}
	}
}

// TestParse_UnsupportedExtension: extensions outside the supported
// set return langUnsupported=true with no error and no symbols.
func TestParse_UnsupportedExtension(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"x.csv", "y.rb", "z.unknown", "Makefile"} {
		path := fixtureFile(t, name, "content")
		syms, unsupported, err := Parse(path)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if !unsupported {
			t.Errorf("%s: expected langUnsupported=true", name)
		}
		if syms != nil {
			t.Errorf("%s: expected nil syms, got %v", name, syms)
		}
	}
}

// TestParse_EmptyFile: an empty supported-extension file produces no
// syms but no error.
func TestParse_EmptyFile(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "empty.go", "")
	syms, unsupported, err := Parse(path)
	if err != nil || unsupported {
		t.Errorf("got err=%v unsupported=%v", err, unsupported)
	}
	if len(syms) != 0 {
		t.Errorf("got %d syms, want 0", len(syms))
	}
}

// TestParse_StartLineMonotonic: returned syms appear in source order
// (StartLine ascending). Critical for the drift-signal diff in
// commit 4 where we pair index symbols with live symbols by ordering.
func TestParse_StartLineMonotonic(t *testing.T) {
	t.Parallel()
	path := fixtureFile(t, "x.go", `package main
func a() {}
func b() {}
func c() {}
`)
	syms, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(syms); i++ {
		if syms[i].StartLine <= syms[i-1].StartLine {
			t.Errorf("not monotonic at idx %d: %v", i, syms)
		}
	}
}

// TestParse_MissingFile: nonexistent path surfaces an error.
func TestParse_MissingFile(t *testing.T) {
	t.Parallel()
	if _, _, err := Parse("/nonexistent/path.go"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

// TestParse_EmptyPath: explicit empty-path guard.
func TestParse_EmptyPath(t *testing.T) {
	t.Parallel()
	if _, _, err := Parse(""); err == nil {
		t.Errorf("expected error for empty path")
	}
}
