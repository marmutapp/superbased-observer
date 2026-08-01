package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file is THE one owner of the listing-honesty rules. Both consumers
// use it:
//
//   - plugins/plugingen's tests, over the generated tree and the
//     hand-written OpenCode package
//     (TestListingsStateThePrerequisiteAndClaimNoSavings);
//   - scripts/assemble-plugins-repo.sh, over the FINAL assembled public
//     tree, by shelling out to `plugingen -honesty-check <dir>`.
//
// The assembler used to carry its own literal-phrase list, which meant the
// root README it authors itself was gated by the WEAKER half of the rule:
// "cuts context usage by 40%" contains none of the literal phrases and
// sailed through. One checker, one rule, every file.

// savingsPhrases are the literal claim shapes that must never appear.
var savingsPhrases = []string{
	"token savings", "save tokens", "saves tokens", "reduce your token",
	"cheaper turns", "savings", "compression saves", "less context",
}

// percentFigure matches a percentage figure written either way ("40%",
// "40 percent"). A bare "%" is deliberately NOT matched: the Cursor README
// legitimately contains a `printf '%s'` shell snippet, and prose says
// "percent-escaped".
var percentFigure = regexp.MustCompile(`(?i)\d+(?:\.\d+)?\s*(?:%|percent\b)`)

// efficiencyVocabulary is what turns a percentage into a SAVINGS claim.
var efficiencyVocabulary = []string{
	"token", "context", "cost", "spend", "usage", "bill", "cheaper",
	"faster", "smaller", "overhead", "compress",
}

// savingsClaim reports the first %-flavoured efficiency claim or literal
// banned phrase in body, or "" when the text is clean.
//
// Why a pattern class and not just the literal list: the compression
// -savings claim was RETRACTED after measurement (it measured +60% cost),
// and the shapes it could come back in are unbounded — "cuts context usage
// by 40%" contains none of the literal phrases. The rule is therefore:
// a percentage figure standing within a sentence's distance of
// token/context/cost/usage vocabulary is a savings claim.
func savingsClaim(body string) string {
	lower := strings.ToLower(body)
	for _, banned := range savingsPhrases {
		if strings.Contains(lower, banned) {
			return banned
		}
	}
	const window = 80
	for _, loc := range percentFigure.FindAllStringIndex(lower, -1) {
		start := loc[0] - window
		if start < 0 {
			start = 0
		}
		end := loc[1] + window
		if end > len(lower) {
			end = len(lower)
		}
		around := lower[start:end]
		for _, word := range efficiencyVocabulary {
			if strings.Contains(around, word) {
				return strings.TrimSpace(lower[loc[0]:loc[1]]) + " near " + word
			}
		}
	}
	return ""
}

// honestyCheckTree applies the full honesty rule to EVERY regular file
// under root:
//
//   - no savings claim, literal or %-pattern (savingsClaim);
//   - every README.md carries binaryPrereqSentence verbatim.
//
// It is the `-honesty-check` entry point scripts/assemble-plugins-repo.sh
// runs against the assembled public tree, so the landing page that script
// authors is gated by exactly the rule the generated files are.
//
// Every offending file is reported, not just the first: a gate that stops
// at one finding turns a review into a game of whack-a-mole.
func honestyCheckTree(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("plugingen -honesty-check: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("plugingen -honesty-check: %s is not a directory", root)
	}

	var problems []string
	scanned := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks and other non-regular entries are refused outright
			// by the assembler before this runs (a symlinked README would
			// otherwise smuggle unscanned prose into the tree). Reaching
			// one here means that guard was bypassed, so fail loudly
			// rather than skip.
			problems = append(problems, fmt.Sprintf("%s: not a regular file (%s) — the assembled tree must contain regular files only", path, d.Type()))
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // G304: path comes from walking the caller-supplied assembled tree.
		if readErr != nil {
			return readErr
		}
		scanned++
		body := string(raw)
		if claim := savingsClaim(body); claim != "" {
			problems = append(problems, fmt.Sprintf("%s: makes a savings claim (%s) — that claim is RETRACTED", path, claim))
		}
		if d.Name() == "README.md" && !strings.Contains(body, binaryPrereqSentence) {
			problems = append(problems, fmt.Sprintf("%s: does not carry the prerequisite sentence verbatim: %q", path, binaryPrereqSentence))
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("plugingen -honesty-check: %w", walkErr)
	}
	if scanned == 0 {
		return fmt.Errorf("plugingen -honesty-check: %s contains no files — refusing to report a vacuous pass", root)
	}
	if len(problems) > 0 {
		return errors.New("plugingen -honesty-check failed:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}
