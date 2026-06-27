package types

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		filename string
		want     ContentType
	}{
		{
			name: "json object",
			body: `{"name":"foo","count":3}`,
			want: JSON,
		},
		{
			name: "json array with whitespace",
			body: "  \n [1, 2, 3]  ",
			want: JSON,
		},
		{
			name: "jsonl trumps unknown sniff",
			body: `{"a":1}` + "\n" + `{"b":2}`,
			// Two standalone objects is not strict JSON, but the first line
			// sniffs as json; since json.Valid fails we expect Text. Filename
			// hint promotes it to JSON.
			filename: "events.jsonl",
			want:     JSON,
		},
		{
			name: "plain text with braces falls through",
			body: `{this is not json — it's prose with {curly} decorations and no parse path.`,
			want: Text,
		},
		{
			name:     "code by filename",
			body:     "package main\n\nfunc main() {}\n",
			filename: "main.go",
			want:     Code,
		},
		{
			name:     "code filename overrides sniff",
			body:     "not parseable as anything special",
			filename: "handler.py",
			want:     Code,
		},
		{
			name:     "json sniff overrides code filename",
			body:     `{"error":"E_NOTFOUND"}`,
			filename: "main.go",
			want:     JSON,
		},
		{
			name: "logs by pattern — bracketed timestamp",
			body: "[2025-03-12 11:22:33] starting up\n[2025-03-12 11:22:34] listening on :8080\n[2025-03-12 11:22:35] ready\n",
			want: Logs,
		},
		{
			name: "logs by pattern — ISO timestamp",
			body: "2025-03-12T11:22:33Z INFO started\n2025-03-12T11:22:34Z WARN retry\n2025-03-12T11:22:35Z ERROR gave up\n",
			want: Logs,
		},
		{
			name: "logs by leveled tag",
			body: "[INFO] hello\n[WARN] careful\n[ERROR] oh no\n[INFO] ok\n",
			want: Logs,
		},
		{
			name:     "logs by extension",
			body:     "line one\nline two\n",
			filename: "server.log",
			want:     Logs,
		},
		{
			name: "diff by @@ hunk + --- header",
			body: "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n-old\n+new\n context\n",
			want: Diff,
		},
		{
			name: "diff by git header",
			body: "diff --git a/x b/x\nindex 111..222 100644\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-foo\n+bar\n",
			want: Diff,
		},
		{
			name: "markdown rule is not diff",
			body: "# Title\n\n---\n\nparagraph\n",
			want: Text,
		},
		{
			name: "html with doctype",
			body: "<!DOCTYPE html>\n<html><body>hi</body></html>",
			want: HTML,
		},
		{
			name: "html without doctype",
			body: "<html>\n<body>\n<p>ok</p>\n</body>\n</html>\n",
			want: HTML,
		},
		{
			name:     "html by extension",
			body:     "<p>just a fragment</p>",
			filename: "snippet.html",
			want:     HTML,
		},
		{
			name:     "markdown filename is text",
			body:     "# readme",
			filename: "README.md",
			want:     Text,
		},
		{
			name: "empty body is unknown",
			body: "",
			want: Unknown,
		},
		{
			name: "whitespace-only body is unknown",
			body: "   \n\n\t  \n",
			want: Unknown,
		},
		{
			name: "plain prose is text",
			body: "Here are my findings. The main issue is that the cache layer does not invalidate properly on writes.",
			want: Text,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect([]byte(tc.body), tc.filename)
			if got != tc.want {
				t.Fatalf("Detect() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetect_LineNumberedReadOutput pins that Claude Code's Read-tool
// `<num>\t<content>\n` envelope is transparently unwrapped during
// detection. Pre-fix this returned Text or Logs (the line-numbered
// digits looked like log timestamps); post-fix the underlying file
// content drives classification.
func TestDetect_LineNumberedReadOutput(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		filename string
		want     ContentType
	}{
		{
			name:     "line-numbered JSON file",
			body:     "1\t{\n2\t  \"name\": \"express\",\n3\t  \"version\": \"5.2.1\"\n4\t}",
			filename: "package.json",
			want:     JSON,
		},
		{
			name:     "line-numbered Go file (filename hint)",
			body:     "1\tpackage foo\n2\t\n3\tfunc Bar() {}\n",
			filename: "/repo/foo.go",
			want:     Code,
		},
		{
			name:     "line-numbered JSON without filename hint",
			body:     "1\t{\n2\t  \"a\": 1\n3\t}",
			filename: "",
			want:     JSON,
		},
		{
			name:     "non-line-numbered short body unchanged",
			body:     "hello world",
			filename: "",
			want:     Text,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect([]byte(tc.body), tc.filename); got != tc.want {
				t.Errorf("Detect(%q, %q) = %v, want %v", tc.body, tc.filename, got, tc.want)
			}
		})
	}
}

// TestDetect_BashOutputShapes pins that common Bash tool output
// patterns (ls -la, grep, find, wc -l) classify as Logs so they go
// through LogsCompressor's dedupe + head/tail strategy.
func TestDetect_BashOutputShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ContentType
	}{
		{
			name: "ls -la output",
			body: "total 192\ndrwxr-xr-x  6 user group  4096 May  7 11:00 .\ndrwxrwxrwt 33 root root  20480 May  7 12:00 ..\n-rw-r--r--  1 user group   1024 May  7 09:00 file.txt\n-rw-r--r--  1 user group    512 May  7 08:00 other.txt",
			want: Logs,
		},
		{
			name: "grep -n output",
			body: "lib/foo.go:12:func Bar() {\nlib/foo.go:34:  return Baz()\nlib/baz.go:8:func Baz() {\nlib/baz.go:9:  return 42\n}",
			want: Logs,
		},
		{
			name: "find paths",
			body: "./lib/foo.go\n./lib/bar.go\n./lib/baz.go\n./test/foo_test.go",
			want: Logs,
		},
		{
			name: "wc -l output",
			body: "  631 lib/application.js\n   81 lib/express.js\n  527 lib/request.js\n  104 lib/response.js",
			want: Logs,
		},
		{
			name: "single-line echo not classified",
			body: "hello world",
			want: Text,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect([]byte(tc.body), ""); got != tc.want {
				t.Errorf("Detect(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestDetect_DiffShapes pins diff classification across the shapes
// Claude Code actually emits: bare unified diff from `git diff` via
// Bash, line-numbered diff from Read of a .diff/.patch file, and
// `diff --git` headed diffs.
func TestDetect_DiffShapes(t *testing.T) {
	bareDiff := "--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n func Foo() {\n-  return 1\n+  return 2\n }"
	gitDiff := "diff --git a/foo.go b/foo.go\nindex abc..def 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n-old\n+new"
	lineNumberedPatch := "1\t--- a/foo.go\n2\t+++ b/foo.go\n3\t@@ -1,3 +1,3 @@\n4\t func Foo() {\n5\t-  return 1\n6\t+  return 2\n7\t }"

	cases := []struct {
		name     string
		body     string
		filename string
		want     ContentType
	}{
		{name: "bare unified diff (git diff via Bash)", body: bareDiff, want: Diff},
		{name: "diff --git header", body: gitDiff, want: Diff},
		{name: "line-numbered diff via Read", body: lineNumberedPatch, want: Diff},
		{name: "line-numbered .patch via Read with filename hint", body: lineNumberedPatch, filename: "fix.patch", want: Diff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect([]byte(tc.body), tc.filename); got != tc.want {
				t.Errorf("Detect(%q, %q) = %v, want %v", tc.body, tc.filename, got, tc.want)
			}
		})
	}
}

func TestStripLineNumbers(t *testing.T) {
	in := "1\t{\n2\t  \"a\": 1\n3\t}\n"
	want := "{\n  \"a\": 1\n}\n"
	if got := string(StripLineNumbers([]byte(in))); got != want {
		t.Errorf("StripLineNumbers:\n  got: %q\n  want: %q", got, want)
	}
}

func TestIsLineNumbered_TooShort(t *testing.T) {
	// Two-line bodies are below the threshold (3 lines minimum) — guard
	// against mis-detecting "1\tfoo\n2\tbar" as line-numbered when it
	// might just be a tab-indented snippet.
	if IsLineNumbered([]byte("1\tfoo\n2\tbar")) {
		t.Errorf("two-line body should not classify as line-numbered")
	}
}

// TestDetect_CodexShellEnvelope_StripsAndReclassifies (V7-3) covers
// the load-bearing benefit: an envelope wrapping JSON / Code / Markdown
// classifies as that inner type, NOT as Logs.
func TestDetect_CodexShellEnvelope_StripsAndReclassifies(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ContentType
	}{
		{
			name: "json-inner",
			body: "Exit code: 0\nWall time: 0.123 seconds\nOutput:\n{\"key\":\"value\",\"count\":3}\n",
			want: JSON,
		},
		{
			name: "diff-inner",
			body: "Exit code: 0\nWall time: 0.456s\nOutput:\ndiff --git a/foo.go b/foo.go\nindex abc123..def456 100644\n--- a/foo.go\n+++ b/foo.go\n",
			want: Diff,
		},
		{
			name: "html-inner",
			body: "Exit code: 0\nWall time: 0.5\nOutput:\n<!DOCTYPE html>\n<html><head></head><body>hi</body></html>\n",
			want: HTML,
		},
		{
			name: "logs-inner-still-logs",
			body: "Exit code: 0\nWall time: 0.789 seconds\nOutput:\n[INFO] starting service\n[WARN] config missing\n[INFO] using defaults\n",
			want: Logs,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect([]byte(c.body), "")
			if got != c.want {
				t.Errorf("Detect: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestDetect_CodexShellEnvelope_NegativeExitCode: codex emits negative
// exit codes when the inner process is signal-terminated. Must still
// strip.
func TestDetect_CodexShellEnvelope_NegativeExitCode(t *testing.T) {
	body := "Exit code: -15\nWall time: 1.234 seconds\nOutput:\n{\"trace\":\"abc\"}\n"
	if got := Detect([]byte(body), ""); got != JSON {
		t.Errorf("got %q, want JSON (negative exit should still strip)", got)
	}
}

// TestDetect_CodexShellEnvelope_WallTimeVariants pins the three Wall
// time suffix forms observed in real codex traffic. All must strip.
func TestDetect_CodexShellEnvelope_WallTimeVariants(t *testing.T) {
	inner := "{\"ok\":true}\n"
	for _, prefix := range []string{
		"Exit code: 0\nWall time: 0.123s\nOutput:\n",
		"Exit code: 0\nWall time: 0.123 seconds\nOutput:\n",
		"Exit code: 0\nWall time: 0.123\nOutput:\n",
		"Exit code: 0\nWall time: 12\nOutput:\n",
	} {
		if got := Detect([]byte(prefix+inner), ""); got != JSON {
			t.Errorf("variant %q: got %q, want JSON", prefix, got)
		}
	}
}

// TestDetect_CodexShellEnvelope_NoEnvelopePassthrough: bodies that
// don't START with the exact envelope shape pass through. Verifies
// the \A anchor — non-codex bodies that merely contain the phrase
// should NOT be stripped.
func TestDetect_CodexShellEnvelope_NoEnvelopePassthrough(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ContentType
	}{
		{
			// Envelope-shaped text in the middle of the body must NOT
			// trigger stripping (\A anchor). What classification falls
			// out is whatever the unmodified Detect would have returned
			// — here it's Text because none of the structural sniffers
			// fire. The pin is: same as before my change.
			name: "trailing-not-leading",
			body: "Hello world\nExit code: 0\nWall time: 1.0\nOutput:\n{\"hi\":1}\n",
			want: Text,
		},
		{
			// Missing the literal "Output:\n" line means the regex
			// doesn't match; body passes through. Same as before.
			name: "missing-output-line",
			body: "Exit code: 0\nWall time: 0.1\n{\"a\":1}\n",
			want: Text,
		},
		{
			name: "raw-json-no-envelope",
			body: "{\"key\":\"value\"}\n",
			want: JSON,
		},
		{
			name: "raw-logs-no-envelope",
			body: "[INFO] hello\n[WARN] world\n",
			want: Logs,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect([]byte(c.body), "")
			if got != c.want {
				t.Errorf("Detect: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestDetect_CodexShellEnvelope_EmptyInner: envelope wrapping an
// empty stream classifies as Unknown (matches the existing
// empty-body short-circuit).
func TestDetect_CodexShellEnvelope_EmptyInner(t *testing.T) {
	body := "Exit code: 0\nWall time: 0.001 seconds\nOutput:\n"
	if got := Detect([]byte(body), ""); got != Unknown {
		t.Errorf("empty inner: got %q, want Unknown", got)
	}
}

// TestStripCodexShellEnvelope_Direct: probe the helper directly.
// Returns body unchanged + false when the envelope isn't present.
func TestStripCodexShellEnvelope_Direct(t *testing.T) {
	plain := []byte("no envelope here\n")
	out, stripped := stripCodexShellEnvelope(plain)
	if stripped {
		t.Errorf("plain body wrongly stripped: %q", out)
	}
	if string(out) != string(plain) {
		t.Errorf("plain body mutated: got %q want %q", out, plain)
	}

	wrapped := []byte("Exit code: 0\nWall time: 0.5\nOutput:\nINNER")
	out, stripped = stripCodexShellEnvelope(wrapped)
	if !stripped {
		t.Errorf("wrapped body not stripped")
	}
	if string(out) != "INNER" {
		t.Errorf("inner bytes: got %q want INNER", out)
	}
}

func TestAllReturnsEveryCompressibleType(t *testing.T) {
	got := All()
	wantSet := map[ContentType]bool{
		JSON: true, Code: true, Logs: true, Text: true, Diff: true, HTML: true,
	}
	if len(got) != len(wantSet) {
		t.Fatalf("All() length = %d, want %d", len(got), len(wantSet))
	}
	for _, ct := range got {
		if !wantSet[ct] {
			t.Errorf("unexpected type in All(): %q", ct)
		}
		delete(wantSet, ct)
	}
	if len(wantSet) > 0 {
		t.Errorf("All() missing types: %v", wantSet)
	}
}
