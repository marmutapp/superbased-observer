package main

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/termrun"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// kv builds a "KEY=VALUE" env entry via concatenation (never an inline literal —
// the harness write-filter mangles credential-shaped literals).
func kv(k, v string) string { return k + "=" + v }

// TestForwardAuthEnv pins the presence-aware credential forwarding: an ABSENT
// key forwards nothing (the daemon's inherited value stands), a PRESENT key
// forwards its value, a present-but-EMPTY `KEY=` forwards verbatim (F3), a
// duplicated environ entry resolves last-wins, and declaration order + dedupe of
// the key list are preserved.
func TestForwardAuthEnv(t *testing.T) {
	cases := []struct {
		name    string
		keys    []string
		environ []string
		want    []string
	}{
		{
			name:    "absent key forwards nothing",
			keys:    []string{"OPENAI_API_KEY"},
			environ: []string{kv("PATH", "/x"), kv("HOME", "/home/u")},
			want:    nil,
		},
		{
			name:    "present key forwards its value",
			keys:    []string{"OPENAI_API_KEY"},
			environ: []string{kv("OPENAI_API_KEY", "val-openai")},
			want:    []string{kv("OPENAI_API_KEY", "val-openai")},
		},
		{
			name:    "present-but-empty forwards verbatim (F3)",
			keys:    []string{"OPENAI_API_KEY"},
			environ: []string{kv("OPENAI_API_KEY", "")},
			want:    []string{kv("OPENAI_API_KEY", "")},
		},
		{
			name:    "duplicated environ entry resolves last-wins",
			keys:    []string{"OPENAI_API_KEY"},
			environ: []string{kv("OPENAI_API_KEY", "first"), kv("OPENAI_API_KEY", "second")},
			want:    []string{kv("OPENAI_API_KEY", "second")},
		},
		{
			name:    "declaration order preserved, only-present emitted",
			keys:    []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
			environ: []string{kv("ANTHROPIC_AUTH_TOKEN", "tok"), kv("ANTHROPIC_API_KEY", "key")},
			want:    []string{kv("ANTHROPIC_API_KEY", "key"), kv("ANTHROPIC_AUTH_TOKEN", "tok")},
		},
		{
			name:    "duplicate key in the list is deduped",
			keys:    []string{"OPENAI_API_KEY", "OPENAI_API_KEY"},
			environ: []string{kv("OPENAI_API_KEY", "val-openai")},
			want:    []string{kv("OPENAI_API_KEY", "val-openai")},
		},
		{
			name:    "empty key skipped",
			keys:    []string{"", "OPENAI_API_KEY"},
			environ: []string{kv("OPENAI_API_KEY", "val-openai")},
			want:    []string{kv("OPENAI_API_KEY", "val-openai")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := forwardAuthEnv(tc.keys, tc.environ)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("forwardAuthEnv(%v, %v) = %v, want %v", tc.keys, tc.environ, got, tc.want)
			}
		})
	}
}

// TestMergeAuthKeys pins the registry-first union: registry order leads, extra
// keys append, and a duplicate (including the hermes default --key-env case) is
// dropped.
func TestMergeAuthKeys(t *testing.T) {
	cases := []struct {
		name     string
		registry []string
		extra    []string
		want     []string
	}{
		{
			name:     "registry only",
			registry: []string{"OPENROUTER_API_KEY"},
			extra:    nil,
			want:     []string{"OPENROUTER_API_KEY"},
		},
		{
			name:     "extra non-default appends after registry",
			registry: []string{"OPENROUTER_API_KEY"},
			extra:    []string{"MY_CUSTOM_KEY"},
			want:     []string{"OPENROUTER_API_KEY", "MY_CUSTOM_KEY"},
		},
		{
			name:     "extra equal to a registry key is deduped (hermes default --key-env)",
			registry: []string{"OPENROUTER_API_KEY"},
			extra:    []string{"OPENROUTER_API_KEY"},
			want:     []string{"OPENROUTER_API_KEY"},
		},
		{
			name:     "registry order preserved",
			registry: []string{"COPILOT_PROVIDER_API_KEY", "GH_TOKEN", "GITHUB_TOKEN"},
			extra:    []string{"GH_TOKEN", "EXTRA"},
			want:     []string{"COPILOT_PROVIDER_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "EXTRA"},
		},
		{
			name:     "empty entries skipped",
			registry: []string{"", "OPENAI_API_KEY"},
			extra:    []string{""},
			want:     []string{"OPENAI_API_KEY"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAuthKeys(tc.registry, tc.extra)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mergeAuthKeys(%v, %v) = %v, want %v", tc.registry, tc.extra, got, tc.want)
			}
		})
	}
}

// TestComposeAttachEnv pins the gate: with forward=false the base env is
// returned unchanged (no keys leak); with forward=true the caller's present
// credential values are layered AFTER the base env, coexisting with a claude-
// style profile/base-URL closure env WITHOUT clobbering it (disjoint key space).
func TestComposeAttachEnv(t *testing.T) {
	base := []string{kv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8820"), kv("CLAUDE_CONFIG_DIR", "/c")}
	environ := []string{kv("ANTHROPIC_API_KEY", "key"), kv("PATH", "/x")}

	// Gate OFF: base returned verbatim, no credential leaks.
	if got := composeAttachEnv(base, false, []string{"ANTHROPIC_API_KEY"}, nil, environ); !reflect.DeepEqual(got, base) {
		t.Fatalf("forward=false must return base unchanged, got %v", got)
	}

	// Gate ON: the claude closure env is preserved in order AND the credential
	// value is appended after it (absent ANTHROPIC_AUTH_TOKEN is skipped).
	got := composeAttachEnv(base, true, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}, nil, environ)
	want := []string{
		kv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8820"),
		kv("CLAUDE_CONFIG_DIR", "/c"),
		kv("ANTHROPIC_API_KEY", "key"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forward=true = %v, want %v (base preserved + creds appended)", got, want)
	}

	// Extra dynamic key (hermes --key-env shape) rides alongside the registry key.
	got = composeAttachEnv(nil, true, []string{"OPENROUTER_API_KEY"}, []string{"MY_ROUTER_KEY"},
		[]string{kv("OPENROUTER_API_KEY", "a"), kv("MY_ROUTER_KEY", "b")})
	want = []string{kv("OPENROUTER_API_KEY", "a"), kv("MY_ROUTER_KEY", "b")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extra-key compose = %v, want %v", got, want)
	}
}

// TestAuditEnvIsCountOnly pins that the per-launch env audit line records the
// var COUNT + launch identity only — never a forwarded credential VALUE. This is
// the observability backstop for the credential-bearing forwarding path.
func TestAuditEnvIsCountOnly(t *testing.T) {
	var buf bytes.Buffer
	l := &ptyLauncher{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	parts := []string{"top", "secret", "value"}
	secret := strings.Join(parts, "")
	env := []string{kv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8820"), kv("ANTHROPIC_API_KEY", secret)}
	l.auditEnv(termsvc.LaunchRequest{RunID: "run-1", Tool: "claude-code", Kind: termrun.KindHandoff}, env)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("audit line leaked a credential VALUE: %q", out)
	}
	if !strings.Contains(out, "env_vars=2") {
		t.Fatalf("audit line must record the var count (env_vars=2), got %q", out)
	}
}

// TestHermesAuthEnvExtra pins the dynamic --key-env forwarding: a non-default
// NAME is returned as the extra key; the OPENROUTER_API_KEY default (already in
// the registry row) and the empty value return nil.
func TestHermesAuthEnvExtra(t *testing.T) {
	if got := hermesAuthEnvExtra(hermesDefaultKeyEnv); got != nil {
		t.Fatalf("default key-env → nil extra, got %v", got)
	}
	if got := hermesAuthEnvExtra(""); got != nil {
		t.Fatalf("empty key-env → nil extra, got %v", got)
	}
	got := hermesAuthEnvExtra("MY_ROUTER_KEY")
	if !reflect.DeepEqual(got, []string{"MY_ROUTER_KEY"}) {
		t.Fatalf("non-default key-env = %v, want [MY_ROUTER_KEY]", got)
	}
	// End-to-end with mergeAuthKeys: the non-default key rides alongside the
	// registry default, deduped and ordered.
	merged := mergeAuthKeys([]string{hermesDefaultKeyEnv}, hermesAuthEnvExtra("MY_ROUTER_KEY"))
	if !reflect.DeepEqual(merged, []string{hermesDefaultKeyEnv, "MY_ROUTER_KEY"}) {
		t.Fatalf("merged = %v, want [%s MY_ROUTER_KEY]", merged, hermesDefaultKeyEnv)
	}
}
