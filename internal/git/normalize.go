package git

import (
	"net/url"
	"strings"
)

// defaultRemotePorts maps a URL scheme to the port that transport uses
// implicitly, so that an explicit port matching the default can be
// stripped from the canonical output (a non-default port is real
// identity — e.g. a self-hosted GitLab/Gitea instance — and is
// preserved).
var defaultRemotePorts = map[string]string{
	"ssh":   "22",
	"http":  "80",
	"https": "443",
}

// NormalizeRemote reduces a git remote URL to a canonical, scheme-free,
// credential-free identity string for cross-machine/cross-developer
// project-identity grouping (Team Project Identity Mapping plan, 2026-08-21,
// §1 L1). It is pure and never errors or panics — an input it cannot make
// sense of is returned trimmed and otherwise unchanged (fail open), so a
// caller can always safely persist and hash the result.
//
// Canonical output shape: "<host>/<path>", or "<host>:<port>/<path>" when
// the source transport specified a non-default port. The scheme is dropped
// entirely — RULING (2026-08-21, see the plan's §1 L1 and former §5 OQ3):
// SCP-style ssh, ssh://, and https(s):// forms of the same repository all
// normalize to the identical string. For example
// "git@github.com:org/repo.git", "ssh://git@github.com/org/repo", and
// "https://github.com/org/repo.git" all normalize to "github.com/org/repo".
//
// Rules applied (see the plan's normalization table for the full
// rationale of each):
//   - Host is lowercased (DNS is case-insensitive); path case is preserved
//     (git hosting paths can be case-sensitive; folding case risks merging
//     two genuinely distinct repos).
//   - Userinfo/credentials (a scp "user@" prefix, or URL
//     "user:password@") are discarded — they must never enter a hash input
//     or be persisted.
//   - A port matching the transport's default (22 ssh, 80 http, 443
//     https) is stripped; any other port is preserved as "host:port".
//   - A trailing ".git" suffix and trailing "/" are stripped.
//   - Input this function cannot parse as a URL or an scp-style
//     "[user@]host:path" reference (e.g. a local filesystem path, or
//     unparseable garbage) is returned trimmed, unchanged — never an
//     error.
//
// NormalizeRemote is idempotent: NormalizeRemote(NormalizeRemote(x)) ==
// NormalizeRemote(x) for every x, including its own port-preserving
// "host:port/path" output (re-parsed as an scp-shaped reference whose
// leading path segment is entirely digits followed by "/" or end-of-string
// — a deliberate heuristic that trades a vanishingly rare false positive
// against round-trip stability, since real git paths are never a bare
// number).
func NormalizeRemote(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	if !strings.Contains(trimmed, "://") {
		if host, port, path, ok := parseSCPLike(trimmed); ok {
			return assembleRemote(host, port, path)
		}
		// No scheme and not scp-shaped: a local filesystem path or other
		// unrecognized form. Fail open.
		return trimmed
	}

	u, err := url.Parse(trimmed)
	if err != nil || u.Hostname() == "" {
		return trimmed
	}

	scheme := strings.ToLower(u.Scheme)
	host := u.Hostname()
	port := u.Port()
	if port != "" {
		if def, ok := defaultRemotePorts[scheme]; ok && port == def {
			port = ""
		}
	}
	path := strings.TrimPrefix(u.Path, "/")
	return assembleRemote(host, port, path)
}

// parseSCPLike recognizes the git "[user@]host:path" scp-style remote
// syntax (e.g. "git@github.com:org/repo.git") and the round-trip shape
// this package's own canonical output produces for a non-default port
// ("host:port/path"). It deliberately rejects shapes that only coincidally
// contain a colon but aren't this syntax — most importantly a Windows
// local path like "C:\Users\alice\repo.git", where the segment before the
// colon is a single-character drive letter, never a real hostname.
func parseSCPLike(raw string) (host, port, path string, ok bool) {
	idx := strings.Index(raw, ":")
	if idx < 0 {
		return "", "", "", false
	}
	hostPart := raw[:idx]
	pathPart := raw[idx+1:]
	if hostPart == "" || pathPart == "" {
		return "", "", "", false
	}
	if strings.ContainsAny(hostPart, "/\\") {
		return "", "", "", false
	}
	if len(hostPart) <= 1 {
		return "", "", "", false
	}
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
		if hostPart == "" {
			return "", "", "", false
		}
	}
	if p, rest, isPort := splitLeadingPort(pathPart); isPort {
		return hostPart, p, rest, true
	}
	return hostPart, "", pathPart, true
}

// splitLeadingPort reports whether pathPart begins with a run of ASCII
// digits followed by either "/" or end-of-string — the shape
// NormalizeRemote's own "host:port/path" canonical output takes when
// re-parsed by parseSCPLike. It is the mechanism that keeps NormalizeRemote
// idempotent for port-preserving outputs.
func splitLeadingPort(pathPart string) (port, rest string, ok bool) {
	i := 0
	for i < len(pathPart) && pathPart[i] >= '0' && pathPart[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	if i == len(pathPart) {
		return pathPart, "", true
	}
	if pathPart[i] != '/' {
		return "", "", false
	}
	return pathPart[:i], pathPart[i+1:], true
}

// assembleRemote builds the final canonical string from an already-
// extracted host/port/path triple: lowercases the host, preserves path
// case, strips a trailing ".git" and surrounding slashes, and folds the
// port in only when non-empty.
func assembleRemote(host, port, path string) string {
	host = strings.ToLower(host)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")

	out := host
	if port != "" {
		out += ":" + port
	}
	if path != "" {
		out += "/" + path
	}
	return out
}
