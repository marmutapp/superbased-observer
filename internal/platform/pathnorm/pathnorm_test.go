package pathnorm

import (
	"runtime"
	"strings"
	"testing"
)

// TestNormalize_FormatMatrix pins the canonical-output + format-
// detection contract across every recognised path shape. The table
// is intentionally exhaustive — a regression in any single row
// would silently re-introduce the pre-pathnorm fragility for that
// shape.
func TestNormalize_FormatMatrix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pathnorm pipeline is non-Windows-only for now; Windows host preserves native paths")
	}

	tests := []struct {
		name       string
		in         string
		want       string
		wantFormat Format
	}{
		// --- empty / whitespace ---------------------------------
		{"empty", "", "", FormatEmpty},
		{"single space", " ", "", FormatEmpty},
		{"only whitespace", "   \t  ", "", FormatEmpty},
		{"trim leading", "  /home/user/foo", "/home/user/foo", FormatPOSIXAbsolute},
		{"trim trailing", "/home/user/foo  ", "/home/user/foo", FormatPOSIXAbsolute},
		{"trim both", "  /home/user/foo  ", "/home/user/foo", FormatPOSIXAbsolute},

		// --- POSIX absolute -------------------------------------
		{"posix absolute", "/home/user/foo", "/home/user/foo", FormatPOSIXAbsolute},
		{"posix root", "/", "/", FormatPOSIXAbsolute},

		// --- relative -------------------------------------------
		{"relative dot", "./foo", "./foo", FormatRelative},
		{"relative dotdot", "../bar", "../bar", FormatRelative},
		{"bare filename", "foo.txt", "foo.txt", FormatRelative},

		// --- WSL mnt (already canonical) ------------------------
		{"wsl mnt c", "/mnt/c/Users/foo", "/mnt/c/Users/foo", FormatWSLMnt},
		{"wsl mnt d", "/mnt/d/programsx/test", "/mnt/d/programsx/test", FormatWSLMnt},

		// --- Windows drive-letter absolute ----------------------
		{"windows backslash", `C:\Users\foo`, "/mnt/c/Users/foo", FormatWindowsDrive},
		{"windows forward-slash", "C:/Users/foo", "/mnt/c/Users/foo", FormatWindowsDrive},
		{"windows lowercase drive", `c:\Users\foo`, "/mnt/c/Users/foo", FormatWindowsDrive},
		{"windows mixed separators", `C:\foo/bar`, "/mnt/c/foo/bar", FormatWindowsDrive},
		{"windows drive root", `C:\`, "/mnt/c/", FormatWindowsDrive},

		// --- Surrounding quotes ---------------------------------
		// Operator-reported regression class — codex CLI on Windows
		// emitted POSIX single-quoted commands pre-v1.6.28 that
		// cmd.exe interpreted literally. The normalizer must
		// recover from a quoted path arriving from any upstream
		// tool's debug log or hook stdin.
		{"single-quoted windows", `'C:\Users\foo'`, "/mnt/c/Users/foo", FormatQuoted},
		{"double-quoted windows", `"C:\Users\foo"`, "/mnt/c/Users/foo", FormatQuoted},
		{"single-quoted posix", `'/home/user/foo'`, "/home/user/foo", FormatQuoted},
		{"double-quoted posix", `"/home/user/foo"`, "/home/user/foo", FormatQuoted},
		{"nested quotes", `'"C:\foo"'`, "/mnt/c/foo", FormatQuoted},
		{"quoted with internal whitespace path", `'C:\Program Files\foo'`, "/mnt/c/Program Files/foo", FormatQuoted},

		// --- Quote edge cases (should NOT strip) ----------------
		{"mismatched quote types", `'C:\foo"`, `'C:\foo"`, FormatRelative},
		{"only-quotes single", `''`, `''`, FormatRelative},
		{"only-quotes double", `""`, `""`, FormatRelative},
		{"whitespace-inside quotes", `'   '`, `'   '`, FormatRelative},

		// --- file:// URIs ---------------------------------------
		{"file URI windows literal", "file:///D:/programsx/test", "/mnt/d/programsx/test", FormatFileURI},
		{"file URI windows percent-encoded colon", "file:///d%3A/programsx/test", "/mnt/d/programsx/test", FormatFileURI},
		{"file URI posix", "file:///home/user/foo", "/home/user/foo", FormatFileURI},
		{"file URI uppercase scheme", "FILE:///D:/foo", "/mnt/d/foo", FormatFileURI},
		{"file URI with percent-encoded space", "file:///D:/Program%20Files/foo", "/mnt/d/Program Files/foo", FormatFileURI},

		// --- Extended-length prefix -----------------------------
		{"extended-length", `\\?\C:\Users\foo`, "/mnt/c/Users/foo", FormatExtendedLength},
		{"extended-length forward-slash inner", `\\?\C:/Users/foo`, "/mnt/c/Users/foo", FormatExtendedLength},

		// --- Git Bash drive prefix ------------------------------
		{"git bash /c/", "/c/Users/foo", "/mnt/c/Users/foo", FormatGitBashDrive},
		{"git bash /d/", "/d/programsx", "/mnt/d/programsx", FormatGitBashDrive},
		{"git bash uppercase /C/", "/C/Users/foo", "/mnt/c/Users/foo", FormatGitBashDrive},

		// --- Git Bash false positives (should NOT rewrite) ------
		// Bare /c is too short to be confident; falls through to
		// classify as POSIX absolute. Conservative.
		{"bare /c", "/c", "/c", FormatPOSIXAbsolute},
		{"/c-prefix-not-drive", "/configurations/file", "/configurations/file", FormatPOSIXAbsolute},

		// --- UNC generic (no rewrite) ---------------------------
		// Server-share UNC paths pass through as FormatUNCGeneric —
		// observer can't safely reach them from the running
		// process without a mount point.
		{"unc server share", `\\server\share\file`, `\\server\share\file`, FormatUNCGeneric},

		// --- Combined: quotes + file URI ------------------------
		{"quoted file URI", `'file:///D:/foo'`, "/mnt/d/foo", FormatQuoted},

		// --- Combined: quotes + windows drive -------------------
		{"quoted with mixed-case drive", `'c:/Users/Foo'`, "/mnt/c/Users/Foo", FormatQuoted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotFormat := NormalizeWithFormat(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if gotFormat != tc.wantFormat {
				t.Errorf("Normalize(%q) format = %v (%s), want %v (%s)",
					tc.in, gotFormat, gotFormat, tc.wantFormat, tc.wantFormat)
			}
		})
	}
}

// TestNormalize_TildeExpansion verifies the `~/foo` and bare `~`
// shapes expand to $HOME. Uses t.Setenv so the test is hermetic and
// portable across hosts.
func TestNormalize_TildeExpansion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tilde expansion test exercises HOME, which is POSIX-shaped")
	}
	t.Setenv("HOME", "/tmp/fake-home")

	tests := []struct {
		name       string
		in         string
		want       string
		wantFormat Format
	}{
		{"bare tilde", "~", "/tmp/fake-home", FormatTilde},
		{"tilde slash", "~/foo", "/tmp/fake-home/foo", FormatTilde},
		{"tilde nested", "~/code/observer", "/tmp/fake-home/code/observer", FormatTilde},

		// NOT expanded — only ~/ and bare ~ are in scope. ~user/
		// requires a user-db lookup that's out of scope.
		{"tilde user", "~someoneelse/foo", "~someoneelse/foo", FormatRelative},

		// NOT expanded — env vars are deliberately ignored
		// (security risk: env may carry secrets).
		{"env var dollar", "$HOME/foo", "$HOME/foo", FormatRelative},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotFormat := NormalizeWithFormat(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if gotFormat != tc.wantFormat {
				t.Errorf("Normalize(%q) format = %v (%s), want %v (%s)",
					tc.in, gotFormat, gotFormat, tc.wantFormat, tc.wantFormat)
			}
		})
	}
}

// TestNormalize_UNCtoWSL pins the `\\wsl.localhost\<distro>\...`
// rewrite. Two variants tested: matching distro (rewrite fires) and
// non-matching distro (passes through as FormatUNCGeneric since
// we can't safely assume the foreign distro is reachable).
func TestNormalize_UNCtoWSL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("UNC-to-WSL rewrite is Linux-only")
	}

	t.Run("matching distro - wsl.localhost", func(t *testing.T) {
		t.Setenv("WSL_DISTRO_NAME", "Ubuntu-20.04")
		got, fmt := NormalizeWithFormat(`\\wsl.localhost\Ubuntu-20.04\home\user\foo`)
		if got != "/home/user/foo" {
			t.Errorf("UNC rewrite = %q, want /home/user/foo", got)
		}
		if fmt != FormatUNCWSL {
			t.Errorf("format = %v, want FormatUNCWSL", fmt)
		}
	})

	t.Run("matching distro - wsl$", func(t *testing.T) {
		t.Setenv("WSL_DISTRO_NAME", "Debian")
		got, fmt := NormalizeWithFormat(`\\wsl$\Debian\home\user\foo`)
		if got != "/home/user/foo" {
			t.Errorf("UNC rewrite = %q, want /home/user/foo", got)
		}
		if fmt != FormatUNCWSL {
			t.Errorf("format = %v, want FormatUNCWSL", fmt)
		}
	})

	t.Run("non-matching distro passes through", func(t *testing.T) {
		t.Setenv("WSL_DISTRO_NAME", "Ubuntu-20.04")
		// Path is for a different distro; observer can't reach it
		// from the running WSL → classified as generic UNC and
		// passed through unchanged.
		in := `\\wsl.localhost\Debian\home\user\foo`
		got, fmt := NormalizeWithFormat(in)
		if got != in {
			t.Errorf("non-matching UNC should pass through; got %q want %q", got, in)
		}
		if fmt != FormatUNCGeneric {
			t.Errorf("format = %v, want FormatUNCGeneric", fmt)
		}
	})

	t.Run("no WSL_DISTRO_NAME passes through", func(t *testing.T) {
		t.Setenv("WSL_DISTRO_NAME", "")
		// Observer not running inside WSL → can't safely rewrite.
		in := `\\wsl.localhost\Ubuntu-20.04\home\user\foo`
		got, fmt := NormalizeWithFormat(in)
		if got != in {
			t.Errorf("no-distro UNC should pass through; got %q want %q", got, in)
		}
		if fmt != FormatUNCGeneric {
			t.Errorf("format = %v, want FormatUNCGeneric", fmt)
		}
	})
}

// TestNormalize_NeverPanic exercises the contract that the
// normalizer NEVER errors / NEVER panics, even on weird inputs.
// Adapter callers can trust the result without defensive try/catch.
func TestNormalize_NeverPanic(t *testing.T) {
	weirds := []string{
		"",
		"\x00",
		"\x00\x00",
		"\n\n\n",
		"\r\n\r\n",
		strings.Repeat("/foo", 100),
		"file://",
		"file:///",
		`'`,
		`"`,
		`''''''`,
		`""""""`,
		`\\?\`,
		`\\?\\\?\C:\foo`,
		`\\wsl.localhost\`,
		`\\wsl$\`,
		"/c/",
		"~",
		"~",
		"~~",
		"~~~",
	}
	for _, in := range weirds {
		// Each input must complete without panic and return SOME
		// string + SOME format. The exact value is implementation-
		// defined for these pathological cases; only the never-
		// panic contract matters.
		_, _ = NormalizeWithFormat(in)
		_ = Normalize(in)
	}
}

// TestNormalize_PreservesNonWindowsInputs is a regression guard
// against accidentally aggressive rewrites. A path that's already
// in a canonical form (or a Linux-native path with no foreign
// markers) MUST round-trip unchanged.
func TestNormalize_PreservesNonWindowsInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows preservation contract")
	}
	preserved := []string{
		"/",
		"/home/user",
		"/home/user/code/observer",
		"/usr/local/bin/observer",
		"/mnt/c/Users/foo",
		"/tmp/foo",
		"./relative",
		"../relative",
		"foo.txt",
		"foo/bar/baz",
	}
	for _, in := range preserved {
		got := Normalize(in)
		if got != in {
			t.Errorf("Normalize(%q) = %q; expected unchanged", in, got)
		}
	}
}

// TestFormat_String ensures every Format constant has a stable
// label and no two formats share one. Used by adapter telemetry —
// label drift would silently break log-grep monitoring.
func TestFormat_String(t *testing.T) {
	all := []Format{
		FormatUnknown,
		FormatEmpty,
		FormatPOSIXAbsolute,
		FormatWSLMnt,
		FormatWindowsDrive,
		FormatGitBashDrive,
		FormatFileURI,
		FormatUNCWSL,
		FormatUNCGeneric,
		FormatExtendedLength,
		FormatTilde,
		FormatRelative,
		FormatQuoted,
	}
	seen := map[string]Format{}
	for _, f := range all {
		s := f.String()
		if s == "" {
			t.Errorf("Format(%d).String() returned empty", int(f))
		}
		if prev, dup := seen[s]; dup {
			t.Errorf("Format string %q used by both %v and %v", s, prev, f)
		}
		seen[s] = f
	}
}
