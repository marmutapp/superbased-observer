// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (c) 2026 Marmut App

package main

import (
	"strings"
	"testing"
)

// TestCodexProxyFailClosedMsg_NoProxyRouteConflict: the --no-proxy-route conflict
// copy (unified with codexNoProxyRouteConflict) names the offending file, the
// provider, and the write-config/.bak revert path.
func TestCodexProxyFailClosedMsg_NoProxyRouteConflict(t *testing.T) {
	msg := codexProxyFailClosedMsg(reasonNoProxyRouteConflict, "http://127.0.0.1:8820",
		[]string{"/home/u/.codex/config.toml"})
	for _, want := range []string{
		"/home/u/.codex/config.toml",
		"--no-proxy-route",
		"openai-observer",
		".bak",
		"refusing to launch",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-proxy-route conflict msg missing %q:\n%s", want, msg)
		}
	}
}

// TestCodexProxyFailClosedMsg_ProxyDownConflict: the daemon-down conflict copy
// (the new symmetric fix) offers BOTH fixes — start the daemon, or strip the
// baked-in route — and names the dead proxy + the offending config.
func TestCodexProxyFailClosedMsg_ProxyDownConflict(t *testing.T) {
	msg := codexProxyFailClosedMsg(reasonProxyDownConflict, "http://127.0.0.1:8820",
		[]string{"/home/u/.codex/config.toml"})
	for _, want := range []string{
		"http://127.0.0.1:8820/v1",
		"/home/u/.codex/config.toml",
		"observer start",
		"unreachable",
		"refusing to launch",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("proxy-down conflict msg missing %q:\n%s", want, msg)
		}
	}
}
