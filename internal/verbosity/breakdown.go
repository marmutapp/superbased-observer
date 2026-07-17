package verbosity

import "strings"

// Breakdown is the full output-composition picture for a scope (a session, a
// model, an aggregate): visible text (narrative + fenced shown-artifacts)
// PLUS authored code (file writes + shell commands), language-resolved. The
// store seam assembles it by folding in each assistant-text segmentation and
// each authored-code action; this package owns the rollup math.
type Breakdown struct {
	// Visible is the accumulated narrative + fenced-artifact segmentation
	// across the scope's assistant-text turns.
	Visible VisibleBreakdown
	// Written maps canonical language → bytes authored via file-write/edit
	// actions (resolved from the file path).
	Written map[string]int64
	// Command maps shell dialect → bytes authored via run_command actions.
	Command map[string]int64
	// WrittenUnknownExt is the ledger: raw extension → bytes for file writes
	// whose path did not resolve to a known language (drives the §4 close-out).
	WrittenUnknownExt map[string]int64
}

// NewBreakdown returns a ready-to-accumulate Breakdown.
func NewBreakdown() *Breakdown {
	return &Breakdown{
		Visible:           VisibleBreakdown{ArtifactLang: map[string]int64{}, ArtifactUnknownTags: map[string]int64{}},
		Written:           map[string]int64{},
		Command:           map[string]int64{},
		WrittenUnknownExt: map[string]int64{},
	}
}

// Add merges other into vb (summing every bucket). Accumulates a
// VisibleBreakdown across many assistant-text turns.
func (vb *VisibleBreakdown) Add(o VisibleBreakdown) {
	vb.NarrativeBytes += o.NarrativeBytes
	vb.ArtifactBytes += o.ArtifactBytes
	vb.ArtifactUntaggedBytes += o.ArtifactUntaggedBytes
	if vb.ArtifactLang == nil {
		vb.ArtifactLang = map[string]int64{}
	}
	for k, v := range o.ArtifactLang {
		vb.ArtifactLang[k] += v
	}
	if vb.ArtifactUnknownTags == nil {
		vb.ArtifactUnknownTags = map[string]int64{}
	}
	for k, v := range o.ArtifactUnknownTags {
		vb.ArtifactUnknownTags[k] += v
	}
}

// AddVisibleText segments one assistant visible-text block and folds it in.
func (b *Breakdown) AddVisibleText(text string) {
	b.Visible.Add(SegmentVisible(text))
}

// AddWrite attributes file-write/edit bytes to a language by resolving the
// path; unresolved paths feed the unknown-ext ledger.
func (b *Breakdown) AddWrite(path string, bytes int64) {
	if bytes <= 0 {
		return
	}
	if lang, ok := FileType(path); ok {
		b.Written[lang.Name] += bytes
		return
	}
	b.WrittenUnknownExt[extOf(path)] += bytes
}

// AddCommand attributes run_command bytes to a shell dialect resolved from
// the raw tool name (Bash/PowerShell/pwsh/cmd/sh → canonical), defaulting to
// a generic shell when the name is unknown. Commands are always category=code.
func (b *Breakdown) AddCommand(rawToolName string, bytes int64) {
	if bytes <= 0 {
		return
	}
	lang := "bash"
	if l, ok := LangForTag(strings.ToLower(strings.TrimSpace(rawToolName))); ok && l.Category == Code {
		lang = l.Name
	}
	b.Command[lang] += bytes
}

// CodeBytes is the headline "code" total: every category=code byte across
// authored writes, shell commands, AND fenced code shown-artifacts (D4 —
// shell/code shown blocks count as code).
func (b *Breakdown) CodeBytes() int64 {
	var c int64
	for lang, by := range b.Written {
		if CategoryOf(lang) == Code {
			c += by
		}
	}
	for _, by := range b.Command {
		c += by
	}
	for lang, by := range b.Visible.ArtifactLang {
		if CategoryOf(lang) == Code {
			c += by
		}
	}
	return c
}

// ExplainBytes is the headline "explanation" total: narrative prose plus
// prose-ish shown artifacts (untagged + prose-category + unrecognized-tag
// fences). Code/docs/config/data writes are neither — see ByCategory.
func (b *Breakdown) ExplainBytes() int64 {
	e := b.Visible.NarrativeBytes + b.Visible.ArtifactUntaggedBytes
	for lang, by := range b.Visible.ArtifactLang {
		if CategoryOf(lang) == Prose {
			e += by
		}
	}
	for _, by := range b.Visible.ArtifactUnknownTags {
		e += by
	}
	return e
}

// CodeByLang returns code bytes per language across writes, commands, and
// code shown-artifacts (the operator's "what languages are coming through").
func (b *Breakdown) CodeByLang() map[string]int64 {
	out := map[string]int64{}
	for lang, by := range b.Written {
		if CategoryOf(lang) == Code {
			out[lang] += by
		}
	}
	for lang, by := range b.Command {
		out[lang] += by
	}
	for lang, by := range b.Visible.ArtifactLang {
		if CategoryOf(lang) == Code {
			out[lang] += by
		}
	}
	return out
}

// ByCategory rolls EVERY authored + visible byte up by category, so a view
// can show code vs docs vs config vs data vs prose without re-deriving.
func (b *Breakdown) ByCategory() map[Category]int64 {
	out := map[Category]int64{}
	out[Prose] += b.Visible.NarrativeBytes
	for c, by := range b.Visible.ArtifactByCategory() {
		out[c] += by
	}
	for lang, by := range b.Written {
		out[CategoryOf(lang)] += by
	}
	for _, by := range b.Command {
		out[Code] += by
	}
	for _, by := range b.WrittenUnknownExt {
		out[Unknown] += by
	}
	return out
}

// extOf returns the lowercased extension (with dot) of a path for the
// unknown-ext ledger, or "(none)" when the basename has no extension.
func extOf(path string) string {
	p := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		return base[dot:]
	}
	return "(none)"
}
