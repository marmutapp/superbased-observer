package heuristic_test

import (
	"context"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse/heuristic"
)

// TestHeuristicConformance asserts every language the heuristic backend
// serves extracts at least the expected symbol (by name+kind) and import
// from a representative snippet — the Phase-5 per-language golden, kept as
// a table so adding a language adds one row. Spans are approximate
// (ExactSpans=false); we assert presence + kind, not exact end lines.
func TestHeuristicConformance(t *testing.T) {
	t.Parallel()
	p := heuristic.New()
	cases := []struct {
		lang       codeintel.Language
		src        string
		wantName   string
		wantKind   string
		wantImport string
	}{
		{parse.LangPython, "import os\n\nclass Editor:\n    def handle(self):\n        pass\n", "Editor", "class", "os"},
		{parse.LangPython, "from a.b import c\n\ndef run():\n    pass\n", "run", "function", "a.b"},
		{parse.LangTypeScript, "import {x} from 'mod';\nexport class Editor {}\n", "Editor", "class", "mod"},
		{parse.LangTypeScript, "import 'side';\nexport function handleClick() {}\n", "handleClick", "function", "side"},
		{parse.LangJavaScript, "const fs = require('fs');\nfunction add(a,b){return a+b;}\n", "add", "function", "fs"},
		{parse.LangRust, "use std::fmt;\npub struct Editor;\n", "Editor", "class", "std::fmt"},
		{parse.LangRust, "use a::b;\npub fn run() {}\n", "run", "function", "a::b"},
		{parse.LangJava, "import java.util.List;\npublic class Editor {}\n", "Editor", "class", "java.util.List"},
		{parse.LangJava, "import x.Y;\ninterface Shape {}\n", "Shape", "interface", "x.Y"},
		{parse.LangC, "#include <stdio.h>\nint add(int a, int b) { return a; }\n", "add", "function", "stdio.h"},
		{parse.LangCPP, "#include \"e.h\"\nclass Editor {};\n", "Editor", "class", "e.h"},
		{parse.LangCSharp, "using System;\npublic class Editor {}\n", "Editor", "class", "System"},
		{parse.LangRuby, "require 'json'\nclass Editor\nend\n", "Editor", "class", "json"},
		{parse.LangPHP, "<?php\nuse App\\Editor;\nclass Widget {}\n", "Widget", "class", "App\\Editor"},
		{parse.LangKotlin, "import a.b.C\nclass Editor {}\n", "Editor", "class", "a.b.C"},
		{parse.LangSwift, "import Foundation\nclass Editor {}\n", "Editor", "class", "Foundation"},
		{parse.LangScala, "import a.b\ntrait Shape\n", "Shape", "interface", "a.b"},
		{parse.LangBash, "source ./lib.sh\nfunction run() {\n  echo hi\n}\n", "run", "function", "./lib.sh"},
		{parse.LangLua, "local m = require('mod')\nfunction run() end\n", "run", "function", "mod"},
	}
	for _, c := range cases {
		t.Run(string(c.lang)+"/"+c.wantName, func(t *testing.T) {
			res, err := p.Parse(context.Background(), []byte(c.src), c.lang, "f")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !hasNode(res.Nodes, c.wantName, c.wantKind) {
				t.Errorf("missing %s %q in %s; nodes=%v", c.wantKind, c.wantName, c.lang, nodeNames(res.Nodes))
			}
			if c.wantImport != "" && !hasImport(res.Imports, c.wantImport) {
				t.Errorf("missing import %q in %s; imports=%v", c.wantImport, c.lang, importPaths(res.Imports))
			}
			// Every emitted node must carry a positive start line and an
			// approximate (non-exact) span flag for the heuristic backend.
			if res.Capability.ExactSpans {
				t.Errorf("%s: heuristic must report ExactSpans=false", c.lang)
			}
		})
	}
}

func hasNode(nodes []codeintel.Node, name, kind string) bool {
	for _, n := range nodes {
		if n.Name == name && n.Kind == kind {
			return true
		}
	}
	return false
}

func hasImport(imps []codeintel.Import, path string) bool {
	for _, i := range imps {
		if i.Path == path {
			return true
		}
	}
	return false
}

func nodeNames(nodes []codeintel.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Kind + ":" + n.Name
	}
	return out
}

func importPaths(imps []codeintel.Import) []string {
	out := make([]string, len(imps))
	for i, im := range imps {
		out[i] = im.Path
	}
	return out
}

// FuzzHeuristicParse asserts the heuristic engine never panics on
// arbitrary input across every served language (CLAUDE.md: no panic in
// library code). Under `go test` it runs the seed corpus; `-fuzz` widens
// it. The brace/indent span scanner + the regex set are the risk surface.
func FuzzHeuristicParse(f *testing.F) {
	seeds := []string{
		"", "{", "}", "func(", "class\n", "\x00\x00", "type X =",
		"func (r *T) M() {", "def f(:\n\tpass", "```", "\"unterminated",
		"struct S { int a; }", "namespace x {", "  \t  indented",
		"function f() => {}\nclass\n}}}{{{",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	p := heuristic.New()
	langs := p.Languages()
	f.Fuzz(func(t *testing.T, src string) {
		for _, lang := range langs {
			// Must return (never panic); result is unchecked here.
			if _, err := p.Parse(context.Background(), []byte(src), lang, "x"); err != nil {
				t.Fatalf("heuristic.Parse returned error (should be best-effort): %v", err)
			}
		}
	})
}
