package browser

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestNewRequiresHandler(t *testing.T) {
	if _, err := New(Options{Addr: "127.0.0.1:0"}); err == nil {
		t.Fatalf("expected error when Handler is nil")
	}
}

func TestNewRefusesNonLoopback(t *testing.T) {
	_, err := New(Options{
		Addr:    "10.0.0.5:9999",
		Handler: func(context.Context, []byte) error { return nil },
	})
	if !errors.Is(err, ErrNonLoopback) {
		t.Fatalf("err = %v, want ErrNonLoopback", err)
	}
}

func TestNewAllowsNonLoopbackWhenOptedIn(t *testing.T) {
	// Bind to loopback port 0 but with AllowNonLoopback set — the guard
	// must be bypassed without erroring. (We still bind loopback so the
	// test doesn't open a real external port.)
	r, err := New(Options{
		Addr:             "127.0.0.1:0",
		AllowNonLoopback: true,
		Handler:          func(context.Context, []byte) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = r.Shutdown(context.Background())
}

func startReceiver(t *testing.T, h Handler) *Receiver {
	t.Helper()
	r, err := New(Options{Addr: "127.0.0.1:0", Handler: h})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Shutdown(ctx)
	})
	return r
}

func post(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestCaptureSuccess(t *testing.T) {
	var got []byte
	r := startReceiver(t, func(_ context.Context, body []byte) error {
		got = append([]byte(nil), body...)
		return nil
	})
	url := "http://" + r.Addr() + capturePath
	resp := post(t, url, []byte(`{"site":"chatgpt-web"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if string(got) != `{"site":"chatgpt-web"}` {
		t.Errorf("handler got %q", string(got))
	}
}

func TestCaptureHandlerError(t *testing.T) {
	r := startReceiver(t, func(context.Context, []byte) error {
		return errors.New("boom")
	})
	url := "http://" + r.Addr() + capturePath
	resp := post(t, url, []byte(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestCaptureEmptyBody(t *testing.T) {
	r := startReceiver(t, func(context.Context, []byte) error { return nil })
	url := "http://" + r.Addr() + capturePath
	resp := post(t, url, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCaptureMethodNotAllowed(t *testing.T) {
	r := startReceiver(t, func(context.Context, []byte) error { return nil })
	url := "http://" + r.Addr() + capturePath
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// startReceiverOpts starts a receiver with custom Options (loopback bind).
func startReceiverOpts(t *testing.T, opts Options) *Receiver {
	t.Helper()
	opts.Addr = "127.0.0.1:0"
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Shutdown(ctx)
	})
	return r
}

func TestCaptureRejectsMissingToken(t *testing.T) {
	r := startReceiverOpts(t, Options{
		Token:   "s3cr3t",
		Handler: func(context.Context, []byte) error { return nil },
	})
	resp := post(t, "http://"+r.Addr()+capturePath, []byte(`{"site":"x"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no token)", resp.StatusCode)
	}
}

func TestCaptureRejectsWrongToken(t *testing.T) {
	r := startReceiverOpts(t, Options{
		Token:   "s3cr3t",
		Handler: func(context.Context, []byte) error { return nil },
	})
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Addr()+capturePath, bytes.NewReader([]byte(`{}`)))
	req.Header.Set(TokenHeader, "nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (wrong token)", resp.StatusCode)
	}
}

func TestCaptureAcceptsCorrectToken(t *testing.T) {
	var got []byte
	r := startReceiverOpts(t, Options{
		Token: "s3cr3t",
		Handler: func(_ context.Context, body []byte) error {
			got = append([]byte(nil), body...)
			return nil
		},
	})
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Addr()+capturePath, bytes.NewReader([]byte(`{"site":"chatgpt-web"}`)))
	req.Header.Set(TokenHeader, "s3cr3t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (correct token)", resp.StatusCode)
	}
	if string(got) != `{"site":"chatgpt-web"}` {
		t.Errorf("handler got %q", string(got))
	}
}

func TestCaptureRejectsNonLoopbackHost(t *testing.T) {
	// A Host header pointing at a non-loopback name (DNS-rebinding shape) is
	// rejected 403 even though the bind is loopback and no token is set.
	r := startReceiverOpts(t, Options{
		Handler: func(context.Context, []byte) error { return nil },
	})
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Addr()+capturePath, bytes.NewReader([]byte(`{}`)))
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (non-loopback Host)", resp.StatusCode)
	}
}

func TestHostIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8821", true},
		{"localhost", true},
		{"localhost:8821", true},
		{"[::1]:8821", true},
		{"::1", true},
		{"evil.example.com", false},
		{"evil.example.com:80", false},
		{"10.0.0.5:9999", false},
	}
	for _, tc := range tests {
		if got := hostIsLoopback(tc.host); got != tc.want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestDefaultAddrApplied(t *testing.T) {
	// A blank Addr should resolve to DefaultAddr; assert via guardLoopback
	// (DefaultAddr must be loopback).
	if err := guardLoopback(DefaultAddr, false); err != nil {
		t.Fatalf("DefaultAddr must be loopback: %v", err)
	}
}
