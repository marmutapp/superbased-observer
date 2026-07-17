package processobs

import (
	"strings"
	"testing"
)

func TestScrubArgvModes(t *testing.T) {
	t.Parallel()
	argv := []string{"npm", "test", "--secret=hunter2"}

	cases := []struct {
		name          string
		mode          string
		storeArgCount bool
		wantPreview   bool // preview non-empty
		wantArgc      int
	}{
		{"preview", "preview", true, true, 3},
		{"hash_only", "hash_only", true, false, 3},
		{"hash_only_no_argc", "hash_only", false, false, 0},
		{"off", "off", true, false, 3},
		{"unknown_falls_back_to_preview", "weird", true, true, 3},
	}
	var hashes []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &FieldScrubber{ArgvMode: tc.mode, MaxPreviewBytes: 512, StoreArgCount: tc.storeArgCount}
			preview, hash, argc := s.ScrubArgv(argv)
			if (preview != "") != tc.wantPreview {
				t.Errorf("preview=%q wantNonEmpty=%v", preview, tc.wantPreview)
			}
			if argc != tc.wantArgc {
				t.Errorf("argc=%d want %d", argc, tc.wantArgc)
			}
			if hash == "" || !strings.HasPrefix(hash, "sha256:") {
				t.Errorf("hash must always be set and prefixed: %q", hash)
			}
			hashes = append(hashes, hash)
		})
	}
	// The hash is identical across all modes for the same argv (it is a
	// pure fingerprint, independent of preview policy).
	for _, h := range hashes {
		if h != hashes[0] {
			t.Fatalf("argv hash varied by mode: %q vs %q", h, hashes[0])
		}
	}

	if p, h, a := (&FieldScrubber{ArgvMode: "preview"}).ScrubArgv(nil); p != "" || h != "" || a != 0 {
		t.Errorf("empty argv must yield zeroes, got %q/%q/%d", p, h, a)
	}
}

func TestScrubArgvRedactsAndCaps(t *testing.T) {
	t.Parallel()
	s := &FieldScrubber{
		ArgvMode:        "preview",
		MaxPreviewBytes: 10,
		Redact:          func(in string) string { return strings.ReplaceAll(in, "hunter2", "[R]") },
	}
	preview, _, _ := s.ScrubArgv([]string{"login", "--pw", "hunter2"})
	if strings.Contains(preview, "hunter2") {
		t.Errorf("secret survived redaction: %q", preview)
	}
	if len([]byte(preview)) > 10+len("…") {
		t.Errorf("preview not capped: %q (%d bytes)", preview, len(preview))
	}
}

func TestCapIsRuneSafe(t *testing.T) {
	t.Parallel()
	s := &FieldScrubber{MaxPreviewBytes: 5}
	// "héllo" — 'é' is 2 bytes; a naive byte cut at 5 could split it.
	got := s.cap("héllo world")
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis on truncation: %q", got)
	}
	// The kept prefix must be valid UTF-8 (no split rune).
	for _, r := range got {
		if r == '�' {
			t.Errorf("truncation split a rune: %q", got)
		}
	}
}

func TestEnvPosture(t *testing.T) {
	t.Parallel()
	s := &FieldScrubber{
		EnvEnabled:    true,
		StorePathHash: true,
		EnvAllowlist:  []string{"MY_FLAG", "MY_SECRET_TOKEN"},
	}
	env := map[string]string{
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
		"PATH":               "/usr/bin:/bin",
		"VIRTUAL_ENV":        "/home/u/proj/.venv",
		"CI":                 "true",
		"MY_FLAG":            "on",
		"MY_SECRET_TOKEN":    "sk-abc123",
		"UNLISTED":           "whatever",
	}
	got := s.EnvPosture(env)

	if got["ANTHROPIC_BASE_URL_present"] != "true" {
		t.Error("proxy var presence not recorded")
	}
	if h := got["PATH_hash"]; !strings.HasPrefix(h, "sha256:") {
		t.Errorf("PATH must be hashed, got %q", h)
	}
	if _, leaked := got["PATH"]; leaked {
		t.Error("raw PATH value leaked")
	}
	if got["VIRTUAL_ENV_basename"] != ".venv" {
		t.Errorf("venv basename = %q, want .venv", got["VIRTUAL_ENV_basename"])
	}
	if got["CI_present"] != "true" {
		t.Error("CI presence not recorded")
	}
	if got["MY_FLAG"] != "on" {
		t.Errorf("allowlisted non-secret value = %q, want on", got["MY_FLAG"])
	}
	// A secret-looking allowlist key records presence ONLY — never value.
	if _, leaked := got["MY_SECRET_TOKEN"]; leaked {
		t.Error("secret-named allowlist key leaked its value")
	}
	if got["MY_SECRET_TOKEN_present"] != "true" {
		t.Error("secret-named allowlist key should record presence")
	}
	if _, ok := got["UNLISTED"]; ok {
		t.Error("non-allowlisted key must not be captured")
	}

	// Disabled → nil regardless of input.
	if (&FieldScrubber{EnvEnabled: false}).EnvPosture(env) != nil {
		t.Error("EnvEnabled=false must yield nil")
	}
}

func TestHashStringEmpty(t *testing.T) {
	t.Parallel()
	if HashString("") != "" {
		t.Error("empty input must hash to empty")
	}
	if !strings.HasPrefix(HashString("x"), "sha256:") {
		t.Error("non-empty input must be prefixed")
	}
}
