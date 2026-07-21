// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (c) 2026 Marmut App

package main

import (
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// codexProxyFailClosedMsg builds the actionable refusal printed when a codex
// launch cannot honestly proceed because a persistent $CODEX_HOME/config.toml
// route the launcher can't neutralize (codex 0.130+ reads the file and drops the
// argv `-c openai_base_url` override) would either silently defeat
// --no-proxy-route (reasonNoProxyRouteConflict) or send codex into an
// unreachable proxy (reasonProxyDownConflict). offenders is the list of
// config.toml paths that route to the proxy (from codexConfigsRoutingToProxy).
//
// The reasonNoProxyRouteConflict branch reproduces codexNoProxyRouteConflict's
// wording verbatim so the two entry points (this launcher path and the
// runCodexAttach client-side B3-1 check that calls codexNoProxyRouteConflict
// directly) stay consistent; the reasonProxyDownConflict branch is the new
// daemon-down copy that additionally offers "start the daemon" as the primary
// fix.
func codexProxyFailClosedMsg(reason proxyFallbackReason, proxyURL string, offenders []string) string {
	joined := strings.Join(offenders, ", ")
	primary := primaryAcceptedURL(acceptedProxyBaseURLs(proxyURL))
	switch reason {
	case reasonProxyDownConflict:
		return fmt.Sprintf(
			"observer codex: refusing to launch — the observer proxy at %s is unreachable, but %s still routes codex to it (via the top-level openai_base_url key or model_provider=%q + [model_providers.%s].base_url), and codex 0.130+ reads that file, so every request would hit the dead proxy and fail (the launcher can't neutralize a config.toml route — its `-c` override is dropped). Fix it one of two ways: start the daemon with `observer start` (recommended — restores capture), OR remove that routing from %s (or restore its config.toml.bak.* backup) to run codex against its own provider. Then re-run.",
			primary, joined, proxyroute.ProviderName, proxyroute.ProviderName, joined,
		)
	default: // reasonNoProxyRouteConflict
		return fmt.Sprintf(
			"observer codex: refusing to launch under --no-proxy-route — %s still routes codex to the observer proxy (%s), either via the top-level openai_base_url key or via model_provider=%q + [model_providers.%s].base_url, and codex 0.130+ reads that file, so it would KEEP routing through the proxy and capture turns you asked not to capture. This was written by `observer codex --write-config` or `observer init` (or hand-edited); remove that routing from %s (or restore its config.toml.bak.* backup) and re-run.",
			joined, primary, proxyroute.ProviderName, proxyroute.ProviderName, joined,
		)
	}
}
