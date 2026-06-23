package failure

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Error categories (spec §6.2 failure_context.error_category).
const (
	CategoryTimeout     = "timeout"
	CategoryCompilation = "compilation"
	CategoryTestFailure = "test_failure"
	CategoryPermission  = "permission"
	CategoryRuntime     = "runtime"
	CategoryUnknown     = "unknown"
)

// MaxErrorMessageLen is the cap applied to error messages stored in
// failure_context.error_message (spec §6.2).
const MaxErrorMessageLen = 500

// MaxCommandSummaryLen caps the command string retained for display.
const MaxCommandSummaryLen = 200

// CommandHash produces a stable SHA256 hex digest of a normalized command
// so that "ls -la" and "ls  -la" collide. Normalization: trim surrounding
// whitespace and collapse internal whitespace runs to a single space.
func CommandHash(command string) string {
	normalized := normalize(command)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// CommandSummary returns the command clipped to MaxCommandSummaryLen.
func CommandSummary(command string) string {
	s := strings.TrimSpace(command)
	if len(s) <= MaxCommandSummaryLen {
		return s
	}
	return s[:MaxCommandSummaryLen]
}

// TruncateErrorMessage clips message to MaxErrorMessageLen.
func TruncateErrorMessage(message string) string {
	if len(message) <= MaxErrorMessageLen {
		return message
	}
	return message[:MaxErrorMessageLen]
}

var (
	timeoutRE     = regexp.MustCompile(`(?i)\b(deadline exceeded|timed? ?out|operation timed out|context deadline)\b`)
	permissionRE  = regexp.MustCompile(`(?i)\b(permission denied|eacces|not permitted|forbidden|access denied|401 unauthorized|403 forbidden)\b`)
	compilationRE = regexp.MustCompile(`(?i)(\bcompilation (error|failed)\b|\bcannot (find|resolve) (symbol|type|import|module)\b|\bundefined\b\s*[:\(]|\bundefined (reference|symbol)\b|\bsyntax error\b|\bparse error\b|could not compile|\bbuild failed\b|package .* is not in (std|goroot)|error\[E\d+\])`)
	testFailureRE = regexp.MustCompile(`(?i)(--- FAIL|\bFAIL\b|AssertionError|assert\w* (failed|false)|expected .* (got|received)|test failed|\d+ tests? failed|\d+ failing|tests? run: \d+.*failures: [1-9])`)
	runtimeRE     = regexp.MustCompile(`(?i)\b(null ?pointer|nil pointer|segmentation fault|segfault|stack overflow|panic:|traceback|runtime error|out of memory|oomkilled|killed|connection refused|broken pipe|no such file|enoent|command not found)\b`)
)

// Categorize classifies an error message into one of the Category* values.
// Empty input → CategoryUnknown.
func Categorize(message string) string {
	if strings.TrimSpace(message) == "" {
		return CategoryUnknown
	}
	// Order matters — more specific matches first.
	switch {
	case timeoutRE.MatchString(message):
		return CategoryTimeout
	case permissionRE.MatchString(message):
		return CategoryPermission
	case compilationRE.MatchString(message):
		return CategoryCompilation
	case testFailureRE.MatchString(message):
		return CategoryTestFailure
	case runtimeRE.MatchString(message):
		return CategoryRuntime
	}
	return CategoryUnknown
}

func normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse whitespace runs.
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}
