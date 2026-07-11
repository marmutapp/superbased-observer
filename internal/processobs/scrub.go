package processobs

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// RedactFunc removes secrets from a string. The caller injects the
// existing secret scrubber (internal/scrub.Scrub) so this pure package
// does not hard-depend on it. A nil RedactFunc is treated as identity
// (no redaction) — callers that care about privacy must supply one.
type RedactFunc func(string) string

// FieldScrubber applies the field-level capping/hashing/posture rules
// from spec §8 + §12.2 before a run is persisted. It holds policy
// (argv mode, caps, allowlist) and an injected RedactFunc; it is pure
// and safe for concurrent use (no mutable state).
type FieldScrubber struct {
	// ArgvMode is "preview" | "hash_only" | "off".
	ArgvMode string
	// MaxPreviewBytes caps the stored argv preview (and any other capped
	// string). Zero disables capping.
	MaxPreviewBytes int
	// StoreArgCount keeps argc even when the preview is suppressed.
	StoreArgCount bool
	// EnvEnabled gates environment-posture capture.
	EnvEnabled bool
	// EnvAllowlist names env keys whose scrubbed value may be stored.
	EnvAllowlist []string
	// StorePathHash stores a hash of PATH rather than its value.
	StorePathHash bool
	// Redact removes secrets; nil = identity.
	Redact RedactFunc
}

// redact applies the injected RedactFunc, tolerating a nil func.
func (s *FieldScrubber) redact(in string) string {
	if s.Redact == nil {
		return in
	}
	return s.Redact(in)
}

// ScrubArgv reduces an argv slice to (preview, hash, argc) per the
// configured ArgvMode (spec §8 Command, §19 Q1):
//
//   - hash is ALWAYS computed over the full, unredacted argv joined by NUL
//     — it is a stable fingerprint, never reversible to content, and the
//     §9.2.4 action-correlation pass matches on the argv prefix via the
//     preview, not the hash.
//   - preview is the scrubbed, space-joined argv, capped to MaxPreviewBytes;
//     empty in "hash_only" / "off" modes.
//   - argc is len(argv) when StoreArgCount (or in preview mode), else 0.
func (s *FieldScrubber) ScrubArgv(argv []string) (preview, hash string, argc int) {
	if len(argv) == 0 {
		return "", "", 0
	}
	hash = hashStrings(argv)

	switch s.ArgvMode {
	case "off":
		if s.StoreArgCount {
			argc = len(argv)
		}
		return "", hash, argc
	case "hash_only":
		if s.StoreArgCount {
			argc = len(argv)
		}
		return "", hash, argc
	default: // "preview" (and any unknown mode falls back to the safe-ish preview)
		preview = s.cap(s.redact(strings.Join(argv, " ")))
		return preview, hash, len(argv)
	}
}

// ScrubPath redacts and caps a path (cwd, exe path). Hashing of paths is
// the caller's choice via HashString; ScrubPath keeps the (scrubbed)
// path so the dashboard can show project-relative locations (§8 Working
// directory). External-path redaction policy is applied by the caller.
func (s *FieldScrubber) ScrubPath(p string) string {
	if p == "" {
		return ""
	}
	return s.cap(s.redact(p))
}

// EnvPosture builds the allowlisted, posture-only environment map from a
// raw env (KEY=VALUE pairs as a map). It NEVER stores a full value that
// isn't either (a) a boolean/presence flag we compute, or (b) an
// explicitly allowlisted key whose value is still scrubbed + capped
// (spec §8.1, §12.1). PATH is reduced to a hash when StorePathHash.
//
// env is the process environment as a key->value map; pass nil/empty to
// get a nil result. Returns nil when EnvEnabled is false.
func (s *FieldScrubber) EnvPosture(env map[string]string) map[string]string {
	if !s.EnvEnabled || len(env) == 0 {
		return nil
	}
	out := make(map[string]string)

	// Proxy-routing posture: presence + whether it points at the Observer
	// proxy. We record presence as a boolean; the "points at proxy" refine
	// is the caller's (it knows the proxy address) — here we record presence.
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"} {
		if _, ok := env[k]; ok {
			out[k+"_present"] = "true"
		}
	}

	// PATH: hash only (never the value).
	if v, ok := env["PATH"]; ok && s.StorePathHash && v != "" {
		out["PATH_hash"] = HashString(v)
	}

	// Runtime hints: presence + scrubbed basename only.
	for _, k := range []string{"VIRTUAL_ENV", "CONDA_PREFIX", "NODE_OPTIONS"} {
		if v, ok := env[k]; ok {
			out[k+"_present"] = "true"
			if base := filepath.Base(v); base != "" && base != "." && base != string(filepath.Separator) {
				out[k+"_basename"] = s.cap(s.redact(base))
			}
		}
	}

	// CI / container flags: presence only.
	for _, k := range []string{"CI", "CONTAINER", "KUBERNETES_SERVICE_HOST"} {
		if _, ok := env[k]; ok {
			out[k+"_present"] = "true"
		}
	}

	// Operator allowlist: scrubbed + capped value, but never a key that
	// matches a secret pattern (presence only for those).
	for _, k := range s.EnvAllowlist {
		v, ok := env[k]
		if !ok {
			continue
		}
		if looksSecret(k) {
			out[k+"_present"] = "true"
			continue
		}
		out[k] = s.cap(s.redact(v))
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// cap truncates s to MaxPreviewBytes (rune-safe), appending an ellipsis
// marker so a truncated value is visibly distinct. Zero/negative cap = no
// truncation.
func (s *FieldScrubber) cap(in string) string {
	if s.MaxPreviewBytes <= 0 || len(in) <= s.MaxPreviewBytes {
		return in
	}
	// Trim to a rune boundary at or below the byte cap.
	cut := s.MaxPreviewBytes
	for cut > 0 && !utf8RuneStart(in[cut]) {
		cut--
	}
	return in[:cut] + "…"
}

// looksSecret reports whether an env KEY name signals a credential, so its
// value is never stored (presence only). Conservative substring match on
// the lowercased key (spec §8.1 "any key matching secret patterns").
func looksSecret(key string) bool {
	k := strings.ToLower(key)
	for _, marker := range []string{"secret", "token", "password", "passwd", "apikey", "api_key", "access_key", "private_key", "credential", "auth"} {
		if strings.Contains(k, marker) {
			return true
		}
	}
	return false
}

// HashString returns the hex sha256 of s, prefixed "sha256:". Used for
// PATH and any field captured as a hash. Empty input returns "".
func HashString(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

// hashStrings hashes a slice joined by NUL and returns the "sha256:"-
// prefixed hex digest. NUL is a safe separator: argv elements are
// NUL-terminated C strings and cannot themselves contain one.
func hashStrings(ss []string) string {
	h := sha256.Sum256([]byte(strings.Join(ss, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}

// utf8RuneStart reports whether b is a leading byte of a UTF-8 rune (i.e.
// not a 10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
