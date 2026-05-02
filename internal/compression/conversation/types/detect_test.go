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
