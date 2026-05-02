package patterns

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/failure"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// Pattern type ids. Stored in project_patterns.pattern_type.
const (
	TypeHotFile          = "hot_file"
	TypeCoChange         = "co_change"
	TypeCommonCommand    = "common_command"
	TypeEditTestPair     = "edit_test_pair"
	TypeOnboardingSeq    = "onboarding_sequence"
	TypeCrossTool        = "cross_tool_file"
	TypeKnowledgeSnippet = "knowledge_snippet"
)

// halfLivesByType is spec §6.2 decay_half_life_days per pattern type. Newly
// derived rows inherit these; users can patch them in SQL post-hoc.
var halfLivesByType = map[string]int{
	TypeHotFile:          14,
	TypeCoChange:         21,
	TypeCommonCommand:    30,
	TypeEditTestPair:     45,
	TypeOnboardingSeq:    60,
	TypeCrossTool:        30,
	TypeKnowledgeSnippet: 45,
}

// Options parameterize Derive.
type Options struct {
	// ProjectRoot restricts derivation to one project. Empty means all.
	ProjectRoot string
	// Since restricts actions to timestamp >= Since.
	Since time.Time
	// TopN caps the number of rows written per (project, pattern_type).
	// Zero means 25.
	TopN int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// Result summarizes one derivation pass.
type Result struct {
	ProjectsProcessed int            `json:"projects_processed"`
	PatternsWritten   int            `json:"patterns_written"`
	ByType            map[string]int `json:"by_type"`
	DurationMs        int64          `json:"duration_ms"`
}

// Deriver runs a derivation pass.
type Deriver struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) *Deriver { return &Deriver{db: db} }

// Derive computes patterns for the requested project(s) and overwrites the
// project_patterns rows of the five managed types. Patterns of other types
// (e.g. user-authored rows) are left untouched.
func (d *Deriver) Derive(ctx context.Context, opts Options) (Result, error) {
	start := time.Now()
	if opts.TopN <= 0 {
		opts.TopN = 25
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	res := Result{ByType: map[string]int{}}

	ids, err := d.listProjects(ctx, opts.ProjectRoot)
	if err != nil {
		return Result{}, err
	}
	res.ProjectsProcessed = len(ids)

	for _, pid := range ids {
		counts, err := d.deriveProject(ctx, pid, opts)
		if err != nil {
			return res, err
		}
		for t, n := range counts {
			res.ByType[t] += n
			res.PatternsWritten += n
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

func (d *Deriver) listProjects(ctx context.Context, root string) ([]int64, error) {
	q := `SELECT id FROM projects`
	var args []any
	if root != "" {
		q += " WHERE root_path = ?"
		args = append(args, root)
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns: list projects: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("patterns: scan project id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *Deriver) deriveProject(ctx context.Context, pid int64, opts Options) (map[string]int, error) {
	counts := map[string]int{}
	// Clear the managed types so a derivation pass is idempotent: multiple
	// runs against the same data converge on the same rows.
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM project_patterns
		 WHERE project_id = ? AND pattern_type IN (?, ?, ?, ?, ?, ?, ?)`,
		pid, TypeHotFile, TypeCoChange, TypeCommonCommand, TypeEditTestPair,
		TypeOnboardingSeq, TypeCrossTool, TypeKnowledgeSnippet,
	); err != nil {
		return nil, fmt.Errorf("patterns: clear: %w", err)
	}

	hot, err := d.hotFiles(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeHotFile] = len(hot)
	for _, p := range hot {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	co, err := d.coChange(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeCoChange] = len(co)
	for _, p := range co {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	cmd, err := d.commonCommands(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeCommonCommand] = len(cmd)
	for _, p := range cmd {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	et, err := d.editTestPairs(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeEditTestPair] = len(et)
	for _, p := range et {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	ob, err := d.onboardingSequences(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeOnboardingSeq] = len(ob)
	for _, p := range ob {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	ct, err := d.crossTool(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeCrossTool] = len(ct)
	for _, p := range ct {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}

	ks, err := d.knowledgeSnippets(ctx, pid, opts)
	if err != nil {
		return nil, err
	}
	counts[TypeKnowledgeSnippet] = len(ks)
	for _, p := range ks {
		if err := d.insert(ctx, pid, p); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// crossToolData is the pattern_data for TypeCrossTool rows. Captures which
// tool first touched a file, which tool last touched it, and how many
// different tools have picked it up. Useful "cross-agent provenance" view
// when a user runs the same project through multiple AI tools.
type crossToolData struct {
	FilePath   string   `json:"file_path"`
	FirstTool  string   `json:"first_tool"`
	FirstAt    string   `json:"first_at"`
	LastTool   string   `json:"last_tool"`
	LastAt     string   `json:"last_at"`
	ToolCount  int      `json:"tool_count"`
	Tools      []string `json:"tools"`
	TotalTouch int      `json:"total_touch"`
}

func (d *Deriver) crossTool(ctx context.Context, pid int64, opts Options) ([]row, error) {
	q := `SELECT target, tool, timestamp
	      FROM actions
	      WHERE project_id = ?
	            AND action_type IN ('read_file', 'edit_file', 'write_file')
	            AND target != '' AND tool != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY target, timestamp"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.crossTool: %w", err)
	}
	defer rows.Close()

	type agg struct {
		firstTool, lastTool string
		firstTS, lastTS     time.Time
		tools               map[string]bool
		totalTouch          int
	}
	byTarget := map[string]*agg{}
	for rows.Next() {
		var target, tool, ts string
		if err := rows.Scan(&target, &tool, &ts); err != nil {
			return nil, fmt.Errorf("patterns.crossTool scan: %w", err)
		}
		parsed, _ := time.Parse(time.RFC3339Nano, ts)
		a, ok := byTarget[target]
		if !ok {
			a = &agg{tools: map[string]bool{}, firstTS: parsed, firstTool: tool}
			byTarget[target] = a
		}
		a.tools[tool] = true
		a.totalTouch++
		if parsed.After(a.lastTS) {
			a.lastTS = parsed
			a.lastTool = tool
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type kv struct {
		path string
		a    *agg
	}
	var all []kv
	for path, a := range byTarget {
		if len(a.tools) < 2 {
			continue
		}
		all = append(all, kv{path, a})
	}
	sort.Slice(all, func(i, j int) bool {
		if len(all[i].a.tools) != len(all[j].a.tools) {
			return len(all[i].a.tools) > len(all[j].a.tools)
		}
		return all[i].a.totalTouch > all[j].a.totalTouch
	})
	if len(all) > opts.TopN {
		all = all[:opts.TopN]
	}
	maxTouch := 0
	for _, e := range all {
		if e.a.totalTouch > maxTouch {
			maxTouch = e.a.totalTouch
		}
	}
	var out []row
	for _, e := range all {
		tools := keysOf(e.a.tools)
		conf := 0.0
		if maxTouch > 0 {
			conf = float64(e.a.totalTouch) / float64(maxTouch)
		}
		out = append(out, row{
			Type: TypeCrossTool,
			Data: crossToolData{
				FilePath:   e.path,
				FirstTool:  e.a.firstTool,
				FirstAt:    e.a.firstTS.UTC().Format(time.RFC3339),
				LastTool:   e.a.lastTool,
				LastAt:     e.a.lastTS.UTC().Format(time.RFC3339),
				ToolCount:  len(tools),
				Tools:      tools,
				TotalTouch: e.a.totalTouch,
			},
			Confidence:       conf,
			LastReinforcedAt: e.a.lastTS,
			ObservationCount: e.a.totalTouch,
			SourceTools:      tools,
			SourceSessions:   0,
		})
	}
	return out, nil
}

// row is the in-flight shape we write into project_patterns.
type row struct {
	Type             string
	Data             any
	Confidence       float64
	LastReinforcedAt time.Time
	ObservationCount int
	SourceTools      []string
	SourceSessions   int
}

func (d *Deriver) insert(ctx context.Context, pid int64, r row) error {
	data, err := json.Marshal(r.Data)
	if err != nil {
		return fmt.Errorf("patterns: marshal %s: %w", r.Type, err)
	}
	tools, err := json.Marshal(r.SourceTools)
	if err != nil {
		return fmt.Errorf("patterns: marshal tools: %w", err)
	}
	half := halfLivesByType[r.Type]
	if half == 0 {
		half = 30
	}
	_, err = d.db.ExecContext(ctx,
		`INSERT INTO project_patterns (project_id, pattern_type, pattern_data,
			confidence, last_reinforced_at, decay_half_life_days,
			observation_count, source_tools, source_session_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pid, r.Type, string(data), r.Confidence,
		r.LastReinforcedAt.UTC().Format(time.RFC3339Nano), half,
		r.ObservationCount, string(tools), r.SourceSessions,
	)
	if err != nil {
		return fmt.Errorf("patterns: insert %s: %w", r.Type, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Hot files
// -----------------------------------------------------------------------------

type hotFileData struct {
	FilePath   string `json:"file_path"`
	Reads      int    `json:"reads"`
	Edits      int    `json:"edits"`
	Writes     int    `json:"writes"`
	TotalTouch int    `json:"total_touch"`
}

func (d *Deriver) hotFiles(ctx context.Context, pid int64, opts Options) ([]row, error) {
	q := `SELECT target,
	             SUM(CASE WHEN action_type = 'read_file'  THEN 1 ELSE 0 END) AS reads,
	             SUM(CASE WHEN action_type = 'edit_file'  THEN 1 ELSE 0 END) AS edits,
	             SUM(CASE WHEN action_type = 'write_file' THEN 1 ELSE 0 END) AS writes,
	             COUNT(*) AS total,
	             MAX(timestamp) AS last_ts,
	             COUNT(DISTINCT session_id) AS sessions,
	             GROUP_CONCAT(DISTINCT tool) AS tools
	      FROM actions
	      WHERE project_id = ?
	            AND action_type IN ('read_file', 'edit_file', 'write_file')
	            AND target != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += ` GROUP BY target ORDER BY total DESC LIMIT ?`
	args = append(args, opts.TopN)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.hotFiles: %w", err)
	}
	defer rows.Close()

	var out []row
	var maxTouch int
	var entries []hotFileMeta
	for rows.Next() {
		var m hotFileMeta
		var lastTS, toolsCSV string
		if err := rows.Scan(&m.path, &m.reads, &m.edits, &m.writes, &m.total,
			&lastTS, &m.sessions, &toolsCSV); err != nil {
			return nil, fmt.Errorf("patterns.hotFiles scan: %w", err)
		}
		if ts, err := time.Parse(time.RFC3339Nano, lastTS); err == nil {
			m.lastTS = ts
		}
		m.tools = splitAndSort(toolsCSV)
		entries = append(entries, m)
		if m.total > maxTouch {
			maxTouch = m.total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, m := range entries {
		conf := 0.0
		if maxTouch > 0 {
			conf = float64(m.total) / float64(maxTouch)
		}
		out = append(out, row{
			Type: TypeHotFile,
			Data: hotFileData{FilePath: m.path, Reads: m.reads, Edits: m.edits,
				Writes: m.writes, TotalTouch: m.total},
			Confidence:       conf,
			LastReinforcedAt: m.lastTS,
			ObservationCount: m.total,
			SourceTools:      m.tools,
			SourceSessions:   m.sessions,
		})
	}
	return out, nil
}

type hotFileMeta struct {
	path                                  string
	reads, edits, writes, total, sessions int
	lastTS                                time.Time
	tools                                 []string
}

// -----------------------------------------------------------------------------
// Co-change pairs
// -----------------------------------------------------------------------------

type coChangeData struct {
	FileA     string `json:"file_a"`
	FileB     string `json:"file_b"`
	PairCount int    `json:"pair_count"`
}

func (d *Deriver) coChange(ctx context.Context, pid int64, opts Options) ([]row, error) {
	// Walk edit_file / write_file actions in session order; every pair that
	// shares a session forms an undirected edge. Keyed by sorted-tuple to
	// avoid dup (a,b) + (b,a).
	q := `SELECT session_id, target, timestamp, tool
	      FROM actions
	      WHERE project_id = ? AND action_type IN ('edit_file', 'write_file')
	            AND target != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY session_id, timestamp"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.coChange: %w", err)
	}
	defer rows.Close()

	type pairKey struct{ a, b string }
	pairCount := map[pairKey]int{}
	pairLast := map[pairKey]time.Time{}
	pairSessions := map[pairKey]map[string]bool{}
	pairTools := map[pairKey]map[string]bool{}

	currentSession := ""
	currentFiles := map[string]bool{}
	currentLast := map[string]time.Time{}
	currentTools := map[string]bool{}

	flush := func() {
		if len(currentFiles) < 2 {
			return
		}
		files := keysOf(currentFiles)
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				a, b := files[i], files[j]
				if a > b {
					a, b = b, a
				}
				k := pairKey{a, b}
				pairCount[k]++
				ts := currentLast[files[i]]
				if t2 := currentLast[files[j]]; t2.After(ts) {
					ts = t2
				}
				if ts.After(pairLast[k]) {
					pairLast[k] = ts
				}
				if pairSessions[k] == nil {
					pairSessions[k] = map[string]bool{}
				}
				pairSessions[k][currentSession] = true
				if pairTools[k] == nil {
					pairTools[k] = map[string]bool{}
				}
				for tool := range currentTools {
					pairTools[k][tool] = true
				}
			}
		}
	}

	for rows.Next() {
		var sess, target, tsStr, tool string
		if err := rows.Scan(&sess, &target, &tsStr, &tool); err != nil {
			return nil, fmt.Errorf("patterns.coChange scan: %w", err)
		}
		if sess != currentSession {
			flush()
			currentSession = sess
			currentFiles = map[string]bool{}
			currentLast = map[string]time.Time{}
			currentTools = map[string]bool{}
		}
		currentFiles[target] = true
		if ts, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			currentLast[target] = ts
		}
		currentTools[tool] = true
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Top-N by count, then package.
	type kv struct {
		k pairKey
		n int
	}
	all := make([]kv, 0, len(pairCount))
	for k, n := range pairCount {
		if n < 2 {
			continue // one-off pairings are noise.
		}
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	if len(all) > opts.TopN {
		all = all[:opts.TopN]
	}
	maxN := 0
	if len(all) > 0 {
		maxN = all[0].n
	}
	var out []row
	for _, e := range all {
		conf := 0.0
		if maxN > 0 {
			conf = float64(e.n) / float64(maxN)
		}
		out = append(out, row{
			Type:             TypeCoChange,
			Data:             coChangeData{FileA: e.k.a, FileB: e.k.b, PairCount: e.n},
			Confidence:       conf,
			LastReinforcedAt: pairLast[e.k],
			ObservationCount: e.n,
			SourceTools:      sortedKeysOf(pairTools[e.k]),
			SourceSessions:   len(pairSessions[e.k]),
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Common commands
// -----------------------------------------------------------------------------

type commonCommandData struct {
	Command     string  `json:"command"`
	CommandHash string  `json:"command_hash"`
	RunCount    int     `json:"run_count"`
	SuccessRate float64 `json:"success_rate"`
}

func (d *Deriver) commonCommands(ctx context.Context, pid int64, opts Options) ([]row, error) {
	q := `SELECT target,
	             COUNT(*) AS n,
	             SUM(CASE WHEN success THEN 1 ELSE 0 END) AS ok,
	             MAX(timestamp) AS last_ts,
	             COUNT(DISTINCT session_id) AS sessions,
	             GROUP_CONCAT(DISTINCT tool) AS tools
	      FROM actions
	      WHERE project_id = ? AND action_type = 'run_command' AND target != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += " GROUP BY target HAVING n > 1 ORDER BY n DESC LIMIT ?"
	args = append(args, opts.TopN)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.commonCommands: %w", err)
	}
	defer rows.Close()
	var entries []commandMeta
	var maxN int
	for rows.Next() {
		var m commandMeta
		var lastTS, toolsCSV string
		if err := rows.Scan(&m.cmd, &m.n, &m.ok, &lastTS, &m.sessions, &toolsCSV); err != nil {
			return nil, fmt.Errorf("patterns.commonCommands scan: %w", err)
		}
		if ts, err := time.Parse(time.RFC3339Nano, lastTS); err == nil {
			m.lastTS = ts
		}
		m.tools = splitAndSort(toolsCSV)
		entries = append(entries, m)
		if m.n > maxN {
			maxN = m.n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []row
	for _, m := range entries {
		conf := 0.0
		if maxN > 0 {
			conf = float64(m.n) / float64(maxN)
		}
		rate := 0.0
		if m.n > 0 {
			rate = float64(m.ok) / float64(m.n)
		}
		out = append(out, row{
			Type: TypeCommonCommand,
			Data: commonCommandData{
				Command:     m.cmd,
				CommandHash: failure.CommandHash(m.cmd),
				RunCount:    m.n,
				SuccessRate: rate,
			},
			Confidence:       conf,
			LastReinforcedAt: m.lastTS,
			ObservationCount: m.n,
			SourceTools:      m.tools,
			SourceSessions:   m.sessions,
		})
	}
	return out, nil
}

type commandMeta struct {
	cmd             string
	n, ok, sessions int
	lastTS          time.Time
	tools           []string
}

// -----------------------------------------------------------------------------
// Edit → test pairs
// -----------------------------------------------------------------------------

type editTestData struct {
	EditTarget  string `json:"edit_target"`
	TestCommand string `json:"test_command"`
	PairCount   int    `json:"pair_count"`
}

func (d *Deriver) editTestPairs(ctx context.Context, pid int64, opts Options) ([]row, error) {
	// Scan sessions; whenever an edit_file on target X is followed within
	// the same session by a run_command that looks like a test (contains
	// "test" case-insensitive), count the (X → command) pair.
	q := `SELECT session_id, action_type, target, timestamp, tool
	      FROM actions
	      WHERE project_id = ? AND action_type IN ('edit_file', 'write_file', 'run_command')
	            AND target != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY session_id, timestamp"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.editTestPairs: %w", err)
	}
	defer rows.Close()

	type pk struct{ edit, cmd string }
	pairCount := map[pk]int{}
	pairLast := map[pk]time.Time{}
	pairSessions := map[pk]map[string]bool{}
	pairTools := map[pk]map[string]bool{}

	currentSession := ""
	pendingEdits := map[string]time.Time{} // target → most-recent edit ts
	currentTool := ""

	for rows.Next() {
		var sess, aType, target, tsStr, tool string
		if err := rows.Scan(&sess, &aType, &target, &tsStr, &tool); err != nil {
			return nil, fmt.Errorf("patterns.editTestPairs scan: %w", err)
		}
		ts, _ := time.Parse(time.RFC3339Nano, tsStr)
		if sess != currentSession {
			currentSession = sess
			pendingEdits = map[string]time.Time{}
			currentTool = tool
		}
		switch aType {
		case models.ActionEditFile, models.ActionWriteFile:
			pendingEdits[target] = ts
		case models.ActionRunCommand:
			if !looksLikeTestCommand(target) {
				continue
			}
			for editTarget := range pendingEdits {
				k := pk{editTarget, target}
				pairCount[k]++
				if ts.After(pairLast[k]) {
					pairLast[k] = ts
				}
				if pairSessions[k] == nil {
					pairSessions[k] = map[string]bool{}
				}
				pairSessions[k][sess] = true
				if pairTools[k] == nil {
					pairTools[k] = map[string]bool{}
				}
				pairTools[k][currentTool] = true
				pairTools[k][tool] = true
			}
			// Clear pending edits after a test so each edit→test pair is
			// counted once per bracket.
			pendingEdits = map[string]time.Time{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type kv struct {
		k pk
		n int
	}
	all := make([]kv, 0, len(pairCount))
	for k, n := range pairCount {
		if n < 2 {
			continue
		}
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	if len(all) > opts.TopN {
		all = all[:opts.TopN]
	}
	maxN := 0
	if len(all) > 0 {
		maxN = all[0].n
	}
	var out []row
	for _, e := range all {
		conf := 0.0
		if maxN > 0 {
			conf = float64(e.n) / float64(maxN)
		}
		out = append(out, row{
			Type:             TypeEditTestPair,
			Data:             editTestData{EditTarget: e.k.edit, TestCommand: e.k.cmd, PairCount: e.n},
			Confidence:       conf,
			LastReinforcedAt: pairLast[e.k],
			ObservationCount: e.n,
			SourceTools:      sortedKeysOf(pairTools[e.k]),
			SourceSessions:   len(pairSessions[e.k]),
		})
	}
	return out, nil
}

func looksLikeTestCommand(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "test") ||
		strings.Contains(lower, "pytest") ||
		strings.Contains(lower, "jest") ||
		strings.Contains(lower, "rspec")
}

// -----------------------------------------------------------------------------
// Onboarding sequences
// -----------------------------------------------------------------------------

type onboardingData struct {
	FirstReads   []string `json:"first_reads"`
	SessionCount int      `json:"session_count"`
}

func (d *Deriver) onboardingSequences(ctx context.Context, pid int64, opts Options) ([]row, error) {
	// For each session, take the first 3 read_file targets in timestamp
	// order. Group by the tuple. Top-N tuples become onboarding patterns.
	q := `SELECT session_id, target, timestamp, tool FROM actions
	      WHERE project_id = ? AND action_type = 'read_file' AND target != ''`
	args := []any{pid}
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY session_id, timestamp"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns.onboardingSequences: %w", err)
	}
	defer rows.Close()
	type seqMeta struct {
		count    int
		lastTS   time.Time
		sessions map[string]bool
		tools    map[string]bool
	}
	byKey := map[string]*seqMeta{}
	byKeyFiles := map[string][]string{}

	currentSession := ""
	currentFirst := []string{}
	currentLast := time.Time{}
	currentTool := ""
	flush := func() {
		if len(currentFirst) < 2 { // 2-or-3 file prefixes are still useful
			return
		}
		key := strings.Join(currentFirst, "→")
		m, ok := byKey[key]
		if !ok {
			m = &seqMeta{sessions: map[string]bool{}, tools: map[string]bool{}}
			byKey[key] = m
			byKeyFiles[key] = append([]string{}, currentFirst...)
		}
		m.count++
		if currentLast.After(m.lastTS) {
			m.lastTS = currentLast
		}
		m.sessions[currentSession] = true
		m.tools[currentTool] = true
	}

	for rows.Next() {
		var sess, target, tsStr, tool string
		if err := rows.Scan(&sess, &target, &tsStr, &tool); err != nil {
			return nil, fmt.Errorf("patterns.onboardingSequences scan: %w", err)
		}
		if sess != currentSession {
			flush()
			currentSession = sess
			currentFirst = currentFirst[:0]
			currentLast = time.Time{}
			currentTool = tool
		}
		if len(currentFirst) < 3 {
			currentFirst = append(currentFirst, target)
			if ts, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
				if ts.After(currentLast) {
					currentLast = ts
				}
			}
		}
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type kv struct {
		k string
		m *seqMeta
	}
	all := make([]kv, 0, len(byKey))
	for k, m := range byKey {
		if m.count < 2 {
			continue
		}
		all = append(all, kv{k, m})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].m.count > all[j].m.count })
	if len(all) > opts.TopN {
		all = all[:opts.TopN]
	}
	maxN := 0
	if len(all) > 0 {
		maxN = all[0].m.count
	}
	var out []row
	for _, e := range all {
		conf := 0.0
		if maxN > 0 {
			conf = float64(e.m.count) / float64(maxN)
		}
		out = append(out, row{
			Type:             TypeOnboardingSeq,
			Data:             onboardingData{FirstReads: byKeyFiles[e.k], SessionCount: len(e.m.sessions)},
			Confidence:       conf,
			LastReinforcedAt: e.m.lastTS,
			ObservationCount: e.m.count,
			SourceTools:      sortedKeysOf(e.m.tools),
			SourceSessions:   len(e.m.sessions),
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func splitAndSort(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	sort.Strings(parts)
	return parts
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return keysOf(m)
}

// -----------------------------------------------------------------------------
// Knowledge snippets — extracted from preceding_reasoning
// -----------------------------------------------------------------------------

type knowledgeSnippetData struct {
	Span        string `json:"span"`
	SourceCount int    `json:"source_count"`
}

// knowledgeSignals are substrings that indicate a sentence carries
// reusable knowledge (a decision, observation, or constraint).
var knowledgeSignals = []string{
	"because", "since", "due to",
	"pattern", "convention", "structure",
	"always", "never", "must", "should",
	"important", "careful", "note",
	"uses ", "follows", "requires",
	"configured", "depends on", "expects",
	"found that", "noticed", "observed",
	"architecture", "design", "approach",
}

func (d *Deriver) knowledgeSnippets(ctx context.Context, pid int64, opts Options) ([]row, error) {
	q := `SELECT preceding_reasoning, tool, session_id, timestamp
	      FROM actions
	      WHERE project_id = ? AND preceding_reasoning != ''`
	var args []any
	args = append(args, pid)
	if !opts.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, opts.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("patterns: knowledge query: %w", err)
	}
	defer rows.Close()

	type spanInfo struct {
		count    int
		tools    map[string]bool
		sessions map[string]bool
		lastSeen time.Time
	}
	spans := map[string]*spanInfo{}

	for rows.Next() {
		var reasoning, tool, sessionID, ts string
		if err := rows.Scan(&reasoning, &tool, &sessionID, &ts); err != nil {
			return nil, fmt.Errorf("patterns: knowledge scan: %w", err)
		}
		timestamp, _ := time.Parse(time.RFC3339Nano, ts)
		for _, sent := range extractSentences(reasoning) {
			if !isKnowledgeBearing(sent) {
				continue
			}
			norm := normalizeSentence(sent)
			si, ok := spans[norm]
			if !ok {
				si = &spanInfo{tools: map[string]bool{}, sessions: map[string]bool{}}
				spans[norm] = si
			}
			si.count++
			si.tools[tool] = true
			si.sessions[sessionID] = true
			if timestamp.After(si.lastSeen) {
				si.lastSeen = timestamp
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("patterns: knowledge rows: %w", err)
	}

	var out []row
	maxCount := 0
	for _, si := range spans {
		if si.count > maxCount {
			maxCount = si.count
		}
	}
	if maxCount == 0 {
		return nil, nil
	}
	for span, si := range spans {
		conf := float64(si.count) / float64(maxCount)
		out = append(out, row{
			Type:             TypeKnowledgeSnippet,
			Data:             knowledgeSnippetData{Span: span, SourceCount: si.count},
			Confidence:       conf,
			LastReinforcedAt: si.lastSeen,
			ObservationCount: si.count,
			SourceTools:      keysOf(si.tools),
			SourceSessions:   len(si.sessions),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ObservationCount > out[j].ObservationCount
	})
	if len(out) > opts.TopN {
		out = out[:opts.TopN]
	}
	return out, nil
}

func extractSentences(text string) []string {
	text = strings.ReplaceAll(text, "\n", " ")
	var sents []string
	for _, sep := range []string{". ", "! ", "? "} {
		parts := strings.Split(text, sep)
		if len(parts) > 1 {
			text = strings.Join(parts, sep)
		}
	}
	for _, s := range splitOnSentenceBoundaries(text) {
		s = strings.TrimSpace(s)
		if len(s) >= 30 {
			sents = append(sents, s)
		}
	}
	return sents
}

func splitOnSentenceBoundaries(text string) []string {
	var result []string
	var current strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		current.WriteRune(runes[i])
		if (runes[i] == '.' || runes[i] == '!' || runes[i] == '?') &&
			i+1 < len(runes) && runes[i+1] == ' ' {
			result = append(result, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

func isKnowledgeBearing(sentence string) bool {
	lower := strings.ToLower(sentence)
	for _, sig := range knowledgeSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

func normalizeSentence(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// DecayFactor returns 2^(-days_since_reinforced / half_life_days).
//
// Exposed for the MCP tool / CLI to apply at read time so users can see
// decayed confidence without mutating the DB. Zero half_life → 1.
func DecayFactor(lastReinforced time.Time, halfLifeDays int, now time.Time) float64 {
	if halfLifeDays <= 0 || lastReinforced.IsZero() {
		return 1
	}
	days := now.Sub(lastReinforced).Hours() / 24
	// 2 ** -(days/half) = exp(ln2 * -days/half)
	const ln2 = 0.6931471805599453
	return math.Exp(-ln2 * days / float64(halfLifeDays))
}
