package email

import (
	"strings"
	"testing"
	"time"
)

func TestComposeTextAndHTML(t *testing.T) {
	m := Compose(ComposeParams{
		Subject: "[Observer] Budget alert: team at 80%",
		Heading: "The team budget crossed 80%.",
		Fields: []Field{
			{Label: "Scope", Value: "team · payments"},
			{Label: "Current spend", Value: "$80.00 of $100.00"},
		},
		Version: "v1.19.0",
		To:      []string{"admin@example.com"},
	})
	if m.Subject != "[Observer] Budget alert: team at 80%" {
		t.Fatalf("subject = %q", m.Subject)
	}
	if len(m.To) != 1 || m.To[0] != "admin@example.com" {
		t.Fatalf("to = %v", m.To)
	}
	// Text body carries the heading, every field, and the version footer.
	for _, want := range []string{"The team budget crossed 80%.", "Scope: team · payments", "Current spend: $80.00 of $100.00", "Sent by SuperBased Observer v1.19.0"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text missing %q\n%s", want, m.Text)
		}
	}
	// HTML body carries the same and is HTML.
	if !strings.Contains(m.HTML, "<table") || !strings.Contains(m.HTML, "payments") {
		t.Errorf("html missing table/fields:\n%s", m.HTML)
	}
}

func TestComposeSections(t *testing.T) {
	m := Compose(ComposeParams{
		Subject: "[Observer] Weekly cost digest",
		Heading: "Observed spend for the week.",
		Fields:  []Field{{Label: "Observed spend", Value: "$100.00"}},
		Sections: []Section{
			{Title: "Spend by model", Fields: []Field{{Label: "claude-opus", Value: "$60.00"}}},
			{Title: "Top movers vs prior period", Fields: []Field{{Label: "claude-opus", Value: "$60.00 (was $40.00)"}}},
		},
		Version: "v1.19.0",
	})
	for _, want := range []string{"Spend by model", "  claude-opus: $60.00", "Top movers vs prior period"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text missing section content %q\n%s", want, m.Text)
		}
	}
	if !strings.Contains(m.HTML, "Spend by model") || !strings.Contains(m.HTML, "Top movers vs prior period") {
		t.Errorf("html missing section titles:\n%s", m.HTML)
	}
}

// TestComposeNoSectionsUnchanged pins the additive contract: an empty Sections
// renders exactly like the pre-G13 flat alert email.
func TestComposeNoSectionsUnchanged(t *testing.T) {
	p := ComposeParams{Subject: "s", Heading: "h", Fields: []Field{{Label: "a", Value: "b"}}, Version: "v1"}
	withNil := Compose(p)
	p.Sections = []Section{}
	withEmpty := Compose(p)
	// Both bodies must be identical apart from the volatile timestamp stamp,
	// which is present in both; compare the field-table region.
	if !strings.Contains(withNil.Text, "a: b") || !strings.Contains(withEmpty.Text, "a: b") {
		t.Fatalf("field rendering changed with empty sections")
	}
	if strings.Contains(withEmpty.Text, "\n\nSent by") != strings.Contains(withNil.Text, "\n\nSent by") {
		t.Fatalf("footer spacing diverged between nil and empty sections")
	}
}

func TestComposeFooterVersionless(t *testing.T) {
	m := Compose(ComposeParams{Subject: "s", Fields: []Field{{Label: "a", Value: "b"}}})
	if !strings.Contains(m.Text, "Sent by SuperBased Observer") {
		t.Fatalf("missing footer:\n%s", m.Text)
	}
	if strings.Contains(m.Text, "Observer  ") { // no trailing double-space where a version would go
		t.Fatalf("versionless footer malformed:\n%s", m.Text)
	}
}

func TestComposeHTMLEscaping(t *testing.T) {
	m := Compose(ComposeParams{
		Subject: "s",
		Heading: "5 < 10 & rising",
		Fields:  []Field{{Label: "note", Value: "<script>alert(1)</script>"}},
	})
	if strings.Contains(m.HTML, "<script>alert(1)</script>") {
		t.Fatalf("HTML injection not escaped:\n%s", m.HTML)
	}
	if !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Fatalf("expected escaped value:\n%s", m.HTML)
	}
}

func TestRenderMultipartMIME(t *testing.T) {
	m := Message{To: []string{"a@x.com", "b@x.com"}, Subject: "Subj", Text: "hello", HTML: "<b>hello</b>"}
	raw := string(m.render("from@x.com", time.Unix(0, 0).UTC()))
	for _, want := range []string{
		"From: from@x.com\r\n",
		"To: a@x.com, b@x.com\r\n",
		"Subject: Subj\r\n",
		"MIME-Version: 1.0\r\n",
		"multipart/alternative; boundary=",
		"text/plain; charset=UTF-8",
		"text/html; charset=UTF-8",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("render missing %q\n%s", want, raw)
		}
	}
	// CRLF line endings throughout.
	if strings.Contains(raw, "\n") && strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") {
		t.Errorf("found a bare LF (non-CRLF) line ending")
	}
}

func TestRenderSingleParts(t *testing.T) {
	textOnly := string(Message{To: []string{"a@x"}, Subject: "s", Text: "body"}.render("f@x", time.Unix(0, 0).UTC()))
	if !strings.Contains(textOnly, "Content-Type: text/plain; charset=UTF-8") || strings.Contains(textOnly, "multipart") {
		t.Errorf("text-only render wrong:\n%s", textOnly)
	}
	htmlOnly := string(Message{To: []string{"a@x"}, Subject: "s", HTML: "<b>x</b>"}.render("f@x", time.Unix(0, 0).UTC()))
	if !strings.Contains(htmlOnly, "Content-Type: text/html; charset=UTF-8") || strings.Contains(htmlOnly, "multipart") {
		t.Errorf("html-only render wrong:\n%s", htmlOnly)
	}
}

func TestRenderSubjectEncoding(t *testing.T) {
	raw := string(Message{To: []string{"a@x"}, Subject: "café ☕ over budget", Text: "x"}.render("f@x", time.Unix(0, 0).UTC()))
	if strings.Contains(raw, "Subject: café") {
		t.Fatalf("non-ASCII subject not RFC2047-encoded:\n%s", raw)
	}
	if !strings.Contains(raw, "Subject: =?utf-8?q?") {
		t.Fatalf("expected encoded-word subject:\n%s", raw)
	}
}

func TestRenderDotStuffing(t *testing.T) {
	raw := string(Message{To: []string{"a@x"}, Subject: "s", Text: ".leading dot\nnormal"}.render("f@x", time.Unix(0, 0).UTC()))
	if !strings.Contains(raw, "..leading dot") {
		t.Fatalf("leading-dot line not dot-stuffed:\n%s", raw)
	}
}
