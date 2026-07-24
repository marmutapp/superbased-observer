package host

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeFakeShell writes an executable script at <dir>/<name> whose body is the
// given shell code, and returns its path. filepath.Base(path) == name so
// loginArgv routes it correctly.
func writeFakeShell(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	return p
}

func TestCaptureLoginPath_Success(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake shell not applicable on Windows")
	}
	// A fake "bash" that ignores its args and prints a fixed PATH.
	shell := writeFakeShell(t, "bash", `printf '%s' "/opt/a:/opt/b"`)

	got, err := CaptureLoginPath(shell, time.Second)
	if err != nil {
		t.Fatalf("CaptureLoginPath: %v", err)
	}
	want := []string{"/opt/a", "/opt/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCaptureLoginPath_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake shell not applicable on Windows")
	}
	shell := writeFakeShell(t, "bash", `sleep 5`)

	start := time.Now()
	_, err := CaptureLoginPath(shell, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("capture did not honor timeout: took %s", elapsed)
	}
}

func TestCaptureLoginPath_UnknownShell(t *testing.T) {
	_, err := CaptureLoginPath("/usr/bin/nu", time.Second)
	if !errors.Is(err, ErrUnsupportedShell) {
		t.Errorf("err = %v, want ErrUnsupportedShell", err)
	}
}

func TestCaptureLoginPath_EmptyShell(t *testing.T) {
	_, err := CaptureLoginPath("", time.Second)
	if !errors.Is(err, ErrUnsupportedShell) {
		t.Errorf("err = %v, want ErrUnsupportedShell", err)
	}
}

func TestCaptureLoginPath_Fish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake shell not applicable on Windows")
	}
	// The fake fish ignores args and prints a colon-joined PATH, matching what
	// `string join : $PATH` would emit.
	shell := writeFakeShell(t, "fish", `printf '%s' "/usr/local/bin:/home/u/.local/bin"`)
	got, err := CaptureLoginPath(shell, time.Second)
	if err != nil {
		t.Fatalf("CaptureLoginPath(fish): %v", err)
	}
	if len(got) != 2 || got[1] != "/home/u/.local/bin" {
		t.Errorf("got %v", got)
	}
}

func TestNewEnv_Shape(t *testing.T) {
	env := NewEnv(Options{})
	if env.GOOS != runtime.GOOS {
		t.Errorf("GOOS = %q, want %q", env.GOOS, runtime.GOOS)
	}
	if env.Stat == nil || env.EvalSymlinks == nil || env.Glob == nil {
		t.Error("NewEnv left a filesystem probe nil")
	}
	// LoginPath is nil only on a Windows daemon.
	if runtime.GOOS == "windows" {
		if env.LoginPath != nil {
			t.Error("LoginPath should be nil on a Windows daemon")
		}
	} else if env.LoginPath == nil {
		t.Error("LoginPath should be set on a POSIX daemon")
	}
	// PathExt is populated only on a Windows daemon; nil on POSIX.
	if runtime.GOOS != "windows" && env.PathExt != nil {
		t.Errorf("PathExt should be nil off Windows, got %v", env.PathExt)
	}
}
