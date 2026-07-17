package routingapply

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/routing"
)

// BackupPrefix is the suffix marker observer backups carry:
// <file>.bak-observer-<stamp>. Exported so frontends can render backup
// paths without re-deriving the convention.
const BackupPrefix = ".bak-observer-"

// Tool support modes per the §R10.2 matrix.
const (
	// ModeWritable — observer writes the tool's config directly
	// (claude-code per-subagent frontmatter; mechanical + reversible).
	ModeWritable = "writable"
	// ModeSnippet — observer emits an exact native-config snippet for
	// the operator to paste (the spec's third emission mode).
	ModeSnippet = "snippet"
	// ModeAdvisory — config not reliably file-addressable / org-side
	// policy; evidence only, no snippet.
	ModeAdvisory = "advisory"
)

// ToolMode resolves a tool name to its §R10.2 support mode. ok=false
// for unknown tools — callers reject with Tools() in the message.
func ToolMode(tool string) (string, bool) {
	switch tool {
	case "claude-code":
		return ModeWritable, true
	case "codex", "aider", "zed", "kilo-code", "cline", "opencode":
		return ModeSnippet, true
	case "cursor", "copilot":
		return ModeAdvisory, true
	}
	return "", false
}

// Tools lists the closed §R10.2 tool vocabulary in matrix order.
func Tools() []string {
	return []string{"claude-code", "codex", "aider", "zed", "kilo-code", "cline", "opencode", "cursor", "copilot"}
}

// Change is one proposed per-subagent frontmatter edit. JSON tags are
// the dashboard wire shape.
type Change struct {
	Path string `json:"path"`
	// Agent is the persona id (the file's basename without .md).
	Agent string `json:"agent"`
	// FromModel "" means the frontmatter has no model line (inherits).
	FromModel string `json:"from_model"`
	ToModel   string `json:"to_model"`
	Rationale string `json:"rationale"`
}

// Stamp renders the canonical backup timestamp for a write batch.
func Stamp(now time.Time) string {
	return now.UTC().Format("20060102T150405Z")
}

// Plan matches recommendations to agent files under dirs and plans the
// idempotent edits. Pure given the dirs — testable with temp dirs.
// Recommendations without a SuggestedModel (keeps / insufficient
// evidence) are silently passed over; actionable-but-unmatchable ones
// land in skipped with the reason.
func Plan(dirs []string, recs []routing.SubagentRecommendation) (changes []Change, skipped []string) {
	for _, rec := range recs {
		if rec.SuggestedModel == "" {
			continue // keep / insufficient evidence — nothing to write
		}
		path, ok := findAgentFile(dirs, rec.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf("%s: suggested %s but no agent file found under %s", rec.Name, rec.SuggestedModel, strings.Join(dirs, ", ")))
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", rec.Name, err))
			continue
		}
		current, hasFM := frontmatterModel(string(body))
		if !hasFM {
			skipped = append(skipped, fmt.Sprintf("%s: %s has no frontmatter block — not edited (add one manually)", rec.Name, path))
			continue
		}
		if current == rec.SuggestedModel {
			continue // idempotent: already at target
		}
		changes = append(changes, Change{
			Path: path, Agent: rec.Name,
			FromModel: current, ToModel: rec.SuggestedModel,
			Rationale: rec.Rationale,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, skipped
}

func findAgentFile(dirs []string, name string) (string, bool) {
	for _, dir := range dirs {
		path := filepath.Join(dir, name+".md")
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// frontmatterModel extracts the model: value from a leading YAML
// frontmatter block. hasFrontmatter=false when the file has no block.
func frontmatterModel(body string) (model string, hasFrontmatter bool) {
	fm, _, ok := splitFrontmatter(body)
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(fm, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "model:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "model:")), true
		}
	}
	return "", true
}

// splitFrontmatter splits a "---\n...\n---\n" leading block.
func splitFrontmatter(body string) (fm, rest string, ok bool) {
	if !strings.HasPrefix(body, "---\n") && !strings.HasPrefix(body, "---\r\n") {
		return "", "", false
	}
	inner := body[strings.Index(body, "\n")+1:]
	fm, rest, found := strings.Cut(inner, "\n---")
	if !found {
		return "", "", false
	}
	// Drop the remainder of the closing --- line.
	if i := strings.Index(rest, "\n"); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = ""
	}
	return fm, rest, true
}

// Write performs one backup-then-edit. The model: line is replaced in
// place, or appended to the frontmatter when absent — every other byte
// of the file is preserved. The backup lands at
// <path>.bak-observer-<stamp>; same-batch writes share one stamp.
func Write(c Change, stamp string) error {
	body, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.Path+BackupPrefix+stamp, body, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	fm, rest, ok := splitFrontmatter(string(body))
	if !ok {
		return fmt.Errorf("frontmatter disappeared between plan and write")
	}
	lines := strings.Split(fm, "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "model:") {
			lines[i] = "model: " + c.ToModel
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, "model: "+c.ToModel)
	}
	updated := "---\n" + strings.Join(lines, "\n") + "\n---\n" + rest
	return os.WriteFile(c.Path, []byte(updated), 0o600)
}

// Backup is one restorable observer backup: the NEWEST backup of an
// agent file (older stamps exist on disk but revert always restores the
// newest, matching the CLI's contract).
type Backup struct {
	Path   string `json:"path"`
	Backup string `json:"backup"`
	Stamp  string `json:"stamp"`
}

// ListBackups returns the newest observer backup per agent file under
// dirs, sorted by path. Unreadable dirs are skipped (an absent agents
// dir is the normal case, not an error).
func ListBackups(dirs []string) []Backup {
	var out []Backup
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		newest := map[string]string{} // original name → newest backup name
		for _, e := range entries {
			name := e.Name()
			idx := strings.Index(name, BackupPrefix)
			if idx < 0 {
				continue
			}
			orig := name[:idx]
			if prev, ok := newest[orig]; !ok || name > prev {
				newest[orig] = name
			}
		}
		for orig, backup := range newest {
			out = append(out, Backup{
				Path:   filepath.Join(dir, orig),
				Backup: filepath.Join(dir, backup),
				Stamp:  strings.TrimPrefix(backup, orig+BackupPrefix),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Restored is one completed revert: Path was overwritten with Backup's
// content.
type Restored struct {
	Path   string `json:"path"`
	Backup string `json:"backup"`
}

// RevertFile restores ONE agent file from its newest observer backup.
// A missing backup returns an error wrapping os.ErrNotExist so HTTP
// frontends can 404 it.
func RevertFile(path string) (Restored, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	var newest string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Restored{}, fmt.Errorf("routingapply.RevertFile: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+BackupPrefix) {
			continue
		}
		if name > newest {
			newest = name
		}
	}
	if newest == "" {
		return Restored{}, fmt.Errorf("routingapply.RevertFile: no observer backup for %s: %w", path, os.ErrNotExist)
	}
	backupPath := filepath.Join(dir, newest)
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return Restored{}, fmt.Errorf("routingapply.RevertFile: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Restored{}, fmt.Errorf("routingapply.RevertFile: %w", err)
	}
	return Restored{Path: path, Backup: backupPath}, nil
}

// Revert restores every agent file under dirs from its NEWEST observer
// backup (the CLI's --revert contract). Files without backups are left
// alone; an empty result means nothing was restorable.
func Revert(dirs []string) ([]Restored, error) {
	var out []Restored
	for _, b := range ListBackups(dirs) {
		res, err := RevertFile(b.Path)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// WeakModel picks the cheap/housekeeping model the evidence supports
// (the first downshift suggestion), defaulting to the haiku
// representative when no recommendation carries one.
func WeakModel(recs []routing.SubagentRecommendation) string {
	for _, rec := range recs {
		if rec.SuggestedModel != "" {
			return rec.SuggestedModel
		}
	}
	return "claude-haiku-4-5"
}

// AdvisorySnippet returns the paste-able native-config snippet for
// tools observer doesn't write directly (§R10.2 third emission mode).
// ok=false for tools without one (claude-code is written, not pasted;
// cursor/copilot are advisory-only).
func AdvisorySnippet(tool, weak string) (string, bool) {
	switch tool {
	case "codex":
		return fmt.Sprintf("# ~/.codex/config.toml\n[profiles.observer-frugal]\nmodel = %q\n\n# run: codex --profile observer-frugal for housekeeping-class work\n", weak), true
	case "aider":
		return fmt.Sprintf("# ~/.aider.conf.yml\nweak-model: %s    # commits/summaries (Aider's weak-model class)\n# editor-model: <main editing model>\n", weak), true
	case "zed":
		return fmt.Sprintf("// settings.json → assistant profiles\n\"assistant\": { \"inline_assistant_model\": %q }\n", weak), true
	case "kilo-code":
		return fmt.Sprintf("// Kilo: set the per-mode model for low-stakes modes\n// Settings → Modes → (Architect stays frontier) → set Code/Debug fallback to %q\n", weak), true
	case "cline", "opencode":
		return fmt.Sprintf("// %s: set the per-mode model in the tool's provider settings\n// low-stakes mode model: %q\n", tool, weak), true
	}
	return "", false
}
