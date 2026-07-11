package heuristic

import (
	"context"
	"reflect"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
)

// wantSym is an expected (Name, Kind, StartLine) symbol.
type wantSym struct {
	name  string
	kind  string
	start int
}

func TestParse_Table(t *testing.T) {
	cases := []struct {
		name     string
		lang     codeintel.Language
		filename string
		src      string
		wantSyms []wantSym
	}{
		{
			name:     "python",
			lang:     parse.LangPython,
			filename: "mod.py",
			src: `import os
from collections import OrderedDict as OD

class Greeter:
    def __init__(self, name):
        self.name = name

    def greet(self):
        print("hi", self.name)

def main():
    g = Greeter("x")
    g.greet()
`,
			wantSyms: []wantSym{
				{"Greeter", "class", 4},
				{"__init__", "function", 5},
				{"greet", "function", 8},
				{"main", "function", 11},
			},
		},
		{
			name:     "typescript",
			lang:     parse.LangTypeScript,
			filename: "svc.ts",
			src: `import { Foo } from "./foo";
import React from "react";

export interface Service {
  run(): void;
}

export type ID = string;

export class Server {
  start() {
    listen(8080);
  }
}

export function boot() {
  const s = new Server();
  s.start();
}

const handler = (x: number) => {
  process(x);
};
`,
			wantSyms: []wantSym{
				{"Service", "interface", 4},
				{"ID", "type", 8},
				{"Server", "class", 10},
				{"boot", "function", 16},
				{"handler", "function", 21},
			},
		},
		{
			name:     "rust",
			lang:     parse.LangRust,
			filename: "lib.rs",
			src: `use std::collections::HashMap;
use crate::util::helper;

pub struct Point {
    x: i32,
    y: i32,
}

pub trait Shape {
    fn area(&self) -> f64;
}

pub enum Color {
    Red,
    Green,
}

impl Point {
    pub fn new(x: i32, y: i32) -> Self {
        Point { x, y }
    }
}

fn compute() -> i32 {
    let p = Point::new(1, 2);
    add(p.x, p.y)
}
`,
			wantSyms: []wantSym{
				{"Point", "class", 4},
				{"Shape", "interface", 9},
				{"area", "function", 10},
				{"Color", "type", 13},
				{"Point", "class", 18}, // impl Point
				{"new", "function", 19},
				{"compute", "function", 24},
			},
		},
	}

	p := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := p.Parse(context.Background(), []byte(tc.src), tc.lang, tc.filename)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			// ExactSpans MUST be false for every served language.
			if res.Capability.ExactSpans {
				t.Errorf("ExactSpans = true, want false (heuristic is non-destructive)")
			}
			if !res.Capability.Symbols || !res.Capability.Imports || !res.Capability.Calls {
				t.Errorf("served capability flags = %+v, want all of Symbols/Imports/Calls true", res.Capability)
			}
			// And via the Capabilities accessor.
			if cap := p.Capabilities(tc.lang); cap.ExactSpans {
				t.Errorf("Capabilities(%s).ExactSpans = true, want false", tc.lang)
			}

			// Every expected symbol must be present with matching kind+start.
			for _, ws := range tc.wantSyms {
				if !hasSym(res.Nodes, ws) {
					t.Errorf("missing symbol %+v\n  got nodes: %s", ws, dumpNodes(res.Nodes))
				}
			}

			if len(res.Imports) == 0 {
				t.Errorf("expected at least one import, got none")
			}
			if len(res.Calls) == 0 {
				t.Errorf("expected at least one call site, got none")
			}
		})
	}
}

// TestParse_PythonImportAlias checks the alias capture path.
func TestParse_PythonImportAlias(t *testing.T) {
	p := New()
	res, err := p.Parse(context.Background(), []byte("import numpy as np\n"), parse.LangPython, "x.py")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Imports) != 1 {
		t.Fatalf("imports = %d, want 1", len(res.Imports))
	}
	if got := res.Imports[0]; got.Path != "numpy" || got.Alias != "np" {
		t.Errorf("import = %+v, want Path=numpy Alias=np", got)
	}
}

// TestParse_CallEnclosing verifies a call inside a function attaches to it.
func TestParse_CallEnclosing(t *testing.T) {
	p := New()
	src := "def outer():\n    inner_call()\n"
	res, err := p.Parse(context.Background(), []byte(src), parse.LangPython, "x.py")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var found bool
	for _, c := range res.Calls {
		if c.Name == "inner_call" {
			found = true
			if c.Enclosing < 0 || res.Nodes[c.Enclosing].Name != "outer" {
				t.Errorf("inner_call enclosing = %d, want index of 'outer'", c.Enclosing)
			}
		}
	}
	if !found {
		t.Errorf("call 'inner_call' not captured")
	}
}

// TestCapabilities_Unserved confirms go (and unknown langs) get the zero
// capability and an empty parse.
func TestCapabilities_Unserved(t *testing.T) {
	p := New()
	if cap := p.Capabilities(parse.LangGo); cap != (codeintel.LanguageCapability{}) {
		t.Errorf("Capabilities(go) = %+v, want zero (go/ast owns Go)", cap)
	}
	res, err := p.Parse(context.Background(), []byte("func main() {}\n"), parse.LangGo, "main.go")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Nodes) != 0 || len(res.Imports) != 0 || len(res.Calls) != 0 {
		t.Errorf("unserved lang produced output: %+v", res)
	}
}

// TestLanguagesServed asserts the exact served set (NOT go).
func TestLanguagesServed(t *testing.T) {
	want := []codeintel.Language{
		parse.LangPython, parse.LangTypeScript, parse.LangTSX, parse.LangJavaScript,
		parse.LangJSX, parse.LangRust, parse.LangJava, parse.LangC, parse.LangCPP,
		parse.LangCSharp, parse.LangRuby, parse.LangPHP, parse.LangKotlin,
		parse.LangSwift, parse.LangScala, parse.LangBash, parse.LangLua,
	}
	got := New().Languages()
	set := map[codeintel.Language]bool{}
	for _, l := range got {
		set[l] = true
	}
	if set[parse.LangGo] {
		t.Errorf("go must NOT be served by heuristic")
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("language %s not served", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("served count = %d, want %d (%v)", len(got), len(want), got)
	}
}

// TestParse_Deterministic parses the same input twice and requires
// byte-identical results.
func TestParse_Deterministic(t *testing.T) {
	p := New()
	src := []byte(`import { a } from "m";
export class C {
  m() { call(); other(); }
}
export function f() { go_it(); }
`)
	a, err := p.Parse(context.Background(), src, parse.LangTypeScript, "x.ts")
	if err != nil {
		t.Fatalf("Parse a: %v", err)
	}
	b, err := p.Parse(context.Background(), src, parse.LangTypeScript, "x.ts")
	if err != nil {
		t.Fatalf("Parse b: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Parse not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}

// TestParse_NoPanic feeds malformed / pathological input across every
// served language and requires no panic and no error.
func TestParse_NoPanic(t *testing.T) {
	p := New()
	inputs := [][]byte{
		nil,
		[]byte(""),
		[]byte("\n\n\n"),
		[]byte("{{{{{{{{"),
		[]byte("}}}}}}}}"),
		[]byte("class \"unterminated string and brace {"),
		[]byte("def ("),
		[]byte("function function function ("),
		[]byte("\"\\"),                           // dangling escape
		[]byte("`unterminated backtick"),         //
		[]byte("import import import import"),    //
		[]byte("    \t  \t   indented forever "), //
		[]byte("é日本語 ("),                         // multibyte + call paren
	}
	for _, lang := range p.Languages() {
		for _, in := range inputs {
			res, err := p.Parse(context.Background(), in, lang, "f")
			if err != nil {
				t.Errorf("Parse(%s) errored on %q: %v", lang, in, err)
			}
			_ = res
		}
	}
}

func hasSym(nodes []codeintel.Node, w wantSym) bool {
	for _, n := range nodes {
		if n.Name == w.name && n.Kind == w.kind && n.StartLine == w.start {
			return true
		}
	}
	return false
}

func dumpNodes(nodes []codeintel.Node) string {
	s := ""
	for _, n := range nodes {
		s += "\n    " + n.Kind + " " + n.Name + " @" + itoa(n.StartLine)
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
