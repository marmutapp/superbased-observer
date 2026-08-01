package patterns

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/tooltax"
)

// preWPT5Literals is the VERBATIM action-type set each patterns.go query
// site carried as an inline SQL literal before WP-T5 replaced them with
// tooltax lookups. This is the before/after pin the plan asks for: the
// swap was pure de-duplication, so every derived set must still equal the
// literal set it replaced, character for character.
//
// A tooltax change that moves one of these sets is not forbidden — it is
// required to be DELIBERATE. It fails here first, and the fixture is
// updated in the same commit that argues for the new semantics.
var preWPT5Literals = []struct {
	site    string
	sql     string
	want    []string
	derived []string
}{
	{
		site:    "crossTool",
		sql:     "action_type IN ('read_file', 'edit_file', 'write_file')",
		want:    []string{"edit_file", "read_file", "write_file"},
		derived: fileActionTypes,
	},
	{
		site:    "hotFiles",
		sql:     "action_type IN ('read_file', 'edit_file', 'write_file')",
		want:    []string{"edit_file", "read_file", "write_file"},
		derived: fileActionTypes,
	},
	{
		site:    "coChange",
		sql:     "action_type IN ('edit_file', 'write_file')",
		want:    []string{"edit_file", "write_file"},
		derived: mutatingFileActionTypes,
	},
	{
		site:    "commonCommands",
		sql:     "action_type = 'run_command'",
		want:    []string{"run_command"},
		derived: commandActionTypes,
	},
	{
		site:    "editTestPairs",
		sql:     "action_type IN ('edit_file', 'write_file', 'run_command')",
		want:    []string{"edit_file", "run_command", "write_file"},
		derived: mutatingFileOrCommandActionTypes,
	},
	{
		site:    "onboardingSequences",
		sql:     "action_type = 'read_file'",
		want:    []string{"read_file"},
		derived: []string{tooltax.ActionReadFile},
	},
}

// TestScopeSetsMatchPreWPT5Literals is the fixture pin.
func TestScopeSetsMatchPreWPT5Literals(t *testing.T) {
	for _, c := range preWPT5Literals {
		t.Run(c.site, func(t *testing.T) {
			got := append([]string(nil), c.derived...)
			sort.Strings(got)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("%s: tooltax-derived set %v != pre-WP-T5 literal %v (was: %s)",
					c.site, got, want, c.sql)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s: tooltax-derived set %v != pre-WP-T5 literal %v (was: %s)",
						c.site, got, want, c.sql)
				}
			}
		})
	}
}

// TestScopeSetsAreCanonicalTooltaxTypes pins that no site scopes on a
// string tooltax does not know — the failure mode the literals had (a
// typo'd action_type silently matches nothing).
func TestScopeSetsAreCanonicalTooltaxTypes(t *testing.T) {
	known := map[string]bool{}
	for _, at := range tooltax.ActionTypes() {
		known[at] = true
	}
	for _, c := range preWPT5Literals {
		for _, at := range c.derived {
			if !known[at] {
				t.Errorf("%s scopes on %q, which is not a registered tooltax action type", c.site, at)
			}
		}
	}
}

// TestInPlaceholders pins the IN-list renderer, including the empty-set
// case: an empty scope must match NOTHING, never everything.
func TestInPlaceholders(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{-1, "NULL"},
		{0, "NULL"},
		{1, "?"},
		{2, "?, ?"},
		{3, "?, ?, ?"},
	}
	for _, c := range cases {
		if got := inPlaceholders(c.n); got != c.want {
			t.Errorf("inPlaceholders(%d) = %q; want %q", c.n, got, c.want)
		}
	}
}

// TestScopesConsiderOnlyTheirActionTypes is the behavioural half of the
// pin: it seeds ONE action of every canonical action type (twice, in two
// sessions with two tools) and asserts each derivation considered exactly
// the action types its scope set names — so a widened set would show up
// as a leaked row, not just a changed slice.
func TestScopesConsiderOnlyTheirActionTypes(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

	// target → the action type that produced it.
	typeOf := map[string]string{}
	const secondRead = "second-read.go"
	const testCommand = "go test ./..."

	var events []models.ToolEvent
	step := 0
	add := func(session, tool, actionType, target string) {
		step++
		events = append(events, evt(nextID(), session, tool, actionType, target,
			base.Add(time.Duration(step)*time.Second), true))
		typeOf[target] = actionType
	}
	sessions := []struct{ id, tool string }{
		{"sA", models.ToolClaudeCode},
		{"sB", models.ToolCodex},
	}
	for _, s := range sessions {
		for _, at := range tooltax.ActionTypes() {
			add(s.id, s.tool, at, "f/"+at+".go")
		}
		// A second read so onboardingSequences has a 2-file prefix, and a
		// test-shaped command so editTestPairs has a bracket to close.
		add(s.id, s.tool, tooltax.ActionReadFile, secondRead)
		add(s.id, s.tool, tooltax.ActionRunCommand, testCommand)
	}
	for i := range events {
		events[i].ProjectRoot = root
	}
	seedEvents(t, database, root, events)

	ctx := context.Background()
	pid := onlyProjectID(t, database)
	d := New(database)
	opts := Options{TopN: 500, Now: func() time.Time { return base.Add(time.Hour) }}

	typesOf := func(targets ...string) []string {
		seen := map[string]bool{}
		for _, tgt := range targets {
			at, ok := typeOf[tgt]
			if !ok {
				t.Fatalf("derivation produced an unseeded target %q", tgt)
			}
			seen[at] = true
		}
		out := make([]string, 0, len(seen))
		for at := range seen {
			out = append(out, at)
		}
		sort.Strings(out)
		return out
	}

	// hotFiles — the whole file category.
	hot, err := d.hotFiles(ctx, pid, opts)
	if err != nil {
		t.Fatalf("hotFiles: %v", err)
	}
	var hotTargets []string
	for _, r := range hot {
		hotTargets = append(hotTargets, r.Data.(hotFileData).FilePath)
	}
	assertTypeSet(t, "hotFiles", typesOf(hotTargets...), fileActionTypes)

	// crossTool — the whole file category (two tools touched every target).
	cross, err := d.crossTool(ctx, pid, opts)
	if err != nil {
		t.Fatalf("crossTool: %v", err)
	}
	var crossTargets []string
	for _, r := range cross {
		crossTargets = append(crossTargets, r.Data.(crossToolData).FilePath)
	}
	assertTypeSet(t, "crossTool", typesOf(crossTargets...), fileActionTypes)

	// coChange — the file category MINUS read_file.
	co, err := d.coChange(ctx, pid, opts)
	if err != nil {
		t.Fatalf("coChange: %v", err)
	}
	var coTargets []string
	for _, r := range co {
		data := r.Data.(coChangeData)
		coTargets = append(coTargets, data.FileA, data.FileB)
	}
	assertTypeSet(t, "coChange", typesOf(coTargets...), mutatingFileActionTypes)

	// commonCommands — the whole cmd category.
	cmds, err := d.commonCommands(ctx, pid, opts)
	if err != nil {
		t.Fatalf("commonCommands: %v", err)
	}
	var cmdTargets []string
	for _, r := range cmds {
		cmdTargets = append(cmdTargets, r.Data.(commonCommandData).Command)
	}
	assertTypeSet(t, "commonCommands", typesOf(cmdTargets...), commandActionTypes)

	// editTestPairs — mutating file types on the edit side, cmd on the
	// test side. The union is the query scope; the edit halves prove the
	// read half of the file category never enters.
	pairs, err := d.editTestPairs(ctx, pid, opts)
	if err != nil {
		t.Fatalf("editTestPairs: %v", err)
	}
	var editTargets, cmdSides []string
	for _, r := range pairs {
		data := r.Data.(editTestData)
		editTargets = append(editTargets, data.EditTarget)
		cmdSides = append(cmdSides, data.TestCommand)
	}
	assertTypeSet(t, "editTestPairs(edit side)", typesOf(editTargets...), mutatingFileActionTypes)
	assertTypeSet(t, "editTestPairs(command side)", typesOf(cmdSides...), commandActionTypes)

	// onboardingSequences — read_file alone.
	onboard, err := d.onboardingSequences(ctx, pid, opts)
	if err != nil {
		t.Fatalf("onboardingSequences: %v", err)
	}
	var reads []string
	for _, r := range onboard {
		reads = append(reads, r.Data.(onboardingData).FirstReads...)
	}
	if len(reads) == 0 {
		t.Fatal("onboardingSequences produced no rows to check")
	}
	assertTypeSet(t, "onboardingSequences", typesOf(reads...), []string{tooltax.ActionReadFile})
}

func assertTypeSet(t *testing.T, site string, got, want []string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("%s: derivation produced no rows — the assertion would be vacuous", site)
	}
	w := append([]string(nil), want...)
	sort.Strings(w)
	if len(got) != len(w) {
		t.Fatalf("%s considered action types %v; want exactly %v", site, got, w)
	}
	for i := range got {
		if got[i] != w[i] {
			t.Fatalf("%s considered action types %v; want exactly %v", site, got, w)
		}
	}
}

// onlyProjectID returns the id of the single seeded project.
func onlyProjectID(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := database.QueryRowContext(context.Background(),
		`SELECT id FROM projects ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("project id: %v", err)
	}
	return id
}
