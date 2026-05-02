package conversation

import (
	"strings"
	"testing"
)

func TestPrefixHashStableAcrossCalls(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Text: "you are helpful"},
		{Role: RoleUser, Text: "hello"},
	}
	a := PrefixHash(msgs, 2)
	b := PrefixHash(msgs, 2)
	if a != b {
		t.Fatalf("hash not stable across calls: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hex length = %d, want 64 (SHA-256)", len(a))
	}
}

func TestPrefixHashChangesWhenPrefixChanges(t *testing.T) {
	base := []Message{
		{Role: RoleSystem, Text: "you are helpful"},
		{Role: RoleUser, Text: "hello"},
	}
	mutated := []Message{
		{Role: RoleSystem, Text: "you are helpful!"},
		{Role: RoleUser, Text: "hello"},
	}
	if PrefixHash(base, 2) == PrefixHash(mutated, 2) {
		t.Fatal("hash should differ when system prompt changes")
	}
}

func TestPrefixHashUnchangedByTailMutation(t *testing.T) {
	base := []Message{
		{Role: RoleSystem, Text: "S"},
		{Role: RoleUser, Text: "U1"},
		{Role: RoleAssistant, Text: "A"},
	}
	mutated := []Message{
		{Role: RoleSystem, Text: "S"},
		{Role: RoleUser, Text: "U1"},
		{Role: RoleAssistant, Text: "A-different-tail"},
	}
	if PrefixHash(base, 2) != PrefixHash(mutated, 2) {
		t.Fatal("prefix hash should be insensitive to tail mutation")
	}
}

func TestPrefixHashRoleSensitive(t *testing.T) {
	same := []Message{{Role: RoleUser, Text: "A"}}
	diff := []Message{{Role: RoleAssistant, Text: "A"}}
	if PrefixHash(same, 1) == PrefixHash(diff, 1) {
		t.Fatal("different roles must produce different hashes")
	}
}

func TestPrefixHashEmptyInputs(t *testing.T) {
	if PrefixHash(nil, 0) != "" {
		t.Error("nil input should hash to empty string")
	}
	if PrefixHash([]Message{}, 5) != "" {
		t.Error("empty slice should hash to empty string")
	}
}

func TestSplitIndex(t *testing.T) {
	msgs := []Message{
		{ByteLen: 100}, // cum=100
		{ByteLen: 200}, // cum=300
		{ByteLen: 150}, // cum=450
		{ByteLen: 50},  // cum=500
	}
	cases := []struct {
		target int
		want   int
	}{
		{0, 0},
		{-5, 0},
		{99, 0},
		{100, 1},
		{250, 1}, // cum 300 > 250 so only first fits
		{300, 2},
		{500, 4},
		{10000, 4},
	}
	for _, tc := range cases {
		got := SplitIndex(msgs, tc.target)
		if got != tc.want {
			t.Errorf("SplitIndex(target=%d) = %d, want %d", tc.target, got, tc.want)
		}
	}
}

func TestSummarizeTruncatesLongText(t *testing.T) {
	msg := Message{Role: RoleUser, Text: strings.Repeat("x", 100)}
	got := Summarize([]Message{msg}, 1)
	if !strings.HasPrefix(got, RoleUser+":") {
		t.Errorf("Summarize missing role prefix: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Summarize should truncate long text: %q", got)
	}
}
