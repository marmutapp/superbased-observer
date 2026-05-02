package suggest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/learn"
	"github.com/marmutapp/superbased-observer/internal/intelligence/patterns"
)

// ManagedBlockStart / End bracket the suggest-managed section. Independent
// from learn's so both commands can coexist in the same file.
const (
	ManagedBlockStart = "<!-- sbso:suggest:start -->"
	ManagedBlockEnd   = "<!-- sbso:suggest:end -->"
)

// Options parameterize Build.
type Options struct {
	ProjectRoot string
	Days        int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
	// MaxPerSection caps each pattern-list length in the rendered body.
	// Zero means 5.
	MaxPerSection int
}

// Input is the derived material the body is built from.
type Input struct {
	HotFiles        []patternRow `json:"hot_files"`
	CommonCommands  []patternRow `json:"common_commands"`
	EditTestPairs   []patternRow `json:"edit_test_pairs"`
	OnboardingReads []string     `json:"onboarding_reads,omitempty"`
	CoChangePairs   []patternRow `json:"co_change_pairs,omitempty"`
	Rules           []learn.Rule `json:"rules"`
}

type patternRow struct {
	Key        string  `json:"key"`
	Detail     string  `json:"detail,omitempty"`
	Confidence float64 `json:"confidence"`
	Count      int     `json:"count"`
}

// Load pulls patterns + learn rules for the project into one Input shape.
func Load(ctx context.Context, db *sql.DB, opts Options) (Input, error) {
	if opts.MaxPerSection <= 0 {
		opts.MaxPerSection = 5
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	var in Input

	// Pattern query — join via project root to a project_id.
	var pid int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = ?`, opts.ProjectRoot).Scan(&pid)
	if err == nil {
		pats, err := loadPatterns(ctx, db, pid, opts.MaxPerSection, opts.Now())
		if err != nil {
			return Input{}, err
		}
		in.HotFiles = pats.hot
		in.CommonCommands = pats.cmds
		in.EditTestPairs = pats.etp
		in.OnboardingReads = pats.onboarding
		in.CoChangePairs = pats.coChange
	} else if err != sql.ErrNoRows {
		return Input{}, fmt.Errorf("suggest.Load project: %w", err)
	}

	rules, err := learn.New(db).Derive(ctx, learn.Options{
		ProjectRoot: opts.ProjectRoot,
		Days:        opts.Days,
	})
	if err != nil {
		return Input{}, err
	}
	in.Rules = rules
	return in, nil
}

type patternLoad struct {
	hot        []patternRow
	cmds       []patternRow
	etp        []patternRow
	onboarding []string
	coChange   []patternRow
}

func loadPatterns(ctx context.Context, db *sql.DB, pid int64, maxN int, now time.Time) (patternLoad, error) {
	var out patternLoad
	rows, err := db.QueryContext(ctx,
		`SELECT pattern_type, pattern_data, COALESCE(confidence, 0),
		        COALESCE(last_reinforced_at, ''), COALESCE(decay_half_life_days, 30),
		        COALESCE(observation_count, 0)
		 FROM project_patterns WHERE project_id = ?`, pid)
	if err != nil {
		return out, fmt.Errorf("suggest.loadPatterns: %w", err)
	}
	defer rows.Close()

	type entry struct {
		row   patternRow
		typ   string
		extra []string
	}
	var all []entry
	for rows.Next() {
		var pType, pData, lastTS string
		var conf float64
		var half, obs int
		if err := rows.Scan(&pType, &pData, &conf, &lastTS, &half, &obs); err != nil {
			return out, fmt.Errorf("suggest.loadPatterns scan: %w", err)
		}
		var last time.Time
		if lastTS != "" {
			last, _ = time.Parse(time.RFC3339Nano, lastTS)
		}
		decayed := conf * patterns.DecayFactor(last, half, now)

		e := entry{typ: pType, row: patternRow{Confidence: decayed, Count: obs}}
		switch pType {
		case patterns.TypeHotFile:
			var hf struct {
				FilePath   string `json:"file_path"`
				Reads      int    `json:"reads"`
				Edits      int    `json:"edits"`
				TotalTouch int    `json:"total_touch"`
			}
			_ = json.Unmarshal([]byte(pData), &hf)
			e.row.Key = hf.FilePath
			e.row.Detail = fmt.Sprintf("%d read, %d edit", hf.Reads, hf.Edits)
			e.row.Count = hf.TotalTouch
		case patterns.TypeCommonCommand:
			var cc struct {
				Command     string  `json:"command"`
				RunCount    int     `json:"run_count"`
				SuccessRate float64 `json:"success_rate"`
			}
			_ = json.Unmarshal([]byte(pData), &cc)
			e.row.Key = cc.Command
			e.row.Detail = fmt.Sprintf("%d runs, %.0f%% success", cc.RunCount, cc.SuccessRate*100)
			e.row.Count = cc.RunCount
		case patterns.TypeEditTestPair:
			var ep struct {
				EditTarget  string `json:"edit_target"`
				TestCommand string `json:"test_command"`
				PairCount   int    `json:"pair_count"`
			}
			_ = json.Unmarshal([]byte(pData), &ep)
			e.row.Key = ep.EditTarget
			e.row.Detail = "⇒ " + ep.TestCommand
			e.row.Count = ep.PairCount
		case patterns.TypeOnboardingSeq:
			var ob struct {
				FirstReads []string `json:"first_reads"`
			}
			_ = json.Unmarshal([]byte(pData), &ob)
			e.extra = ob.FirstReads
		case patterns.TypeCoChange:
			var cc struct {
				FileA     string `json:"file_a"`
				FileB     string `json:"file_b"`
				PairCount int    `json:"pair_count"`
			}
			_ = json.Unmarshal([]byte(pData), &cc)
			e.row.Key = cc.FileA + " + " + cc.FileB
			e.row.Count = cc.PairCount
		default:
			continue
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Partition by type, sort by decayed confidence desc, cap.
	type sorted struct {
		entries []entry
	}
	byType := map[string]*sorted{
		patterns.TypeHotFile:       {},
		patterns.TypeCommonCommand: {},
		patterns.TypeEditTestPair:  {},
		patterns.TypeOnboardingSeq: {},
		patterns.TypeCoChange:      {},
	}
	for _, e := range all {
		if s, ok := byType[e.typ]; ok {
			s.entries = append(s.entries, e)
		}
	}
	sortByConf := func(xs []entry) []entry {
		for i := range xs {
			for j := i + 1; j < len(xs); j++ {
				if xs[j].row.Confidence > xs[i].row.Confidence {
					xs[i], xs[j] = xs[j], xs[i]
				}
			}
		}
		return xs
	}
	toPatternRows := func(xs []entry) []patternRow {
		if len(xs) > maxN {
			xs = xs[:maxN]
		}
		out := make([]patternRow, 0, len(xs))
		for _, e := range xs {
			out = append(out, e.row)
		}
		return out
	}
	out.hot = toPatternRows(sortByConf(byType[patterns.TypeHotFile].entries))
	out.cmds = toPatternRows(sortByConf(byType[patterns.TypeCommonCommand].entries))
	out.etp = toPatternRows(sortByConf(byType[patterns.TypeEditTestPair].entries))
	out.coChange = toPatternRows(sortByConf(byType[patterns.TypeCoChange].entries))
	if seq := sortByConf(byType[patterns.TypeOnboardingSeq].entries); len(seq) > 0 {
		out.onboarding = seq[0].extra
	}
	return out, nil
}

// RenderMarkdown builds the body for CLAUDE.md / AGENTS.md.
func RenderMarkdown(in Input, now time.Time) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "## Project context (derived)\n\n")
	fmt.Fprintf(&b, "_Auto-generated by `observer suggest` on %s._\n\n",
		now.UTC().Format("2006-01-02"))

	section(&b, "Hot files (recent traffic)", in.HotFiles, markdownBullet)
	section(&b, "Common commands", in.CommonCommands, markdownBullet)
	section(&b, "Edit → test patterns", in.EditTestPairs, markdownBullet)
	if len(in.OnboardingReads) > 0 {
		fmt.Fprint(&b, "### Typical onboarding reads\n\n")
		fmt.Fprintln(&b, "When starting a new task, read these first to load context:")
		for _, f := range in.OnboardingReads {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		fmt.Fprintln(&b)
	}
	section(&b, "Files that tend to change together", in.CoChangePairs, markdownBullet)
	if len(in.Rules) > 0 {
		fmt.Fprint(&b, "### Known command corrections\n\n")
		for _, r := range in.Rules {
			fmt.Fprintf(&b, "- **`%s`** (%s, %d failure(s), %d recovered)",
				oneLine(r.CommandSummary), defaultIf(r.ErrorCategory, "uncategorized"),
				r.FailureCount, r.RecoveryCount)
			if len(r.EditedFiles) > 0 {
				fmt.Fprintf(&b, " — previously fixed by editing: %s",
					strings.Join(quoteBacks(r.EditedFiles), ", "))
			}
			fmt.Fprintln(&b)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// RenderCursorRules produces a compact .cursorrules body.
func RenderCursorRules(in Input, now time.Time) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# sbso:suggest — generated %s\n",
		now.UTC().Format("2006-01-02"))
	for _, r := range in.HotFiles {
		fmt.Fprintf(&b, "- Hot file: %s (%s)\n", r.Key, r.Detail)
	}
	for _, r := range in.CommonCommands {
		fmt.Fprintf(&b, "- Common command: %s (%s)\n", r.Key, r.Detail)
	}
	for _, r := range in.EditTestPairs {
		fmt.Fprintf(&b, "- After editing %s, run %s\n", r.Key, strings.TrimPrefix(r.Detail, "⇒ "))
	}
	for _, r := range in.Rules {
		fmt.Fprintf(&b, "- Command `%s` (%s, x%d): previously fixed by editing %s\n",
			oneLine(r.CommandSummary), defaultIf(r.ErrorCategory, "?"),
			r.FailureCount, strings.Join(quoteBacks(r.EditedFiles), ", "))
	}
	return b.String()
}

// Apply rewrites the managed block in path. Same semantics as learn.Apply —
// see `learn` package for details; suggest uses its own marker pair.
func Apply(path string, body string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("suggest.Apply read %s: %w", path, err)
	}
	updated := rewrite(existing, body)
	if bytes.Equal(existing, updated) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("suggest.Apply mkdir: %w", err)
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return false, fmt.Errorf("suggest.Apply write %s: %w", path, err)
	}
	return true, nil
}

func rewrite(existing []byte, body string) []byte {
	block := ManagedBlockStart + "\n" + body + "\n" + ManagedBlockEnd
	startIdx := bytes.Index(existing, []byte(ManagedBlockStart))
	endIdx := bytes.Index(existing, []byte(ManagedBlockEnd))
	var head, tail []byte
	if startIdx >= 0 && endIdx > startIdx {
		head = bytes.TrimRight(existing[:startIdx], "\n")
		tail = bytes.TrimLeft(existing[endIdx+len(ManagedBlockEnd):], "\n")
	} else {
		head = bytes.TrimRight(existing, "\n")
	}
	var out []byte
	if len(head) > 0 {
		out = append(out, head...)
		out = append(out, '\n', '\n')
	}
	out = append(out, []byte(block)...)
	out = append(out, '\n')
	if len(tail) > 0 {
		out = append(out, '\n')
		out = append(out, tail...)
		if !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
	}
	return out
}

func section(b *bytes.Buffer, title string, rows []patternRow, render func(patternRow) string) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "### %s\n\n", title)
	for _, r := range rows {
		fmt.Fprintln(b, render(r))
	}
	fmt.Fprintln(b)
}

func markdownBullet(r patternRow) string {
	line := fmt.Sprintf("- `%s`", r.Key)
	if r.Detail != "" {
		line += " — " + r.Detail
	}
	return line
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func defaultIf(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func quoteBacks(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, "`"+x+"`")
	}
	return out
}
