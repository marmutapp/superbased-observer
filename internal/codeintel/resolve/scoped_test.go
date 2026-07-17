package resolve

import "testing"

func TestGoClassify(t *testing.T) {
	r := goScopeRules{}
	cases := []struct {
		raw, callee string
		wantShape   CallShape
		wantQual    string
	}{
		{"process(x)", "process", ShapeBare, ""},
		{"  process()", "process", ShapeBare, ""},
		{"json.Marshal(v)", "Marshal", ShapeQualified, "json"},
		{"pkg.Foo[T](a, b)", "Foo", ShapeQualified, "pkg"},
		{"Foo[T]()", "Foo", ShapeBare, ""},
		{"x.Method(y)", "Method", ShapeQualified, "x"}, // qualifier=var; store decides it's not an import
		{"a.b().Foo()", "Foo", ShapeComplex, ""},
		{"arr[i].Foo()", "Foo", ShapeComplex, ""},
		{"obj.Inner.Deep()", "Deep", ShapeComplex, ""}, // multi-segment qualifier rejected
		{"", "Foo", ShapeComplex, ""},
	}
	for _, c := range cases {
		gotShape, gotQual := r.Classify(c.callee, c.raw)
		if gotShape != c.wantShape || gotQual != c.wantQual {
			t.Errorf("Classify(%q,%q) = (%v,%q), want (%v,%q)",
				c.callee, c.raw, gotShape, gotQual, c.wantShape, c.wantQual)
		}
	}
}

// nodes spread across two packages, both defining `process`, plus a helper
// in pkg "a" and a function "Marshal" in an imported package "json".
func scopedFixture() []ScopedNodeRef {
	return []ScopedNodeRef{
		{ID: 1, Name: "process", FileID: 10, Pkg: "/repo/a"},
		{ID: 2, Name: "process", FileID: 20, Pkg: "/repo/b"},
		{ID: 3, Name: "helper", FileID: 10, Pkg: "/repo/a"},
		{ID: 4, Name: "Marshal", FileID: 30, Pkg: "/repo/vendor/json"},
	}
}

func TestScopedResolveBareStaysInPackage(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := scopedFixture()
	// A bare process() called from a file in pkg /repo/a must bind to id 1
	// (same package), never id 2 in /repo/b.
	calls := []ScopedCall{
		{EdgeID: 100, CallerFile: 11, CallerPkg: "/repo/a", Callee: "process", RawText: "process(x)"},
	}
	got := ScopedResolve(rules, nodes, nil, calls)
	if len(got) != 1 {
		t.Fatalf("want 1 binding, got %d (%+v)", len(got), got)
	}
	if got[0].DstID != 1 {
		t.Errorf("bare process bound to %d, want 1 (same package)", got[0].DstID)
	}
	if got[0].Confidence < 0.85 {
		t.Errorf("confidence = %v, want >= 0.9-ish for a unique same-pkg bind", got[0].Confidence)
	}
}

func TestScopedResolveBareNoPackageMatchLeavesNameMatched(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := scopedFixture()
	// process() called from pkg /repo/c (no local process) -> NO scoped
	// binding (must NOT over-link to a or b).
	calls := []ScopedCall{
		{EdgeID: 101, CallerFile: 40, CallerPkg: "/repo/c", Callee: "process", RawText: "process()"},
	}
	got := ScopedResolve(rules, nodes, nil, calls)
	if len(got) != 0 {
		t.Errorf("want 0 scoped bindings (left name-matched), got %+v", got)
	}
}

func TestScopedResolveQualifiedBindsImportedPackage(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := scopedFixture()
	imports := map[int64][]ImportBinding{
		50: {{Local: "json", Pkg: "json"}},
	}
	calls := []ScopedCall{
		{EdgeID: 102, CallerFile: 50, CallerPkg: "/repo/a", Callee: "Marshal", RawText: "json.Marshal(v)"},
	}
	got := ScopedResolve(rules, nodes, imports, calls)
	if len(got) != 1 || got[0].DstID != 4 {
		t.Fatalf("qualified json.Marshal -> %+v, want bind to id 4", got)
	}
}

func TestScopedResolveReceiverVarLeftNameMatched(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := scopedFixture()
	// x.process() where x is NOT an imported package -> no scoped binding.
	calls := []ScopedCall{
		{EdgeID: 103, CallerFile: 11, CallerPkg: "/repo/a", Callee: "process", RawText: "x.process()"},
	}
	got := ScopedResolve(rules, nodes, nil, calls) // no imports for file 11
	if len(got) != 0 {
		t.Errorf("receiver-var call should stay name-matched, got %+v", got)
	}
}

func TestGoReceiverType(t *testing.T) {
	r := goScopeRules{}
	cases := []struct {
		sig, recvVar, wantType string
		wantOK                 bool
	}{
		{"func (r *Foo) M(x int) error", "r", "Foo", true},
		{"func (r Foo) M()", "r", "Foo", true},
		{"func (s *Server[T]) Handle()", "s", "Server", true},
		{"func (r *Foo) M()", "x", "", false},       // receiver var mismatch
		{"func Plain(x int) error", "x", "", false}, // not a method
		{"func (*Foo) M()", "r", "", false},         // unnamed receiver
		{"", "r", "", false},
	}
	for _, c := range cases {
		got, ok := r.ReceiverType(c.sig, c.recvVar)
		if got != c.wantType || ok != c.wantOK {
			t.Errorf("ReceiverType(%q,%q) = (%q,%v), want (%q,%v)", c.sig, c.recvVar, got, ok, c.wantType, c.wantOK)
		}
	}
}

func TestScopedResolveReceiverSelfCall(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := []ScopedNodeRef{
		{ID: 1, Name: "Bar", FQN: "Foo.Bar", FileID: 10, Pkg: "/repo/a"},
		{ID: 2, Name: "Run", FQN: "Foo.Run", FileID: 10, Pkg: "/repo/a"},
		{ID: 3, Name: "Bar", FQN: "Baz.Bar", FileID: 10, Pkg: "/repo/a"}, // a different type's Bar
		{ID: 4, Name: "Bar", FQN: "Foo.Bar", FileID: 99, Pkg: "/repo/b"}, // a Foo.Bar in another pkg
	}
	// Inside func (r *Foo) Run(), the call r.Bar() must bind to Foo.Bar in
	// THIS package (id 1) — not Baz.Bar, not the other package's Foo.Bar.
	calls := []ScopedCall{
		{
			EdgeID: 200, CallerFile: 10, CallerPkg: "/repo/a", Callee: "Bar",
			RawText: "r.Bar()", CallerSig: "func (r *Foo) Run() error",
		},
	}
	got := ScopedResolve(rules, nodes, nil, calls)
	if len(got) != 1 || got[0].DstID != 1 {
		t.Fatalf("receiver self-call r.Bar() -> %+v, want bind to id 1 (Foo.Bar same pkg)", got)
	}
}

func TestScopedResolveReceiverMismatchLeftNameMatched(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := []ScopedNodeRef{
		{ID: 1, Name: "Bar", FQN: "Foo.Bar", FileID: 10, Pkg: "/repo/a"},
	}
	// y is NOT the receiver (receiver is r) -> not bound (could be any type).
	calls := []ScopedCall{
		{
			EdgeID: 201, CallerFile: 10, CallerPkg: "/repo/a", Callee: "Bar",
			RawText: "y.Bar()", CallerSig: "func (r *Foo) Run()",
		},
	}
	if got := ScopedResolve(rules, nodes, nil, calls); len(got) != 0 {
		t.Errorf("non-receiver var y.Bar() should stay name-matched, got %+v", got)
	}
}

func TestScopedResolveLocalVarReceiver(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := []ScopedNodeRef{
		{ID: 1, Name: "Bar", FQN: "Foo.Bar", FileID: 10, Pkg: "/repo/a"},
		{ID: 2, Name: "Bar", FQN: "Baz.Bar", FileID: 10, Pkg: "/repo/a"}, // another type's Bar
		{ID: 3, Name: "Bar", FQN: "Foo.Bar", FileID: 99, Pkg: "/repo/b"}, // Foo.Bar in another pkg
	}
	// x := NewFoo(); x.Bar() — RecvType "Foo" inferred at parse time — must
	// bind to Foo.Bar in THIS package (id 1), not Baz.Bar, not the other pkg.
	calls := []ScopedCall{
		{
			EdgeID: 300, CallerFile: 11, CallerPkg: "/repo/a", Callee: "Bar",
			RawText: "x.Bar()", RecvType: "Foo",
		},
	}
	got := ScopedResolve(rules, nodes, nil, calls)
	if len(got) != 1 || got[0].DstID != 1 {
		t.Fatalf("local-var x.Bar() (RecvType Foo) -> %+v, want bind to id 1 (Foo.Bar same pkg)", got)
	}
}

func TestScopedResolveLocalVarUnknownTypeLeftNameMatched(t *testing.T) {
	rules, _ := ScopeRulesFor("go")
	nodes := []ScopedNodeRef{
		{ID: 1, Name: "Bar", FQN: "Foo.Bar", FileID: 10, Pkg: "/repo/a"},
	}
	// RecvType inferred as "Other", which has no Other.Bar in the package ->
	// no scoped binding (the edge stays name-matched, no regression).
	calls := []ScopedCall{
		{
			EdgeID: 301, CallerFile: 11, CallerPkg: "/repo/a", Callee: "Bar",
			RawText: "x.Bar()", RecvType: "Other",
		},
	}
	if got := ScopedResolve(rules, nodes, nil, calls); len(got) != 0 {
		t.Errorf("unknown-type local-var receiver should stay name-matched, got %+v", got)
	}
}

func TestScopeRulesForUnknownLang(t *testing.T) {
	if _, ok := ScopeRulesFor("cobol"); ok {
		t.Error("expected no scoped rules for cobol")
	}
	if _, ok := ScopeRulesFor("go"); !ok {
		t.Error("expected scoped rules for go")
	}
}
