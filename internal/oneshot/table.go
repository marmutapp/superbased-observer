package oneshot

import "time"

// PriceBasis is the fixed disclaimer every dollar figure in this report
// carries, in both the terminal footer and the --json "price_basis"
// field. This report never shows a cost without it (CLAUDE.md honesty
// rule / the retracted-savings-claim class the website accuracy-check CI
// gate forbids) — never drop it, never reword it per call site.
const PriceBasis = "estimated list price, not invoiced"

// Row is one tool×model line in the report. It is a plain value type — no
// cost.Row, no cost.TokenBundle leaks in here. cmd/observer/usage.go maps
// a cost.Summary row into this shape at the single boundary seam
// (CLAUDE.md #2).
type Row struct {
	// Tool is the adapter's canonical name (e.g. "claude-code", "codex").
	Tool string
	// Model is the model id as recorded by the adapter/proxy.
	Model string
	// Input / Output / CacheRead / CacheCreation are token counts. A zero
	// CacheRead or CacheCreation renders as an em-dash by Render — "no
	// cache data for this row", not a zero to sum.
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	// Turns is the number of underlying API turns this row aggregates.
	Turns int
	// USD is the estimated list-price cost for this row. See PriceBasis.
	USD float64
	// Reliability is the weakest reliability tag among the underlying
	// rows this line aggregates: "accurate" / "approximate" /
	// "unreliable" / "unknown" (mirrors cost.Row.Reliability). The zero
	// value ("") means "not set" and never earns the "~" marker — callers
	// should always set this from the cost engine in practice.
	Reliability string
	// PricingSource records how the pricing-table lookup resolved for
	// this row's model: "exact" / "date-stripped" / "family" / "miss" /
	// "mixed" (mirrors cost.Row.PricingSource). Carried through for the
	// --json shape; Render does not badge on it today.
	PricingSource string
}

// Approx reports whether this row's tool self-reports rounded or
// otherwise less-than-wire-accurate counts — the signal that earns the
// "~" marker on the tool cell (plan §1.4). "accurate" (proxy-captured)
// and the unset zero value never mark; "approximate", "unreliable", and
// "unknown" all do, because each means the number is something less than
// a wire-accurate per-turn count.
func (r Row) Approx() bool {
	return r.Reliability != "" && r.Reliability != "accurate"
}

// PartialScan describes a scan that was cut short by the wall-clock
// budget (plan §1.3 / §2.2 step 7). A nil *PartialScan on Table means the
// scan ran to completion (or no budget was set).
type PartialScan struct {
	// Budget is the human display of the budget that was hit, e.g. "30s".
	Budget string
	// FilesWalked is how many session files were read before the budget
	// expired.
	FilesWalked int
	// FilesTotal is the best-known total file count the scan discovered
	// (walked + not-yet-walked). May equal FilesWalked when the total was
	// never fully enumerated before the budget expired.
	FilesTotal int
}

// EmptyCorpus describes a fully empty scan — zero rows found anywhere in
// the window (plan §1.4's "no session files found" case). A nil
// *EmptyCorpus on Table means at least one row was found.
type EmptyCorpus struct {
	// Home is the display value for the location scanned, e.g. "$HOME" or
	// an absolute path.
	Home string
	// Looked lists the home-relative watch roots that were checked (e.g.
	// "~/.claude/projects", "~/.codex/sessions").
	Looked []string
	// AdapterCount is how many adapters were checked.
	AdapterCount int
}

// Table is the whole one-shot report: the rendered rows, their totals, and
// every honest caveat about them. It is a plain value type built by
// cmd/observer/usage.go from a cost.Summary (the one mapping seam — see
// doc.go) and consumed by Render and by the --json marshaler (the
// "superbased.usage/1" shape, plan §2.5).
type Table struct {
	// WindowSince is the lower bound of the reporting window (the zero
	// time.Time means "all time"). WindowLabel is its human rendering
	// ("last 30 days", "all time", "since 2026-06-01") — normally the
	// label Window returned.
	WindowSince time.Time
	WindowLabel string

	// Rows is the tool×model rollup, already sorted the way the caller
	// wants it printed; Render does not re-sort.
	Rows []Row

	// ToolCount / ModelCount are the distinct tool / model counts across
	// Rows, for the TOTAL line ("N tools · M models").
	ToolCount  int
	ModelCount int

	// TotalInput / TotalOutput / TotalCacheRead / TotalCacheCreation /
	// TotalTurns / TotalUSD are the column sums for the TOTAL line.
	TotalInput         int64
	TotalOutput        int64
	TotalCacheRead     int64
	TotalCacheCreation int64
	TotalTurns         int
	TotalUSD           float64

	// Reliability is the weakest Row.Reliability across the whole table
	// ("accurate" > "approximate" > "unreliable" > "unknown"), mirroring
	// cost.Summary.Reliability.
	Reliability string
	// Tier names the capture tier this report is drawn from. This
	// command always reads local session files, never the proxy, so
	// Render treats the empty value as "log" — `observer cost` after
	// `observer start` is the sibling that can show "proxy".
	Tier string

	// UnknownModelCount / UnpricedTokens mirror cost.Summary: how many
	// distinct models had no pricing entry, and how much input+output
	// token volume they represent. Feeds the "unpriced_models" Note.
	UnknownModelCount int
	UnpricedTokens    int64

	// Partial is non-nil when the scan was cut short by the wall-clock
	// budget. Empty is non-nil when Rows is empty (zero rows found).
	Partial *PartialScan
	Empty   *EmptyCorpus

	// Notes are the conditional honesty lines derived by Notes() from the
	// integration capability registry plus this same scan/cost-engine
	// data. Render prints these verbatim, in order, after the fixed
	// footer (or, for an empty corpus, uses the "empty_state" note's Text
	// as the whole output).
	Notes []Note
}

// Note is one conditional honesty line in the report footer (and in the
// --json "notes" array). Code is a stable machine-readable discriminator
// ("no_local_token_source", "gap", "unpriced_models", "partial",
// "empty_state"); Tools optionally names which tool(s) it concerns; Text
// is the exact human-readable line Render prints.
type Note struct {
	Code  string
	Tools []string
	Text  string
}
