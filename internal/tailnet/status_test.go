package tailnet

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withStubTailscale prepends a temp dir containing an executable `tailscale`
// shim to PATH, so Detect/ServeStatus exercise the real exec path against
// deterministic output. Skips on Windows (the shim is a POSIX shell script).
func withStubTailscale(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script tailscale shim is POSIX-only")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestServeCommand(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"colon-form yields bare port", ":8123", "tailscale serve --bg 8123"},
		{"trims + strips colon", "  :8123  ", "tailscale serve --bg 8123"},
		{"already bare", "8123", "tailscale serve --bg 8123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServeCommand(tt.port); got != tt.want {
				t.Errorf("ServeCommand(%q) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

func TestDetectAbsentBinary(t *testing.T) {
	// An empty PATH guarantees the binary is not found.
	t.Setenv("PATH", "")
	st := Detect(context.Background())
	if st.Present {
		t.Errorf("Detect with empty PATH: Present=true, want false")
	}
	if st.LoggedIn || st.Host != "" {
		t.Errorf("Detect absent: unexpected %+v", st)
	}
	if Host(context.Background()) != "" {
		t.Errorf("Host absent: want empty")
	}
}

func TestDetectRunning(t *testing.T) {
	withStubTailscale(t, `
if [ "$1" = "status" ]; then
  echo '{"BackendState":"Running","Self":{"DNSName":"box.tail-scale.ts.net."}}'
  exit 0
fi
exit 1
`)
	st := Detect(context.Background())
	if !st.Present || !st.LoggedIn {
		t.Fatalf("want present+loggedIn, got %+v", st)
	}
	if st.Host != "box.tail-scale.ts.net" {
		t.Errorf("Host = %q, want trailing-dot-trimmed", st.Host)
	}
	if st.State != "Running" {
		t.Errorf("State = %q", st.State)
	}
}

func TestDetectNeedsLogin(t *testing.T) {
	withStubTailscale(t, `
if [ "$1" = "status" ]; then
  echo '{"BackendState":"NeedsLogin","Self":{"DNSName":""}}'
  exit 0
fi
exit 1
`)
	st := Detect(context.Background())
	if !st.Present {
		t.Fatalf("want present, got %+v", st)
	}
	if st.LoggedIn {
		t.Errorf("NeedsLogin should not be LoggedIn")
	}
	if st.Host != "" {
		t.Errorf("Host = %q, want empty when not up", st.Host)
	}
}

func TestDetectStatusExitNonZero(t *testing.T) {
	// Binary present but `status` fails → Present=true, not-logged-in.
	withStubTailscale(t, `exit 1`)
	st := Detect(context.Background())
	if !st.Present || st.LoggedIn {
		t.Errorf("present-but-failed: got %+v", st)
	}
}

func TestServeStatus(t *testing.T) {
	tests := []struct {
		name           string
		script         string
		port           string
		wantConfigured bool
		wantDetectable bool
	}{
		{
			name:           "empty port",
			script:         `exit 0`,
			port:           "",
			wantConfigured: false,
			wantDetectable: false,
		},
		{
			name:           "mapping present",
			script:         `if [ "$1" = "serve" ]; then echo 'https://box.ts.net:443 -> http://127.0.0.1:8123'; exit 0; fi; exit 1`,
			port:           ":8123",
			wantConfigured: true,
			wantDetectable: true,
		},
		{
			name:           "no mapping",
			script:         `if [ "$1" = "serve" ]; then echo 'No serve config'; exit 0; fi; exit 1`,
			port:           ":8123",
			wantConfigured: false,
			wantDetectable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withStubTailscale(t, tt.script)
			gotConf, gotDet := ServeStatus(context.Background(), tt.port)
			if gotConf != tt.wantConfigured || gotDet != tt.wantDetectable {
				t.Errorf("ServeStatus(%q) = (%v,%v), want (%v,%v)", tt.port, gotConf, gotDet, tt.wantConfigured, tt.wantDetectable)
			}
		})
	}
}

func TestServeStatusAbsentBinary(t *testing.T) {
	t.Setenv("PATH", "")
	conf, det := ServeStatus(context.Background(), ":8123")
	if conf || det {
		t.Errorf("absent binary: want (false,false), got (%v,%v)", conf, det)
	}
}

func TestRunServe(t *testing.T) {
	t.Run("invalid port never execs", func(t *testing.T) {
		res := RunServe(context.Background(), "not-a-port")
		if res.OK || res.Err == "" {
			t.Errorf("invalid port: want failure, got %+v", res)
		}
	})
	t.Run("success", func(t *testing.T) {
		withStubTailscale(t, "exit 0\n")
		res := RunServe(context.Background(), ":34109")
		if !res.OK || res.EnableURL != "" || res.Err != "" {
			t.Errorf("success: got %+v", res)
		}
	})
	t.Run("enable gate surfaces url", func(t *testing.T) {
		withStubTailscale(t, `echo "Serve is not enabled on your tailnet."
echo "To enable, visit:"
echo "        https://login.tailscale.com/f/serve?node=nsudg7LtHj11CNTRL"
exit 1
`)
		res := RunServe(context.Background(), "34109")
		if res.EnableURL != "https://login.tailscale.com/f/serve?node=nsudg7LtHj11CNTRL" {
			t.Errorf("enable gate: want the consent URL, got %+v", res)
		}
	})
	t.Run("other failure is an error", func(t *testing.T) {
		withStubTailscale(t, "echo 'some other failure' 1>&2\nexit 1\n")
		res := RunServe(context.Background(), "34109")
		if res.OK || res.EnableURL != "" || res.Err == "" {
			t.Errorf("failure: got %+v", res)
		}
	})
	t.Run("access-denied flags NeedsPrivilege not a raw error", func(t *testing.T) {
		withStubTailscale(t, "echo 'sending serve config: Access denied: serve config denied' 1>&2\nexit 1\n")
		res := RunServe(context.Background(), "34109")
		if !res.NeedsPrivilege || res.OK || res.EnableURL != "" {
			t.Errorf("access denied: want NeedsPrivilege, got %+v", res)
		}
		if res.Err == "" {
			t.Errorf("access denied: want a friendly Err, got empty")
		}
	})
	t.Run("permission-denied also flags NeedsPrivilege", func(t *testing.T) {
		withStubTailscale(t, "echo 'permission denied' 1>&2\nexit 1\n")
		res := RunServe(context.Background(), "34109")
		if !res.NeedsPrivilege {
			t.Errorf("permission denied: want NeedsPrivilege, got %+v", res)
		}
	})
	t.Run("enable gate takes precedence over privilege phrasing", func(t *testing.T) {
		withStubTailscale(t, `echo "Access denied"
echo "https://login.tailscale.com/f/serve?node=X"
exit 1
`)
		res := RunServe(context.Background(), "34109")
		if res.EnableURL == "" || res.NeedsPrivilege {
			t.Errorf("enable gate precedence: got %+v", res)
		}
	})
}

func TestOperatorGrantArgv(t *testing.T) {
	got := OperatorGrantArgv("marmutapp")
	want := []string{"sudo", "tailscale", "set", "--operator=marmutapp"}
	if len(got) != len(want) {
		t.Fatalf("argv len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestLoginArgv(t *testing.T) {
	if got, want := LoginArgv(true), []string{"tailscale", "up"}; !equalStrs(got, want) {
		t.Errorf("LoginArgv(root) = %v, want %v", got, want)
	}
	if got, want := LoginArgv(false), []string{"sudo", "tailscale", "up"}; !equalStrs(got, want) {
		t.Errorf("LoginArgv(non-root) = %v, want %v", got, want)
	}
}

func TestInstallArgv(t *testing.T) {
	got := InstallArgv()
	want := []string{"sudo", "sh", "-c", "curl -fsSL --proto '=https' --tlsv1.2 https://tailscale.com/install.sh | sh"}
	if !equalStrs(got, want) {
		t.Errorf("InstallArgv() = %v, want %v", got, want)
	}
}

// equalStrs reports element-wise slice equality (test helper).
func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCurrentDaemonUser(t *testing.T) {
	// The real os/user.Current() must yield a usernameRe-valid name on the test
	// host; assert it round-trips into a sane grant argv rather than pinning a
	// specific name.
	name, isRoot, err := CurrentDaemonUser()
	if err != nil {
		t.Fatalf("CurrentDaemonUser: %v", err)
	}
	if !usernameRe.MatchString(name) {
		t.Errorf("username %q failed usernameRe", name)
	}
	if isRoot && name != "root" {
		t.Errorf("isRoot=true but name=%q", name)
	}
}
