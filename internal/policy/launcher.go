package policy

import "strings"

// Launcher wrappers — commands whose JOB is to start ANOTHER program,
// with that program (and its arguments) recoverable from the
// launcher's own argv.
//
// WHY THIS FILE EXISTS. R-155 (persistence installs: crontab,
// `schtasks /Create`, Run-key writes, `systemctl enable`) matched on
// Command.Base. `Start-Process schtasks.exe -ArgumentList
// '/Create',…` parses as base `start-process` with `schtasks.exe` in a
// POSITIONAL slot, so nothing fired and the command was ALLOWED in
// ModeEnforce, in all three dialects. Same for `wsl.exe -- schtasks
// /Create …` and cmd's `start schtasks /Create …`. Measured
// 2026-07-27 against the real engine; see docs/security.md H5.
//
// THE DESIGN RULE, and the reason this is a parse-layer file rather
// than an R-155 special case: a launcher is resolved into the inner
// command ONCE, here, and the inner command is then handed to the
// ordinary matcher pipeline as a first-class Command. R-155 keeps
// exactly one implementation of "what persistence looks like"
// (CLAUDE.md rule 4: one owner per piece of state), and EVERY other
// rule — destructive, exfil, taint — gets the same coverage for free.
// A wrapper-shaped special case inside one matcher would have to be
// written again for the next rule, and the §W4.6 record is that
// wrapper shapes are exactly what the R-155 bypasses used.
//
// Resolution is table-driven (CLAUDE.md rule 5): one row per launcher,
// each row a resolver from the launcher's argv to the launched argv.
// Nothing here decides anything about policy.

// THE NEVER-SUPPRESS RULE (docs/security.md H5, ROUND 2).
//
// The paragraph above says "ambiguity resolves toward analyzing more,
// never guessing". The first version of this file violated that in
// exactly ONE place and shipped a critical bypass there: a boolean
// (`bound`) decided that because SOME argv token resolved as a
// Start-Process parameter, the cmd-START readings were not worth
// generating. That single suppression made
//
//	start powershell -c "schtasks /Create /TN evil /TR C:\evil.exe"
//	start /b crontab -e
//	start sh -c "crontab -e"
//
// ALLOW in ModeEnforce in all three dialects, while the `-Command`,
// `-enc` and dash-free spellings of the same lines denied. Adding an
// argument to a denied command made it allowed — the signature of a
// reading being deleted rather than a rule being missed.
//
// So: a resolver NEVER chooses between readings. Every reading the
// argv admits is emitted, deduped, and analysed. That covers three
// distinct guesses this file used to make silently, each now a
// generated alternative instead:
//
//   - WHICH GRAMMAR the word names — cmd.exe's START builtin or the
//     Start-Process cmdlet. Both, always.
//   - WHETHER A PARAMETER TOKEN IS THE LAUNCHER'S. `-c` prefixes
//     -Credential, `-e` prefixes -Environment/-ErrorAction — but they
//     are also how `powershell` and `crontab` spell their OWN flags.
//     Reading them as the launcher's DELETES the launched program's
//     arguments (psCollectValue eats the next token), which is how
//     `-c "<payload>"` vanished above.
//   - WHERE A LEXED VALUE ENDS. psCollectValue re-joins comma-run
//     tokens; a directory literally named `C:\temp,` therefore ate the
//     program that followed it, and `Start-Process -WorkingDirectory
//     'C:\temp,' schtasks.exe -ArgumentList …` ALLOWED.
//
// A reading that names an argument rather than a program matches no
// rule and costs a little analysis. A suppressed reading costs a
// bypass. When even the readings run out — no candidate, the depth
// limit, the fan-out bound, the expansion budget — the unit is marked
// Command.Unanalyzed and R-157 denies. Nothing here returns "nothing
// to see" for a wrapper it could not read.

// launchResult is what a launcher row's resolver returns.
type launchResult struct {
	// candidates is EVERY argv the launched command could plausibly
	// be (program first), before dedup. See the never-suppress rule
	// above: a resolver adds readings, it never picks between them.
	candidates [][]string
	// named reports that the invocation NAMED something to launch.
	//
	// `named` with an EMPTY candidate list is a PARSE FAILURE on a
	// known-dangerous wrapper, and it is recorded as such
	// (Command.Unanalyzed → R-157) instead of the old silent
	// `return nil`. It is distinct from "there was nothing to
	// launch" — an interactive `wsl`, `wsl -l -v`, `start` with only
	// switches — which is not a failure and must not deny.
	named bool
}

// launcherSpec is one row of the launcher table.
type launcherSpec struct {
	// name is the wrapper name recorded on the inner Command's
	// Wrappers, so a rule (and the R-155 ETW carve-out) can tell a
	// launched command from a directly-typed one.
	name string
	// resolve maps the launcher's own argv to every argv the launched
	// command could plausibly be. See launchResult and the
	// never-suppress rule above.
	resolve func(argv []string) launchResult
	// inner is the dialect the launched command runs under. Empty
	// inherits the launcher's own dialect.
	inner Dialect
}

// launchers is the launcher table, keyed by canonical base.
//
// `saps` and `start` are listed alongside `start-process` because
// psAliases only fires in the PowerShell dialect, and an event may
// carry a Windows command line under a POSIX/cmd dialect tag —
// matching it there errs toward analyzing more, the same way
// canonicalBase's unconditional lowercasing does. All three share one
// resolver, which covers BOTH things the word `start` can name (the
// cmdlet and cmd.exe's builtin); see startProcessCandidates.
// `runas` is Windows' own privilege launcher and is here rather than
// in the wrapper tables because its command is ONE quoted operand
// (`runas /user:admin "crontab -e"`), not an argv tail. Adding
// sudo/doas to cmdWrappers closed a dialect asymmetry; leaving out
// their native Windows peer left the same hole under a different
// spelling — `runas /user:admin "crontab -e"` ALLOWED in all three
// dialects (docs/security.md H5 round 2).
var launchers = map[string]launcherSpec{
	"start-process": {name: "start-process", resolve: startProcessCandidates},
	"saps":          {name: "start-process", resolve: startProcessCandidates},
	"start":         {name: "start-process", resolve: startProcessCandidates},
	"wsl":           {name: "wsl", resolve: wslInnerArgv, inner: DialectPosix},
	"runas":         {name: "runas", resolve: runasCandidates},
}

// launcherWrapperNames is the set of names launchers records on
// Command.Wrappers, derived from the table so the two can never drift.
var launcherWrapperNames = func() map[string]bool {
	m := make(map[string]bool, len(launchers))
	for _, spec := range launchers {
		m[spec.name] = true
	}
	return m
}()

// viaLauncher reports whether the unit was reached by unwrapping a
// launcher wrapper rather than being invoked directly.
func viaLauncher(cmd *Command) bool {
	if cmd == nil {
		return false
	}
	for _, w := range cmd.Wrappers {
		if launcherWrapperNames[w] {
			return true
		}
	}
	return false
}

// launcherFor returns the launcher row for a parsed unit.
func launcherFor(cmd *Command) (launcherSpec, bool) {
	if len(cmd.Argv) < 2 {
		return launcherSpec{}, false
	}
	spec, ok := launchers[cmd.Base]
	return spec, ok
}

// unwrapLauncher resolves a launcher unit into the launched
// command(s) and parses each as a nested unit.
//
// EVERY exit that does not analyse the launched command marks
// cmd.Unanalyzed, so R-157 denies. Before that existed, all four of
// these paths returned nil and the wrapper analysed as an ordinary
// command that matched nothing — a launcher we could not read was
// SAFER than one we could:
//
//   - no candidate although the invocation named a target;
//   - past maxUnwrapDepth (`wsl -- ` ×5 denied, ×6 ALLOWED — the
//     "recorded on Payload rather than dropped" comment described a
//     field no rule reads);
//   - more readings than maxLauncherCandidates;
//   - the shared expansion budget exhausted.
func unwrapLauncher(cmd *Command, spec launcherSpec, st parseState) []Command {
	res := spec.resolve(cmd.Argv)
	candidates := dedupCandidates(res.candidates)
	if len(candidates) == 0 {
		if res.named {
			cmd.Unanalyzed = unanalyzedLauncherTarget
		}
		return nil
	}
	if st.depth >= maxUnwrapDepth {
		cmd.Payload, cmd.PayloadKind = strings.Join(candidates[0], " "), "shell"
		cmd.Unanalyzed = unanalyzedNestingLimit
		return nil
	}
	if len(candidates) > maxLauncherCandidates {
		cmd.Unanalyzed = unanalyzedLauncherFanOut
		candidates = candidates[:maxLauncherCandidates]
	}
	if !st.afford(len(candidates)) {
		cmd.Unanalyzed = unanalyzedExpansionBudget
		return nil
	}
	dialect := spec.inner
	if dialect == "" {
		dialect = cmd.Dialect
	}
	var out []Command
	for _, inner := range candidates {
		processUnit(unit{words: inner}, dialect, st.deeper(), &out)
	}
	for i := range out {
		out[i].Wrappers = append([]string{spec.name}, out[i].Wrappers...)
		out[i].Sudo = out[i].Sudo || cmd.Sudo
	}
	return out
}

// dedupCandidates normalizes, then drops empty and repeated candidate
// argvs, so an unambiguous launcher costs exactly one nested unit. It
// is the single funnel EVERY resolver's readings pass through, which
// is why the normalization lives here rather than in each resolver.
//
// NORMALIZATION = strip surviving literal quoting from every token.
// A candidate becomes a first-class Command, so it must reach the
// matchers spelled the way the SAME command typed directly would be;
// otherwise a wrapper changes a verdict through nothing but a lexer
// artifact. That is not hypothetical:
//
//	Start-Process crontab -ArgumentList '-l'
//
// denied R-155 under the CMD dialect while `crontab -l` allowed. The
// cmd lexer does not treat `'` as a quote (correctly — cmd.exe does
// not either), so the cmd-START reading of this PowerShell line
// carried the token `'-l'`, safeCrontabList looked for `-l`, missed,
// and a read-only listing became a critical deny. The Start-Process
// reading of the same line was already correct because
// psSplitArguments trims — so the defect was that ONE reading
// normalized and the others did not.
//
// Trimming is monotonic in the safe direction for everything else: an
// unquoted token matches MORE path and flag patterns, not fewer. The
// one class it can relax is a safe-pattern that only matches unquoted
// (`crontab '-l'`) — and there the trimmed reading is the one that
// agrees with the directly-typed verdict, which is the property being
// defended. The R-155 ETW carve-out does not rely on this: it refuses
// launcher-wrapped units outright (viaLauncher).
// EMPTINESS IS CHECKED AFTER NORMALIZATION, NOT BEFORE. A candidate
// whose PROGRAM word normalizes to "" is not a reading at all — it is
// a unit whose Base is the empty string, which matches every rule
// exactly never. Skipping past leading empty words recovers the real
// program; a candidate with nothing left is dropped, and if that
// leaves the launcher with NO reading, unwrapLauncher's `named` path
// marks it Unanalyzed and R-157 denies. Both halves matter, and the
// missing check was live:
//
//	wsl '' crontab -e
//
// ALLOWED in ModeEnforce in all three dialects — wslInnerArgv reads
// the empty first operand as the command, so the launched unit's Base
// was "" and R-155 never saw crontab (measured 2026-07-27,
// docs/security.md H5 round 4). The same shape reaches the START
// readings under the cmd dialect, where `”` is not quoting and
// normalizeCandidate is the first thing to empty it.
func dedupCandidates(in [][]string) [][]string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, c := range in {
		if len(c) == 0 {
			continue
		}
		c = normalizeCandidate(c)
		for len(c) > 0 && c[0] == "" {
			c = c[1:]
		}
		if len(c) == 0 {
			continue
		}
		k := strings.Join(c, "\x00")
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

// normalizeCandidate strips literal quoting from a candidate argv's
// tokens, mirroring what psSplitArguments already does to the
// Start-Process reading. It allocates only when a token changes.
func normalizeCandidate(c []string) []string {
	out := c
	for i, t := range c {
		trimmed := strings.Trim(t, `'"`)
		if trimmed == t {
			continue
		}
		if &out[0] == &c[0] {
			out = append([]string(nil), c...)
		}
		out[i] = trimmed
	}
	return out
}

// --- PowerShell Start-Process ------------------------------------------

// psParamRole marks the two Start-Process parameters whose values this
// resolver actually needs. Every other parameter is in the table only
// so that its VALUE is not mistaken for a positional.
type psParamRole int

// psParamRole values.
const (
	psRoleOther psParamRole = iota
	psRoleFilePath
	psRoleArgumentList
)

// psParam is one row of a cmdlet's parameter table.
type psParam struct {
	// name is the lower-cased parameter name (or documented alias).
	name string
	// canon is the canonical parameter this row spells, when the row
	// is an ALIAS. Empty means the row is its own canonical name.
	//
	// It exists so the ambiguity rule can tell "two spellings of ONE
	// parameter" (-Args and -ArgumentList, which `-arg` prefixes
	// both of) from "two DIFFERENT parameters" (-Environment and
	// -ErrorAction, which `-e` prefixes both of). PowerShell binds
	// the first and refuses the second.
	canon string
	// valued marks a parameter that consumes a value; the rest are
	// switches.
	valued bool
	// role marks the parameters this resolver reads.
	role psParamRole
}

// canonName returns the canonical parameter a row spells.
func (p psParam) canonName() string {
	if p.canon != "" {
		return p.canon
	}
	return p.name
}

// psStartProcessParams is Start-Process's parameter set plus the
// common parameters that take a value.
//
// EVERY parameter matters even when its value is uninteresting,
// because a valued parameter's value must not be counted as a
// positional: `Start-Process -WorkingDirectory C:\x schtasks.exe`
// binds `C:\x` to -WorkingDirectory, so the FilePath is `schtasks.exe`
// — a table missing -WorkingDirectory would resolve the program to
// `C:\x` and lose the persistence install.
//
// `-Args` is listed as a row of its own: it is a real alias of
// -ArgumentList and is NOT a prefix of it ("args" vs "argu"), so
// prefix matching alone would never find it.
var psStartProcessParams = []psParam{
	{name: "filepath", valued: true, role: psRoleFilePath},
	{name: "argumentlist", valued: true, role: psRoleArgumentList},
	{name: "args", canon: "argumentlist", valued: true, role: psRoleArgumentList},
	{name: "credential", valued: true},
	{name: "environment", valued: true},
	{name: "redirectstandarderror", valued: true},
	{name: "redirectstandardinput", valued: true},
	{name: "redirectstandardoutput", valued: true},
	{name: "verb", valued: true},
	{name: "windowstyle", valued: true},
	{name: "workingdirectory", valued: true},
	{name: "loaduserprofile"},
	{name: "nonewwindow"},
	{name: "passthru"},
	{name: "usenewenvironment"},
	{name: "wait"},
	// Common parameters (they bind on every cmdlet).
	{name: "erroraction", valued: true},
	{name: "errorvariable", valued: true},
	{name: "informationaction", valued: true},
	{name: "informationvariable", valued: true},
	{name: "outbuffer", valued: true},
	{name: "outvariable", valued: true},
	{name: "pipelinevariable", valued: true},
	{name: "progressaction", valued: true},
	{name: "warningaction", valued: true},
	{name: "warningvariable", valued: true},
	{name: "debug"},
	{name: "verbose"},
	// ShouldProcess parameters: Start-Process declares
	// SupportsShouldProcess, so both bind on it.
	{name: "confirm"},
	{name: "whatif"},
}

// psResolveParam binds a parameter TOKEN to a table row, and it is the
// reason nothing in this file ever compares a parameter name with `==`:
//
//	an EXACT (case-insensitive) name wins outright — this is what
//	  keeps `-Verb` from being ambiguous with `-Verbose`; then
//	the token must be a prefix of ONE parameter. `-fi` and `-Ver`
//	  name one each. `-arg` prefixes both -ArgumentList and its alias
//	  -Args, which are the SAME parameter (psParam.canon), so it
//	  binds. `-e` prefixes -Environment, -ErrorAction and
//	  -ErrorVariable — three DIFFERENT parameters — so it binds to
//	  nothing.
//
// THE AMBIGUITY RULE IS PowerShell's, THE CONSEQUENCE IS NOT.
// PowerShell REFUSES to run a command carrying an ambiguous
// abbreviation; this resolver only refuses to BIND one. The two are
// not the same thing and the difference is deliberate, because the
// tempting shortcut — "PowerShell would reject this line, so allow it"
// — would be a bypass generator: this table is a SUBSET of the real
// parameter set (Start-Process's own parameters plus the common ones),
// so any parameter it does not know would read as ambiguous, and
// spelling a real parameter we happen to be missing would become a way
// to buy an allow. An unbound token instead flows into the
// never-suppress cross-product: psBindModes generates BOTH the reading
// where it swallows the next token and the reading where it does not,
// and both are analysed. `Start-Process -e Stop schtasks.exe
// -ArgumentList '/Create'` therefore still denies — correctly, on the
// reading where `schtasks.exe` is the program.
//
// The previous rule bound to whichever row appeared FIRST whenever the
// candidates merely agreed on valued-ness and role, while its comment
// claimed to bind the way PowerShell binds. That claim was false (an
// independent review measured it, docs/security.md H5 round 4); the
// behaviour is now the stricter one and the comment says what it does.
//
// Guessing is what the modes exist to avoid: guessing "switch" hides a
// program (`-w C:\x schtasks.exe` would resolve FilePath to `C:\x`) and
// guessing "valued" hides a different one — see startProcessCandidates.
func psResolveParam(name string, params []psParam) psBinding {
	name = strings.ToLower(name)
	if name == "" {
		return psBinding{}
	}
	var match psParam
	found := false
	for _, p := range params {
		if p.name == name {
			return psBinding{param: p, bound: true, exact: true}
		}
		if !strings.HasPrefix(p.name, name) {
			continue
		}
		if found {
			if match.canonName() != p.canonName() {
				return psBinding{} // ambiguous: binds to nothing
			}
			continue // another spelling of the SAME parameter
		}
		match, found = p, true
	}
	if !found {
		return psBinding{}
	}
	return psBinding{param: match, bound: true}
}

// psBinding is the outcome of binding one parameter token.
type psBinding struct {
	// param is the row the token bound to; meaningless when !bound.
	param psParam
	// bound reports that the token names a table row.
	bound bool
	// exact reports that the token spelled the parameter's FULL name
	// (or a documented alias) rather than an abbreviation.
	//
	// This distinction is load-bearing, not cosmetic. `-c` is an
	// unambiguous abbreviation of -Credential to PowerShell's own
	// binder, so the first version of this file let it swallow the
	// next token — and `start powershell -c "<persistence>"` lost its
	// entire payload and ALLOWED. An ABBREVIATION is the LAUNCHED
	// program's flag every bit as plausibly as it is the launcher's,
	// so it is never allowed to delete a token in every reading; a
	// full name is the launcher's by construction. See
	// psBindMode.looseValues.
	exact bool
}

// psParamToken splits a PowerShell parameter token into its name and,
// for the `-Name:Value` colon spelling, its inline value. It reports
// ok=false for anything that is not parameter-shaped (a positional, a
// bare "-", a negative number).
func psParamToken(t string) (name, value string, inline, ok bool) {
	if len(t) < 2 || t[0] != '-' {
		return "", "", false, false
	}
	body := t[1:]
	if body[0] >= '0' && body[0] <= '9' {
		return "", "", false, false // a negative number, not a parameter
	}
	if j := strings.IndexByte(body, ':'); j >= 0 {
		return body[:j], body[j+1:], true, true
	}
	return body, "", false, true
}

// psCollectValue returns a parameter's value tokens starting at
// argv[i], plus the index of the last token consumed.
//
// A PowerShell array literal written with spaces after its commas
// (`-ArgumentList '/Create', '/TN', 'evil'`) reaches us as SEVERAL
// lexer tokens, because the lexer splits on whitespace and drops the
// quotes. They are re-joined while a comma sits on either side of the
// seam — the only evidence of the array that survives lexing.
//
// That re-join is a GUESS about where the value ends, and join=false
// is the reading in which it was not made. It matters because a
// trailing comma is legal in an NTFS directory name and
// attacker-creatable: `Start-Process -WorkingDirectory 'C:\temp,'
// schtasks.exe -ArgumentList '/Create',…` joined `C:\temp,` with
// `schtasks.exe`, leaving no positional at all, so the resolver
// produced NO candidate and the line ALLOWED. Both readings are
// emitted (see psBindModes).
func psCollectValue(argv []string, i int, join bool) ([]string, int) {
	if i >= len(argv) {
		return nil, i - 1
	}
	vals := []string{argv[i]}
	for join && i+1 < len(argv) && (strings.HasSuffix(argv[i], ",") || strings.HasPrefix(argv[i+1], ",")) {
		i++
		vals = append(vals, argv[i])
	}
	return vals, i
}

// psSplitArguments turns collected -ArgumentList value tokens into the
// launched program's argument vector.
//
// DOCUMENTED APPROXIMATION, in the spirit of the rest of this package:
// splitting happens on commas AND whitespace. PowerShell's own rule is
// that a single string element is handed to the child process as a
// whole command-line fragment which the child then re-splits, so
// `-ArgumentList '/Create /TN evil'` and `-ArgumentList
// '/Create','/TN','evil'` reach schtasks identically. Splitting both
// gives one argv shape for the matchers. The cost is that a quoted
// path containing a space becomes two tokens; no R-155 arm depends on
// a path staying whole (the ETW carve-out does, and it deliberately
// refuses launcher-wrapped units outright — see
// safeObserverETWTaskRegistration).
//
// Array-literal syntax (`@( … )`) is peeled off each element. An
// element that is a VARIABLE (`$args`, `$e`) is kept verbatim: its
// value is statically unknowable, the same documented approximation
// ParseCommand already makes for `rm -rf $(pwd)`.
func psSplitArguments(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			part = strings.TrimPrefix(part, "@(")
			part = strings.TrimPrefix(part, "(")
			part = strings.TrimSuffix(part, ")")
			part = strings.Trim(part, `'"`)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// startProcessCandidates resolves the `start` family — the
// `Start-Process` cmdlet, its aliases `saps` / `start`, and cmd.exe's
// unrelated `START` builtin, which shares the word.
//
// It returns every interpretation the argv could carry, because the
// word alone does not settle which launcher was meant: in the
// PowerShell dialect canonicalBase has ALREADY rewritten `start` to
// `start-process`, so by the time we get here a genuine `start /b
// schtasks …` is indistinguishable from the cmdlet.
//
// It emits, ALWAYS and without a discriminator:
//
//   - every Start-Process CMDLET reading the argv's ambiguities admit
//     (psBindModes), and
//   - both cmd-START operand readings (cmdStartCandidates).
//
// The deleted discriminator is the H5 round-2 bypass. It read: a
// token that binds to a Start-Process parameter settles the line as
// the cmdlet, so the cmd-START readings are not generated. `-c` binds
// -Credential and `-e` binds the agreeing -Environment /
// -ErrorAction / -ErrorVariable set, so `start powershell -c "…"` and
// `start powershell -e <base64>` "settled" as the cmdlet — and the
// surviving cmdlet reading then ate the payload as that parameter's
// value. `-a -c -d -e -f -i -l -n -o -r -u` and longer prefixes all
// did it. The same suppression turned `/`-switches into positionals
// with no cmd reading left to correct them, so `start /b crontab -e`
// resolved its program to `/b`. See the never-suppress rule above.
func startProcessCandidates(argv []string) launchResult {
	var out [][]string
	for _, m := range psBindModes(argv) {
		resolved, at := psStartProcessArgv(argv, m)
		out = append(out, resolved)
		if at > 0 {
			// THE TAIL-VERBATIM READING. Once a mode has located the
			// program token, everything after it in the ORIGINAL argv
			// is emitted untouched, alongside the reconstruction.
			//
			// It exists because reconstruction necessarily rewrites:
			// it re-groups -ArgumentList arrays, whitespace-splits a
			// single-string element the way PowerShell's child would,
			// and drops the parameters it decided were the
			// launcher's. Each of those is right for some line and
			// destructive for another, and the destructive cases are
			// not reachable by any GLOBAL flag because two tokens in
			// one line can need opposite answers: in `Start-Process
			// -w C:\x powershell -enc <b64>` the unbound `-w` must
			// swallow its value for `powershell` to be the program,
			// while the unbound `-enc` must NOT, or the payload is
			// deleted. One flag cannot say both. The verbatim tail
			// sidesteps the question entirely — it alters nothing —
			// and the reconstruction stays for the cases only it can
			// read (`schtasks.exe '/Create /TN evil'` needs the
			// whitespace split).
			out = append(out, argv[at:])
		}
	}
	out = append(out, cmdStartCandidates(argv)...)
	return launchResult{candidates: out, named: startNamesTarget(argv)}
}

// startNamesTarget reports whether the invocation carries anything
// that could name a launched program, so that resolving to NOTHING is
// distinguishable from there being nothing to resolve. `start`, `start
// /b` and `start /d C:\x` name nothing and open a shell; anything else
// named a target, and failing to recover it is a parse failure that
// must deny rather than pass silently.
func startNamesTarget(argv []string) bool {
	for i := 1; i < len(argv); i++ {
		if t := argv[i]; t != "" && t[0] != '/' {
			return true
		}
	}
	return false
}

// psBindMode selects how psStartProcessArgv resolves the places the
// cmdlet grammar is genuinely ambiguous AFTER lexing. Every field is
// a guess this file is not allowed to make on its own: a mode with
// the flag set and a mode with it clear are both generated whenever
// the argv actually contains the ambiguity, and both are analysed.
type psBindMode struct {
	// unknownValued reads a parameter token that binds to NO table
	// row (`-w`: -Wait is a switch but -WindowStyle and
	// -WorkingDirectory are valued; or a parameter this table does
	// not know) as one that swallows the next token. Read as a
	// switch instead, `-w C:\x schtasks.exe` resolves FilePath to
	// `C:\x` and hides the program; read as valued, a launched
	// program's own flag eats its own argument.
	unknownValued bool
	// looseValues lets a PREFIX-matched valued parameter swallow its
	// value. False is the reading in which only an EXACT name does —
	// so `-c`, `-e` and friends stay with the launched program
	// instead of deleting its arguments.
	looseValues bool
	// joinCommaRuns re-joins a comma-separated value run
	// (psCollectValue). False is the reading in which a value is
	// exactly one token.
	joinCommaRuns bool
}

// psBindModes returns the readings to generate for one argv: the FULL
// CROSS-PRODUCT of the ambiguities the argv actually contains.
//
// It is a cross-product and not one-flip-at-a-time because the
// property test found the combination hole immediately: `Start-Process
// -w C:\x sh -c "<payload>"` needs unknownValued (so the unbindable
// `-w` swallows `C:\x` and `sh` becomes the program) AND
// looseValues=false (so `-c` stays with `sh` instead of binding
// -Credential) — no single flip reaches it, and every host spelling in
// that family ALLOWED while each flip was generated on its own.
// Flipping one guess at a time is just a smaller enumeration.
//
// Gating on ambiguities that are PRESENT is what keeps this cheap: an
// unambiguous line still costs exactly one cmdlet reading, which is
// every benign Start-Process the suite pins. The cost of a crafted
// line is bounded twice over — maxLauncherCandidates per unit and the
// shared expansion budget across the parse — and both fail CLOSED.
func psBindModes(argv []string) []psBindMode {
	base := psBindMode{looseValues: true, joinCommaRuns: true}
	var unknown, abbreviated, commas bool
	for i := 1; i < len(argv); i++ {
		if strings.HasSuffix(argv[i], ",") || strings.HasPrefix(argv[i], ",") {
			commas = true
		}
		name, _, _, ok := psParamToken(argv[i])
		if !ok {
			continue
		}
		switch b := psResolveParam(name, psStartProcessParams); {
		case !b.bound:
			unknown = true
		case b.param.valued && !b.exact:
			abbreviated = true
		}
	}
	var flips []func(*psBindMode)
	if unknown {
		flips = append(flips, func(m *psBindMode) { m.unknownValued = true })
	}
	if abbreviated {
		flips = append(flips, func(m *psBindMode) { m.looseValues = false })
	}
	if commas {
		flips = append(flips, func(m *psBindMode) { m.joinCommaRuns = false })
	}
	modes := []psBindMode{base}
	for _, flip := range flips {
		next := make([]psBindMode, 0, len(modes)*2)
		for _, m := range modes {
			alt := m
			flip(&alt)
			next = append(next, m, alt)
		}
		modes = next
	}
	return modes
}

// psStartProcessArgv resolves the `Start-Process` CMDLET reading of an
// argv into the argv of the program it launches, under one binding
// mode.
//
// Both binding forms are honored, because both are how the cmdlet is
// really used: the named parameters (-FilePath / -ArgumentList, in any
// legal abbreviation or the `-Name:Value` spelling) and the POSITIONAL
// ones (`Start-Process <FilePath> <ArgumentList>` — positions 0 and 1).
//
// A parameter token this mode decides is NOT the launcher's is KEPT
// as an argument of the launched program rather than dropped. It has
// to be: dropping it is a deletion by another name, and it is what
// stopped `Start-Process -Verb RunAs powershell -c "crontab -e"` from
// ever reaching powershell's own -Command parsing.
func psStartProcessArgv(argv []string, m psBindMode) ([]string, int) {
	type tok struct {
		text string
		// at is the token's index in the original argv, so the caller
		// can emit the verbatim tail that begins at the program.
		at int
		// positional marks a token that is not parameter-shaped, so
		// only it may be taken as the FilePath. A kept flag keeps its
		// place in the tail but can never BE the program.
		positional bool
	}
	var (
		filePath string
		fileAt   = -1
		listArgs []string
		tail     []tok
	)
	for i := 1; i < len(argv); i++ {
		name, inlineValue, inline, ok := psParamToken(argv[i])
		if !ok {
			tail = append(tail, tok{text: argv[i], at: i, positional: true})
			continue
		}
		b := psResolveParam(name, psStartProcessParams)
		if !b.bound {
			if m.unknownValued && !inline {
				_, i = psCollectValue(argv, i+1, m.joinCommaRuns)
				continue
			}
			tail = append(tail, tok{text: argv[i], at: i})
			continue
		}
		// A token the launcher does not own EXACTLY belongs to the
		// launched program and is KEPT, never dropped. An abbreviated
		// SWITCH is unconditional here because it consumes nothing,
		// so no mode has to choose: dropping it silently deleted a
		// flag from the launched argv, and `start crontab -l` denied
		// R-155 as a bare `crontab` because `-l` prefixes
		// -LoadUserProfile. A read-only listing became a critical
		// deny through nothing but a parameter abbreviation.
		if !b.exact && !(b.param.valued && m.looseValues) {
			tail = append(tail, tok{text: argv[i], at: i})
			continue
		}
		if !b.param.valued {
			continue
		}
		var vals []string
		if inline {
			vals = []string{inlineValue}
		} else {
			vals, i = psCollectValue(argv, i+1, m.joinCommaRuns)
		}
		switch b.param.role {
		case psRoleFilePath:
			if filePath == "" && len(vals) > 0 {
				filePath, fileAt = strings.Trim(vals[0], `'"`), i
			}
		case psRoleArgumentList:
			listArgs = append(listArgs, psSplitArguments(vals)...)
		case psRoleOther:
		}
	}
	// An EMPTY positional is skipped when picking the program: it is
	// what `start "" schtasks …`'s placeholder title lexes to, and
	// taking it as the FilePath would resolve the program to nothing.
	if filePath == "" {
		for i, t := range tail {
			v := strings.Trim(t.text, `'"`)
			if !t.positional || v == "" {
				continue
			}
			filePath, fileAt = v, t.at
			tail = append(tail[:i:i], tail[i+1:]...)
			break
		}
	}
	if filePath == "" {
		return nil, -1
	}
	// EVERY remaining tail token joins the argument list, in argv
	// order, including when -ArgumentList was also named.
	// Start-Process binds at most two positionals, so a third has no
	// legal meaning — but the cmd lexer does not treat `'` as a quote
	// (correctly: cmd.exe does not either), so a single-quoted
	// PowerShell array delivered under the cmd dialect spills its tail
	// into positional slots. Folding them back in errs toward
	// analyzing MORE; dropping them lost `Start-Process cmd.exe
	// -ArgumentList '/c','schtasks /Create …'` under exactly that
	// dialect.
	texts := make([]string, 0, len(tail))
	for _, t := range tail {
		texts = append(texts, t.text)
	}
	out := make([]string, 0, 1+len(listArgs)+len(texts))
	out = append(out, filePath)
	out = append(out, listArgs...)
	out = append(out, psSplitArguments(texts)...)
	return out, fileAt
}

// --- cmd.exe START -----------------------------------------------------

// cmdStartValuedFlags are the START switches that consume a following
// token, so it is not mistaken for the program.
var cmdStartValuedFlags = map[string]bool{"/d": true, "/node": true, "/affinity": true, "/machine": true}

// cmdStartCandidates resolves cmd.exe's `START` builtin — `start
// ["title"] [/switches] program [args…]`.
//
// START treats a QUOTED first operand as the WINDOW TITLE, and our
// lexers strip quotes, so after lexing `start "My Task" schtasks
// /Create …` and `start schtasks /Create …` are the same token
// sequence. Rather than guess — guessing "the first operand is the
// program" is what let `start "x" schtasks /Create` through — BOTH
// readings are returned: the run starting at the first operand and the
// run starting at the second. At most one of them is a real program;
// the other names an argument and matches nothing.
func cmdStartCandidates(argv []string) [][]string {
	var out [][]string
	for i := 1; i < len(argv) && len(out) < 2; i++ {
		t := argv[i]
		if t == "" {
			continue // the `""` placeholder title
		}
		if t[0] == '/' {
			if cmdStartValuedFlags[strings.ToLower(t)] {
				i++
			}
			continue
		}
		out = append(out, argv[i:])
	}
	return out
}

// --- wsl.exe -----------------------------------------------------------

// wslValuedFlags are the wsl.exe options that consume a following
// token. Without them `wsl -d Ubuntu crontab evil` would resolve its
// command to `Ubuntu`.
var wslValuedFlags = map[string]bool{
	"-d": true, "--distribution": true,
	"--distribution-id": true,
	"-u":                true, "--user": true,
	"--cd": true, "--shell-type": true,
}

// wslExecFlags are the wsl.exe options after which the REST of the
// argv is the command, exactly like `--`.
var wslExecFlags = map[string]bool{"-e": true, "--exec": true, "--": true}

// wslInnerArgv resolves `wsl.exe [options] [--] <command> [args…]`
// into the Linux command it runs. The inner command is parsed under
// the POSIX dialect (the launched process is a Linux one), which is
// the launcher row's `inner` field.
//
// wsl's own grammar is unambiguous, so exactly one candidate is ever
// produced and `named` coincides with having produced it: every
// no-candidate path here is a wsl invocation that names NO command —
// an interactive `wsl`, `wsl -l -v`, a dangling `--exec` — rather
// than one we failed to read. Those must not deny, which is why
// launchResult separates the two facts instead of treating "no
// candidate" as dangerous everywhere.
func wslInnerArgv(argv []string) launchResult {
	for i := 1; i < len(argv); i++ {
		t := argv[i]
		if t == "" || t[0] != '-' {
			return launchResult{candidates: [][]string{argv[i:]}, named: true}
		}
		low := strings.ToLower(t)
		if wslExecFlags[low] {
			if i+1 < len(argv) {
				return launchResult{candidates: [][]string{argv[i+1:]}, named: true}
			}
			return launchResult{}
		}
		if strings.ContainsRune(low, '=') {
			continue // --distribution=Ubuntu: the value is inline
		}
		if wslValuedFlags[low] && i+1 < len(argv) {
			i++
		}
	}
	return launchResult{}
}

// --- runas.exe ---------------------------------------------------------

// runasCandidates resolves Windows' `runas [/switches] "<command
// line>"` into the command it elevates.
//
// Every runas option is `/name` or `/name:value` (/user:, /noprofile,
// /profile, /env, /netonly, /savecred, /smartcard, /trustlevel:,
// /showtrustlevels) — there are no space-separated values to consume,
// so the first operand begins the command line.
//
// That command line is ONE quoted operand, which the lexers deliver as
// a single token with its spaces intact, so it is re-split on
// whitespace here. That is the same documented approximation
// psSplitArguments makes, with the same cost (a quoted path
// containing a space becomes two tokens) and the same reason: the
// matchers need an argv, and no R-155 arm depends on a path staying
// whole.
func runasCandidates(argv []string) launchResult {
	for i := 1; i < len(argv); i++ {
		t := argv[i]
		if t == "" || t[0] == '/' {
			continue
		}
		words := strings.Fields(strings.Join(argv[i:], " "))
		return launchResult{candidates: [][]string{words}, named: true}
	}
	return launchResult{}
}
