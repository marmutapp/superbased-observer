package treesitter_test

import (
	"context"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse"
	"github.com/marmutapp/superbased-observer/internal/codeintel/parse/treesitter"
)

// newBackend constructs the backend and fails the test on any compile
// warning — every embedded grammar must compile under wazero.
func newBackend(t *testing.T) parse.Parser {
	t.Helper()
	b, warnings := treesitter.New()
	if len(warnings) != 0 {
		t.Fatalf("treesitter.New warnings: %v", warnings)
	}
	return b
}

func TestServedLanguagesAreExact(t *testing.T) {
	b := newBackend(t)
	want := []codeintel.Language{
		parse.LangRust, parse.LangTypeScript, parse.LangTSX, parse.LangPython,
		parse.LangJavaScript, parse.LangJSX,
		parse.LangJava, parse.LangC, parse.LangCPP, parse.LangCSharp,
		parse.LangRuby, parse.LangPHP,
		parse.LangKotlin, parse.LangSwift, parse.LangScala, parse.LangBash, parse.LangLua,
	}
	for _, lang := range want {
		cap := b.Capabilities(lang)
		if !cap.Symbols || !cap.ExactSpans || !cap.Imports || !cap.Calls {
			t.Errorf("%s: capability = %+v, want all true", lang, cap)
		}
		if !cap.CanCollapse() {
			t.Errorf("%s: CanCollapse=false, want true (exact spans)", lang)
		}
	}
	// A language this backend does not serve reports the zero capability.
	if c := b.Capabilities(parse.LangGo); c.Symbols || c.ExactSpans {
		t.Errorf("go: expected zero capability from treesitter, got %+v", c)
	}
}

// findNode returns the first node with the given name, or a zero node.
func findNode(res codeintel.ParseResult, name string) (codeintel.Node, bool) {
	for _, n := range res.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return codeintel.Node{}, false
}

func hasImport(res codeintel.ParseResult, path string) bool {
	for _, i := range res.Imports {
		if i.Path == path {
			return true
		}
	}
	return false
}

func hasCall(res codeintel.ParseResult, name string) bool {
	for _, c := range res.Calls {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestExactSpansPerLanguage(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()

	type wantSym struct {
		name string
		kind string
	}
	cases := []struct {
		name    string
		lang    codeintel.Language
		src     string
		syms    []wantSym
		imports []string
		calls   []string
	}{
		{
			name: "rust",
			lang: parse.LangRust,
			src: "use std::collections::HashMap;\n" +
				"pub struct Point {\n    x: i32,\n    y: i32,\n}\n" +
				"pub trait Shape {\n    fn area(&self) -> f64;\n}\n" +
				"impl Point {\n    fn dist(&self) -> f64 {\n        compute(self.x)\n    }\n}\n" +
				"fn compute(v: i32) -> f64 { 0.0 }\n",
			syms:    []wantSym{{"Point", "class"}, {"Shape", "interface"}, {"dist", "method"}, {"compute", "function"}},
			imports: []string{"std::collections::HashMap"},
			calls:   []string{"compute"},
		},
		{
			name: "typescript",
			lang: parse.LangTypeScript,
			src: "import { foo } from \"./mod\";\n" +
				"export interface Animal {\n    name: string;\n}\n" +
				"export class Dog implements Animal {\n    name = \"rex\";\n    bark(): string {\n        return foo(this.name);\n    }\n}\n" +
				"type Id = number;\n" +
				"function makeDog(): Dog {\n    return new Dog();\n}\n",
			syms:    []wantSym{{"Animal", "interface"}, {"Dog", "class"}, {"bark", "method"}, {"Id", "type"}, {"makeDog", "function"}},
			imports: []string{"./mod"},
			calls:   []string{"foo"},
		},
		{
			name: "tsx",
			lang: parse.LangTSX,
			src: "import React from \"react\";\n" +
				"interface Props {\n    title: string;\n}\n" +
				"function Title(props: Props) {\n    return render(props.title);\n}\n",
			syms:    []wantSym{{"Props", "interface"}, {"Title", "function"}},
			imports: []string{"react"},
			calls:   []string{"render"},
		},
		{
			name: "javascript",
			lang: parse.LangJavaScript,
			src: "import { foo } from \"./mod\";\n" +
				"const lib = require(\"./lib\");\n" +
				"class Widget {\n    render() {\n        return draw(this.id);\n    }\n}\n" +
				"function build(opts) {\n    return new Widget();\n}\n" +
				"const arrow = () => helper();\n",
			syms:    []wantSym{{"Widget", "class"}, {"render", "method"}, {"build", "function"}, {"arrow", "function"}},
			imports: []string{"./mod", "./lib"}, // ./lib proves the require() #eq? predicate
			calls:   []string{"draw", "helper"},
		},
		{
			name: "jsx",
			lang: parse.LangJSX,
			src: "import React from \"react\";\n" +
				"function Button(props) {\n    return mount(props.label);\n}\n",
			syms:    []wantSym{{"Button", "function"}},
			imports: []string{"react"},
			calls:   []string{"mount"},
		},
		{
			name: "python",
			lang: parse.LangPython,
			src: "import os\n" +
				"from collections import OrderedDict\n" +
				"class Greeter:\n    def greet(self):\n        return shout(self.name)\n" +
				"def shout(s):\n    return s.upper()\n",
			syms:    []wantSym{{"Greeter", "class"}, {"greet", "method"}, {"shout", "function"}},
			imports: []string{"os", "collections"},
			calls:   []string{"shout"},
		},
		{
			name: "java",
			lang: parse.LangJava,
			src: "package demo;\n" +
				"import java.util.List;\n" +
				"public interface Shape {\n    double area();\n}\n" +
				"public class Circle implements Shape {\n" +
				"    public double area() {\n        return compute(this.r);\n    }\n" +
				"    double r;\n}\n" +
				"class Helper {\n    static double compute(double v) {\n        return v;\n    }\n}\n",
			syms:    []wantSym{{"Shape", "interface"}, {"Circle", "class"}, {"area", "method"}, {"Helper", "class"}, {"compute", "method"}},
			imports: []string{"java.util.List"},
			calls:   []string{"compute"},
		},
		{
			name: "c",
			lang: parse.LangC,
			src: "#include <stdio.h>\n" +
				"#include \"util.h\"\n" +
				"struct Point {\n    int x;\n    int y;\n};\n" +
				"typedef int Id;\n" +
				"int add(int a, int b) {\n    return compute(a);\n}\n",
			syms:    []wantSym{{"Point", "class"}, {"Id", "type"}, {"add", "function"}},
			imports: []string{"<stdio.h>", "util.h"},
			calls:   []string{"compute"},
		},
		{
			name: "cpp",
			lang: parse.LangCPP,
			src: "#include <vector>\n" +
				"namespace geo {\n" +
				"class Point {\n public:\n  double dist() {\n    return compute(x_);\n  }\n  double x_;\n};\n" +
				"}\n" +
				"double compute(double v) {\n    return v;\n}\n",
			syms:    []wantSym{{"Point", "class"}, {"dist", "method"}, {"geo", "module"}, {"compute", "function"}},
			imports: []string{"<vector>"},
			calls:   []string{"compute"},
		},
		{
			name: "csharp",
			lang: parse.LangCSharp,
			src: "using System;\n" +
				"using System.Collections.Generic;\n" +
				"namespace Demo {\n" +
				"  public interface IShape {\n    double Area();\n  }\n" +
				"  public class Circle : IShape {\n" +
				"    public double Area() {\n      return Compute(R);\n    }\n" +
				"    public double R;\n  }\n}\n",
			syms:    []wantSym{{"IShape", "interface"}, {"Circle", "class"}, {"Area", "method"}},
			imports: []string{"System", "System.Collections.Generic"},
			calls:   []string{"Compute"},
		},
		{
			name: "ruby",
			lang: parse.LangRuby,
			src: "require \"set\"\n" +
				"require_relative \"helper\"\n" +
				"class Greeter\n" +
				"  def greet\n" +
				"    shout(@name)\n" +
				"  end\n" +
				"end\n" +
				"def shout(s)\n  s.upcase\nend\n",
			syms:    []wantSym{{"Greeter", "class"}, {"greet", "method"}, {"shout", "method"}},
			imports: []string{"set", "helper"},
			calls:   []string{"shout"},
		},
		{
			name: "php",
			lang: parse.LangPHP,
			src: "<?php\n" +
				"namespace App;\n" +
				"use App\\Models\\User;\n" +
				"interface Shape {\n    public function area(): float;\n}\n" +
				"class Circle implements Shape {\n" +
				"    public function area(): float {\n        return compute($this->r);\n    }\n}\n" +
				"function compute($v) {\n    return $v;\n}\n",
			syms:    []wantSym{{"Shape", "interface"}, {"Circle", "class"}, {"area", "method"}, {"compute", "function"}},
			imports: []string{"App\\Models\\User"},
			calls:   []string{"compute"},
		},
		{
			name: "kotlin",
			lang: parse.LangKotlin,
			src: "package demo\n" +
				"import kotlin.collections.List\n" +
				"interface Shape {\n    fun area(): Double\n}\n" +
				"class Circle(val radius: Double) : Shape {\n" +
				"    override fun area(): Double {\n        return compute(radius)\n    }\n}\n" +
				"fun compute(v: Double): Double {\n    return v * v\n}\n",
			syms:    []wantSym{{"Shape", "class"}, {"Circle", "class"}, {"area", "function"}, {"compute", "function"}},
			imports: []string{"kotlin.collections.List"},
			calls:   []string{"compute"},
		},
		{
			name: "swift",
			lang: parse.LangSwift,
			src: "import Foundation\n" +
				"protocol Shape {\n    func area() -> Double\n}\n" +
				"class Circle: Shape {\n" +
				"    let radius: Double\n" +
				"    func area() -> Double {\n        return compute(radius)\n    }\n}\n" +
				"func compute(_ v: Double) -> Double {\n    return v * v\n}\n",
			syms:    []wantSym{{"Shape", "interface"}, {"Circle", "class"}, {"area", "function"}, {"compute", "function"}},
			imports: []string{"Foundation"},
			calls:   []string{"compute"},
		},
		{
			name: "scala",
			lang: parse.LangScala,
			src: "package demo\n" +
				"import scala.collection.mutable\n" +
				"trait Shape {\n  def area(): Double\n}\n" +
				"class Circle(radius: Double) extends Shape {\n" +
				"  def area(): Double = {\n    compute(radius)\n  }\n}\n" +
				"object Helper {\n  def compute(v: Double): Double = v * v\n}\n",
			// Scala imports intentionally not captured (flat per-segment path
			// fields; see scala.scm) — ships S + C.
			syms:    []wantSym{{"Shape", "interface"}, {"Circle", "class"}, {"area", "function"}, {"Helper", "module"}, {"compute", "function"}},
			imports: nil,
			calls:   []string{"compute"},
		},
		{
			name: "bash",
			lang: parse.LangBash,
			src: "#!/usr/bin/env bash\n" +
				"greet() {\n  echo \"hi\"\n  compute 5\n}\n" +
				"function compute {\n  echo \"$1\"\n}\n",
			syms:    []wantSym{{"greet", "function"}, {"compute", "function"}},
			imports: nil,
			calls:   []string{"compute"},
		},
		{
			name: "lua",
			lang: parse.LangLua,
			src: "local helper = require(\"helper\")\n" +
				"function greet(name)\n  return shout(name)\nend\n" +
				"function M.compute(v)\n  return v * v\nend\n" +
				"function obj:method()\n  return self.x\nend\n",
			syms:    []wantSym{{"greet", "function"}, {"compute", "function"}, {"method", "method"}},
			imports: []string{"helper"},
			calls:   []string{"shout"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := b.Parse(ctx, []byte(tc.src), tc.lang, tc.name)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if res.Parser != "treesitter:"+string(tc.lang) {
				t.Errorf("Parser = %q", res.Parser)
			}
			for _, ws := range tc.syms {
				n, ok := findNode(res, ws.name)
				if !ok {
					t.Errorf("symbol %q not found; nodes=%v", ws.name, res.Nodes)
					continue
				}
				if n.Kind != ws.kind {
					t.Errorf("symbol %q: kind=%q want %q", ws.name, n.Kind, ws.kind)
				}
				// Exact spans: a real start, and an end strictly after start
				// (multi-line decls span >1 line; the byte end is recorded).
				if n.StartLine < 1 {
					t.Errorf("symbol %q: StartLine=%d", ws.name, n.StartLine)
				}
				if n.EndLine < n.StartLine {
					t.Errorf("symbol %q: EndLine=%d < StartLine=%d", ws.name, n.EndLine, n.StartLine)
				}
				if n.EndByte <= n.StartByte {
					t.Errorf("symbol %q: EndByte=%d <= StartByte=%d (not an exact span)", ws.name, n.EndByte, n.StartByte)
				}
				if n.Signature == "" {
					t.Errorf("symbol %q: empty signature", ws.name)
				}
			}
			for _, imp := range tc.imports {
				if !hasImport(res, imp) {
					t.Errorf("import %q not found; imports=%v", imp, res.Imports)
				}
			}
			for _, call := range tc.calls {
				if !hasCall(res, call) {
					t.Errorf("call %q not found; calls=%v", call, res.Calls)
				}
			}
		})
	}
}

// TestRequirePredicate proves the #eq? predicate gates require() imports: a
// non-require call with a string argument must NOT be captured as an import.
func TestRequirePredicate(t *testing.T) {
	b := newBackend(t)
	src := "const a = require(\"./real\");\n" +
		"console.log(\"not-an-import\");\n" +
		"translate(\"also-not\");\n"
	res, err := b.Parse(context.Background(), []byte(src), parse.LangJavaScript, "x.js")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasImport(res, "./real") {
		t.Errorf("require(\"./real\") not captured; imports=%v", res.Imports)
	}
	for _, bad := range []string{"not-an-import", "also-not"} {
		if hasImport(res, bad) {
			t.Errorf("string arg %q wrongly captured as import (predicate not applied)", bad)
		}
	}
}

// TestMultiLineSpanCollapsible verifies a multi-line function records an end
// line greater than its start — the property that makes it body-collapsible.
func TestMultiLineSpanCollapsible(t *testing.T) {
	b := newBackend(t)
	src := "def big():\n    a = 1\n    b = 2\n    return a + b\n"
	res, err := b.Parse(context.Background(), []byte(src), parse.LangPython, "x.py")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	n, ok := findNode(res, "big")
	if !ok {
		t.Fatalf("function big not found")
	}
	if n.EndLine <= n.StartLine {
		t.Errorf("big: EndLine=%d not > StartLine=%d", n.EndLine, n.StartLine)
	}
}

// TestEmptyAndMalformedAreSafe ensures empty and broken input never panic
// and return a well-formed (possibly empty) result.
func TestEmptyAndMalformedAreSafe(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()
	for _, tc := range []struct {
		lang codeintel.Language
		src  string
	}{
		{parse.LangPython, ""},
		{parse.LangRust, "fn ("},
		{parse.LangTypeScript, "class {{{ <<<"},
		{parse.LangPython, "def \x00\xff bad("},
	} {
		res, err := b.Parse(ctx, []byte(tc.src), tc.lang, "x")
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.lang, err)
		}
		_ = res
	}
}
