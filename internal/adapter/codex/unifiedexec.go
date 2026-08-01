package codex

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file resolves modern Codex's UNIFIED EXEC tool call.
//
// Older Codex builds emit one response_item/function_call per tool with
// a self-describing name (`shell`, `exec_command`, `update_plan`, …) and
// a JSON argument object. Current builds — and the Open Interpreter
// rebadge, which reuses this parser verbatim — collapse the whole tool
// surface into a SINGLE response_item/custom_tool_call named "exec"
// whose `input` is a small JavaScript program that calls the real tool:
//
//	const r = await tools.exec_command({cmd:"sed -n '1,240p' PROGRESS.md",
//	    "workdir":"/home/u/repo","yield_time_ms":10000}); text(r.output);
//
//	const patch = "*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n*** End Patch";
//	const result = await tools.apply_patch(patch); text(result);
//
//	const p = await tools.update_plan({explanation:"…",plan:[
//	    {step:"Read PROGRESS.md",status:"completed"}]}); text(p);
//
//	const r = await tools.write_stdin({session_id:62552,chars:"",
//	    yield_time_ms:30000,max_output_tokens:12000}); text(JSON.stringify(r));
//
// `exec` is therefore a DISPATCHER, not a shell tool. Treating the whole
// family as one run_command with an empty Target (the pre-fix behaviour)
// both blinded every consumer of actions.target — command-class
// resolution, internal/guard's dangerous-command policy, run_command
// analytics — and over-counted codex's shell activity, because ~24% of
// the family is not a shell command at all.
//
// GROUNDING. Measured over every rollout JSONL under ~/.codex/sessions
// and ~/.openinterpreter/sessions on 2026-07-31 (7,087 custom_tool_call
// rows named "exec"), bucketed by the SET of tools.<name> calls the
// program contains:
//
//	exec_command                  5379
//	apply_patch                    818
//	write_stdin                    713
//	update_plan                     85
//	exec_command + update_plan      83
//	exec_command + write_stdin       4
//	apply_patch  + exec_command      2
//	(no tools.* call at all)          3
//
// Those four inner names are the ENTIRE observed vocabulary, and the
// taxonomy already has a row for each one (internal/tooltax): they
// resolve through the adapter's existing tooltax-sourced actionMap
// rather than through a hand-written switch, so no tooltax edit is
// needed and a fifth verb lands in the honest ActionUnknown bucket
// instead of being silently absorbed into run_command.
//
// The three programs with no tools.* call at all were harness
// introspection over the injected ALL_TOOLS array
// (`ALL_TOOLS.filter(x => x.name === "exec_command"); text(meta);`) —
// they invoke no tool, so they are the documented residual class; see
// unifiedExecResidualNote.

// jsToolCall is one `tools.<name>( … )` invocation located in a
// unified-exec program, with the RAW source of its argument list (the
// text between the outermost parentheses, un-decoded).
type jsToolCall struct {
	Name string
	Args string
	// At is the byte index in the program where this call's `tools.`
	// token starts. It bounds binding resolution: only an assignment
	// textually BEFORE the call site can have supplied its argument
	// (see jsStringBinding).
	At int
}

// unifiedExecCall is the resolved identity of a unified-exec program:
// which inner tool it invoked and that call's primary argument.
type unifiedExecCall struct {
	// Name is the inner tools.<name>. Empty when the program invoked
	// no tool at all (the residual class).
	Name string
	// Target is the decoded primary argument for every inner call
	// EXCEPT apply_patch: the command for exec_command, the written
	// characters for write_stdin, the plan explanation (or first
	// step) for update_plan. Empty is a legitimate value —
	// write_stdin polls a running session with chars:"" in 513 of
	// the 713 live rows, and there is genuinely no argument to
	// report.
	Target string
	// PatchText is apply_patch's decoded patch envelope. Callers run
	// it through applyPatchTarget so the row's Target follows the
	// adapter's existing patch-target convention (first changed
	// path, project-relative). All 820 live apply_patch calls pass
	// the envelope by IDENTIFIER (`const patch = "…"; await
	// tools.apply_patch(patch)`), so resolving the const binding is
	// the load-bearing path; an inline literal is accepted too.
	PatchText string
	// NoToolCall reports that the program PROVABLY invoked no tool —
	// it contains no reference to the dispatcher object in ANY form
	// (see programProvablyInvokesNoTool). Only that proof licenses the
	// emit site to type the row models.ActionHarnessCall. A program
	// the scanner merely FAILED to resolve leaves this false and falls
	// back to the taxonomy's conservative exec row; see RESIDUAL
	// CLASS 1.
	NoToolCall bool
}

// unifiedExecToolName is the outer custom_tool_call name that carries a
// JavaScript program instead of a tool-specific argument object.
const unifiedExecToolName = "exec"

// jsToolsPrefix is the dispatcher namespace every inner call is reached
// through. Nothing else in the program invokes a tool.
const jsToolsPrefix = "tools."

// jsScanLimit bounds the single linear pass over an `input` blob. Live
// programs top out in the low hundreds of kilobytes (an apply_patch
// envelope carrying whole new files); the cap only guarantees a
// malformed or hostile blob cannot turn parsing into a long stall.
const jsScanLimit = 4 << 20

// unifiedExecArgKeys is the per-inner-call primary-argument ladder:
// the object-literal keys to try, IN ORDER, for that call's Target.
// One row per inner call, each key grounded in live rows — update_plan
// carries `explanation` in 92 live calls and only `plan[].step` in the
// other 76, which is why it needs two rungs and the others need one.
// A call with no row here contributes no Target rather than a guess.
var unifiedExecArgKeys = map[string][]string{
	"exec_command": {"cmd"},
	"write_stdin":  {"chars"},
	"update_plan":  {"explanation", "step"},
}

// unifiedExecPatchCalls are the inner calls whose primary argument is a
// raw patch envelope rather than an object-literal field.
var unifiedExecPatchCalls = map[string]bool{
	"apply_patch": true,
}

// unifiedExecBookkeeping marks inner calls that are a bookkeeping
// PREAMBLE rather than the substantive work of the program. A
// custom_tool_call becomes exactly one action row, so a program that
// updates the plan and then runs a command has to pick one identity:
// in all 86 live mixed programs the update_plan is the preamble and
// the other call is the work (73 are literally ordered update_plan →
// exec_command). Choosing the first NON-bookkeeping call reproduces
// that, and still yields todo_update for the 85 programs that only
// update the plan.
var unifiedExecBookkeeping = map[string]bool{
	"update_plan": true,
}

// RESIDUAL CLASS 1 — PROVABLY NO TOOL CALL. The FIRST honest residual:
// a program that invokes no tools.<name> at all (3 live rows, all
// ALL_TOOLS introspection over the injected tool descriptors). It is
// not a command, an edit, a plan update or a stdin write, and calling
// it any of those would be a fabrication — the emit site types it
// models.ActionHarnessCall with an empty Target.
//
// The membership test is a PROOF, not the scanner's silence. The
// scanner resolves the call SYNTAX Codex actually emits; syntax it does
// not model is invisible to it, and several such shapes reach the
// dispatcher object without ever emitting a `tools.<name>(` sequence:
//
//	text(`${await tools.exec_command({cmd:"rm -rf /tmp/x"})}`)  // template literal
//	await tools["exec_command"]({cmd:"..."})               // bracket access
//	await tools?.exec_command({cmd:"..."})                 // optional chaining
//
// Treating those as "no tool call" would type a REAL shell run as a
// harness call — losing the taxonomy's conservative exec row and
// skipping target-based safety classification entirely. So the residual
// is entered only when programProvablyInvokesNoTool says the dispatcher
// identifier appears NOWHERE in the program; everything else the
// scanner could not resolve falls back to the taxonomy's `exec` row
// (run_command) with an empty Target — the pre-fix behaviour, which is
// wrong-but-conservative rather than wrong-and-permissive.

// RESIDUAL CLASS 2 — HOISTED-ARRAY FAN-OUT. The SECOND honest
// residual. The program builds an array of work and maps the
// dispatcher over it, so the call site holds an identifier rather
// than a value:
//
//	const cmds = ["git status", "sed -n '1,80p' x.go"];
//	const rs = await Promise.all(cmds.map(cmd => tools.exec_command({cmd, …})));
//
//	const p = [{step:"Inspect current code", status:"in_progress"}, …];
//	await tools.update_plan({plan: p});
//
// Measured 2026-07-31 over all 7,087 live programs: 184 exec_command +
// 2 update_plan + 1 apply_patch = 187 programs, 2.6% of the family.
// These have no single primary argument, and the array element shapes
// are NOT uniform (plain strings, ["label", cmd] pairs, [cmd,
// token_budget] pairs, patch lines awaiting a join), so picking a slot
// would be a guess about which one is the command. They keep the
// correct ACTION TYPE and an EMPTY Target, and the whole program stays
// in raw_tool_input.
//
// A hoisted STRING is different and IS resolved: `const patch = "…";
// tools.apply_patch(patch)` binds exactly one value, so following the
// binding is a decode, not a choice (see jsStringBinding — this is the
// dominant apply_patch form, and with String.raw support it resolves
// 818 of the 819 live apply_patch calls).

// parseUnifiedExec resolves a unified-exec `input` program into the
// inner call it dispatched to plus that call's primary argument. It
// never panics and never errors: an unparseable, truncated or empty
// program simply yields the zero value, which the emit site treats as
// the residual class.
func parseUnifiedExec(input string) unifiedExecCall {
	calls := scanJSToolCalls(input)
	if len(calls) == 0 {
		// Silence is not proof: only a program with no reference to
		// the dispatcher object AT ALL is the residual class. See
		// RESIDUAL CLASS 1.
		return unifiedExecCall{NoToolCall: programProvablyInvokesNoTool(input)}
	}
	chosen := calls[0]
	for _, c := range calls {
		if !unifiedExecBookkeeping[c.Name] {
			chosen = c
			break
		}
	}
	out := unifiedExecCall{Name: chosen.Name}
	if unifiedExecPatchCalls[chosen.Name] {
		out.PatchText = jsPatchArgument(input, chosen.Args, chosen.At)
		return out
	}
	for _, key := range unifiedExecArgKeys[chosen.Name] {
		if v, ok := jsStringField(chosen.Args, key); ok {
			out.Target = v
			break
		}
	}
	return out
}

// unifiedExecDispatcherIdent is the injected dispatcher object every
// inner tool is reached through, in any syntax.
const unifiedExecDispatcherIdent = "tools"

// programProvablyInvokesNoTool reports whether a unified-exec program
// PROVABLY invokes no inner tool: the identifier `tools` does not occur
// anywhere in it as a whole token.
//
// This is deliberately a raw-byte scan — NOT string- and comment-aware
// like scanJSToolCalls. That asymmetry is the point. The scanner's job
// is to find the call Codex actually made, so it must ignore text
// inside literals (an apply_patch envelope embedding this very file
// would otherwise read as a shell command). This function's job is the
// opposite: to refuse to certify a program as tool-free. A `tools`
// occurrence inside a string or a comment could still be reached (the
// program could eval it, or the literal could be a truncation artefact
// of a real call), so any occurrence at all withdraws the proof and the
// caller falls back to the taxonomy's conservative exec row.
//
// Whole-token matching means `ALL_TOOLS` (uppercase — the shape all
// three live residual programs use) and `mytools` do not withdraw the
// proof; a bare `tools`, `tools.x`, `tools["x"]`, `tools?.x` and
// `const t = tools` all do.
//
// The proof scans the WHOLE program with no jsScanLimit truncation: a
// dispatcher reference past the cap must still count, or an oversized
// program could be certified tool-free by being long.
func programProvablyInvokesNoTool(src string) bool {
	for i := 0; i+len(unifiedExecDispatcherIdent) <= len(src); {
		j := strings.Index(src[i:], unifiedExecDispatcherIdent)
		if j < 0 {
			return true
		}
		at := i + j
		end := at + len(unifiedExecDispatcherIdent)
		leftBoundary := at == 0 || !isJSIdentByte(src[at-1])
		rightBoundary := end >= len(src) || !isJSIdentByte(src[end])
		if leftBoundary && rightBoundary {
			return false
		}
		i = at + 1
	}
	return true
}

// scanJSToolCalls walks a program ONCE — tracking JavaScript string and
// comment state — and returns every `tools.<name>( … )` invocation in
// source order.
//
// String-awareness is load-bearing, not cosmetic. apply_patch programs
// embed whole source files inside a single JavaScript string literal,
// and those literals routinely contain the text "tools.exec_command("
// (this repository's own adapter sources do). A regexp over the raw
// program would mis-identify such a patch as a shell command — the exact
// class of over-typing this fix exists to remove.
func scanJSToolCalls(src string) []jsToolCall {
	if len(src) > jsScanLimit {
		src = src[:jsScanLimit]
	}
	var out []jsToolCall
	for i := 0; i < len(src); {
		switch {
		case isJSQuote(src[i]):
			i = skipJSString(src, i)
		case strings.HasPrefix(src[i:], "//"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				return out
			}
			i += nl + 1
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += 2 + end + 2
		case strings.HasPrefix(src[i:], jsToolsPrefix) &&
			(i == 0 || !isJSIdentByte(src[i-1])):
			name, after := readJSIdent(src, i+len(jsToolsPrefix))
			if name == "" {
				i += len(jsToolsPrefix)
				continue
			}
			open := skipJSSpace(src, after)
			if open >= len(src) || src[open] != '(' {
				// `tools.exec_command` mentioned but not called
				// (the ALL_TOOLS introspection shape).
				i = after
				continue
			}
			args, end := readJSParenGroup(src, open)
			out = append(out, jsToolCall{Name: name, Args: args, At: i})
			i = end
		default:
			i++
		}
	}
	return out
}

// jsStringField returns the decoded value of the first `key: "…"` pair
// in an object-literal source, and whether it was found. Keys appear
// BOTH bare (`cmd:`) and quoted (`"plan":`) in live programs, sometimes
// within one call, so both forms are accepted. Values that are not
// string literals (numbers, arrays, nested objects) are skipped rather
// than stringified. Found-but-empty is reported as ("", true) so the
// caller can distinguish "no such field" from a genuinely empty one.
func jsStringField(src, key string) (string, bool) {
	for i := 0; i < len(src); {
		var name string
		var after int
		switch {
		case isJSQuote(src[i]):
			name, after = readJSString(src, i)
		case isJSIdentStart(src[i]):
			name, after = readJSIdent(src, i)
		default:
			i++
			continue
		}
		colon := skipJSSpace(src, after)
		if colon >= len(src) || src[colon] != ':' {
			i = after
			continue
		}
		val := skipJSSpace(src, colon+1)
		if val >= len(src) || !isJSQuote(src[val]) {
			i = colon + 1
			continue
		}
		v, end := readJSString(src, val)
		if name == key {
			return v, true
		}
		i = end
	}
	return "", false
}

// jsPatchArgument resolves apply_patch's single argument to its decoded
// patch envelope. The argument is either an inline string literal or —
// in all 820 live calls — an identifier bound earlier in the same
// program by `const patch = "*** Begin Patch…"`. callAt is the byte
// offset of the call itself, which bounds the binding search.
func jsPatchArgument(program, args string, callAt int) string {
	trimmed := strings.TrimLeft(args, " \t\r\n")
	if trimmed == "" {
		return ""
	}
	if v, ok := readJSStringExpr(trimmed, 0); ok {
		return v
	}
	if !isJSIdentStart(trimmed[0]) {
		return ""
	}
	ident, _ := readJSIdent(trimmed, 0)
	if ident == "" {
		return ""
	}
	return jsStringBinding(program, ident, callAt)
}

// jsStringBinding resolves `<ident> = <string literal>` in a program
// and returns the decoded literal. It matches the assignment regardless
// of the declaration keyword (const / let / var / bare), because the
// keyword is read and discarded as just another identifier. `==`, `===`
// and `=>` are excluded so a comparison is never mistaken for a
// binding. A binding to anything that is not a string literal (an
// array, an object, a call) yields "" — see RESIDUAL CLASS 2.
//
// THREE RULES, all three load-bearing (WP-T6 finding F5):
//
//  1. STRING- AND COMMENT-AWARE, the same scan the call scanner runs.
//     Without it a decoy inside a comment — `// const patch = "…FAKE…"`
//     — or inside an unrelated literal beats the real binding, and the
//     row reports a file the program never touched.
//  2. BEFORE THE CALL. An assignment textually after the call site
//     cannot have supplied its argument; counting one would report a
//     value the call never saw. Past the limit, unresolved.
//  3. LAST ASSIGNMENT WINS. `const p = A; p = B; tools.apply_patch(p)`
//     passes B. Taking the first match reported A. A last assignment to
//     a NON-string expression withdraws any earlier string, because the
//     value at the call site is then not a literal we can decode.
//
// APPROXIMATION, stated honestly: this is textual nearest-dominating
// resolution, not scope- or control-flow-aware evaluation. A binding
// inside a branch, a loop or a nested function that does not execute
// (or executes with a different value) is resolved as if it did. A real
// JavaScript parser is out of scope for an adapter, and the failure
// mode is bounded — the row's ACTION TYPE comes from the call itself
// and is unaffected; only the patch-derived Target can be wrong, with
// the whole program preserved in raw_tool_input as the evidence trail.
// Zero live programs (819 apply_patch calls, 2026-07-31) contain a
// conditional or repeated binding of the patch identifier.
func jsStringBinding(src, ident string, before int) string {
	if len(src) > jsScanLimit {
		src = src[:jsScanLimit]
	}
	if before < 0 || before > len(src) {
		before = len(src)
	}
	// last is the value of the newest assignment seen so far; ok
	// records whether that newest assignment was string-valued (a
	// later non-string assignment must invalidate an earlier string).
	var last string
	var ok bool
	for i := 0; i < before; {
		switch {
		case isJSQuote(src[i]):
			i = skipJSString(src, i)
		case strings.HasPrefix(src[i:], "//"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				i = len(src)
				continue
			}
			i += nl + 1
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
				continue
			}
			i += 2 + end + 2
		case isJSIdentStart(src[i]) && (i == 0 || !isJSIdentByte(src[i-1])):
			name, after := readJSIdent(src, i)
			if name != ident {
				i = after
				continue
			}
			eq := skipJSSpace(src, after)
			if eq >= len(src) || src[eq] != '=' ||
				(eq+1 < len(src) && (src[eq+1] == '=' || src[eq+1] == '>')) {
				i = after
				continue
			}
			last, ok = readJSStringExpr(src, skipJSSpace(src, eq+1))
			i = after
		default:
			i++
		}
	}
	if !ok {
		return ""
	}
	return last
}

// jsStringRawPrefix is the tagged-template form live Codex uses to hoist
// a patch envelope that itself contains backslashes: `String.raw` makes
// the literal's escape sequences INERT, so the decoder must not process
// them (6 of the 819 live apply_patch programs use it — every one of
// them patches Go source containing "\t" / "\n" that must survive as
// two characters).
const jsStringRawPrefix = "String.raw"

// readJSStringExpr reads a string-VALUED expression at index i: a plain
// literal, or a String.raw-tagged template whose escapes stay inert.
// Reports whether one was found at all, so callers can distinguish an
// empty string from a non-string expression.
func readJSStringExpr(src string, i int) (string, bool) {
	if i >= len(src) {
		return "", false
	}
	if isJSQuote(src[i]) {
		v, _ := readJSString(src, i)
		return v, true
	}
	if strings.HasPrefix(src[i:], jsStringRawPrefix) {
		j := skipJSSpace(src, i+len(jsStringRawPrefix))
		if j < len(src) && src[j] == '`' {
			// The literal's escapes are inert, but `\`` still
			// terminates nothing — step over escape PAIRS while
			// hunting the closing backtick, and keep both bytes.
			var b strings.Builder
			for k := j + 1; k < len(src); k++ {
				if src[k] == '\\' && k+1 < len(src) {
					b.WriteByte(src[k])
					k++
					b.WriteByte(src[k])
					continue
				}
				if src[k] == '`' {
					break
				}
				b.WriteByte(src[k])
			}
			return b.String(), true
		}
	}
	return "", false
}

// readJSParenGroup reads a balanced parenthesis group starting at the
// '(' at index open, and returns the RAW inner source plus the index
// just past the closing ')'. String and comment state is tracked so a
// parenthesis inside a literal never closes the group. An unbalanced
// group (truncated program) yields everything to the end of source —
// honest partial data rather than a panic.
func readJSParenGroup(src string, open int) (string, int) {
	depth := 0
	for i := open; i < len(src); {
		switch {
		case isJSQuote(src[i]):
			i = skipJSString(src, i)
		case strings.HasPrefix(src[i:], "//"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				return src[open+1:], len(src)
			}
			i += nl + 1
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return src[open+1:], len(src)
			}
			i += 2 + end + 2
		case src[i] == '(':
			depth++
			i++
		case src[i] == ')':
			depth--
			if depth == 0 {
				return src[open+1 : i], i + 1
			}
			i++
		default:
			i++
		}
	}
	return src[open+1:], len(src)
}

// skipJSString returns the index just past the string literal that
// starts at i. An unterminated literal consumes the rest of the source.
func skipJSString(src string, i int) int {
	_, end := readJSString(src, i)
	return end
}

// readJSString decodes the JavaScript string literal starting at index
// i (which must be a quote) and returns the decoded value plus the
// index just past the closing quote. Template literals are read as
// plain strings — no live program uses `${}` substitution, and reading
// one literally is strictly better than mis-tokenising the program.
func readJSString(src string, i int) (string, int) {
	quote := src[i]
	var b strings.Builder
	for j := i + 1; j < len(src); j++ {
		c := src[j]
		switch c {
		case quote:
			return b.String(), j + 1
		case '\\':
			if j+1 >= len(src) {
				return b.String(), len(src)
			}
			v, next := decodeJSEscape(src, j+1)
			b.WriteString(v)
			j = next - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), len(src)
}

// decodeJSEscape decodes the escape sequence whose body starts at index
// i (just past the backslash) and returns the replacement text plus the
// index just past the sequence. Unknown escapes decode to the escaped
// character itself, which is what JavaScript does.
func decodeJSEscape(src string, i int) (string, int) {
	switch c := src[i]; c {
	case 'n':
		return "\n", i + 1
	case 't':
		return "\t", i + 1
	case 'r':
		return "\r", i + 1
	case 'b':
		return "\b", i + 1
	case 'f':
		return "\f", i + 1
	case 'v':
		return "\v", i + 1
	case '0':
		// \0 is NUL only when not followed by another digit.
		if i+1 >= len(src) || src[i+1] < '0' || src[i+1] > '9' {
			return "\x00", i + 1
		}
		return "0", i + 1
	case '\n':
		// Line continuation: the newline is not part of the value.
		return "", i + 1
	case 'x':
		if v, ok := hexValue(src, i+1, 2); ok {
			return string(rune(v)), i + 3
		}
		return "x", i + 1
	case 'u':
		return decodeJSUnicodeEscape(src, i)
	default:
		_, size := utf8.DecodeRuneInString(src[i:])
		return src[i : i+size], i + size
	}
}

// decodeJSUnicodeEscape decodes \uHHHH (including a surrogate pair
// followed by a second \uHHHH) and \u{H…}. i points at the 'u'.
func decodeJSUnicodeEscape(src string, i int) (string, int) {
	if i+1 < len(src) && src[i+1] == '{' {
		end := strings.IndexByte(src[i+2:], '}')
		if end < 0 {
			return "u", i + 1
		}
		if v, ok := hexValue(src, i+2, end); ok && v <= utf8.MaxRune {
			return string(rune(v)), i + 2 + end + 1
		}
		return "u", i + 1
	}
	hi, ok := hexValue(src, i+1, 4)
	if !ok {
		return "u", i + 1
	}
	next := i + 5
	if utf16.IsSurrogate(rune(hi)) && next+5 < len(src) &&
		src[next] == '\\' && src[next+1] == 'u' {
		if lo, ok := hexValue(src, next+2, 4); ok {
			if r := utf16.DecodeRune(rune(hi), rune(lo)); r != utf8.RuneError {
				return string(r), next + 6
			}
		}
	}
	return string(rune(hi)), next
}

// hexValue parses exactly n hex digits starting at i.
func hexValue(src string, i, n int) (int, bool) {
	if n <= 0 || i+n > len(src) {
		return 0, false
	}
	v := 0
	for _, c := range []byte(src[i : i+n]) {
		switch {
		case c >= '0' && c <= '9':
			v = v*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v*16 + int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = v*16 + int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// readJSIdent reads the identifier starting at index i and returns it
// plus the index just past it. Returns "" when i is not an identifier
// start.
func readJSIdent(src string, i int) (string, int) {
	if i >= len(src) || !isJSIdentStart(src[i]) {
		return "", i
	}
	j := i
	for j < len(src) && isJSIdentByte(src[j]) {
		j++
	}
	return src[i:j], j
}

// skipJSSpace returns the index of the first non-whitespace byte at or
// after i.
func skipJSSpace(src string, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func isJSQuote(c byte) bool { return c == '"' || c == '\'' || c == '`' }

func isJSIdentStart(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isJSIdentByte(c byte) bool {
	return isJSIdentStart(c) || (c >= '0' && c <= '9')
}
