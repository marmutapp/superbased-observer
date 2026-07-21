package policy

import (
	"strconv"
	"strings"
)

// Exfiltration & network rules R-170…R-173 (spec §5.3) — the exfil
// category table, deferred from G1 to G9 where its proxy half lands.
//
// R-172 spans TWO rows under one public ID (approved deviation 3):
// the shell-arg half (secrets in network-command arguments, evaluated
// on every shell event) and the api_request half (typed detector
// findings stamped onto proxy egress events by the guard layer,
// §8.2). Both rows agree on category/severity per validateRules.
//
// Category note: spec §4.3 declares CategorySecrets "secrets egress
// (§8.2)" while the §5.3 catalog table places R-172 under exfil. One
// ID cannot span two categories (validateRules), so BOTH R-172 halves
// use CategoryExfil — the §5.3 table is the catalog of record.
// CategorySecrets stays declared-but-reserved.

// remoteFetchBases are the canonical download-command bases R-170/
// R-171 treat as remote-content sources. psparse resolves the
// PowerShell aliases (curl/wget/iwr/irm) to the invoke-* cmdlet names
// before matching. Table-driven per Module rule 5.
var remoteFetchBases = map[string]bool{
	"curl": true, "wget": true,
	"invoke-webrequest": true, "invoke-restmethod": true,
}

// pipeInterpreterBases are the interpreter bases that turn piped
// remote content into code execution (the R-170 sink set).
var pipeInterpreterBases = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "csh": true, "tcsh": true,
	"python": true, "python2": true, "python3": true,
	"node": true, "nodejs": true, "perl": true, "ruby": true, "php": true,
	"pwsh": true, "powershell": true, "invoke-expression": true,
	"cmd": true,
}

// dnsLookupBases are the DNS-query bases R-173 watches.
// resolve-dnsname is the PowerShell cmdlet (no common alias).
var dnsLookupBases = map[string]bool{
	"nslookup": true, "dig": true, "host": true, "resolve-dnsname": true,
}

// exfilRules assembles the §5.3 table rows in catalog order.
func exfilRules() []Rule {
	return []Rule{
		{
			ID: "R-170", Category: CategoryExfil, Severity: SeverityCritical,
			AppliesTo: []EventKind{KindShellExec},
			Match:     matchRemoteCodePipe,
			Observe:   DecisionFlag, Enforce: DecisionDeny,
			Doc:    "remote content piped into an interpreter (curl|sh-class remote-code execution)",
			Advice: "Download to a file, review it, then execute — or pin the script to a checksum you have verified.",
		},
		{
			ID: "R-171", Category: CategoryExfil, Severity: SeverityHigh,
			AppliesTo: []EventKind{KindShellExec},
			Match:     nil, MatchCmd: matchFileUpload,
			SafePat: []SafeFn{safeUploadLocalhostOnly},
			Observe: DecisionFlag, Enforce: DecisionAsk,
			Doc:    "shell command uploading file contents to a remote destination",
			Advice: "Confirm the destination is one you intend to receive this file; project files leaving the machine is the classic exfil shape.",
		},
		{
			ID: "R-172", Category: CategoryExfil, Severity: SeverityCritical,
			AppliesTo: []EventKind{KindShellExec},
			Match:     nil, MatchCmd: matchSecretsInNetworkArgs,
			Observe: DecisionFlag, Enforce: DecisionDeny,
			Doc:    "secret-shaped value in the arguments of a network-touching command",
			Advice: "Pass credentials via files with restrictive permissions or environment configuration the command reads itself — never inline on a network command line.",
		},
		{
			// The §8.2 proxy half: the guard layer runs the typed
			// detectors over the final outbound request body and
			// stamps Event.Secrets; this row turns the findings into
			// the R-172 verdict (mask/deny resolution happens at the
			// proxy seam per [guard.proxy].egress_action).
			ID: "R-172", Category: CategoryExfil, Severity: SeverityCritical,
			AppliesTo: []EventKind{KindAPIRequest},
			Match:     matchSecretsOnAPIRequest,
			Observe:   DecisionFlag, Enforce: DecisionDeny,
			Doc:    "secret-shaped content in an outbound LLM API request",
			Advice: "Check what placed the secret in the conversation (a file read, a shell output); rotate it if it already left, and add an [guard.proxy] egress_allow pattern only for known-fake fixtures.",
		},
		{
			ID: "R-173", Category: CategoryExfil, Severity: SeverityWarn,
			AppliesTo: []EventKind{KindShellExec},
			Match:     nil, MatchCmd: matchDNSExfilShape,
			Observe: DecisionFlag, Enforce: DecisionFlag,
			Doc:    "DNS lookup of an encoded-looking subdomain (DNS-tunnel exfil shape)",
			Advice: "Long random-looking DNS labels are the classic low-bandwidth exfil channel; verify the domain is one your tooling legitimately queries.",
		},
	}
}

// matchRemoteCodePipe implements R-170 over three shapes (documented
// approximations — static analysis of substituted content is
// best-effort by design, gap F1):
//
//  1. `curl … | sh` / `irm … | iex`: a remote-fetch unit marked as a
//     pipeline segment followed by an interpreter pipeline segment at
//     the same unwrap depth.
//  2. `iex (irm …)`: an invoke-expression unit whose raw text carries
//     a fetch form, or with a fetch unit elsewhere in the same event
//     (PowerShell paren grouping does not lex into substitution
//     units, so the raw-text check is load-bearing).
//  3. `sh -c "$(curl …)"`: an interpreter unit with a -c/-Command
//     payload whose substitution produced a fetch unit one level
//     deeper.
func matchRemoteCodePipe(ctx *MatchContext) (bool, string) {
	cmds := ctx.Cmds
	// Shape 1: fetch piped into interpreter at the same depth.
	for i := range cmds {
		c := &cmds[i]
		if !remoteFetchBases[c.Base] || !c.Pipeline {
			continue
		}
		for j := i + 1; j < len(cmds); j++ {
			n := &cmds[j]
			if n.Depth == c.Depth && n.Pipeline && pipeInterpreterBases[n.Base] {
				return true, c.Base + " output piped into " + n.Base
			}
		}
	}
	for i := range cmds {
		c := &cmds[i]
		// Shape 2: invoke-expression over fetched content.
		if c.Base == "invoke-expression" {
			lr := strings.ToLower(c.Raw)
			if strings.Contains(lr, "irm ") || strings.Contains(lr, "iwr ") ||
				strings.Contains(lr, "invoke-restmethod") || strings.Contains(lr, "invoke-webrequest") ||
				strings.Contains(lr, "curl ") || strings.Contains(lr, "wget ") {
				return true, "invoke-expression over fetched content"
			}
			for j := range cmds {
				if j != i && remoteFetchBases[cmds[j].Base] {
					return true, "invoke-expression with a remote fetch in the same command"
				}
			}
		}
		// Shape 3: interpreter -c payload built from a substitution
		// that fetches.
		if pipeInterpreterBases[c.Base] && (c.HasShortFlag('c') || hasPSCommandFlag(c)) {
			for j := range cmds {
				if cmds[j].Depth == c.Depth+1 && remoteFetchBases[cmds[j].Base] {
					return true, c.Base + " -c executing substituted " + cmds[j].Base + " output"
				}
			}
		}
	}
	return false, ""
}

// hasPSCommandFlag reports a PowerShell-style -Command/-EncodedCommand
// parameter on the unit (case-insensitive — PS parameters are).
func hasPSCommandFlag(c *Command) bool {
	for _, t := range c.Argv[1:] {
		lt := strings.ToLower(t)
		if lt == "-command" || lt == "-encodedcommand" || lt == "-c" {
			return true
		}
	}
	return false
}

// uploadDataFlags are the curl-family flags whose @-prefixed value
// posts file contents (table-driven; extend here).
var uploadDataFlags = []string{"-d", "--data", "--data-binary", "--data-ascii", "--data-urlencode"}

// matchFileUpload implements R-171: curl-family upload forms plus
// scp/sftp/rsync with a remote destination.
//
// Documented approximations: the §5.3 "non-allowlisted domains"
// qualifier is narrowed to a localhost safe-pattern in v1 — a domain
// allowlist vocabulary joins when [guard.proxy] grows one; "scp/rsync
// to NEW hosts" needs host history the pure engine cannot hold, so
// every remote destination matches (observe-mode default keeps this a
// flag).
func matchFileUpload(ctx *MatchContext, cmd *Command) (bool, string) {
	switch cmd.Base {
	case "curl":
		if cmd.HasLongFlag("--upload-file") || cmd.HasShortFlag('T') {
			return true, "curl --upload-file"
		}
		for _, f := range uploadDataFlags {
			if v, ok := cmd.FlagValue(f); ok && strings.HasPrefix(v, "@") {
				return true, "curl " + f + " " + v
			}
		}
		if v, ok := cmd.FlagValue("-F", "--form"); ok && strings.Contains(v, "=@") {
			return true, "curl --form " + v
		}
	case "invoke-webrequest", "invoke-restmethod":
		// PS parameters are case-insensitive and not in the posix
		// flag tables; scan argv directly.
		for i := 1; i < len(cmd.Argv); i++ {
			if strings.EqualFold(cmd.Argv[i], "-infile") {
				return true, cmd.Base + " -InFile"
			}
		}
	case "scp", "sftp", "rsync":
		for _, p := range cmd.Positionals() {
			if isRemoteCopySpec(p) {
				return true, cmd.Base + " to " + p
			}
		}
	}
	return false, ""
}

// isRemoteCopySpec reports an scp/rsync remote-destination operand
// (user@host:path or host:path). Single-letter prefixes before ':'
// are treated as Windows drive paths, never hosts.
func isRemoteCopySpec(p string) bool {
	i := strings.IndexByte(p, ':')
	if i <= 1 {
		return false // no colon, or a drive letter ("C:\x")
	}
	host := p[:i]
	if strings.ContainsAny(host, "/\\") {
		return false // a path with a stray colon, not host:path
	}
	return true
}

// safeUploadLocalhostOnly exempts R-171 uploads whose URL targets are
// all loopback — posting a file to a local dev server is routine.
// Units with no URL-shaped argument are NOT exempt (scp/rsync forms
// carry host specs, not URLs).
func safeUploadLocalhostOnly(_ *MatchContext, cmd *Command) bool {
	if cmd == nil {
		return false
	}
	urls := 0
	for _, t := range cmd.Argv[1:] {
		lt := strings.ToLower(t)
		if !strings.HasPrefix(lt, "http://") && !strings.HasPrefix(lt, "https://") {
			continue
		}
		urls++
		if !isLoopbackURL(lt) {
			return false
		}
	}
	return urls > 0
}

// isLoopbackURL reports whether a lowercased URL targets loopback.
func isLoopbackURL(u string) bool {
	rest := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	host := rest
	if i := strings.IndexAny(rest, "/:?#"); i >= 0 {
		host = rest[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "[::1]", "::1", "0.0.0.0":
		return true
	}
	return false
}

// matchSecretsInNetworkArgs implements the R-172 shell-arg half: the
// injected typed detector (Config.SecretDetect) over every argument
// and heredoc of a network-touching command. Pattern-certain types
// only — the provider excludes entropy hits (random-looking tokens
// are routine in shell args).
func matchSecretsInNetworkArgs(ctx *MatchContext, cmd *Command) (bool, string) {
	if ctx.Cfg.SecretDetect == nil || !networkCommandBases[cmd.Base] {
		return false, ""
	}
	for _, a := range cmd.Argv[1:] {
		if types := ctx.Cfg.SecretDetect(a); len(types) > 0 {
			return true, "secret-shaped value (" + strings.Join(types, ", ") + ") in " + cmd.Base + " arguments"
		}
	}
	if cmd.Heredoc != "" {
		if types := ctx.Cfg.SecretDetect(cmd.Heredoc); len(types) > 0 {
			return true, "secret-shaped value (" + strings.Join(types, ", ") + ") in " + cmd.Base + " heredoc input"
		}
	}
	return false, ""
}

// matchSecretsOnAPIRequest implements the R-172 proxy half: the guard
// layer ran the typed detectors over the final outbound body and
// stamped the findings; any finding is a hit. The detail names types
// and counts only — never values.
func matchSecretsOnAPIRequest(ctx *MatchContext) (bool, string) {
	if len(ctx.Event.Secrets) == 0 {
		return false, ""
	}
	return true, "detected " + SummarizeSecretFindings(ctx.Event.Secrets) + " in the outbound request body"
}

// SummarizeSecretFindings renders findings as a stable, content-free
// "type×count" summary ("github_pat×2, entropy×1"), in first-seen
// type order. Exported for the guard layer's verdict reasons and the
// per-session dedup signature — both sides must render identically.
func SummarizeSecretFindings(findings []SecretFinding) string {
	var order []string
	counts := map[string]int{}
	for _, f := range findings {
		if counts[f.Type] == 0 {
			order = append(order, f.Type)
		}
		counts[f.Type]++
	}
	var b strings.Builder
	for i, ty := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(ty)
		if counts[ty] > 1 {
			b.WriteString("×")
			b.WriteString(strconv.Itoa(counts[ty]))
		}
	}
	return b.String()
}

// matchDNSExfilShape implements R-173: a DNS-lookup command whose
// queried name carries an encoded-looking leading label (≥20 chars of
// base64-ish alphabet with a digit or mixed case — plain English
// labels and PTR lookups stay quiet). Single-event analysis cannot
// see the §5.3 "in loops" qualifier; per-lookup shape is the v1
// approximation (severity warn, flag in both modes).
func matchDNSExfilShape(_ *MatchContext, cmd *Command) (bool, string) {
	if !dnsLookupBases[cmd.Base] {
		return false, ""
	}
	for _, p := range cmd.Positionals() {
		if !strings.Contains(p, ".") {
			continue
		}
		label := p[:strings.IndexByte(p, '.')]
		if encodedLabel(label) {
			return true, cmd.Base + " of encoded-looking label " + label + "."
		}
	}
	return false, ""
}

// encodedLabel grades one DNS label: ≥20 chars of [A-Za-z0-9_-] AND
// (a digit present, or both cases present). "internationalization"
// stays quiet; base64/hex payload labels do not.
func encodedLabel(label string) bool {
	if len(label) < 20 {
		return false
	}
	var hasDigit, hasUpper, hasLower bool
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return hasDigit || (hasUpper && hasLower)
}
