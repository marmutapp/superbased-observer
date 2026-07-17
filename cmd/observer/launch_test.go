package main

import (
	"strings"
	"testing"
)

func TestResolveProxyURL(t *testing.T) {
	cases := []struct {
		name     string
		port     int
		override string
		want     string
	}{
		{"override wins", 8820, "http://example:9000", "http://example:9000"},
		{"default port when zero", 0, "", "http://127.0.0.1:8820"},
		{"default port when negative", -1, "", "http://127.0.0.1:8820"},
		{"configured port", 9100, "", "http://127.0.0.1:9100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProxyURL(tc.port, tc.override); got != tc.want {
				t.Errorf("resolveProxyURL(%d, %q) = %q, want %q", tc.port, tc.override, got, tc.want)
			}
		})
	}
}

func TestResolveToolBin(t *testing.T) {
	t.Run("override path returned verbatim", func(t *testing.T) {
		got, err := resolveToolBin("cline", "/opt/cline", "--cline-path")
		if err != nil || got != "/opt/cline" {
			t.Fatalf("got (%q, %v), want (/opt/cline, nil)", got, err)
		}
	})
	t.Run("missing binary errors name the flag", func(t *testing.T) {
		_, err := resolveToolBin("definitely-not-a-real-binary-xyz", "", "--x-path")
		if err == nil || !strings.Contains(err.Error(), "--x-path") {
			t.Fatalf("err = %v, want one mentioning --x-path", err)
		}
	})
}

func TestApplyBaseURLEnv(t *testing.T) {
	t.Run("injects unset keys, sorted, appended", func(t *testing.T) {
		env, applied, presets := applyBaseURLEnv(
			[]string{"PATH=/bin"},
			map[string]string{"OPENAI_BASE_URL": "http://p/v1", "COPILOT_PROVIDER_TYPE": "openai"},
		)
		if len(presets) != 0 {
			t.Errorf("presets = %v, want none", presets)
		}
		if !containsKV(env, "OPENAI_BASE_URL=http://p/v1") || !containsKV(env, "COPILOT_PROVIDER_TYPE=openai") {
			t.Errorf("env missing injected keys: %v", env)
		}
		// applied is sorted for determinism.
		if strings.Join(applied, ",") != "COPILOT_PROVIDER_TYPE,OPENAI_BASE_URL" {
			t.Errorf("applied = %v, want sorted", applied)
		}
	})

	t.Run("user's non-empty value wins (preset, not clobbered)", func(t *testing.T) {
		env, applied, presets := applyBaseURLEnv(
			[]string{"OPENAI_BASE_URL=https://mine/v1"},
			map[string]string{"OPENAI_BASE_URL": "http://p/v1"},
		)
		if len(applied) != 0 {
			t.Errorf("applied = %v, want none (user wins)", applied)
		}
		if strings.Join(presets, ",") != "OPENAI_BASE_URL" {
			t.Errorf("presets = %v", presets)
		}
		if envValue(env, "OPENAI_BASE_URL") != "https://mine/v1" {
			t.Errorf("user value clobbered: %q", envValue(env, "OPENAI_BASE_URL"))
		}
		if countKey(env, "OPENAI_BASE_URL") != 1 {
			t.Errorf("OPENAI_BASE_URL appears %d times, want 1", countKey(env, "OPENAI_BASE_URL"))
		}
	})

	t.Run("empty existing value counts as unset", func(t *testing.T) {
		_, applied, presets := applyBaseURLEnv(
			[]string{"OPENAI_BASE_URL="},
			map[string]string{"OPENAI_BASE_URL": "http://p/v1"},
		)
		if len(presets) != 0 {
			t.Errorf("presets = %v, want none (empty is unset)", presets)
		}
		if len(applied) != 1 {
			t.Errorf("applied = %v, want injected", applied)
		}
	})

	t.Run("never injects a key not in the inject map (no secret leakage)", func(t *testing.T) {
		env, _, _ := applyBaseURLEnv(
			[]string{"PATH=/bin"},
			map[string]string{"OPENAI_BASE_URL": "http://p/v1"},
		)
		if countKey(env, "COPILOT_PROVIDER_API_KEY") != 0 {
			t.Error("helper injected a key it was never given")
		}
	})
}

func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "B=2", "A=3"}
	if got := envValue(env, "A"); got != "3" { // last wins
		t.Errorf("envValue last-wins = %q, want 3", got)
	}
	if got := envValue(env, "Z"); got != "" {
		t.Errorf("envValue absent = %q, want empty", got)
	}
}
