package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Phase 2 of the remote-dashboard-access plan (§4.4): serve the dashboard over
// Tailscale HTTPS, VIEW tier only. `tailscale serve` terminates TLS on the
// tailnet and forwards PLAINTEXT to a loopback backend, injecting identity
// headers. That makes address-based trust fatal — a tailnet-forwarded request
// arrives from 127.0.0.1, so "loopback ⇒ trusted owner" would hand every
// remote user execute/RCE. This file binds a DEDICATED loopback backend that is
// classified remote-exposed AT CONSTRUCTION: it runs the full
// remoteGuardedHandler (auth for EVERY request, no RemoteAddr bypass) and is
// the ONLY place forwarded-identity headers are consumed. The owner-trusted
// direct listener never runs this chain and never reads these headers.

// tailnetIdentityHeaders are the forwarded-identity headers `tailscale serve`
// injects. They are consumed ONLY on the dedicated backend socket and stripped
// there before any downstream handler runs, so a client-supplied copy can never
// be trusted (plan §4.4 — `Tailscale-User-Login` is spoofable otherwise).
var tailnetIdentityHeaders = []string{
	"Tailscale-User-Login",
	"Tailscale-User-Name",
	"Tailscale-User-Profile-Pic",
}

// tailnetFunnelHeader marks a request that arrived over `tailscale funnel`
// (public internet, anonymous) rather than private `tailscale serve`. Funnel is
// explicitly NOT a supported transport for this plan (§9); its only use here is
// to refuse execute-tier routes outright as defence-in-depth on top of the
// capability check.
const tailnetFunnelHeader = "Tailscale-Funnel-Request"

type backendCtxKey int

const (
	tailnetUserKey backendCtxKey = iota
	tailnetFunnelKey
)

// tailnetUserFromContext returns the tailnet login captured on the backend
// socket (audit only — Phase-2 authz is the device-session cookie, never the
// tailnet identity), or "" on the direct listener / when absent.
func tailnetUserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tailnetUserKey).(string)
	return v
}

// funnelFromContext reports whether the request arrived over tailscale funnel
// (captured on the backend socket only).
func funnelFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(tailnetFunnelKey).(bool)
	return v
}

// captureBackendProvenance is the FIRST middleware on the dedicated
// tailnet-serve backend listener. It reads the forwarded provenance headers
// (identity + funnel marker), stashes them in the request context, then STRIPS
// the raw headers so no downstream handler can read a spoofed copy (plan §4.4).
// This is the ONLY place these headers are consumed.
func captureBackendProvenance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
		funnel := strings.TrimSpace(r.Header.Get(tailnetFunnelHeader)) != ""
		for _, h := range tailnetIdentityHeaders {
			r.Header.Del(h)
		}
		r.Header.Del(tailnetFunnelHeader)
		ctx := r.Context()
		if login != "" {
			ctx = context.WithValue(ctx, tailnetUserKey, login)
		}
		if funnel {
			ctx = context.WithValue(ctx, tailnetFunnelKey, true)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// remoteBackendHandler composes the tailnet-serve backend chain (plan §4.4):
// captureBackendProvenance (strip+stash forwarded headers) → the standard
// remoteGuardedHandler (Host allow-list + Origin/CSRF + deny-by-default
// capability authz + the controller's own routes). It is used ONLY by
// ListenAndServeTailnetBackend, never by the owner-trusted direct listener.
func (s *Server) remoteBackendHandler(rc RemoteController) http.Handler {
	// securityHeaders is the OUTERMOST wrapper so the execute-tier CSP + headers
	// (plan §8.1 item 7) ride every response on the remote-exposed tailnet-serve
	// backend too, not only the direct listener.
	return securityHeaders(captureBackendProvenance(s.remoteGuardedHandler(rc)))
}

// ListenAndServeTailnetBackend runs the dedicated tailnet-serve backend on addr
// (a loopback address distinct from the direct dashboard listener) until ctx is
// cancelled. Unlike ListenAndServe — whose loopback path is owner-trusted —
// this listener is classified remote-exposed at construction: EVERY request
// runs the full auth stack even though the peer is loopback (that is the whole
// point, plan §4.4). It refuses to start unless the [remote] substrate is
// Ready() and addr is an explicit loopback host:port.
func (s *Server) ListenAndServeTailnetBackend(ctx context.Context, addr string) error {
	rc := s.opts.Remote
	if rc == nil || !rc.Ready() {
		return errors.New("dashboard: refusing to start the tailscale backend without a Ready() [remote] substrate (auth + host allow-list + rate limit) — this closes the tailnet-serve-to-loopback trust trap (plan §4.4)")
	}
	if !hostIsLoopback(hostnameOnly(addr)) {
		return fmt.Errorf("dashboard: tailscale backend addr %q must be an explicit loopback address — `tailscale serve` forwards to loopback and a non-loopback backend would be directly reachable (plan §4.4)", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.remoteBackendHandler(rc),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
