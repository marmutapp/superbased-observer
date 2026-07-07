package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/codeintel"
)

// -----------------------------------------------------------------------------
// search_symbols — project-wide full-text symbol search (Tier C).
//
// Wraps codeintel.Provider.Search (the FTS index over name / fqn /
// signature, camelCase + snake_case aware). Where get_symbols answers
// "give me the body of X in file F", search_symbols answers "where is the
// symbol that looks like Q across the project?" — discovery without
// spawning `grep` through the shell tool (whose output would be re-fed to
// the compressor). Returns metadata only (no bodies); the agent follows up
// with get_symbols for the bodies it wants.
//
// Always-on (registered in builtinTools) and fails open: an unavailable or
// not-yet-indexed project returns ok=true + degraded + the
// index_unavailable warning, never an error.
// -----------------------------------------------------------------------------

const (
	searchSymbolsDefaultLimit = 20
	searchSymbolsMaxLimit     = 100
)

type searchSymbolsTool struct {
	cg codeintel.Provider
}

func newSearchSymbolsTool(cg codeintel.Provider) Tool {
	if cg == nil {
		cg = codeintel.Unavailable()
	}
	return &searchSymbolsTool{cg: cg}
}

func (*searchSymbolsTool) Name() string { return "search_symbols" }

func (*searchSymbolsTool) Description() string {
	return "Project-wide symbol search over the code index (full-text on name / " +
		"fully-qualified name / signature, camelCase + snake_case aware). Use to " +
		"LOCATE a symbol when you don't know its file — far cheaper than `grep` " +
		"through the shell tool. Returns metadata only (name, kind, file, line); " +
		"follow up with get_symbols for the bodies. Omit `project_root` to search " +
		"every indexed project. When the index is missing/unavailable, `degraded` " +
		"is set and results are empty — run `observer index` on the project."
}

func (*searchSymbolsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search text matched against symbol name / fqn / signature. Required.",
			},
			"project_root": map[string]any{
				"type":        "string",
				"description": "Absolute path to the project root to scope the search. Omit to search across every indexed project.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"maximum":     searchSymbolsMaxLimit,
				"description": fmt.Sprintf("Max results. Default %d, capped at %d.", searchSymbolsDefaultLimit, searchSymbolsMaxLimit),
			},
		},
		"required": []string{"query"},
	}
}

type searchSymbolsArgs struct {
	Query       string `json:"query"`
	ProjectRoot string `json:"project_root"`
	Limit       int    `json:"limit"`
}

type searchSymbolsResult struct {
	OK       bool              `json:"ok"`
	Degraded bool              `json:"degraded,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	Results  []searchSymbolHit `json:"results"`
}

type searchSymbolHit struct {
	Name                string `json:"name"`
	FQN                 string `json:"fqn,omitempty"`
	Kind                string `json:"kind"`
	File                string `json:"file"`
	ProjectRelativePath string `json:"project_relative_path,omitempty"`
	Language            string `json:"language,omitempty"`
	StartLine           int    `json:"start_line"`
	EndLine             int    `json:"end_line"`
}

func (t *searchSymbolsTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchSymbolsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Query == "" {
		return nil, errors.New("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = searchSymbolsDefaultLimit
	}
	if limit > searchSymbolsMaxLimit {
		limit = searchSymbolsMaxLimit
	}

	out := searchSymbolsResult{OK: true, Results: []searchSymbolHit{}}
	if !t.cg.Available() {
		out.Degraded = true
		out.Warnings = appendWarning(out.Warnings, WarningIndexUnavailable)
		return out, nil
	}

	project := args.ProjectRoot
	if project != "" {
		if abs, err := filepath.Abs(project); err == nil {
			project = abs
		}
	}

	matches, err := t.cg.Search(ctx, project, args.Query, limit)
	if err != nil {
		// Provider methods fail open (nil-on-error), but defend in depth.
		out.Degraded = true
		out.Warnings = appendWarning(out.Warnings, WarningIndexUnavailable)
		return out, nil
	}
	for _, m := range matches {
		hit := searchSymbolHit{
			Name:      m.Name,
			FQN:       m.FQN,
			Kind:      m.Kind,
			File:      m.File,
			Language:  m.Language,
			StartLine: m.StartLine,
			EndLine:   m.EndLine,
		}
		if project != "" {
			if rel, relErr := filepath.Rel(project, m.File); relErr == nil {
				hit.ProjectRelativePath = filepath.ToSlash(rel)
			}
		}
		out.Results = append(out.Results, hit)
	}
	return out, nil
}
