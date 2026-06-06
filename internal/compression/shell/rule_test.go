package shell

import (
	"strings"
	"testing"
)

// runRule is a small helper that drives a single Rule with the given input
// lines and returns the emitted output (Push for each input, then Flush).
func runRule(r Rule, lines []string) []string {
	var out []string
	emit := func(s string) { out = append(out, s) }
	for _, l := range lines {
		r.Push(l, emit)
	}
	r.Flush(emit)
	return out
}

func TestLineDrop(t *testing.T) {
	r, err := newLineDrop(RuleSpec{Pattern: "^DEBUG "})
	if err != nil {
		t.Fatalf("newLineDrop: %v", err)
	}
	got := runRule(r, []string{
		"DEBUG starting",
		"INFO starting",
		"DEBUG step 2",
		"done",
	})
	want := []string{"INFO starting", "done"}
	assertEqualLines(t, got, want)
}

func TestLineDropInvalidRegex(t *testing.T) {
	if _, err := newLineDrop(RuleSpec{Pattern: "[unterminated"}); err == nil {
		t.Fatal("expected regex compile error")
	}
}

func TestLineKeep(t *testing.T) {
	r, err := newLineKeep(RuleSpec{Pattern: "^INFO"})
	if err != nil {
		t.Fatalf("newLineKeep: %v", err)
	}
	got := runRule(r, []string{"INFO a", "DEBUG b", "INFO c"})
	assertEqualLines(t, got, []string{"INFO a", "INFO c"})
}

func TestDedupConsecutive(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "no duplicates",
			in:   []string{"a", "b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "middle run",
			in:   []string{"a", "b", "b", "b", "c"},
			want: []string{"a", "b [×3]", "c"},
		},
		{
			name: "trailing run flushed",
			in:   []string{"a", "b", "b"},
			want: []string{"a", "b [×2]"},
		},
		{
			name: "single value only",
			in:   []string{"only"},
			want: []string{"only"},
		},
		{
			name: "non-adjacent duplicates untouched",
			in:   []string{"a", "b", "a"},
			want: []string{"a", "b", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runRule(&DedupConsecutive{}, tc.in)
			assertEqualLines(t, got, tc.want)
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		spec RuleSpec
		in   []string
		want []string
	}{
		{
			name: "below threshold passes through",
			spec: RuleSpec{Type: "truncate", Head: 2, Tail: 2},
			in:   []string{"a", "b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "elides middle",
			spec: RuleSpec{Type: "truncate", Head: 2, Tail: 2},
			in:   []string{"a", "b", "c", "d", "e", "f"},
			want: []string{"a", "b", "… [2 lines elided]", "e", "f"},
		},
		{
			name: "custom marker",
			spec: RuleSpec{Type: "truncate", Head: 1, Tail: 1, Marker: "... %d more ..."},
			in:   []string{"a", "b", "c", "d"},
			want: []string{"a", "... 2 more ...", "d"},
		},
		{
			name: "tail only",
			spec: RuleSpec{Type: "truncate", Head: 0, Tail: 2},
			in:   []string{"a", "b", "c", "d"},
			want: []string{"… [2 lines elided]", "c", "d"},
		},
		{
			name: "head only",
			spec: RuleSpec{Type: "truncate", Head: 2, Tail: 0},
			in:   []string{"a", "b", "c", "d"},
			want: []string{"a", "b", "… [2 lines elided]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newTruncate(tc.spec)
			if err != nil {
				t.Fatalf("newTruncate: %v", err)
			}
			got := runRule(r, tc.in)
			assertEqualLines(t, got, tc.want)
		})
	}
}

func TestSquashWhitespace(t *testing.T) {
	got := runRule(&SquashWhitespace{}, []string{
		"a",
		"",
		"",
		"",
		"b",
		"   ",
		"\t",
		"c",
	})
	// Both pure-blank and whitespace-only lines count as blank; the first blank
	// in each run is kept.
	assertEqualLines(t, got, []string{"a", "", "b", "   ", "c"})
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    FilterSpec
		wantErr string
	}{
		{
			name:    "missing command",
			spec:    FilterSpec{Rules: []RuleSpec{{Type: "squash_whitespace"}}},
			wantErr: "filter.command is required",
		},
		{
			name:    "line_drop missing pattern",
			spec:    FilterSpec{Command: "git", Rules: []RuleSpec{{Type: "line_drop"}}},
			wantErr: "requires pattern",
		},
		{
			name:    "truncate missing head and tail",
			spec:    FilterSpec{Command: "go", Rules: []RuleSpec{{Type: "truncate"}}},
			wantErr: "requires head or tail",
		},
		{
			name:    "truncate marker missing %d",
			spec:    FilterSpec{Command: "go", Rules: []RuleSpec{{Type: "truncate", Head: 1, Tail: 1, Marker: "elided"}}},
			wantErr: "marker must contain %d",
		},
		{
			name:    "hook missing name",
			spec:    FilterSpec{Command: "docker", Rules: []RuleSpec{{Type: "hook"}}},
			wantErr: "hook requires name",
		},
		{
			name:    "empty type",
			spec:    FilterSpec{Command: "go", Rules: []RuleSpec{{}}},
			wantErr: "type is required",
		},
		{
			name:    "unknown type",
			spec:    FilterSpec{Command: "go", Rules: []RuleSpec{{Type: "magic"}}},
			wantErr: `unknown type "magic"`,
		},
		{
			name: "valid",
			spec: FilterSpec{
				Command: "git",
				Rules: []RuleSpec{
					{Type: "line_drop", Pattern: "^hint:"},
					{Type: "squash_whitespace"},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpec(tc.spec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func assertEqualLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch\n got: %v\nwant: %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("line %d mismatch\n got: %q\nwant: %q\nfull got: %v\nfull want: %v", i, got[i], want[i], got, want)
		}
	}
}
