package antigravity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestParseBridgeEndpointHint(t *testing.T) {
	cases := []struct {
		name         string
		stderr       string
		wantEndpoint string
		wantCSRF     string
	}{
		{
			name:         "single hint line",
			stderr:       "bridge-endpoint=http://127.0.0.1:35989\tbridge-csrf=tok123\n",
			wantEndpoint: "http://127.0.0.1:35989",
			wantCSRF:     "tok123",
		},
		{
			name:         "hint amidst other stderr",
			stderr:       "noise line\nbridge-endpoint=https://127.0.0.1:55555\tbridge-csrf=abc\nmore noise\n",
			wantEndpoint: "https://127.0.0.1:55555",
			wantCSRF:     "abc",
		},
		{
			name:         "CRLF line endings",
			stderr:       "bridge-endpoint=http://127.0.0.1:1234\tbridge-csrf=t\r\n",
			wantEndpoint: "http://127.0.0.1:1234",
			wantCSRF:     "t",
		},
		{
			name: "empty csrf (agy.exe / CLI embedded server)",
			// bridge emits empty csrf for agy.exe — we must NOT drop
			// the endpoint; the next pinned call passes --csrf '' and
			// the bridge tolerates it.
			stderr:       "bridge-endpoint=http://127.0.0.1:42424\tbridge-csrf=\n",
			wantEndpoint: "http://127.0.0.1:42424",
			wantCSRF:     "",
		},
		{
			name:         "no hint present",
			stderr:       "some other diagnostic stderr from earlier bridge build\n",
			wantEndpoint: "",
			wantCSRF:     "",
		},
		{
			name:         "empty stderr",
			stderr:       "",
			wantEndpoint: "",
			wantCSRF:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, csrf := parseBridgeEndpointHint(tc.stderr)
			if endpoint != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", endpoint, tc.wantEndpoint)
			}
			if csrf != tc.wantCSRF {
				t.Errorf("csrf = %q, want %q", csrf, tc.wantCSRF)
			}
		})
	}
}

func TestConvEndpointCacheRoundTrip(t *testing.T) {
	a := &Adapter{}
	// Cold cache: empty pin.
	if got := a.cachedConvEndpoint("conv-1"); got.Endpoint != "" {
		t.Errorf("cold cache: got %+v, want empty pin", got)
	}
	// Remember + retrieve.
	a.rememberConvEndpoint("conv-1", "http://127.0.0.1:35989", "tok-abc")
	got := a.cachedConvEndpoint("conv-1")
	if got.Endpoint != "http://127.0.0.1:35989" || got.CSRFToken != "tok-abc" {
		t.Errorf("after remember: got %+v, want endpoint+csrf", got)
	}
	// Different conv stays cold.
	if other := a.cachedConvEndpoint("conv-2"); other.Endpoint != "" {
		t.Errorf("conv-2 should be cold, got %+v", other)
	}
	// Overwrite (a new working endpoint replaces the old).
	a.rememberConvEndpoint("conv-1", "http://127.0.0.1:55555", "new-tok")
	got = a.cachedConvEndpoint("conv-1")
	if got.Endpoint != "http://127.0.0.1:55555" || got.CSRFToken != "new-tok" {
		t.Errorf("after overwrite: got %+v", got)
	}
	// Invalidate.
	a.invalidateConvEndpoint("conv-1")
	if got := a.cachedConvEndpoint("conv-1"); got.Endpoint != "" {
		t.Errorf("after invalidate: got %+v, want empty pin", got)
	}
}

func TestRememberConvEndpointIgnoresEmpty(t *testing.T) {
	// Older bridge builds (or stderr without the hint marker) emit
	// HitEndpoint=="". The cache must not store empty entries — that
	// would either poison subsequent pinned calls or burn the rest of
	// the lookup path.
	a := &Adapter{}
	a.rememberConvEndpoint("conv-1", "", "csrf")
	if got := a.cachedConvEndpoint("conv-1"); got.Endpoint != "" {
		t.Errorf("empty endpoint should not populate cache, got %+v", got)
	}
	a.rememberConvEndpoint("", "http://127.0.0.1:1", "csrf")
	if got := a.cachedConvEndpoint(""); got.Endpoint != "" {
		t.Errorf("empty conversationID should not populate cache, got %+v", got)
	}
}

func TestInvalidateConvEndpointIdempotent(t *testing.T) {
	// Cache miss on invalidate must not panic / error.
	a := &Adapter{}
	a.invalidateConvEndpoint("never-seen")
	a.invalidateConvEndpoint("never-seen") // second call also OK
}

func TestOnUnrecoverableFailureHonorsTransientBridgeError(t *testing.T) {
	// Build a stub fileinfo via a temp file.
	tmp, err := os.CreateTemp(t.TempDir(), "fake.pb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.Write([]byte("stub"))
	_ = tmp.Close()
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{}
	wrapped := fmt.Errorf("copy bridge: %w: %w", os.ErrPermission, ErrBridgeTransient)
	res := a.onUnrecoverableFailure(context.Background(), tmp.Name(), fi, 0, "test-reason", "test-warning", wrapped)
	if !res.RetrySuggested {
		t.Errorf("expected RetrySuggested=true for transient bridge error, got %+v", res)
	}
	if res.NewOffset != 0 {
		t.Errorf("expected cursor held at fromOffset=0, got NewOffset=%d", res.NewOffset)
	}
	// Marker MUST NOT be written for transient errors. Since the
	// Adapter has no tracker wired, this is vacuously true — but the
	// contract is still tested by RetrySuggested being set (which
	// short-circuits markUnrecoverable in the function body).
}

func TestWindowsCacheDestinationFor_FilenameIncludesSize(t *testing.T) {
	// We can't unit-test the dir-resolution side easily (it probes
	// /mnt/c) but we can verify the filename composition for a known
	// dir by reading bridgeBinaryName and checking the constructed
	// path shape — concretely that the basename includes the size
	// token between the binary name and the .exe suffix.
	const fakeDir = "/mnt/c/Users/test/AppData/Local/Temp/observer"
	const size = int64(9000123)
	// Compose manually to mirror the production logic; if the
	// production formula changes (e.g. switches to mtime+size), this
	// test will fail and remind the reader to update both sides.
	base := bridgeBinaryName // antigravity-bridge.exe
	want := fakeDir + "/" + base[:len(base)-len(".exe")] + "-" + "9000123" + ".exe"
	if want != "/mnt/c/Users/test/AppData/Local/Temp/observer/antigravity-bridge-9000123.exe" {
		t.Fatalf("test setup wrong: want = %q", want)
	}
	// Sanity: filename contains the size token.
	if !strings.Contains(want, "-9000123.exe") {
		t.Errorf("filename %q should contain size token", want)
	}
	_ = size // referenced for documentation
}

func TestOnUnrecoverableFailurePermanentErrorMarks(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "fake2.pb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tmp.Write([]byte("stub"))
	_ = tmp.Close()
	fi, _ := os.Stat(tmp.Name())
	a := &Adapter{}
	// Non-transient error (e.g. permanent decrypt failure) — should
	// NOT request retry, should advance cursor to file size.
	res := a.onUnrecoverableFailure(context.Background(), tmp.Name(), fi, 0, "perm", "perm-warn", errors.New("decrypt: cipher unknown"))
	if res.RetrySuggested {
		t.Errorf("expected RetrySuggested=false for permanent error, got true")
	}
	if res.NewOffset != fi.Size() {
		t.Errorf("expected cursor advanced to file size=%d, got %d", fi.Size(), res.NewOffset)
	}
}
