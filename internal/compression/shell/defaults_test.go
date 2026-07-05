package shell

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestDefaultsGolden exercises every embedded filter with a canonical input
// sample. Failures mean either a filter regressed or the sample is newer than
// the filter — update the TOML, not the test fixture.
func TestDefaultsGolden(t *testing.T) {
	e, err := NewEngineWithDefaults()
	if err != nil {
		t.Fatalf("NewEngineWithDefaults: %v", err)
	}

	cases := []struct {
		name string
		argv []string
		in   string
		want string
	}{
		{
			name: "git status strips hints",
			argv: []string{"git", "status"},
			in: joinLines(
				"On branch main",
				`Your branch is ahead of 'origin/main' by 1 commit.`,
				`  (use "git push" to publish your local commits)`,
				"",
				"Changes not staged for commit:",
				`  (use "git add <file>..." to update what will be committed)`,
				`  (use "git restore <file>..." to discard changes in working directory)`,
				"\tmodified:   foo.go",
				"",
				"",
				`no changes added to commit (use "git add" and/or "git commit -a")`,
				"hint: You have divergent branches and need to specify how to reconcile them.",
				"hint: You can do so by running one of the following commands sometime before",
				"hint: your next pull:",
			),
			want: joinLines(
				"On branch main",
				`Your branch is ahead of 'origin/main' by 1 commit.`,
				"",
				"Changes not staged for commit:",
				"\tmodified:   foo.go",
				"",
				`no changes added to commit (use "git add" and/or "git commit -a")`,
			),
		},
		{
			name: "go test drops verbose scaffolding",
			argv: []string{"go", "test", "-v", "./..."},
			in: joinLines(
				"=== RUN   TestFoo",
				"    foo_test.go:10: some log",
				"=== RUN   TestFoo/case_1",
				"=== PAUSE TestFoo/case_1",
				"=== CONT  TestFoo/case_1",
				"--- PASS: TestFoo (0.00s)",
				"    --- PASS: TestFoo/case_1 (0.00s)",
				"=== RUN   TestBar",
				"--- FAIL: TestBar (0.01s)",
				"    bar_test.go:20: expected 1, got 2",
				"FAIL",
				"FAIL    example.com/pkg 0.012s",
				"FAIL",
			),
			want: joinLines(
				"    foo_test.go:10: some log",
				"--- FAIL: TestBar (0.01s)",
				"    bar_test.go:20: expected 1, got 2",
				"FAIL",
				"FAIL    example.com/pkg 0.012s",
				"FAIL",
			),
		},
		{
			name: "docker build drops BuildKit noise",
			argv: []string{"docker", "build", "."},
			in: joinLines(
				"#1 [internal] load build definition from Dockerfile",
				"#1 transferring dockerfile: 110B done",
				"#1 DONE 0.0s",
				"#2 [internal] load metadata for docker.io/library/alpine:3.19",
				"#2 DONE 0.1s",
				"#3 [1/2] FROM docker.io/library/alpine:3.19",
				"#3 CACHED",
				"#4 [2/2] RUN apk add --no-cache curl",
				"#4 ... running apk ...",
				"#4 DONE 2.3s",
				"#5 exporting to image",
				"#5 sha256:1234abcd5678...",
				"#5 DONE 0.1s",
				"Successfully built abc123",
			),
			want: joinLines(
				"#1 [internal] load build definition from Dockerfile",
				"#1 transferring dockerfile: 110B done",
				"#2 [internal] load metadata for docker.io/library/alpine:3.19",
				"#3 [1/2] FROM docker.io/library/alpine:3.19",
				"#4 [2/2] RUN apk add --no-cache curl",
				"#4 ... running apk ...",
				"#5 exporting to image",
				"Successfully built abc123",
			),
		},
		{
			name: "docker ps uses generic filter",
			argv: []string{"docker", "ps"},
			in: joinLines(
				"CONTAINER ID   IMAGE   COMMAND   STATUS",
				"",
				"",
				"abc123   alpine   /bin/sh   Up 2 minutes",
			),
			want: joinLines(
				"CONTAINER ID   IMAGE   COMMAND   STATUS",
				"",
				"abc123   alpine   /bin/sh   Up 2 minutes",
			),
		},
		{
			name: "kubectl squashes blank runs",
			argv: []string{"kubectl", "get", "pods"},
			in: joinLines(
				"NAME        READY   STATUS",
				"",
				"",
				"api-7f      1/1     Running",
			),
			want: joinLines(
				"NAME        READY   STATUS",
				"",
				"api-7f      1/1     Running",
			),
		},
		{
			name: "cargo drops Fresh progress",
			argv: []string{"cargo", "build"},
			in: joinLines(
				"    Fresh proc-macro2 v1.0.85",
				"    Fresh unicode-ident v1.0.12",
				"   Compiling my-crate v0.1.0 (/src/my-crate)",
				"    Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.42s",
			),
			want: joinLines(
				"   Compiling my-crate v0.1.0 (/src/my-crate)",
				"    Finished `dev` profile [unoptimized + debuginfo] target(s) in 0.42s",
			),
		},
		{
			name: "pytest drops PASSED markers",
			argv: []string{"pytest", "-v"},
			in: joinLines(
				"tests/test_foo.py::test_alpha PASSED [ 25%]",
				"tests/test_foo.py::test_beta FAILED [ 50%]",
				"tests/test_foo.py::test_gamma PASSED",
				"tests/test_foo.py::test_delta PASSED [100%]",
			),
			want: joinLines(
				"tests/test_foo.py::test_beta FAILED [ 50%]",
			),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			s, err := e.Filter(tc.argv, &out)
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			if _, err := io.WriteString(s, tc.in); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("output mismatch\n got: %q\nwant: %q", got, tc.want)
			}
			if s.Spec() == nil {
				t.Fatalf("expected matched spec for argv %v", tc.argv)
			}
			// Each canonical input should compress to something smaller —
			// surfaces the value of the filter in one number.
			st := s.Stats()
			if st.BytesOut >= st.BytesIn {
				t.Fatalf("%s: no compression (in=%d out=%d)", tc.name, st.BytesIn, st.BytesOut)
			}
		})
	}
}

func TestDefaultsRegistersExpectedCommands(t *testing.T) {
	e, err := NewEngineWithDefaults()
	if err != nil {
		t.Fatalf("NewEngineWithDefaults: %v", err)
	}
	commands := map[string]bool{}
	for _, s := range e.Specs() {
		commands[s.Command] = true
	}
	for _, want := range []string{"git", "go", "docker", "kubectl", "cargo", "pytest"} {
		if !commands[want] {
			t.Errorf("default filter for %q not registered", want)
		}
	}
}

func joinLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}
