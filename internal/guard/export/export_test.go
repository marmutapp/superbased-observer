package export

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ts is the fixed event time used across format tests:
// 2026-06-11 10:00:00 UTC = 1781172000000 epoch ms.
var ts = time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

// fullEvent is an event with every field populated.
func fullEvent() Event {
	actionID := int64(42)
	return Event{
		ID: 7, TS: ts, SessionID: "sess-1", ActionID: &actionID,
		Tool: "claude-code", EventKind: "shell_exec",
		RuleID: "R-101", Category: "destructive", Severity: "critical",
		Decision: "deny", DegradedFrom: "", Enforced: true, Source: "builtin",
		Reason:     "recursive delete outside the project root",
		TargetHash: "aabbcc", TargetExcerpt: "rm -rf /etc",
		TaintOrigin: "", ChainPrev: "p0", ChainHash: "h1",
	}
}

// TestCEFSeverityScore pins the §11.4 severity map — one case per
// table row plus the unknown/empty fallbacks.
func TestCEFSeverityScore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label  string
		want   int
		wantOK bool
	}{
		{"info", 2, true},
		{"warn", 4, true},
		{"medium", 5, true}, // reserved vocabulary — enum doesn't emit it today
		{"high", 8, true},
		{"critical", 10, true},
		{"", 5, false},
		{"bogus", 5, false},
	}
	for _, c := range cases {
		got, ok := CEFSeverityScore(c.label)
		if got != c.want || ok != c.wantOK {
			t.Errorf("CEFSeverityScore(%q) = (%d, %v), want (%d, %v)", c.label, got, ok, c.want, c.wantOK)
		}
	}
}

// TestEscapeCEF pins both escaper dialects: header escapes backslash
// and pipe; extension escapes backslash, equals and newlines — and
// deliberately NOT pipe (the spec's asymmetry).
func TestEscapeCEF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		in         string
		wantHeader string
		wantExt    string
	}{
		{"plain", "abc", "abc", "abc"},
		{"pipe", "a|b", `a\|b`, "a|b"},
		{"backslash", `a\b`, `a\\b`, `a\\b`},
		{"equals", "a=b", "a=b", `a\=b`},
		{"backslash-pipe", `\|`, `\\\|`, `\\|`},
		{"backslash-equals", `\=`, `\\=`, `\\\=`},
		{"newline", "a\nb", "a b", `a\nb`},
		{"crlf", "a\r\nb", "a  b", `a\r\nb`},
		{"all", "x=1|y\\", `x=1\|y\\`, `x\=1|y\\`},
	}
	for _, c := range cases {
		if got := escapeCEFHeader(c.in); got != c.wantHeader {
			t.Errorf("%s: escapeCEFHeader(%q) = %q, want %q", c.name, c.in, got, c.wantHeader)
		}
		if got := escapeCEFExtension(c.in); got != c.wantExt {
			t.Errorf("%s: escapeCEFExtension(%q) = %q, want %q", c.name, c.in, got, c.wantExt)
		}
	}
}

// TestCEFLine pins the full record shape: header order, the fixed
// extension order, empty-field omission, and escaping in situ.
func TestCEFLine(t *testing.T) {
	t.Parallel()
	t.Run("full event", func(t *testing.T) {
		got := CEFLine(fullEvent(), "1.8.3")
		want := "CEF:0|SuperBased|ObserverGuard|1.8.3|R-101|destructive deny|10|" +
			"rt=1781172000000 cat=destructive act=deny outcome=enforced " +
			"cs1Label=sessionId cs1=sess-1 cs2Label=tool cs2=claude-code " +
			"cs3Label=eventKind cs3=shell_exec cs4Label=targetHash cs4=aabbcc " +
			"cs5Label=source cs5=builtin cs6Label=target cs6=rm -rf /etc " +
			"sboEventId=7 sboChainHash=h1 " +
			"msg=recursive delete outside the project root"
		if got != want {
			t.Errorf("CEFLine:\n got %q\nwant %q", got, want)
		}
	})
	t.Run("minimal event omits empty extensions", func(t *testing.T) {
		got := CEFLine(Event{ID: 1, RuleID: "R-1", Severity: "warn", Decision: "flag", Category: "exfil"}, "v")
		want := "CEF:0|SuperBased|ObserverGuard|v|R-1|exfil flag|4|" +
			"cat=exfil act=flag outcome=observed sboEventId=1"
		if got != want {
			t.Errorf("CEFLine:\n got %q\nwant %q", got, want)
		}
	})
	t.Run("unknown severity carries raw label", func(t *testing.T) {
		got := CEFLine(Event{ID: 2, RuleID: "R-2", Severity: "weird", Decision: "flag"}, "v")
		if !strings.Contains(got, "|5|") {
			t.Errorf("unknown severity must score 5: %q", got)
		}
		if !strings.Contains(got, "sboSeverity=weird") {
			t.Errorf("unknown severity must preserve the raw label: %q", got)
		}
	})
	t.Run("header escaping applies to rule id and name", func(t *testing.T) {
		got := CEFLine(Event{RuleID: "R|1", Category: `c\at`, Decision: "deny", Severity: "high"}, "v")
		if !strings.Contains(got, `|R\|1|c\\at deny|`) {
			t.Errorf("header fields must escape pipe and backslash: %q", got)
		}
	})
	t.Run("extension escaping applies to values", func(t *testing.T) {
		ev := Event{
			ID: 3, RuleID: "R-3", Severity: "info", Decision: "flag",
			TargetExcerpt: "FOO=bar baz", Reason: "line1\nline2",
		}
		got := CEFLine(ev, "v")
		if !strings.Contains(got, `cs6=FOO\=bar baz`) {
			t.Errorf("extension values must escape '=': %q", got)
		}
		if !strings.Contains(got, `msg=line1\nline2`) {
			t.Errorf("extension values must escape newlines: %q", got)
		}
	})
}

// TestJSONLLine pins the JSONL wire shape: one line, UTC timestamp,
// omitempty on optional fields, anchors present when set.
func TestJSONLLine(t *testing.T) {
	t.Parallel()
	b, err := JSONLLine(fullEvent())
	if err != nil {
		t.Fatalf("JSONLLine: %v", err)
	}
	if strings.ContainsAny(string(b), "\n\r") {
		t.Errorf("JSONL line must not contain newlines: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]any{
		"id": float64(7), "session_id": "sess-1", "action_id": float64(42),
		"rule_id": "R-101", "severity": "critical", "decision": "deny",
		"enforced": true, "chain_hash": "h1", "ts": "2026-06-11T10:00:00Z",
	} {
		if m[key] != want {
			t.Errorf("field %q = %v, want %v", key, m[key], want)
		}
	}
	if _, present := m["api_turn_id"]; present {
		t.Error("nil APITurnID must be omitted")
	}
	if _, present := m["taint_origin"]; present {
		t.Error("empty taint_origin must be omitted")
	}

	// Non-UTC input normalizes.
	ev := Event{ID: 1, RuleID: "R-1", TS: ts.In(time.FixedZone("X", 3600))}
	b, err = JSONLLine(ev)
	if err != nil {
		t.Fatalf("JSONLLine: %v", err)
	}
	if !strings.Contains(string(b), `"ts":"2026-06-11T10:00:00Z"`) {
		t.Errorf("timestamps must normalize to UTC: %s", b)
	}
}

// TestFilterMatch pins the window + severity gate, including the
// fail-toward-inclusion direction for unknown severities.
func TestFilterMatch(t *testing.T) {
	t.Parallel()
	mk := func(sev string, at time.Time) Event { return Event{Severity: sev, TS: at} }
	cases := []struct {
		name string
		f    Filter
		ev   Event
		want bool
	}{
		{"no filter admits", Filter{}, mk("info", ts), true},
		{"since inclusive", Filter{Since: ts}, mk("info", ts), true},
		{"before since excluded", Filter{Since: ts}, mk("info", ts.Add(-time.Second)), false},
		{"until exclusive", Filter{Until: ts}, mk("info", ts), false},
		{"before until admitted", Filter{Until: ts}, mk("info", ts.Add(-time.Second)), true},
		{"min severity passes equal", Filter{MinSeverity: "high"}, mk("high", ts), true},
		{"min severity passes above", Filter{MinSeverity: "high"}, mk("critical", ts), true},
		{"min severity blocks below", Filter{MinSeverity: "high"}, mk("warn", ts), false},
		{"medium threshold blocks warn", Filter{MinSeverity: "medium"}, mk("warn", ts), false},
		{"unknown event severity included", Filter{MinSeverity: "critical"}, mk("weird", ts), true},
		{"unknown threshold admits all", Filter{MinSeverity: "bogus"}, mk("info", ts), true},
	}
	for _, c := range cases {
		if got := c.f.Match(c.ev); got != c.want {
			t.Errorf("%s: Match = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSeverityLabels pins the CLI validation vocabulary and its order.
func TestSeverityLabels(t *testing.T) {
	t.Parallel()
	got := SeverityLabels()
	want := []string{"info", "warn", "medium", "high", "critical"}
	if len(got) != len(want) {
		t.Fatalf("SeverityLabels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SeverityLabels() = %v, want %v", got, want)
		}
	}
}
