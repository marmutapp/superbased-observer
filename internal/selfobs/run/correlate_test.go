package run

import (
	"regexp"
	"strings"
	"testing"
)

// hex32 is the shape a derived correlation id must render as: 32 lowercase hex
// chars (an OTel-trace-id-shaped 128-bit value).
var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestCorrelationIDShapeAndStability pins the derived-id contract: empty in →
// empty out, everything else → a stable 32-hex digest that is byte-identical
// across calls and differs for different subjects.
func TestCorrelationIDShapeAndStability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantEmpty bool
	}{
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"posix project root", "/home/alice/work/acme-secret-project", false},
		{"windows project root", `C:\Users\alice\src\acme`, false},
		{"home relative", "~/src/acme", false},
		{"bare name", "acme", false},
		{"unicode path", "/home/älice/prøject", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CorrelationID(tc.in)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("CorrelationID(%q) = %q, want empty", tc.in, got)
				}
				return
			}
			if !hex32.MatchString(got) {
				t.Fatalf("CorrelationID(%q) = %q, want 32 lowercase hex chars", tc.in, got)
			}
			if again := CorrelationID(tc.in); again != got {
				t.Fatalf("CorrelationID(%q) not stable: %q then %q", tc.in, got, again)
			}
			if strings.Contains(got, tc.in) {
				t.Fatalf("CorrelationID(%q) = %q leaks its input", tc.in, got)
			}
		})
	}

	if CorrelationID("/home/alice/a") == CorrelationID("/home/alice/b") {
		t.Error("distinct subjects collided")
	}
}

func TestPathShaped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"posix absolute", "/home/alice/work", true},
		{"posix relative", "./work", true},
		{"windows", `C:\Users\alice`, true},
		{"unc", `\\server\share`, true},
		{"home relative", "~/src", true},
		{"uuid", "3f2a1c8e-0b7d-4a11-9d2f-6b0c1e5a7d34", false},
		{"advisor run id", "advisor-30", false},
		{"bare name", "myproject", false},
		{"hex correlation id", "9c1a0f3b2d4e5f60718293a4b5c6d7e8", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PathShaped(tc.in); got != tc.want {
				t.Errorf("PathShaped(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeCorrelationIDPassesThroughOpaqueIDs proves the backstop is
// surgical: it rewrites path-shaped values and leaves ordinary ids alone (so
// existing correlation across sessions/requests is not silently destroyed).
func TestSanitizeCorrelationIDPassesThroughOpaqueIDs(t *testing.T) {
	t.Parallel()

	for _, keep := range []string{"", "advisor-30", "3f2a1c8e-0b7d-4a11", "routing-conformer-trace"} {
		if got := SanitizeCorrelationID(keep); got != keep {
			t.Errorf("SanitizeCorrelationID(%q) = %q, want unchanged", keep, got)
		}
	}
	for _, path := range []string{"/home/alice/work", `C:\Users\alice`, "~/src/acme"} {
		got := SanitizeCorrelationID(path)
		if got == path {
			t.Errorf("SanitizeCorrelationID(%q) passed a path through verbatim", path)
		}
		if !hex32.MatchString(got) {
			t.Errorf("SanitizeCorrelationID(%q) = %q, want a 32-hex digest", path, got)
		}
	}
}
