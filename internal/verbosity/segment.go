package verbosity

import "strings"

// VisibleBreakdown is the result of segmenting one assistant visible-text
// block into narrative prose vs fenced (shown-artifact) content. All
// fields are byte counts (UTF-8, exact). The artifact bytes are split by
// the fence's resolved language; an untagged fence or an unrecognized tag
// is bucketed separately so the headline (which treats untagged shown
// content as prose-ish) and the §4 unknown-tag ledger both have honest
// inputs.
type VisibleBreakdown struct {
	// NarrativeBytes is content outside any fence (category=prose).
	NarrativeBytes int64
	// ArtifactBytes is the total inside fences (incl. the fence delimiter
	// lines): ArtifactBytes == sum(ArtifactLang) + ArtifactUntaggedBytes +
	// sum(ArtifactUnknownTags).
	ArtifactBytes int64
	// ArtifactUntaggedBytes is fenced content with NO info string.
	ArtifactUntaggedBytes int64
	// ArtifactLang maps a recognized canonical language → bytes of fenced
	// content tagged with it.
	ArtifactLang map[string]int64
	// ArtifactUnknownTags maps a raw, UNRECOGNIZED fence tag → bytes (the
	// fence analog of the unknown-ext ledger).
	ArtifactUnknownTags map[string]int64
}

// ArtifactByCategory rolls the fenced-content bytes up by category.
// Untagged fenced content is attributed to Prose (a shown artifact with no
// language is illustrative — part of the explanation, per the model);
// unrecognized-tag content is Unknown.
func (vb VisibleBreakdown) ArtifactByCategory() map[Category]int64 {
	out := map[Category]int64{}
	for lang, b := range vb.ArtifactLang {
		out[CategoryOf(lang)] += b
	}
	if vb.ArtifactUntaggedBytes > 0 {
		out[Prose] += vb.ArtifactUntaggedBytes
	}
	for _, b := range vb.ArtifactUnknownTags {
		out[Unknown] += b
	}
	return out
}

// artifact-bucket kinds for the in-fence accumulator.
const (
	bkUntagged = iota
	bkLang
	bkUnknown
)

// SegmentVisible runs the pure markdown fence scanner over one assistant
// visible-text block. It is a single-pass line state machine (no regex) —
// microsecond-cheap and safe on the proxy hot path. Only triple-backtick /
// triple-tilde fences are treated as code fences; inline `code` stays prose
// (it is identifiers/paths, not shown blocks).
func SegmentVisible(text string) VisibleBreakdown {
	vb := VisibleBreakdown{
		ArtifactLang:        map[string]int64{},
		ArtifactUnknownTags: map[string]int64{},
	}
	var (
		inFence   bool
		fenceChar byte
		fenceLen  int
		bkKind    int
		bkKey     string
	)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		// Byte length of this line including the newline that Split
		// removed, except for the final element (no trailing newline).
		n := int64(len(l))
		if i < len(lines)-1 {
			n++
		}
		ch, runLen, info, isFence := fenceDelim(l)
		if !inFence {
			if isFence {
				inFence = true
				fenceChar = ch
				fenceLen = runLen
				bkKind, bkKey = classifyTag(info)
				vb.addArtifact(bkKind, bkKey, n)
			} else {
				vb.NarrativeBytes += n
			}
			continue
		}
		// Inside a fence. The line (content or closing delimiter) belongs
		// to the block. A closing fence is a delimiter of the same char, at
		// least as long, with no info string (CommonMark).
		vb.addArtifact(bkKind, bkKey, n)
		if isFence && ch == fenceChar && runLen >= fenceLen && info == "" {
			inFence = false
		}
	}
	return vb
}

// addArtifact adds n bytes to ArtifactBytes and the specific in-fence
// bucket. (Go's map[key] += n is valid — no addressable-pointer needed.)
func (vb *VisibleBreakdown) addArtifact(kind int, key string, n int64) {
	vb.ArtifactBytes += n
	switch kind {
	case bkLang:
		vb.ArtifactLang[key] += n
	case bkUnknown:
		vb.ArtifactUnknownTags[key] += n
	default: // bkUntagged
		vb.ArtifactUntaggedBytes += n
	}
}

// classifyTag maps a fence info string to an accumulator bucket: a
// recognized language → (bkLang, canonical name); an unrecognized tag →
// (bkUnknown, raw lowercased tag); no tag → (bkUntagged, "").
func classifyTag(info string) (kind int, key string) {
	tag := firstToken(info)
	if tag == "" {
		return bkUntagged, ""
	}
	if lang, ok := LangForTag(tag); ok {
		return bkLang, lang.Name
	}
	return bkUnknown, strings.ToLower(tag)
}

// fenceDelim reports whether a line is a markdown fence delimiter (>=3
// backticks or tildes after <=3 spaces of indent), returning the fence
// char, its run length, and the trailing info string.
func fenceDelim(l string) (ch byte, runLen int, info string, ok bool) {
	i := 0
	for i < len(l) && l[i] == ' ' && i < 3 {
		i++
	}
	if i >= len(l) || (l[i] != '`' && l[i] != '~') {
		return 0, 0, "", false
	}
	c := l[i]
	j := i
	for j < len(l) && l[j] == c {
		j++
	}
	if j-i < 3 {
		return 0, 0, "", false
	}
	return c, j - i, strings.TrimSpace(l[j:]), true
}

// firstToken returns the leading whitespace-delimited token of s (the
// fence language tag), or "" when s is empty/whitespace.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
