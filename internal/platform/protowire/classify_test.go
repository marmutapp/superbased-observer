package protowire

import "testing"

func TestIsLikelyText(t *testing.T) {
	cases := []struct {
		in   []byte
		want bool
		desc string
	}{
		{[]byte("hello world this is text"), true, "ascii sentence"},
		{[]byte("short"), false, "below min length"},
		{[]byte("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09"), false, "binary"},
		{[]byte("multiline\nwith\ttabs and lots"), true, "whitespace mixed"},
		{[]byte("résumé naïve café à propos"), true, "utf-8 multibyte"},
		{append(append([]byte("9 chars: "), 0x00, 0x00), make([]byte, 20)...), false, "embedded NULs"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := IsLikelyText(c.in)
			if got != c.want {
				t.Fatalf("IsLikelyText(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestIsLikelyUnixTimestamp(t *testing.T) {
	cases := []struct {
		v    uint64
		want bool
	}{
		{0, false},
		{1735689600, true},             // 2025-01-01 (seconds)
		{1735689600000, true},          // 2025-01-01 (ms)
		{2_500_000_000, false},         // 2049 (above ceiling)
		{1, false},                     // too small
		{1_500_000_000_000_000, false}, // above ms ceiling (microsec)
	}
	for _, c := range cases {
		got := IsLikelyUnixTimestamp(c.v)
		if got != c.want {
			t.Errorf("IsLikelyUnixTimestamp(%d) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestIsLikelyToolName(t *testing.T) {
	good := []string{
		"read_file",
		"runShellCommand",
		"google_web_search",
		"webFetch",
		"grep",
		"replace",
	}
	bad := []string{
		"ok", // too short
		"a_extremely_long_tool_name_that_exceeds_thirty_two", // too long
		"123starts_with_digit",
		"has spaces",
		"has-dashes",
		"",
		"hi", // too short
		"i'm fine",
	}
	for _, s := range good {
		if !IsLikelyToolName(s) {
			t.Errorf("IsLikelyToolName(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if IsLikelyToolName(s) {
			t.Errorf("IsLikelyToolName(%q) = true, want false", s)
		}
	}
}
