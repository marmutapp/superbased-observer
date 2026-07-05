package policy

import (
	"strconv"
	"strings"
)

// maxUnwrapDepth bounds recursive unwrapping of nested shell payloads
// (`bash -c "bash -c ..."`), command substitutions and dialect
// switches (spec §4.3: ≤5 levels). Payloads at the limit stay opaque
// on Command.Payload instead of being parsed further.
const maxUnwrapDepth = 5

// Command is one parsed command unit — the shape every rule matcher
// consumes, identical across dialects so rules express facts
// ("recursive delete") rather than syntax. A single Event commonly
// yields several Commands: separator-split units, pipeline segments,
// recursively unwrapped `-c` payloads and command substitutions all
// flatten into one list.
type Command struct {
	// Base is the canonical command name: directory prefix stripped
	// (both separator flavors), lowercased, known executable
	// extensions (.exe/.com/.bat/.cmd) stripped, and — in the
	// PowerShell dialect — aliases resolved to canonical cmdlet
	// names ("rm" → "remove-item"). Lowercasing is deliberately
	// conservative: a case-sensitive-FS binary named "RM" matches
	// the rm rules, which errs toward flagging.
	Base string
	// Argv is the unit's argument vector after wrapper stripping;
	// Argv[0] is the original (pre-canonicalization) command word.
	Argv []string
	// Raw is the unit's text, reconstructed by joining its words —
	// original quoting is not preserved (lexer strips it).
	Raw string
	// Dialect the unit was parsed under (payload units inherit the
	// dialect of their wrapper, e.g. `powershell -Command` payloads
	// are DialectPowerShell).
	Dialect Dialect
	// Depth is the unwrap depth the unit was found at (0 = the
	// event's own command line).
	Depth int
	// Sudo records that a sudo/doas wrapper was stripped — the
	// privilege escalation fact survives even though matching runs
	// against the inner command.
	Sudo bool
	// Wrappers lists every stripped wrapper in order ("sudo",
	// "xargs", ...) so rules can match chain shapes (R-103's
	// `xargs rm`).
	Wrappers []string
	// Pipeline is true when the unit is a segment of a multi-stage
	// pipeline (`a | b`).
	Pipeline bool
	// RedirectTargets are the output-redirection targets (`> file`,
	// `>> file`) — write-classified path candidates for the
	// boundary rules. Input redirections are consumed and dropped
	// (no current rule needs them).
	RedirectTargets []string
	// Heredoc is the concatenated heredoc body attached to the unit
	// (`psql <<EOF ... EOF`), scanned by R-120.
	Heredoc string
	// Payload is a nested code payload that was NOT unwrapped into
	// further Commands: interpreter one-liners (`python -c '...'`,
	// always opaque) and shell payloads at maxUnwrapDepth. Rules
	// scan it with regexes — best-effort by design (gap F1).
	Payload string
	// PayloadKind labels Payload: "python" | "node" | "perl" |
	// "ruby" | "shell" | "powershell" | "cmd".
	PayloadKind string
}

// HasLongFlag reports whether any argv token equals one of the given
// long flags exactly or carries it with an embedded value
// ("--signal=KILL"). Matching is exact per name — "--force" does NOT
// match "--force-with-lease".
func (c *Command) HasLongFlag(names ...string) bool {
	for _, t := range c.flagTokens() {
		for _, n := range names {
			if t == n || strings.HasPrefix(t, n+"=") {
				return true
			}
		}
	}
	return false
}

// HasShortFlag reports whether any single-dash cluster token contains
// one of the given flag letters (`-rf` has 'r' and 'f'). Letters are
// case-sensitive (`chmod -R` vs `rm -r` differ).
func (c *Command) HasShortFlag(letters ...byte) bool {
	for _, t := range c.flagTokens() {
		if len(t) < 2 || t[0] != '-' || t[1] == '-' {
			continue
		}
		for _, l := range letters {
			if strings.IndexByte(t[1:], l) >= 0 {
				return true
			}
		}
	}
	return false
}

// flagTokens returns the argv tokens in flag position: everything
// after Argv[0] up to a literal "--" terminator.
func (c *Command) flagTokens() []string {
	for i := 1; i < len(c.Argv); i++ {
		if c.Argv[i] == "--" {
			return c.Argv[1:i]
		}
	}
	if len(c.Argv) <= 1 {
		return nil
	}
	return c.Argv[1:]
}

// valueFlags lists, per canonical base, the flags known to consume a
// following value, so Positionals doesn't mistake the value for an
// operand ("git -C /x push" — /x is -C's value, not a subcommand).
// The table is best-effort: an unlisted value-flag makes its value
// appear as a positional, which errs toward analyzing MORE (a
// documented approximation, pinned by tests).
var valueFlags = map[string]map[string]bool{
	"git":     setOf("-C", "-c", "--work-tree", "--git-dir", "--namespace"),
	"aws":     setOf("--profile", "--region", "--output", "--endpoint-url"),
	"kubectl": setOf("-n", "--namespace", "--context", "--kubeconfig", "-l", "--selector", "-f", "--filename", "-o"),
	"helm":    setOf("-n", "--namespace", "--kube-context", "-f", "--values"),
	"psql":    setOf("-h", "-p", "-U", "-d", "--host", "--port", "--username", "--dbname"),
	"mysql":   setOf("-h", "-P", "-u", "-D", "--host", "--port", "--user", "--database"),
	"mariadb": setOf("-h", "-P", "-u", "-D", "--host", "--port", "--user", "--database"),
	"find":    setOf("-name", "-iname", "-path", "-ipath", "-type", "-maxdepth", "-mindepth", "-newer", "-mtime", "-mmin", "-size", "-perm", "-user", "-group"),
	"chown":   setOf("--reference", "--from"),
}

// setOf builds a string set from its arguments.
func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// Positionals returns the unit's non-flag operands (subcommands,
// targets), honoring "--" termination, dialect flag prefixes ("-" and
// additionally "/" for cmd) and the per-base valueFlags table. In the
// PowerShell dialect parameter VALUES are intentionally kept ("-Path
// C:\x" — the value IS the target of interest).
func (c *Command) Positionals() []string {
	vf := valueFlags[c.Base]
	var out []string
	rest := false
	for i := 1; i < len(c.Argv); i++ {
		t := c.Argv[i]
		if rest {
			out = append(out, t)
			continue
		}
		if t == "--" {
			rest = true
			continue
		}
		if c.isFlagToken(t) {
			// An "=" form ("--name=value") embeds its value; only
			// the separate form consumes the next token.
			if !strings.ContainsRune(t, '=') && vf[t] && i+1 < len(c.Argv) {
				i++ // skip the flag's value
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// isFlagToken reports whether t is flag-shaped in the unit's dialect.
func (c *Command) isFlagToken(t string) bool {
	if len(t) < 2 {
		return false
	}
	if t[0] == '-' {
		return true
	}
	return c.Dialect == DialectCmd && t[0] == '/'
}

// Sub returns the unit's first positional operand — the subcommand
// for multi-command CLIs ("push" in `git push -f origin main`).
func (c *Command) Sub() string {
	p := c.Positionals()
	if len(p) == 0 {
		return ""
	}
	return p[0]
}

// FlagValue returns the value of the first matching flag, supporting
// both the separate form ("-c VALUE") and the embedded form
// ("--command=VALUE").
func (c *Command) FlagValue(names ...string) (string, bool) {
	for i := 1; i < len(c.Argv); i++ {
		t := c.Argv[i]
		for _, n := range names {
			if t == n && i+1 < len(c.Argv) {
				return c.Argv[i+1], true
			}
			if strings.HasPrefix(t, n+"=") {
				return t[len(n)+1:], true
			}
		}
	}
	return "", false
}

// ParseCommand parses a raw command line in the given dialect into
// the flattened, recursively-unwrapped command-unit list. An empty
// dialect means DialectPosix. Parsing is pure analysis: nothing is
// expanded against an environment or executed.
func ParseCommand(raw string, dialect Dialect) []Command {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return parseDepth(raw, dialect, 0)
}

// ParseArgv parses a pre-split argument vector (boundaries that
// receive argv rather than a command string) through the same
// wrapper-strip / unwrap pipeline as ParseCommand. Command
// substitutions embedded in argv words are stripped and parsed as
// additional units, mirroring parseDepth.
func ParseArgv(argv []string, dialect Dialect) []Command {
	if len(argv) == 0 {
		return nil
	}
	var subs []string
	words := make([]string, 0, len(argv))
	for _, w := range argv {
		if dialect != DialectCmd {
			cw, s := stripSubstitutions(w)
			subs = append(subs, s...)
			w = cw
		}
		if strings.TrimSpace(w) != "" || len(words) > 0 {
			words = append(words, w)
		}
	}
	var out []Command
	processUnit(unit{words: words}, dialect, 0, &out)
	for _, sub := range subs {
		out = append(out, parseDepth(sub, dialect, 1)...)
	}
	return out
}

// unit is one separator-delimited command segment as produced by the
// lexers, before wrapper stripping and unwrapping.
type unit struct {
	words     []string
	redirects []string
	heredoc   string
	pipeline  bool
}

// parseDepth lexes raw under the dialect's rules and processes each
// unit, recursing (bounded by maxUnwrapDepth) through nested
// payloads. Command substitutions ("$(...)", backticks) are stripped
// from the line BEFORE lexing — a substitution can span word
// boundaries, so per-word handling would corrupt unit splitting —
// and their contents parse as additional units. The substituted-away
// value is unknowable statically, so a target like `rm -rf $(pwd)`
// analyzes as a target-less rm plus a separate pwd unit (documented
// approximation).
func parseDepth(raw string, dialect Dialect, depth int) []Command {
	var subs []string
	if dialect != DialectCmd && depth < maxUnwrapDepth {
		raw, subs = stripSubstitutions(raw)
	}
	var units []unit
	switch dialect {
	case DialectPowerShell:
		units = buildUnits(lexLine(raw, psLexSpec), nil)
	case DialectCmd:
		units = buildUnits(lexLine(raw, cmdLexSpec), nil)
	default:
		clean, bodies := extractHeredocs(raw)
		units = buildUnits(lexLine(clean, posixLexSpec), bodies)
	}
	var out []Command
	for _, u := range units {
		processUnit(u, dialect, depth, &out)
	}
	for _, sub := range subs {
		out = append(out, parseDepth(sub, dialect, depth+1)...)
	}
	return out
}

// processUnit turns one lexed unit into a Command (plus any nested
// Commands its payloads unwrap into) and appends them to out.
func processUnit(u unit, dialect Dialect, depth int, out *[]Command) {
	words := stripAssignments(u.words)
	if len(words) == 0 {
		return
	}
	stripped, wrappers, sudo := stripWrappers(words, dialect)
	if len(stripped) == 0 {
		// A bare wrapper ("sudo" with nothing after it) — keep the
		// original words so the unit is still visible.
		stripped, wrappers, sudo = words, nil, false
	}
	raw := strings.Join(u.words, " ")
	cmd := Command{
		Base:            canonicalBase(stripped[0], dialect),
		Argv:            stripped,
		Raw:             raw,
		Dialect:         dialect,
		Depth:           depth,
		Sudo:            sudo,
		Wrappers:        wrappers,
		Pipeline:        u.pipeline,
		RedirectTargets: u.redirects,
		Heredoc:         u.heredoc,
	}
	nested := analyzeNested(&cmd, depth)
	*out = append(*out, cmd)
	*out = append(*out, nested...)
}

// stripAssignments drops leading VAR=value environment assignments
// (`FOO=1 cmd ...` — POSIX prefix assignments).
func stripAssignments(words []string) []string {
	i := 0
	for i < len(words) && isEnvAssign(words[i]) {
		i++
	}
	return words[i:]
}

// isEnvAssign reports whether w is an environment-assignment word
// (NAME=value with a valid identifier name).
func isEnvAssign(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := w[i]
		if !(c == '_' || isASCIILetter(c) || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// wrapperSpec describes how one wrapper command consumes its own
// arguments before the wrapped command begins.
type wrapperSpec struct {
	// argFlags are wrapper flags that consume a following value
	// ("sudo -u root").
	argFlags map[string]bool
	// leadingOperand marks wrappers whose first positional is their
	// own operand, not the wrapped command ("timeout 5 cmd").
	leadingOperand bool
	// assignments marks wrappers that consume VAR=value operands
	// ("env FOO=1 cmd").
	assignments bool
}

// posixWrappers is the table of wrappers stripped before matching
// (spec §4.3). sudo/doas additionally set Command.Sudo. eval/exec are
// handled separately: exec strips like a wrapper; eval re-parses its
// arguments as a command line (see analyzeNested).
var posixWrappers = map[string]wrapperSpec{
	"sudo":    {argFlags: setOf("-u", "-g", "-p", "-C", "-D", "-R", "-T", "-h", "--user", "--group")},
	"doas":    {argFlags: setOf("-u")},
	"env":     {assignments: true, argFlags: setOf("-u", "-S", "-C", "-P", "--unset", "--split-string", "--chdir")},
	"timeout": {leadingOperand: true, argFlags: setOf("-k", "-s", "--kill-after", "--signal")},
	"nice":    {argFlags: setOf("-n", "--adjustment")},
	"nohup":   {},
	"time":    {},
	"stdbuf":  {argFlags: setOf("-i", "-o", "-e")},
	"command": {},
	"builtin": {},
	"exec":    {},
	"xargs": {argFlags: setOf("-I", "-n", "-P", "-d", "-a", "-E", "-s", "-L", "-i",
		"--max-args", "--max-procs", "--delimiter", "--arg-file", "--max-chars", "--max-lines", "--replace")},
}

// stripWrappers iteratively removes leading wrapper commands,
// returning the remaining argv, the stripped wrapper names in order,
// and whether a privilege-escalation wrapper (sudo/doas) was present.
// The cmd dialect has no wrapper convention this models.
func stripWrappers(words []string, dialect Dialect) (rest, wrappers []string, sudo bool) {
	if dialect == DialectCmd {
		return words, nil, false
	}
	for len(words) > 0 {
		base := canonicalBase(words[0], dialect)
		spec, ok := posixWrappers[base]
		if !ok {
			break
		}
		wrappers = append(wrappers, base)
		if base == "sudo" || base == "doas" {
			sudo = true
		}
		words = consumeWrapperArgs(words[1:], spec)
	}
	return words, wrappers, sudo
}

// consumeWrapperArgs skips a wrapper's own flags/operands and returns
// the argv tail where the wrapped command begins.
func consumeWrapperArgs(rest []string, spec wrapperSpec) []string {
	tookOperand := false
	for len(rest) > 0 {
		w := rest[0]
		switch {
		case len(w) > 1 && w[0] == '-':
			name, hasEq := w, false
			if i := strings.IndexByte(w, '='); i >= 0 {
				name, hasEq = w[:i], true
			}
			if spec.argFlags[name] && !hasEq && len(rest) > 1 {
				rest = rest[2:]
			} else {
				rest = rest[1:]
			}
		case spec.assignments && isEnvAssign(w):
			rest = rest[1:]
		case spec.leadingOperand && !tookOperand:
			tookOperand = true
			rest = rest[1:]
		default:
			return rest
		}
	}
	return rest
}

// canonicalBase derives Command.Base from a command word: directory
// prefix stripped, lowercased, known executable extensions stripped,
// PowerShell aliases resolved within that dialect.
func canonicalBase(word string, dialect Dialect) string {
	b := word
	if i := strings.LastIndexAny(b, `/\`); i >= 0 {
		b = b[i+1:]
	}
	b = strings.ToLower(b)
	for _, ext := range [...]string{".exe", ".com", ".bat", ".cmd"} {
		b = strings.TrimSuffix(b, ext)
	}
	if dialect == DialectPowerShell {
		if canon, ok := psAliases[b]; ok {
			return canon
		}
	}
	return b
}

// posixShellBases are the shells whose `-c` payloads unwrap
// recursively as POSIX command lines.
var posixShellBases = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "fish": true,
}

// interpreterSpec describes an interpreter whose one-liner payload is
// extracted opaquely (never parsed as shell — gap F1: we expose the
// payload to regex rules, we do not pretend to understand it).
type interpreterSpec struct {
	flags []string
	kind  string
}

// interpreters maps interpreter bases to their one-liner flags.
var interpreters = map[string]interpreterSpec{
	"python":  {flags: []string{"-c"}, kind: "python"},
	"python2": {flags: []string{"-c"}, kind: "python"},
	"python3": {flags: []string{"-c"}, kind: "python"},
	"py":      {flags: []string{"-c"}, kind: "python"},
	"node":    {flags: []string{"-e", "--eval", "-p", "--print"}, kind: "node"},
	"nodejs":  {flags: []string{"-e", "--eval", "-p", "--print"}, kind: "node"},
	"perl":    {flags: []string{"-e", "-E"}, kind: "perl"},
	"ruby":    {flags: []string{"-e"}, kind: "ruby"},
}

// analyzeNested inspects a freshly-built Command for nested code:
// shell `-c` payloads (recursively parsed up to maxUnwrapDepth),
// PowerShell -Command/-EncodedCommand and `cmd /c` payloads (parsed
// under their own dialect), eval re-parsing, and interpreter
// one-liners (kept opaque on Payload). It may set Payload/PayloadKind
// on cmd and returns the unwrapped nested Commands.
func analyzeNested(cmd *Command, depth int) []Command {
	switch {
	case posixShellBases[cmd.Base]:
		payload, ok := dashCPayload(cmd.Argv)
		if !ok {
			return nil
		}
		return unwrapPayload(cmd, payload, DialectPosix, "shell", depth)
	case cmd.Base == "eval":
		if len(cmd.Argv) < 2 {
			return nil
		}
		return unwrapPayload(cmd, strings.Join(cmd.Argv[1:], " "), cmd.Dialect, "shell", depth)
	case cmd.Base == "powershell" || cmd.Base == "pwsh":
		payload, ok := psCommandPayload(cmd.Argv)
		if !ok {
			return nil
		}
		return unwrapPayload(cmd, payload, DialectPowerShell, "powershell", depth)
	case cmd.Base == "cmd":
		payload, ok := cmdSlashCPayload(cmd.Argv)
		if !ok {
			return nil
		}
		return unwrapPayload(cmd, payload, DialectCmd, "cmd", depth)
	default:
		if spec, ok := interpreters[cmd.Base]; ok {
			if p, found := cmd.FlagValue(spec.flags...); found {
				cmd.Payload, cmd.PayloadKind = p, spec.kind
			}
		}
		return nil
	}
}

// unwrapPayload either recursively parses a nested payload under the
// given dialect or — at the depth limit — records it opaquely on the
// wrapper Command.
func unwrapPayload(cmd *Command, payload string, dialect Dialect, kind string, depth int) []Command {
	if depth >= maxUnwrapDepth {
		cmd.Payload, cmd.PayloadKind = payload, kind
		return nil
	}
	return parseDepth(payload, dialect, depth+1)
}

// dashCPayload finds a POSIX shell's -c payload, accepting combined
// flag clusters ("-lc", "-xec"). It stops at the first operand (a
// script file we cannot read — pure package) or a "--" terminator.
func dashCPayload(argv []string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		t := argv[i]
		if t == "--" || !strings.HasPrefix(t, "-") {
			return "", false
		}
		if strings.HasPrefix(t, "--") {
			continue
		}
		if strings.ContainsRune(t[1:], 'c') && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// extractHeredocs scans a POSIX command line for heredoc
// redirections (<<DELIM, <<-DELIM, quoted delimiters), removes each
// body and its terminator line from the returned string, and replaces
// the redirection with an internal marker word that buildUnits maps
// back to the body. Quoted regions are respected. Limitations
// (documented, pinned by tests): one heredoc per line; an
// unterminated heredoc consumes the rest of the input as its body.
func extractHeredocs(s string) (string, []string) {
	if !strings.Contains(s, "<<") {
		return s, nil
	}
	var out strings.Builder
	var bodies []string
	skips := map[int]int{} // newline index → resume index past the body
	inS, inD := false, false
	i := 0
	for i < len(s) {
		c := s[i]
		if to, ok := skips[i]; ok && c == '\n' {
			out.WriteByte('\n')
			i = to
			continue
		}
		switch {
		case inS:
			out.WriteByte(c)
			if c == '\'' {
				inS = false
			}
			i++
		case inD:
			if c == '\\' && i+1 < len(s) {
				out.WriteByte(c)
				out.WriteByte(s[i+1])
				i += 2
				continue
			}
			out.WriteByte(c)
			if c == '"' {
				inD = false
			}
			i++
		case c == '\'':
			inS = true
			out.WriteByte(c)
			i++
		case c == '"':
			inD = true
			out.WriteByte(c)
			i++
		case c == '\\':
			out.WriteByte(c)
			if i+1 < len(s) {
				out.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case c == '<' && i+1 < len(s) && s[i+1] == '<' && (i+2 >= len(s) || s[i+2] != '<'):
			adv, ok := scanHeredoc(s, i, &bodies, skips, &out)
			if !ok {
				out.WriteString("<<")
				i += 2
				continue
			}
			i = adv
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), bodies
}

// scanHeredoc parses one heredoc redirection starting at s[i] ("<<"),
// records its body and skip range, writes the marker word, and
// returns the index to resume lexing at. ok is false when the text
// after "<<" is not a valid heredoc introducer.
func scanHeredoc(s string, i int, bodies *[]string, skips map[int]int, out *strings.Builder) (int, bool) {
	j := i + 2
	if j < len(s) && s[j] == '-' {
		j++
	}
	var q byte
	if j < len(s) && (s[j] == '\'' || s[j] == '"') {
		q = s[j]
		j++
	}
	st := j
	for j < len(s) && isWordChar(s[j]) {
		j++
	}
	delim := s[st:j]
	if q != 0 {
		if j < len(s) && s[j] == q {
			j++
		} else {
			delim = ""
		}
	}
	if delim == "" {
		return 0, false
	}
	marker := " \x00hd" + strconv.Itoa(len(*bodies)) + "\x00 "
	nl := strings.IndexByte(s[j:], '\n')
	if nl < 0 {
		// Declared but no body present in this input.
		*bodies = append(*bodies, "")
		out.WriteString(marker)
		return j, true
	}
	newlineAt := j + nl
	if _, exists := skips[newlineAt]; exists {
		// Second heredoc on one line — unsupported, treat literally.
		return 0, false
	}
	bodyStart := newlineAt + 1
	bodyLen, total := findHeredocEnd(s[bodyStart:], delim)
	*bodies = append(*bodies, s[bodyStart:bodyStart+bodyLen])
	skips[newlineAt] = bodyStart + total
	out.WriteString(marker)
	return j, true
}

// findHeredocEnd locates the terminator line within body text,
// returning the body length (up to the terminator line) and the total
// length consumed (past the terminator line). Leading tabs on the
// terminator are tolerated (<<- semantics). An unterminated heredoc
// returns the whole text as body.
func findHeredocEnd(text, delim string) (bodyLen, total int) {
	off := 0
	for off <= len(text) {
		end := strings.IndexByte(text[off:], '\n')
		var line string
		var next int
		if end < 0 {
			line, next = text[off:], len(text)
		} else {
			line, next = text[off:off+end], off+end+1
		}
		if line == delim || strings.TrimLeft(line, "\t") == delim {
			return off, next
		}
		if end < 0 {
			break
		}
		off = next
	}
	return len(text), len(text)
}

// isWordChar reports whether c is a heredoc-delimiter word character.
func isWordChar(c byte) bool {
	return c == '_' || isASCIILetter(c) || (c >= '0' && c <= '9')
}

// heredocIndex recognizes the internal heredoc marker words emitted
// by scanHeredoc.
func heredocIndex(w string) (int, bool) {
	if !strings.HasPrefix(w, "\x00hd") || !strings.HasSuffix(w, "\x00") || len(w) < 5 {
		return 0, false
	}
	n, err := strconv.Atoi(w[3 : len(w)-1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ptoken is one lexer output token: a word, or a separator/redirect
// operator.
type ptoken struct {
	text string
	op   bool
}

// lexSpec parametrizes lexLine per dialect.
type lexSpec struct {
	// escape is the dialect's escape character (POSIX '\\',
	// PowerShell '`', cmd '^'); 0 disables escaping.
	escape byte
	// singleQuote enables '...'-style literal quoting.
	singleQuote bool
	// semiSep treats ';' as a command separator (POSIX,
	// PowerShell; cmd uses '&').
	semiSep bool
	// stopParse honors PowerShell's --% stop-parsing token.
	stopParse bool
	// dquoteEscapeSet, when non-empty, restricts which characters
	// the escape char escapes INSIDE double quotes (POSIX: only
	// \" \\ \$ \` — otherwise the backslash is literal).
	dquoteEscapeSet string
}

// Per-dialect lexer parameters.
var (
	posixLexSpec = lexSpec{escape: '\\', singleQuote: true, semiSep: true, dquoteEscapeSet: "\"\\$`"}
	psLexSpec    = lexSpec{escape: '`', singleQuote: true, semiSep: true, stopParse: true}
	cmdLexSpec   = lexSpec{escape: '^', singleQuote: false, semiSep: false}
)

// lexLine tokenizes one command line under the dialect spec into
// words and operator tokens. Operators emitted: ";" (unit separator —
// also used for newlines, "&", "&&", "||" and unquoted parens), "|"
// (pipe), ">" (output redirect whose target is the next word), ">fd"
// (fd-duplication redirect, no target word), "<" (input redirect,
// target dropped by buildUnits).
func lexLine(s string, spec lexSpec) []ptoken {
	lx := &lexer{src: s, spec: spec}
	lx.run()
	return lx.toks
}

// lexer carries lexLine's scanning state.
type lexer struct {
	src   string
	spec  lexSpec
	toks  []ptoken
	cur   strings.Builder
	has   bool
	inS   bool
	inD   bool
	digit bool // current word is all digits so far (fd-prefix detection)
	i     int
}

// flush emits the current word (if any) as a token, honoring the
// PowerShell stop-parsing token.
func (lx *lexer) flush() {
	if !lx.has && lx.cur.Len() == 0 {
		return
	}
	w := lx.cur.String()
	lx.cur.Reset()
	lx.has = false
	lx.digit = false
	if lx.spec.stopParse && w == "--%" {
		// Everything after --% is literal words.
		for _, f := range strings.Fields(lx.src[lx.i:]) {
			lx.toks = append(lx.toks, ptoken{text: f})
		}
		lx.i = len(lx.src)
		return
	}
	lx.toks = append(lx.toks, ptoken{text: w})
}

// write appends one byte to the current word.
func (lx *lexer) write(c byte) {
	if lx.cur.Len() == 0 {
		lx.digit = c >= '0' && c <= '9'
	} else if lx.digit {
		lx.digit = c >= '0' && c <= '9'
	}
	lx.cur.WriteByte(c)
	lx.has = true
}

// op flushes the current word and emits an operator token.
func (lx *lexer) op(text string) {
	lx.flush()
	lx.toks = append(lx.toks, ptoken{text: text, op: true})
}

// run is the scanning loop.
func (lx *lexer) run() {
	s, spec := lx.src, lx.spec
	for lx.i < len(s) {
		c := s[lx.i]
		switch {
		case lx.inS:
			if c == '\'' {
				lx.inS = false
			} else {
				lx.write(c)
			}
			lx.i++
		case lx.inD:
			lx.scanDouble(c)
		case spec.singleQuote && c == '\'':
			lx.inS, lx.has = true, true
			lx.i++
		case c == '"':
			lx.inD, lx.has = true, true
			lx.i++
		case spec.escape != 0 && c == spec.escape:
			if lx.i+1 < len(s) {
				lx.write(s[lx.i+1])
				lx.i += 2
			} else {
				lx.i++
			}
		case c == ' ' || c == '\t' || c == '\r':
			lx.flush()
			lx.i++
		case c == '\n' || (spec.semiSep && c == ';'):
			lx.op(";")
			lx.i++
		case c == '&':
			lx.scanAmp()
		case c == '|':
			if lx.i+1 < len(s) && s[lx.i+1] == '|' {
				lx.op(";")
				lx.i += 2
			} else {
				lx.op("|")
				lx.i++
			}
		case c == '>':
			lx.scanRedirect()
		case c == '<':
			lx.op("<")
			lx.i++
		case c == '(' && lx.cur.Len() == 0:
			// A subshell/group opener at word start acts as a unit
			// separator; inside a word ("$(...)"), it is literal
			// and handled by extractSubstitutions.
			lx.op(";")
			lx.i++
		case c == ')' && !strings.Contains(lx.cur.String(), "("):
			// A closer is a separator unless it closes a
			// substitution opened within the current word
			// ("$(pwd)") — then it is literal.
			lx.op(";")
			lx.i++
		default:
			lx.write(c)
			lx.i++
		}
	}
	lx.flush()
}

// scanDouble handles one byte inside double quotes.
func (lx *lexer) scanDouble(c byte) {
	s, spec := lx.src, lx.spec
	if c == '"' {
		lx.inD = false
		lx.i++
		return
	}
	if spec.escape != 0 && c == spec.escape && lx.i+1 < len(s) {
		n := s[lx.i+1]
		if spec.dquoteEscapeSet == "" || strings.IndexByte(spec.dquoteEscapeSet, n) >= 0 {
			lx.write(n)
			lx.i += 2
			return
		}
	}
	lx.write(c)
	lx.i++
}

// scanAmp handles '&': "&&" separator, "&>" redirect, bare "&"
// separator (background / cmd separator / PS call operator — all
// reduce to a unit boundary; a leading PS call operator simply
// flushes an empty unit, which is a no-op).
func (lx *lexer) scanAmp() {
	s := lx.src
	switch {
	case lx.i+1 < len(s) && s[lx.i+1] == '&':
		lx.op(";")
		lx.i += 2
	case lx.i+1 < len(s) && s[lx.i+1] == '>':
		lx.i += 2
		lx.op(">")
	default:
		lx.op(";")
		lx.i++
	}
}

// scanRedirect handles '>': ">>", ">", and fd-duplication forms
// (">&2"), dropping a pure-digit fd prefix ("2>") from the pending
// word.
func (lx *lexer) scanRedirect() {
	s := lx.src
	if lx.digit && lx.cur.Len() > 0 {
		// "2>"-style fd prefix: the digits are not a word.
		lx.cur.Reset()
		lx.has = false
		lx.digit = false
	}
	lx.i++
	if lx.i < len(s) && s[lx.i] == '>' {
		lx.i++
	}
	if lx.i < len(s) && s[lx.i] == '&' {
		lx.i++
		for lx.i < len(s) && s[lx.i] >= '0' && s[lx.i] <= '9' {
			lx.i++
		}
		lx.op(">fd")
		return
	}
	lx.op(">")
}

// buildUnits groups lexer tokens into units, wiring redirect targets,
// heredoc bodies and pipeline membership.
func buildUnits(toks []ptoken, bodies []string) []unit {
	var units []unit
	var cur unit
	pipeNext := false
	redirNext := false
	dropNext := false
	flush := func() {
		if len(cur.words) > 0 || cur.heredoc != "" || len(cur.redirects) > 0 {
			cur.pipeline = cur.pipeline || pipeNext
			units = append(units, cur)
		}
		cur = unit{}
		redirNext, dropNext = false, false
	}
	for _, t := range toks {
		if t.op {
			switch t.text {
			case ";":
				flush()
				pipeNext = false
			case "|":
				cur.pipeline = true
				flush()
				pipeNext = true
			case ">":
				redirNext = true
			case ">fd":
				// fd duplication — no target word follows.
			case "<":
				dropNext = true
			}
			continue
		}
		if n, ok := heredocIndex(t.text); ok {
			if n < len(bodies) {
				cur.heredoc += bodies[n]
			}
			continue
		}
		switch {
		case redirNext:
			cur.redirects = append(cur.redirects, t.text)
			redirNext = false
		case dropNext:
			dropNext = false // input-redirect source: dropped (no rule consumes it)
		default:
			cur.words = append(cur.words, t.text)
		}
	}
	flush()
	return units
}

// stripSubstitutions removes command substitutions — "$( ... )"
// (nesting-aware) and backtick spans — outside single-quoted regions
// and returns the cleaned line plus the extracted inner texts. POSIX
// expands substitutions inside double quotes, so those are included.
// The caller parses each extracted text as additional command units;
// the substituted-away value itself is statically unknowable, so the
// cleaned line simply omits it.
func stripSubstitutions(s string) (string, []string) {
	if !strings.ContainsAny(s, "`$") {
		return s, nil
	}
	var out strings.Builder
	var subs []string
	inS := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case inS:
			out.WriteByte(c)
			if c == '\'' {
				inS = false
			}
			i++
		case c == '\'':
			inS = true
			out.WriteByte(c)
			i++
		case c == '\\':
			out.WriteByte(c)
			if i+1 < len(s) {
				out.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				out.WriteByte(c)
				i++
				continue
			}
			subs = append(subs, s[i+1:i+1+j])
			i = i + 1 + j + 1
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				switch s[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				j++
			}
			if depth != 0 {
				out.WriteByte(c)
				i++
				continue
			}
			subs = append(subs, s[i+2:j-1])
			i = j
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), subs
}
