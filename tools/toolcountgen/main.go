// Command toolcountgen emits website/tools/tool-count-manifest.json — the
// machine-readable adapter-count manifest that closes the tool-count-drift
// finding from the wave-1 backlog (item B4a): user-facing surfaces have
// separately claimed 31, 30, 29, and 26 tools/adapters at different times,
// and the ONLY place the canonical number lived was a human-authored prose
// line in CLAUDE.md ("Platform adapters (N): ...") that
// website/tools/accuracy-check.mjs regexed out with no independent source to
// cross-check it against.
//
// # Why a generator, not a hand-maintained constant
//
// internal/integration's Capability registry (internal/integration/
// integration.go) is the one-owner source of "what adapters exist" — but its
// row COUNT is not the number CLAUDE.md's Platform-adapters line reports,
// because that line is an EDITORIAL count: "one shipped package = one
// adapter, regardless of how many site/tool variants or registry rows it
// covers" (see website/tools/accuracy-check.mjs's own header comment, which
// first worked this out by hand while building the accuracy-check gate).
// Three registry-row groups don't map 1:1 to the editorial count:
//
//   - antigravity + antigravity-cli (2 registry rows, one shipped product
//     with two capture backends — desktop .pb vs the `agy` CLI) fold to a
//     SINGLE editorial adapter.
//   - kilo-code + kilo-code-cli (2 registry rows) are EXPLICITLY
//     special-cased in CLAUDE.md's line as "(legacy+CLI)" and counted as
//     TWO — the one group that is NOT folded.
//   - chatgpt-web, claude-web, perplexity-web, gemini-web, copilot-web (5
//     registry rows, all served by the single internal/adapter/browserchat
//     package, site = data discriminator) fold to a SINGLE editorial
//     "browserchat" adapter.
//
// Every other registry row is its own one-row, weight-1 family.
//
// This generator encodes that fold as EXPLICIT DATA (foldGroups below) and
// computes the editorial adapterCount from it plus the live registry, so:
//
//   - a new adapter that ships as its own package needs NO change here (it
//     falls into defaultFamilies as a weight-1 family automatically);
//   - a new *variant* of an EXISTING folded family (e.g. a 6th browserchat
//     site) is invisible to the editorial count on purpose, matching
//     CLAUDE.md's own convention;
//   - if CLAUDE.md's fold conventions ever change, foldGroups is the one
//     place to update, and the manifest + every surface it feeds
//     regenerates in lockstep.
//
// website/tools/accuracy-check.mjs reads adapterCount out of the emitted
// manifest and treats CLAUDE.md's own "Platform adapters (N)" regex as an
// independent CROSS-CHECK: the two must agree, or the check fails loudly.
// Neither source is allowed to silently drift alone.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// defaultOutDir is the directory holding the committed artifact, relative
// to the repo root (the directory `make tool-count-build` runs from). The
// drift gate points it at a scratch dir instead.
const defaultOutDir = "website/tools"

// manifestName is the generated file's base name. The generator owns the
// name; the caller only chooses the directory.
const manifestName = "tool-count-manifest.json"

// foldGroup is one editorial fold: N registry rows (Members) counted as
// Weight editorial adapters. Weight is stated explicitly (never derived as
// len(Members)) so a reader — or a future editor — sees the actual
// convention at the call site instead of having to infer it.
type foldGroup struct {
	// Key is the family's manifest key: the editorial adapter's canonical
	// name, chosen to read naturally in a table (not necessarily one of
	// the registry Tool strings it groups).
	Key string
	// Members are the internal/integration registry Tool keys this family
	// covers. Every member must exist in the live registry (checked below)
	// so a renamed or removed adapter fails the build instead of silently
	// shrinking the count.
	Members []string
	// Weight is how many editorial adapters this family counts as.
	Weight int
	// Note documents WHY this group folds the way it does, sourced from
	// CLAUDE.md's own Platform-adapters line and internal/integration's
	// row comments — never fabricated.
	Note string
}

// foldGroups is every registry-row grouping whose editorial weight is NOT
// simply "one row, one adapter". Any registry tool NOT named here becomes
// its own one-row, weight-1 family automatically (see buildFamilies).
//
// This is the explicit data CLAUDE.md's own prose line encodes implicitly —
// see the package doc comment above for the full reasoning per group.
var foldGroups = []foldGroup{
	{
		Key:     "antigravity",
		Members: []string{"antigravity", "antigravity-cli"},
		Weight:  1,
		Note: "One shipped product (Antigravity) with two capture backends: the " +
			"encrypted desktop .pb store and the newer `agy` CLI's plaintext-" +
			"protobuf SQLite .db. CLAUDE.md's Platform-adapters line lists this " +
			"as a single \"antigravity [...]\" item, folding both registry rows " +
			"down to one editorial adapter.",
	},
	{
		Key:     "kilo-code",
		Members: []string{"kilo-code", "kilo-code-cli"},
		Weight:  2,
		Note: "The one group CLAUDE.md explicitly does NOT fold: its Platform-" +
			"adapters line spells this out as \"kilocode (legacy+CLI)\", counting " +
			"the legacy Kilo Code IDE extension and the current Kilo Code CLI as " +
			"two separate editorial adapters even though they're described " +
			"together in one list item.",
	},
	{
		Key: "browserchat",
		Members: []string{
			"chatgpt-web", "claude-web", "perplexity-web", "gemini-web", "copilot-web",
		},
		Weight: 1,
		Note: "Five site variants (site = data discriminator), ALL served by the " +
			"single internal/adapter/browserchat package via one MV3 native-" +
			"messaging bridge. CLAUDE.md's Platform-adapters line lists this as " +
			"one \"browserchat [...]\" item, folding all five registry rows down " +
			"to one editorial adapter.",
	},
}

// family is one resolved editorial adapter: a set of registry rows counted
// as Weight.
type family struct {
	Key     string   `json:"key"`
	Members []string `json:"members"`
	Weight  int      `json:"weight"`
	Note    string   `json:"note,omitempty"`
}

// doc is the generated manifest's envelope. Field order here IS the key
// order in the output; do not reorder casually.
type doc struct {
	Generated generatedMeta `json:"generated"`
	// RegistryRowCount is len(internal/integration.Tools()) at generation
	// time — the raw, unfolded row count.
	RegistryRowCount int `json:"registryRowCount"`
	// Families is every resolved editorial adapter, sorted by Key. Their
	// Weights sum to AdapterCount.
	Families []family `json:"families"`
	// AdapterCount is the editorial adapter count: sum of every family's
	// Weight. This is the number website/tools/accuracy-check.mjs cross-
	// checks against CLAUDE.md's own "Platform adapters (N)" line.
	AdapterCount int `json:"adapterCount"`
}

type generatedMeta struct {
	By         string `json:"by"`
	From       string `json:"from"`
	Regenerate string `json:"regenerate"`
	Gate       string `json:"gate"`
	DoNotEdit  bool   `json:"doNotEdit"`
}

func main() {
	outDir := flag.String("outdir", defaultOutDir, "directory the generated manifest is written to")
	flag.Parse()

	data, err := generate()
	if err != nil {
		log.Fatalf("toolcountgen: %v", err)
	}
	if err := write(filepath.Join(*outDir, manifestName), data); err != nil {
		log.Fatalf("toolcountgen: %v", err)
	}
}

// generate builds the manifest's bytes from internal/integration plus the
// explicit fold-group data above. It is pure: no filesystem beyond the
// registry read, no clock, no environment — which is what makes the drift
// gate meaningful.
func generate() ([]byte, error) {
	rows := integration.Tools() // sorted, deterministic
	if len(rows) == 0 {
		return nil, fmt.Errorf("internal/integration.Tools() is empty — refusing to emit a vacuous manifest")
	}

	families, err := buildFamilies(rows)
	if err != nil {
		return nil, err
	}

	adapterCount := 0
	for _, f := range families {
		adapterCount += f.Weight
	}

	d := doc{
		Generated:        meta(),
		RegistryRowCount: len(rows),
		Families:         families,
		AdapterCount:     adapterCount,
	}
	return encodeJSON(d)
}

// buildFamilies partitions every live registry row into families: the
// explicit foldGroups first, then every remaining row as its own weight-1
// family. It fails loudly (rather than silently under- or double-counting)
// when:
//   - a foldGroups member no longer exists in the live registry (a renamed
//     or removed adapter that foldGroups wasn't updated for), or
//   - a registry row is claimed by more than one foldGroup.
func buildFamilies(rows []string) ([]family, error) {
	rowSet := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		rowSet[r] = struct{}{}
	}

	claimed := make(map[string]string, len(rows)) // registry row -> family key
	families := make([]family, 0, len(rows))

	for _, g := range foldGroups {
		if len(g.Members) == 0 {
			return nil, fmt.Errorf("fold group %q declares no members", g.Key)
		}
		for _, m := range g.Members {
			if _, ok := rowSet[m]; !ok {
				return nil, fmt.Errorf(
					"fold group %q claims registry row %q, which no longer exists in "+
						"internal/integration — update foldGroups", g.Key, m)
			}
			if prior, dup := claimed[m]; dup {
				return nil, fmt.Errorf(
					"registry row %q is claimed by both fold group %q and %q", m, prior, g.Key)
			}
			claimed[m] = g.Key
		}
		families = append(families, family{
			Key:     g.Key,
			Members: append([]string(nil), g.Members...),
			Weight:  g.Weight,
			Note:    g.Note,
		})
	}

	// Every unclaimed row is its own one-row, weight-1 family.
	for _, r := range rows {
		if _, ok := claimed[r]; ok {
			continue
		}
		families = append(families, family{
			Key:     r,
			Members: []string{r},
			Weight:  1,
		})
	}

	sort.Slice(families, func(i, j int) bool { return families[i].Key < families[j].Key })
	return families, nil
}

// meta is the manifest's provenance block.
func meta() generatedMeta {
	return generatedMeta{
		By:         "tools/toolcountgen",
		From:       "internal/integration.Tools()",
		Regenerate: "make tool-count-build",
		Gate:       "make verify-tool-count-build",
		DoNotEdit:  true,
	}
}

// encodeJSON: 2-space indent, no HTML escaping, exactly one trailing
// newline — the same shape web/taxgen and website/track-gen use.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), nil
}

// write puts the bytes at path, creating the parent directory when the
// gate points it at a scratch location.
func write(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	//nolint:gosec // G306: a committed, publicly-distributed source artifact; world-readable is intended.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
