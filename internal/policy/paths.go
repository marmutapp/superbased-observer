package policy

import "strings"

// Path handling in this package is deliberately OS-AGNOSTIC: it never
// uses path/filepath (whose semantics depend on the daemon's runtime
// GOOS) because the daemon's OS and the analyzed client's OS can
// differ (Linux daemon + Windows client via the WSL bridge, and vice
// versa). The same Event must produce the same Verdict everywhere, so
// flavor detection is derived from the path STRING itself: drive
// letters / backslashes / UNC prefixes mean Windows shape, everything
// else POSIX shape. Resolution is purely lexical — no symlink or
// case-folding filesystem truth is consulted (no I/O); that residual
// gap is the documented F1-class caveat.

// isWindowsPath reports whether p is Windows-shaped: a drive-letter
// prefix ("C:"), a backslash separator anywhere, or a UNC prefix.
func isWindowsPath(p string) bool {
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		return true
	}
	return strings.Contains(p, `\`) || strings.HasPrefix(p, `\\`)
}

// isASCIILetter reports whether c is an ASCII letter.
func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isAbsPath reports whether p is absolute in either flavor: a leading
// "/" (POSIX), a drive-letter prefix, or a UNC prefix.
func isAbsPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0])
}

// normPath converts p to the canonical comparable form used
// throughout this package: forward slashes only, repeated separators
// collapsed, "." segments dropped, ".." resolved lexically (clamped
// at the root for absolute paths, preserved when they would escape a
// relative path), drive letters lowercased, and trailing separators
// trimmed (except on a bare root). A preserved leading "~" segment
// passes through so unexpanded home-relative forms remain matchable.
func normPath(p string) string {
	if p == "" {
		return ""
	}
	s := strings.ReplaceAll(p, `\`, "/")
	drive := ""
	if len(s) >= 2 && s[1] == ':' && isASCIILetter(s[0]) {
		drive = strings.ToLower(s[:2])
		s = s[2:]
	}
	rooted := strings.HasPrefix(s, "/")
	segs := strings.Split(s, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		switch seg {
		case "", ".":
			continue
		case "..":
			if n := len(out); n > 0 && out[n-1] != ".." {
				out = out[:n-1]
			} else if !rooted && drive == "" {
				out = append(out, "..")
			}
			// ".." at an absolute root clamps (deleting above /
			// is still deleting /).
		default:
			out = append(out, seg)
		}
	}
	joined := strings.Join(out, "/")
	switch {
	case drive != "" && rooted:
		return drive + "/" + joined
	case drive != "":
		if joined == "" {
			return drive
		}
		return drive + "/" + joined // treat drive-relative as drive-rooted (lexical best effort)
	case rooted:
		return "/" + joined
	default:
		return joined
	}
}

// expandHome rewrites a leading home reference — "~", "~/", "$HOME",
// "%USERPROFILE%" (any case) — to the injected home directory. When
// home is empty the reference is left in place, which still matches
// the "~/"-anchored pattern tables (graceful degradation, documented
// in engine.go).
func expandHome(p, home string) string {
	if home == "" {
		return p
	}
	rest, ok := splitHomePrefix(p)
	if !ok {
		return p
	}
	if rest == "" {
		return home
	}
	return home + "/" + rest
}

// splitHomePrefix strips a recognized home-reference prefix from p and
// returns the remainder (without its leading separator). ok is false
// when p carries no home prefix.
func splitHomePrefix(p string) (rest string, ok bool) {
	for _, pre := range [...]string{"~", "$HOME", "%USERPROFILE%", "%userprofile%"} {
		if !hasFoldPrefix(p, pre) {
			continue
		}
		tail := p[len(pre):]
		if tail == "" {
			return "", true
		}
		if tail[0] == '/' || tail[0] == '\\' {
			return tail[1:], true
		}
	}
	return "", false
}

// hasFoldPrefix is a case-insensitive strings.HasPrefix (ASCII fold —
// path prefixes here are ASCII).
func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// resolvePath resolves target into the canonical comparable form:
// home references expanded, relative paths joined against cwd (or
// projectRoot when cwd is empty), then normalized. Returns "" when
// the path cannot be resolved (relative target with no anchor) —
// callers must treat "" as not-a-hit, never as a hit.
func resolvePath(target, home, cwd, projectRoot string) string {
	if target == "" {
		return ""
	}
	p := expandHome(target, home)
	if !isAbsPath(p) && !strings.HasPrefix(p, "~") {
		anchor := cwd
		if anchor == "" {
			anchor = projectRoot
		}
		if anchor == "" {
			return ""
		}
		p = strings.TrimRight(anchor, "/\\") + "/" + p
	}
	return normPath(p)
}

// comparableFlavors reports whether two normalized paths can be
// meaningfully compared: both Windows-shaped or both POSIX-shaped.
// Mixed flavors mean a missing crossmount translation at the
// boundary; rules treat that as unknown (no hit) rather than
// guessing.
func comparableFlavors(a, b string) bool {
	return isWindowsPath(a) == isWindowsPath(b)
}

// pathsEqual reports whether two normalized paths refer to the same
// location, comparing case-insensitively when either is
// Windows-shaped.
func pathsEqual(a, b string) bool {
	a, b = normPath(a), normPath(b)
	if isWindowsPath(a) || isWindowsPath(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isUnder reports whether child is root itself or lexically beneath
// it. Both arguments are normalized first; comparison is
// case-insensitive when either side is Windows-shaped. Mixed flavors
// are never under each other (see comparableFlavors).
func isUnder(child, root string) bool {
	c, r := normPath(child), normPath(root)
	if c == "" || r == "" {
		return false
	}
	if !comparableFlavors(c, r) {
		return false
	}
	ci := isWindowsPath(c) || isWindowsPath(r)
	if ci {
		c, r = strings.ToLower(c), strings.ToLower(r)
	}
	if c == r {
		return true
	}
	if !strings.HasSuffix(r, "/") {
		r += "/"
	}
	return strings.HasPrefix(c, r)
}

// isFilesystemRoot reports whether the normalized path is a
// filesystem root: "/" or a bare drive ("c:", "c:/").
func isFilesystemRoot(p string) bool {
	p = normPath(p)
	if p == "/" {
		return true
	}
	return len(p) >= 2 && len(p) <= 3 && p[1] == ':' && isASCIILetter(p[0]) &&
		(len(p) == 2 || p[2] == '/')
}

// isRootDepthGlob reports whether the normalized path is a
// root-depth wildcard — "/*", "c:/*" or "<home>/*" — the
// everything-at-once delete shapes R-101 treats as catastrophic.
func isRootDepthGlob(p, home string) bool {
	p = normPath(p)
	if p == "/*" || p == "~/*" {
		return true
	}
	if len(p) == 4 && p[1] == ':' && isASCIILetter(p[0]) && p[2] == '/' && p[3] == '*' {
		return true
	}
	return home != "" && pathsEqual(strings.TrimSuffix(p, "/*"), home) && strings.HasSuffix(p, "/*")
}

// baseName returns the final segment of a normalized path ("" for a
// bare root).
func baseName(p string) string {
	p = normPath(p)
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// hasComponent reports whether any path segment of the normalized
// path equals comp (case-insensitive on Windows-shaped paths). Used
// for ".git"-targeting detection.
func hasComponent(p, comp string) bool {
	n := normPath(p)
	ci := isWindowsPath(n)
	for _, seg := range strings.Split(n, "/") {
		if seg == comp || (ci && strings.EqualFold(seg, comp)) {
			return true
		}
	}
	return false
}

// matchGlob matches a normalized forward-slash path against a glob
// pattern supporting "*" and "?" within a segment and "**" as a
// zero-or-more-segments wildcard. Character classes ("[a-z]") are NOT
// supported — the policy pattern tables don't need them and keeping
// the matcher small keeps it auditable. ci selects ASCII
// case-insensitive matching (used for the sensitive-path tables: see
// rules_boundary.go for why that is always-on there).
func matchGlob(pattern, path string, ci bool) bool {
	pat, p := normPath(pattern), normPath(path)
	if ci {
		pat, p = strings.ToLower(pat), strings.ToLower(p)
	}
	return matchSegs(strings.Split(pat, "/"), strings.Split(p, "/"))
}

// matchSegs matches pattern segments against path segments,
// implementing the "**" zero-or-more-segments semantics recursively
// (pattern depth is small and bounded by the rule tables).
func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if matchSegs(pat[1:], segs) {
			return true
		}
		if len(segs) > 0 {
			return matchSegs(pat, segs[1:])
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if !matchSeg(pat[0], segs[0]) {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

// matchSeg matches one path segment against one pattern segment with
// "*" (any run) and "?" (any single byte) wildcards, using greedy
// star matching with backtracking. Byte-wise: non-ASCII segments
// compare by exact bytes, which is correct for the ASCII pattern
// tables this package ships.
func matchSeg(p, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// matchAnyGlob reports whether path matches any pattern in patterns.
func matchAnyGlob(patterns []string, path string, ci bool) bool {
	for _, pat := range patterns {
		if matchGlob(pat, path, ci) {
			return true
		}
	}
	return false
}

// looksPathish reports whether a shell token plausibly names a
// filesystem path worth resolving against the path rule tables: it
// contains a separator, or starts with a home/env home reference, or
// a dot-segment. Bare words ("config") are deliberately NOT pathish —
// resolving every word against the sensitive tables would be FP soup.
func looksPathish(tok string) bool {
	if tok == "" || strings.HasPrefix(tok, "-") {
		return false
	}
	if strings.ContainsAny(tok, `/\`) {
		return true
	}
	if strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, "$HOME") ||
		hasFoldPrefix(tok, "%USERPROFILE%") {
		return true
	}
	return tok == "." || tok == ".." || strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../")
}
