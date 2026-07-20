package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/remotenotify"
)

// httpNotifySender is the cmd-side transport for the pure remotenotify package:
// it POSTs the built Request over a short-timeout http.Client. It lives HERE,
// not in internal/remotenotify, so the pure core imports no net/http (CLAUDE.md
// module rule #1). This is the single outbound seam the Phase-0 rail adds; it
// is wired ONLY from the dashboard/lifecycle path (the termsession OnExit
// callback), never from the watcher/proxy capture path.
type httpNotifySender struct {
	client *http.Client
	logger *slog.Logger
}

// Send delivers req. A non-2xx or transport error is returned (and the caller
// logs it) — a notification failure must never affect the session it describes.
func (s httpNotifySender) Send(ctx context.Context, req remotenotify.Request) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return fmt.Errorf("build notify request: %w", err)
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	resp, err := s.client.Do(hr)
	if err != nil {
		return fmt.Errorf("deliver notify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// newRemoteNotifier builds the Phase-0 notifier from [remote.notify], or nil
// when the rail is disabled (a nil notifier is a safe no-op at every call
// site). The outbound HTTP client is short-timeout so a hung endpoint can't
// pile up goroutines.
func newRemoteNotifier(cfg config.Config, logger *slog.Logger) *remotenotify.Notifier {
	nc := cfg.Remote.Notify
	if !nc.Enabled {
		return nil
	}
	sender := httpNotifySender{
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
	return remotenotify.New(remotenotify.Config{
		Enabled: nc.Enabled,
		Kind:    nc.Kind,
		URL:     nc.URL,
		Events:  nc.Events,
	}, sender)
}
