// attach_authenv.go — credential-env forwarding for the attach client.
//
// When `observer <tool>` attaches, the tool runs as a DAEMON-spawned child, not
// a child of the caller's shell — so it inherits the DAEMON's environment, not
// the caller's os.Environ(). A bare launch inherits the caller's env directly,
// so a shell-exported-only provider key (never written to any config file)
// reaches a bare-launched tool but would be INVISIBLE to a daemon-spawned one.
// This file restores that parity: the attach client forwards the caller's
// values for the tool's grounded credential-env NAMES (integration.Capability
// .AuthEnv) across the owner-only attach socket, exactly as claudeAttachEnv /
// codexAttachEnv forward the profile-selector vars.

package main

// forwardAuthEnv builds the credential-env slice the attach client forwards
// across the socket, so a shell-exported-only API key reaches the daemon-
// spawned child as it would a bare launch. For each key in keys (declaration
// order preserved, deduped), it emits `KEY=VALUE` ONLY when the key is PRESENT
// in environ — an ABSENT key is skipped (the daemon's inherited value, if any,
// stands), a present-but-empty `KEY=` is forwarded verbatim (F3, the same
// presence-aware semantics as claudeAttachEnv / codexAttachEnv). A duplicated
// key in environ resolves last-wins (lookupEnvValue), matching how the child
// resolves duplicate env entries. launchChildEnv layers this ExtraEnv AFTER the
// inherited env, so a forwarded value wins at the child.
//
// CREDENTIAL-BEARING: the returned entries carry secret VALUES. They transit the
// owner-only (0600 AF_UNIX) attach socket once per launch and must NEVER be
// logged, persisted, or echoed to stderr.
func forwardAuthEnv(keys []string, environ []string) []string {
	var out []string
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if v, ok := lookupEnvValue(environ, k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// composeAttachEnv layers the forwarded credential-env AFTER the base attach env
// (the base-URL + profile vars a tool's attachEnv closure returns), gated by the
// [terminal.attach].forward_auth_env config. When forward is false the base env
// is returned UNCHANGED. The credential keys (registryKeys ∪ extraKeys, merged
// registry-first + deduped) name a DISJOINT key space from the base env, so the
// claude/codex profile+base-URL closures are never clobbered by the append. It
// is the single gate seam launcherAttach calls, kept pure so the true/false
// behavior + non-clobber invariant are table-testable without a live attach
// socket. Credential-bearing (see forwardAuthEnv): never log the result.
func composeAttachEnv(base []string, forward bool, registryKeys, extraKeys, environ []string) []string {
	if !forward {
		return base
	}
	return append(base, forwardAuthEnv(mergeAuthKeys(registryKeys, extraKeys), environ)...)
}

// mergeAuthKeys returns the union of the registry AuthEnv keys and any launcher-
// supplied extra keys — registry keys first, in declaration order, deduped. It
// lets a launcher whose credential env is DYNAMIC (hermes' `--key-env NAME`
// names the env var only at launch time) add a key the static registry row
// cannot know, without duplicating a key already in the row. forwardAuthEnv also
// dedupes, so passing the default `--key-env` value (already in the row) is a
// harmless no-op.
func mergeAuthKeys(registry, extra []string) []string {
	out := make([]string, 0, len(registry)+len(extra))
	seen := make(map[string]bool, len(registry)+len(extra))
	for _, group := range [][]string{registry, extra} {
		for _, k := range group {
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
