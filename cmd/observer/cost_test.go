package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

func TestParseGroupBy(t *testing.T) {
	cases := []struct {
		in   string
		want cost.GroupBy
		err  bool
	}{
		{"", cost.GroupByModel, false},
		{"model", cost.GroupByModel, false},
		{"MODEL", cost.GroupByModel, false},
		{"session", cost.GroupBySession, false},
		{"day", cost.GroupByDay, false},
		{"project", cost.GroupByProject, false},
		{"tool", cost.GroupByTool, false},
		{"none", cost.GroupByNone, false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		got, err := parseGroupBy(tc.in)
		if (err != nil) != tc.err {
			t.Errorf("parseGroupBy(%q): err=%v want err=%v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && got != tc.want {
			t.Errorf("parseGroupBy(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSource(t *testing.T) {
	cases := []struct {
		in   string
		want cost.Source
		err  bool
	}{
		{"", cost.SourceAuto, false},
		{"auto", cost.SourceAuto, false},
		{"AUTO", cost.SourceAuto, false},
		{"proxy", cost.SourceProxy, false},
		{"jsonl", cost.SourceJSONL, false},
		{"mixed", "", true},
	}
	for _, tc := range cases {
		got, err := parseSource(tc.in)
		if (err != nil) != tc.err {
			t.Errorf("parseSource(%q): err=%v want err=%v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && got != tc.want {
			t.Errorf("parseSource(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{9_999, "9999"},
		{10_000, "10.0k"},
		{123_400, "123.4k"},
		{1_000_000, "1.00M"},
		{1_234_567, "1.23M"},
	}
	for _, tc := range cases {
		if got := formatTokens(tc.in); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintCostSummary_Empty(t *testing.T) {
	var buf bytes.Buffer
	printCostSummary(&buf, cost.Summary{})
	if !strings.Contains(buf.String(), "no cost data in range") {
		t.Errorf("empty summary output: %q", buf.String())
	}
}

func TestPrintCostSummary_RendersTotalsAndUnknownBanner(t *testing.T) {
	var buf bytes.Buffer
	printCostSummary(&buf, cost.Summary{
		GroupBy: cost.GroupByModel,
		Source:  cost.SourceAuto,
		Days:    7,
		Rows: []cost.Row{{
			Key:           "claude-sonnet-4-20250514",
			Tokens:        cost.TokenBundle{Input: 1_000_000, Output: 500_000},
			CostUSD:       10.5,
			TurnCount:     3,
			Source:        "proxy",
			Reliability:   "accurate",
			UnknownModels: nil,
		}},
		TotalTokens:       cost.TokenBundle{Input: 1_000_000, Output: 500_000},
		TotalCost:         10.5,
		TurnCount:         3,
		Reliability:       "accurate",
		UnknownModelCount: 1,
	})
	out := buf.String()
	for _, want := range []string{
		"claude-sonnet-4-20250514",
		"1.00M",
		"500.0k",
		"$10.5000",
		"TOTAL",
		"1 model(s) without pricing",
		"window: last 7 day(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
