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

// TestApplyCostFilters_Empty (V4-2) pins the no-op contract: empty
// filter list returns the summary unchanged.
func TestApplyCostFilters_Empty(t *testing.T) {
	in := cost.Summary{Rows: []cost.Row{{Key: "a"}, {Key: "b"}}}
	got := applyCostFilters(in, nil)
	if len(got.Rows) != 2 {
		t.Errorf("got %d rows, want 2 (no-op)", len(got.Rows))
	}
}

// TestApplyCostFilters_SubstringOR (V4-2) pins multi-filter
// substring matching with OR semantics + case-insensitivity.
func TestApplyCostFilters_SubstringOR(t *testing.T) {
	in := cost.Summary{Rows: []cost.Row{
		{Key: "claude-sonnet-4-6"},
		{Key: "gpt-5-mini"},
		{Key: "gemini-2.5-pro"},
		{Key: "claude-haiku-4-5"},
	}}
	got := applyCostFilters(in, []string{"claude", "gpt"})
	if len(got.Rows) != 3 {
		t.Errorf("got %d rows, want 3 (claude+gpt OR): %v", len(got.Rows), got.Rows)
	}
	for _, r := range got.Rows {
		if !strings.Contains(strings.ToLower(r.Key), "claude") &&
			!strings.Contains(strings.ToLower(r.Key), "gpt") {
			t.Errorf("unmatched row leaked: %v", r.Key)
		}
	}
}

// TestApplyCostFilters_CaseInsensitive: filter "CLAUDE" matches
// "claude-sonnet" — operators don't have to know the canonical case.
func TestApplyCostFilters_CaseInsensitive(t *testing.T) {
	in := cost.Summary{Rows: []cost.Row{{Key: "claude-sonnet-4-6"}}}
	got := applyCostFilters(in, []string{"CLAUDE"})
	if len(got.Rows) != 1 {
		t.Errorf("got %d, want 1 (case-insensitive)", len(got.Rows))
	}
}

// TestApplyCostFilters_NoMatch: all-misses produces an empty result
// (and printCostSummary then renders "no cost data in range").
func TestApplyCostFilters_NoMatch(t *testing.T) {
	in := cost.Summary{Rows: []cost.Row{{Key: "claude-sonnet"}}}
	got := applyCostFilters(in, []string{"nonexistent"})
	if len(got.Rows) != 0 {
		t.Errorf("got %d, want 0 (no match)", len(got.Rows))
	}
}
