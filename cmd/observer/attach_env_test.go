package main

import "testing"

// TestCodexAttachEnvForwardsCodexHome pins R2-5: the codex attach client forwards
// the caller's own CODEX_HOME across the attach socket (so the daemon-spawned
// inner launcher + discovery use the caller's profile, not the daemon's), and
// forwards nothing when it is unset.
func TestCodexAttachEnvForwardsCodexHome(t *testing.T) {
	if got := codexAttachEnv([]string{"PATH=/x", "HOME=/home/u"}); got != nil {
		t.Fatalf("no CODEX_HOME → nil forwarded env, got %v", got)
	}
	got := codexAttachEnv([]string{"PATH=/x", "CODEX_HOME=/profile/codex"})
	if len(got) != 1 || got[0] != "CODEX_HOME=/profile/codex" {
		t.Fatalf("codexAttachEnv = %v, want [CODEX_HOME=/profile/codex]", got)
	}
	// Last-wins exec semantics (lookupEnvValue) so a duplicated key resolves the final value.
	got = codexAttachEnv([]string{"CODEX_HOME=/a", "CODEX_HOME=/b"})
	if len(got) != 1 || got[0] != "CODEX_HOME=/b" {
		t.Fatalf("codexAttachEnv dup = %v, want [CODEX_HOME=/b]", got)
	}
	// F3: an explicitly EMPTY CODEX_HOME= is PRESENT and must be forwarded
	// verbatim (not dropped as if unset), so it resets the child to codex's
	// default profile and overrides the daemon's inherited CODEX_HOME.
	got = codexAttachEnv([]string{"PATH=/x", "CODEX_HOME="})
	if len(got) != 1 || got[0] != "CODEX_HOME=" {
		t.Fatalf("codexAttachEnv empty = %v, want [CODEX_HOME=] (present-but-empty must override)", got)
	}
}

// TestClaudeAttachEnvForwardsConfigDir pins R2-5: the claude attach client
// forwards the caller's CLAUDE_CONFIG_DIR / ANTHROPIC_CONFIG_DIR — both of which
// the bare launcher (claudeCredentialsPath) and the claude binary honor — so a
// daemon-spawned attach resolves the same profile a bare launch would.
func TestClaudeAttachEnvForwardsConfigDir(t *testing.T) {
	if got := claudeAttachEnv([]string{"PATH=/x"}); got != nil {
		t.Fatalf("no config dir → nil forwarded env, got %v", got)
	}
	got := claudeAttachEnv([]string{"CLAUDE_CONFIG_DIR=/c", "ANTHROPIC_CONFIG_DIR=/a"})
	if len(got) != 2 || got[0] != "CLAUDE_CONFIG_DIR=/c" || got[1] != "ANTHROPIC_CONFIG_DIR=/a" {
		t.Fatalf("claudeAttachEnv = %v, want [CLAUDE_CONFIG_DIR=/c ANTHROPIC_CONFIG_DIR=/a]", got)
	}
	// Only the set one is forwarded.
	got = claudeAttachEnv([]string{"ANTHROPIC_CONFIG_DIR=/a"})
	if len(got) != 1 || got[0] != "ANTHROPIC_CONFIG_DIR=/a" {
		t.Fatalf("claudeAttachEnv single = %v, want [ANTHROPIC_CONFIG_DIR=/a]", got)
	}
	// F3: an explicitly EMPTY config-dir var is PRESENT and must be forwarded
	// verbatim so it resets the child to claude's default profile rather than
	// silently retaining the daemon's inherited value.
	got = claudeAttachEnv([]string{"CLAUDE_CONFIG_DIR="})
	if len(got) != 1 || got[0] != "CLAUDE_CONFIG_DIR=" {
		t.Fatalf("claudeAttachEnv empty = %v, want [CLAUDE_CONFIG_DIR=] (present-but-empty must override)", got)
	}
}

// TestLookupEnvValuePresence pins the F3 primitive: lookupEnvValue distinguishes
// an ABSENT key (ok=false) from one present-but-empty (ok=true, value ""), the
// distinction envValue cannot make.
func TestLookupEnvValuePresence(t *testing.T) {
	if v, ok := lookupEnvValue([]string{"PATH=/x"}, "CODEX_HOME"); ok || v != "" {
		t.Fatalf("absent key = (%q,%v), want (\"\",false)", v, ok)
	}
	if v, ok := lookupEnvValue([]string{"CODEX_HOME="}, "CODEX_HOME"); !ok || v != "" {
		t.Fatalf("present-but-empty = (%q,%v), want (\"\",true)", v, ok)
	}
	if v, ok := lookupEnvValue([]string{"CODEX_HOME=/a", "CODEX_HOME=/b"}, "CODEX_HOME"); !ok || v != "/b" {
		t.Fatalf("dup last-wins = (%q,%v), want (\"/b\",true)", v, ok)
	}
}
