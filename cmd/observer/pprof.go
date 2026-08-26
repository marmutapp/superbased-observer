package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// pprofEnvVar is the single env knob that arms the diagnostic profiling
// server. Empty (the default) means OFF — the daemon binds nothing and the
// net/http/pprof handlers are never reachable. Set it to a loopback
// host:port (e.g. 127.0.0.1:6060) to expose the standard Go profiling
// endpoints for the lifetime of the daemon.
const pprofEnvVar = "OBSERVER_PPROF_ADDR"

// maybeServePprof starts a localhost-only net/http/pprof server inside the
// daemon errgroup when OBSERVER_PPROF_ADDR is set to a loopback address.
//
// This is permanent, default-off diagnostic infrastructure: the CPU-audit
// work-stream (docs task queue Task 3) needs live CPU/heap/goroutine
// profiles of the running daemon, and black-box /proc forensics can only
// narrow a hot loop, never name it. A 30s `go tool pprof` capture against
// this endpoint names the consuming call stack directly.
//
// Safety posture: the bind address MUST resolve to a loopback host. A
// non-loopback address is refused (the profiling endpoints leak stack
// contents, goroutine dumps, and can trigger expensive captures — they must
// never be exposed off-box). A refused or failed bind is fail-soft: it logs
// and returns, never cancels the proxy/watcher/dashboard siblings.
func maybeServePprof(ctx context.Context, addr string, out, errOut io.Writer) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(errOut, "pprof disabled — %s=%q is not a valid host:port: %v\n", pprofEnvVar, addr, err)
		return
	}
	if !hostIsLoopback(host) {
		fmt.Fprintf(errOut, "pprof disabled — %s=%q is not a loopback address; profiling endpoints must never be exposed off-box\n", pprofEnvVar, addr)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(errOut, "pprof disabled — cannot listen on %s: %v\n", addr, err)
		return
	}
	fmt.Fprintf(out, "  (diagnostic) pprof → http://%s/debug/pprof/ — armed via %s\n", addr, pprofEnvVar)

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			fmt.Fprintf(errOut, "pprof server exited: %v\n", serr)
		}
	}()
}

// hostIsLoopback reports whether host (the host portion of a host:port bind
// address) is a loopback interface. An empty host is treated as NON-loopback
// (an empty host in a listen address binds all interfaces), as is any name
// that resolves to a non-loopback IP.
func hostIsLoopback(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	if host == "localhost" {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}
