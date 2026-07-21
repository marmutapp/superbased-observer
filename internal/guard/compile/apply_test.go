package compile_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/guard/compile"
)

// applyTarget fetches an implemented target by dialect.
func applyTarget(t *testing.T, dialect string) compile.Target {
	t.Helper()
	tgt, ok := compile.TargetFor(dialect)
	if !ok || !tgt.Implemented {
		t.Fatalf("target %s missing or not implemented", dialect)
	}
	return tgt
}

// claudePerms decodes the permissions arrays out of an apply result.
func claudePerms(t *testing.T, raw []byte) (deny, ask []string, top map[string]json.RawMessage) {
	t.Helper()
	top = map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, raw)
	}
	var perms struct {
		Deny []string `json:"deny"`
		Ask  []string `json:"ask"`
	}
	if p, ok := top["permissions"]; ok {
		if err := json.Unmarshal(p, &perms); err != nil {
			t.Fatalf("unmarshal permissions: %v", err)
		}
	}
	return perms.Deny, perms.Ask, top
}

// TestApplyClaudeCode_FreshFile covers the empty/missing settings
// case: all wanted entries land in their arrays, nothing else is
// invented.
func TestApplyClaudeCode_FreshFile(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "claude-code")
	want := []compile.Entry{
		{Action: "deny", Value: "Bash(mkfs:*)"},
		{Action: "ask", Value: "Bash(rm -rf:*)"},
	}
	res, err := tgt.Apply(nil, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Added) != 2 || len(res.Removed) != 0 || res.Updated == nil {
		t.Fatalf("res = %+v", res)
	}
	deny, ask, top := claudePerms(t, res.Updated)
	if len(deny) != 1 || deny[0] != "Bash(mkfs:*)" || len(ask) != 1 || ask[0] != "Bash(rm -rf:*)" {
		t.Errorf("arrays = deny%v ask%v", deny, ask)
	}
	if len(top) != 1 {
		t.Errorf("fresh file grew extra top-level keys: %v", top)
	}
}

// TestApplyClaudeCode_NeverClobber pins the register.go hygiene
// contract: unknown top-level keys, unknown permissions sub-keys and
// user-authored entries all survive; user entries keep their position
// ahead of appended managed entries.
func TestApplyClaudeCode_NeverClobber(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "claude-code")
	existing := []byte(`{
  "hooks": {"PreToolUse": [{"matcher": "*"}]},
  "model": "opus",
  "permissions": {
    "allow": ["Bash(npm run lint)"],
    "deny": ["WebFetch(domain:evil.example)"],
    "additionalDirectories": ["../docs"]
  }
}`)
	want := []compile.Entry{{Action: "deny", Value: "Bash(mkfs:*)"}}
	res, err := tgt.Apply(existing, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	deny, _, top := claudePerms(t, res.Updated)
	if len(deny) != 2 || deny[0] != "WebFetch(domain:evil.example)" || deny[1] != "Bash(mkfs:*)" {
		t.Errorf("deny = %v, want user entry first, managed appended", deny)
	}
	for _, key := range []string{"hooks", "model"} {
		if _, ok := top[key]; !ok {
			t.Errorf("top-level %q clobbered", key)
		}
	}
	var perms map[string]json.RawMessage
	_ = json.Unmarshal(top["permissions"], &perms)
	for _, key := range []string{"allow", "additionalDirectories"} {
		if _, ok := perms[key]; !ok {
			t.Errorf("permissions.%q clobbered", key)
		}
	}
	// Key order preserved: "hooks" still precedes "permissions".
	if hooksIdx, permsIdx := strings.Index(string(res.Updated), `"hooks"`), strings.Index(string(res.Updated), `"permissions"`); hooksIdx > permsIdx {
		t.Error("top-level key order not preserved")
	}
}

// TestApplyClaudeCode_IdempotentAndRetire covers re-apply (no change)
// and managed-entry retirement: a universe entry the policy no longer
// wants is removed; a user entry outside the universe is not.
func TestApplyClaudeCode_IdempotentAndRetire(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "claude-code")
	want := []compile.Entry{{Action: "deny", Value: "Bash(mkfs:*)"}}
	first, err := tgt.Apply(nil, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second, err := tgt.Apply(first.Updated, want)
	if err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if second.Updated != nil || len(second.Added)+len(second.Removed) != 0 {
		t.Fatalf("re-apply not idempotent: %+v", second)
	}

	// Policy shrinks: mkfs is retired, diskpart (never present) added;
	// the user's own deny entry survives.
	existing := []byte(`{"permissions":{"deny":["Bash(mkfs:*)","Bash(my-own-tool:*)"]}}`)
	res, err := tgt.Apply(existing, []compile.Entry{{Action: "deny", Value: "Bash(diskpart:*)"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Value != "Bash(mkfs:*)" {
		t.Errorf("Removed = %v", res.Removed)
	}
	deny, _, _ := claudePerms(t, res.Updated)
	if len(deny) != 2 || deny[0] != "Bash(my-own-tool:*)" || deny[1] != "Bash(diskpart:*)" {
		t.Errorf("deny = %v", deny)
	}
}

// TestApplyClaudeCode_ActionMove covers an entry whose action changed
// (deny→ask override): it leaves the deny array and joins ask.
func TestApplyClaudeCode_ActionMove(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "claude-code")
	existing := []byte(`{"permissions":{"deny":["Bash(mkfs:*)"]}}`)
	res, err := tgt.Apply(existing, []compile.Entry{{Action: "ask", Value: "Bash(mkfs:*)"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	deny, ask, _ := claudePerms(t, res.Updated)
	if len(deny) != 0 || len(ask) != 1 || ask[0] != "Bash(mkfs:*)" {
		t.Errorf("deny=%v ask=%v, want entry moved deny→ask", deny, ask)
	}
}

// TestApplyClaudeCode_Malformed pins the refuse-to-write contract on
// content we cannot safely understand.
func TestApplyClaudeCode_Malformed(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "claude-code")
	for name, body := range map[string]string{
		"broken json":    `{"permissions":`,
		"non-object":     `[1,2]`,
		"non-array deny": `{"permissions":{"deny":"Bash(x)"}}`,
	} {
		if _, err := tgt.Apply([]byte(body), []compile.Entry{{Action: "deny", Value: "Bash(mkfs:*)"}}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// openCodeBashKeys returns permission.bash's keys in file order plus
// the decoded map — order is SEMANTIC in this dialect.
func openCodeBashKeys(t *testing.T, raw []byte) ([]string, map[string]string) {
	t.Helper()
	// Walk the raw bytes with a decoder to recover key order.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var keys []string
	vals := map[string]string{}
	var walk func(prefix string)
	walk = func(prefix string) {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			t.Fatalf("expected object at %s", prefix)
		}
		for dec.More() {
			keyTok, _ := dec.Token()
			key := keyTok.(string)
			full := prefix + "." + key
			if full == ".permission.bash" {
				var inner map[string]json.RawMessage
				_ = inner
				tok, _ := dec.Token()
				if d, ok := tok.(json.Delim); !ok || d != '{' {
					t.Fatalf("permission.bash is not an object")
				}
				for dec.More() {
					kt, _ := dec.Token()
					k := kt.(string)
					var v string
					if err := dec.Decode(&v); err != nil {
						t.Fatalf("bash value: %v", err)
					}
					keys = append(keys, k)
					vals[k] = v
				}
				_, _ = dec.Token()
				continue
			}
			if full == ".permission" {
				walk(full)
				continue
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatalf("skip %s: %v", full, err)
			}
		}
		_, _ = dec.Token()
	}
	walk("")
	return keys, vals
}

// TestApplyOpenCode_FreshFile covers the empty/missing config case.
func TestApplyOpenCode_FreshFile(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "opencode")
	want := []compile.Entry{
		{Action: "deny", Value: "mkfs*"},
		{Action: "ask", Value: "rm -rf *"},
	}
	res, err := tgt.Apply(nil, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	keys, vals := openCodeBashKeys(t, res.Updated)
	if len(keys) != 2 || vals["mkfs*"] != "deny" || vals["rm -rf *"] != "ask" {
		t.Errorf("bash = %v %v", keys, vals)
	}
	if strings.Contains(string(res.Updated), `"*"`) {
		t.Error("fresh file grew a catch-all '*' entry — the client default must stay the client's")
	}
}

// TestApplyOpenCode_LastMatchWinsOrder pins the §13.2 ordering
// inversion: managed entries are appended AFTER user entries so they
// win under OpenCode's last-match-wins resolution.
func TestApplyOpenCode_LastMatchWinsOrder(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "opencode")
	existing := []byte(`{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "edit": "allow",
    "bash": {
      "git push *": "allow",
      "ls *": "allow"
    }
  }
}`)
	want := []compile.Entry{{Action: "ask", Value: "git push --force*"}}
	res, err := tgt.Apply(existing, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	keys, vals := openCodeBashKeys(t, res.Updated)
	if len(keys) != 3 || keys[0] != "git push *" || keys[1] != "ls *" || keys[2] != "git push --force*" {
		t.Fatalf("key order = %v, want user entries first, managed appended last", keys)
	}
	if vals["git push --force*"] != "ask" {
		t.Errorf("managed value = %q", vals["git push --force*"])
	}
	if !strings.Contains(string(res.Updated), `"$schema"`) || !strings.Contains(string(res.Updated), `"edit"`) {
		t.Error("user keys clobbered")
	}
}

// TestApplyOpenCode_StringBashConverts covers the global-default
// string form: it becomes {"*": <default>, managed...} — the default
// stays first (lowest precedence), managed entries win.
func TestApplyOpenCode_StringBashConverts(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "opencode")
	existing := []byte(`{"permission":{"bash":"allow"}}`)
	res, err := tgt.Apply(existing, []compile.Entry{{Action: "deny", Value: "mkfs*"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	keys, vals := openCodeBashKeys(t, res.Updated)
	if len(keys) != 2 || keys[0] != "*" || vals["*"] != "allow" || keys[1] != "mkfs*" || vals["mkfs*"] != "deny" {
		t.Errorf("converted bash = %v %v", keys, vals)
	}

	// With nothing to install, the string form is left untouched.
	res, err = tgt.Apply(existing, nil)
	if err != nil || res.Updated != nil {
		t.Errorf("empty want rewrote the file: %+v, %v", res, err)
	}
}

// TestApplyOpenCode_RetireOverrideIdempotent covers managed-key
// retirement, the policy-floor overwrite of a re-pointed managed key,
// and idempotent re-apply.
func TestApplyOpenCode_RetireOverrideIdempotent(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "opencode")
	existing := []byte(`{"permission":{"bash":{"mkfs*":"allow","my own *":"deny","diskpart*":"deny"}}}`)
	want := []compile.Entry{{Action: "deny", Value: "mkfs*"}}
	res, err := tgt.Apply(existing, want)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	keys, vals := openCodeBashKeys(t, res.Updated)
	if vals["mkfs*"] != "deny" {
		t.Errorf("managed key not set back to deny: %v", vals)
	}
	if _, ok := vals["diskpart*"]; ok {
		t.Error("retired managed key survived")
	}
	if vals["my own *"] != "deny" {
		t.Error("user key clobbered")
	}
	if len(res.Removed) != 1 || res.Removed[0].Value != "diskpart*" {
		t.Errorf("Removed = %v", res.Removed)
	}
	_ = keys

	second, err := tgt.Apply(res.Updated, want)
	if err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if second.Updated != nil || len(second.Added)+len(second.Removed) != 0 {
		t.Errorf("re-apply not idempotent: %+v", second)
	}
}

// TestApplyOpenCode_Malformed pins refuse-to-write on malformed or
// commented (JSONC) content — surfaced as an error, never a rewrite.
func TestApplyOpenCode_Malformed(t *testing.T) {
	t.Parallel()
	tgt := applyTarget(t, "opencode")
	for name, body := range map[string]string{
		"broken":      `{"permission":`,
		"jsonc":       "{\n  // user comment\n  \"permission\": {}\n}",
		"bash number": `{"permission":{"bash":42}}`,
	} {
		if _, err := tgt.Apply([]byte(body), []compile.Entry{{Action: "deny", Value: "mkfs*"}}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
