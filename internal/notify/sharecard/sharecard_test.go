package sharecard

import (
	"encoding/xml"
	"strings"
	"testing"
)

func sampleData() Data {
	return Data{
		PeriodKind:  "week",
		PeriodLabel: "Jun 30 – Jul 6, 2026",
		TotalUSD:    1234.56,
		TurnCount:   1204,
		TopModels: []LineItem{
			{Label: "claude-opus-4-8", CostUSD: 812.40},
			{Label: "gpt-5.6", CostUSD: 301.10},
			{Label: "claude-sonnet-4-5", CostUSD: 121.06},
		},
		Tools: []LineItem{
			{Label: "claude-code", CostUSD: 900.00},
			{Label: "codex", CostUSD: 250.00},
			{Label: "cursor", CostUSD: 84.56},
		},
		CacheReadShare: 0.632,
		HasCacheShare:  true,
		Version:        "vtest",
	}
}

func TestMarkdown_ContainsKeyFacts(t *testing.T) {
	md := sampleData().Markdown()
	for _, want := range []string{
		"Jun 30 – Jul 6, 2026",
		"$1,234.56",
		"1,204 turns",
		"claude-opus-4-8 — $812.40",
		"63% of input tokens",
		"Tool mix:",
		"claude-code, codex, cursor",
		"superbased.app",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
	// Honesty constraint: never claim compression dollar-savings.
	if strings.Contains(strings.ToLower(md), "saved") || strings.Contains(strings.ToLower(md), "compression") {
		t.Errorf("markdown must not frame around compression savings:\n%s", md)
	}
}

func TestSVG_IsWellFormedXML(t *testing.T) {
	svg := sampleData().SVG()
	if err := xml.Unmarshal([]byte(svg), new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		t.Fatalf("SVG is not well-formed XML: %v\n%s", err, svg)
	}
	if !strings.Contains(svg, `width="1200"`) || !strings.Contains(svg, `height="630"`) {
		t.Errorf("SVG missing 1200×630 dimensions")
	}
	for _, want := range []string{"$1,234.56", "63%", "claude-opus-4-8", "TOP MODELS", "TOOL MIX", "superbased.app"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %q", want)
		}
	}
}

func TestSVG_NoExternalResources(t *testing.T) {
	// The only http(s) reference allowed is the SVG namespace URI (never
	// fetched). No fetchable external resources: images, links, imports, fonts.
	svg := sampleData().SVG()
	for _, bad := range []string{"<image", "xlink:href", "href=", "src=", "<link", "@import", "url(http", "@font-face"} {
		if strings.Contains(svg, bad) {
			t.Errorf("SVG must have no external resources, found %q", bad)
		}
	}
	// Exactly one http reference (the xmlns), and it is the w3.org SVG namespace.
	if strings.Count(svg, "http") != 1 || !strings.Contains(svg, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Errorf("unexpected http reference beyond the SVG namespace URI")
	}
}

func TestSVG_EscapesInjection(t *testing.T) {
	d := Data{
		PeriodKind:  "week",
		PeriodLabel: `<script>alert("x")</script>`,
		TotalUSD:    5,
		TopModels:   []LineItem{{Label: `evil</text><rect/>`, CostUSD: 5}},
	}
	svg := d.SVG()
	if strings.Contains(svg, "<script>") || strings.Contains(svg, "</text><rect/>") {
		t.Fatalf("unescaped injection leaked into SVG:\n%s", svg)
	}
	if err := xml.Unmarshal([]byte(svg), new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		t.Fatalf("SVG with hostile input is not well-formed: %v", err)
	}
}

func TestSVG_EmptyStateStillValid(t *testing.T) {
	d := Data{PeriodKind: "month", PeriodLabel: "June 2026"}
	svg := d.SVG()
	if !strings.Contains(svg, "No agent spend recorded this month") {
		t.Errorf("expected empty-state note, got:\n%s", svg)
	}
	if err := xml.Unmarshal([]byte(svg), new(struct {
		XMLName xml.Name `xml:"svg"`
	})); err != nil {
		t.Fatalf("empty-state SVG not well-formed: %v", err)
	}
}

func TestMoneyFormatting(t *testing.T) {
	cases := map[float64]string{
		0:          "$0.00",
		9.5:        "$9.50",
		1000:       "$1,000.00",
		1234567.89: "$1,234,567.89",
		0.005:      "$0.01",
	}
	for in, want := range cases {
		if got := money(in); got != want {
			t.Errorf("money(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestPercentAndPeriodPhrase(t *testing.T) {
	if got := percent(0.634); got != "63%" {
		t.Errorf("percent = %q", got)
	}
	if got := percent(1.5); got != "100%" {
		t.Errorf("percent clamp = %q", got)
	}
	if (Data{PeriodKind: "month"}).periodPhrase() != "this month" {
		t.Error("month phrase wrong")
	}
	if (Data{PeriodKind: ""}).periodPhrase() != "this period" {
		t.Error("default phrase wrong")
	}
}
