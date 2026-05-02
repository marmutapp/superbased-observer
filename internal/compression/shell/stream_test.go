package shell

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestStreamPassThrough(t *testing.T) {
	e := NewEngine()
	var out bytes.Buffer
	s, err := e.Filter([]string{"unknown"}, &out)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if s.Spec() != nil {
		t.Fatalf("expected nil spec for pass-through, got %v", s.Spec())
	}
	in := "line1\nline2\nline3\n"
	if _, err := io.WriteString(s, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := out.String(); got != in {
		t.Fatalf("pass-through mismatch\n got: %q\nwant: %q", got, in)
	}
	st := s.Stats()
	if st.BytesIn != int64(len(in)) || st.BytesOut != int64(len(in)) {
		t.Fatalf("bytes mismatch in=%d out=%d", st.BytesIn, st.BytesOut)
	}
	if st.LinesIn != 3 || st.LinesOut != 3 {
		t.Fatalf("line counts: in=%d out=%d", st.LinesIn, st.LinesOut)
	}
	if st.ReductionRatio() != 0 {
		t.Fatalf("expected 0 reduction for pass-through, got %v", st.ReductionRatio())
	}
}

func TestStreamPartialLine(t *testing.T) {
	e := NewEngine()
	e.AddSpec(FilterSpec{
		Command: "echo",
		Rules:   []RuleSpec{{Type: "line_drop", Pattern: "^drop"}},
	})
	var out bytes.Buffer
	s, err := e.Filter([]string{"echo"}, &out)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	// Split writes mid-line; stream must buffer.
	writes := []string{"keep1\nkeep", "2\ndrop me\nkee", "p3"}
	for _, w := range writes {
		if _, err := io.WriteString(s, w); err != nil {
			t.Fatalf("write %q: %v", w, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := "keep1\nkeep2\nkeep3\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	st := s.Stats()
	if st.LinesIn != 4 {
		t.Fatalf("LinesIn = %d want 4", st.LinesIn)
	}
	if st.LinesOut != 3 {
		t.Fatalf("LinesOut = %d want 3", st.LinesOut)
	}
}

func TestStreamChainsRules(t *testing.T) {
	e := NewEngine()
	e.AddSpec(FilterSpec{
		Command: "test",
		Rules: []RuleSpec{
			{Type: "line_drop", Pattern: "^noise"},
			{Type: "squash_whitespace"},
			{Type: "dedup_consecutive"},
		},
	})
	var out bytes.Buffer
	s, err := e.Filter([]string{"test"}, &out)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	input := strings.Join([]string{
		"start",
		"noise A",
		"repeat",
		"repeat",
		"repeat",
		"",
		"",
		"done",
	}, "\n") + "\n"
	if _, err := io.WriteString(s, input); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := strings.Join([]string{
		"start",
		"repeat [×3]",
		"",
		"done",
	}, "\n") + "\n"
	if got := out.String(); got != want {
		t.Fatalf("chained output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestStreamWriteAfterClose(t *testing.T) {
	e := NewEngine()
	var out bytes.Buffer
	s, _ := e.Filter([]string{"x"}, &out)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.Write([]byte("anything")); err != io.ErrClosedPipe {
		t.Fatalf("want ErrClosedPipe, got %v", err)
	}
	// Double-close is a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestStreamTrailingPartialLineEmits(t *testing.T) {
	e := NewEngine()
	var out bytes.Buffer
	s, _ := e.Filter([]string{"x"}, &out)
	if _, err := io.WriteString(s, "no newline"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Trailing partial line is emitted with a newline appended.
	if got := out.String(); got != "no newline\n" {
		t.Fatalf("got %q", got)
	}
	st := s.Stats()
	if st.LinesIn != 1 || st.LinesOut != 1 {
		t.Fatalf("line counts: in=%d out=%d", st.LinesIn, st.LinesOut)
	}
	if st.BytesOut != st.BytesIn+1 {
		t.Fatalf("expected BytesOut = BytesIn+1 for appended newline, got in=%d out=%d", st.BytesIn, st.BytesOut)
	}
}

func TestStreamReductionRatio(t *testing.T) {
	e := NewEngine()
	e.AddSpec(FilterSpec{
		Command: "test",
		Rules:   []RuleSpec{{Type: "line_drop", Pattern: "^drop"}},
	})
	var out bytes.Buffer
	s, _ := e.Filter([]string{"test"}, &out)
	io.WriteString(s, "keep\ndrop this is longer\ndrop another\nkeep2\n")
	s.Close()

	st := s.Stats()
	if st.BytesOut >= st.BytesIn {
		t.Fatalf("expected reduction, got in=%d out=%d", st.BytesIn, st.BytesOut)
	}
	if r := st.ReductionRatio(); r <= 0 || r >= 1 {
		t.Fatalf("ReductionRatio = %v; want in (0,1)", r)
	}
}

func TestReductionRatioClampsNegative(t *testing.T) {
	// Filter that adds bytes (dedup suffix is longer than the repeated line).
	s := Stats{BytesIn: 100, BytesOut: 200}
	if r := s.ReductionRatio(); r != 0 {
		t.Fatalf("negative reduction should clamp to 0, got %v", r)
	}
}
