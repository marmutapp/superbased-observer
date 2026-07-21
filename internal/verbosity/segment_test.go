package verbosity

import "testing"

func sumMap(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// TestSegmentExactByteAccounting is the load-bearing invariant: every byte
// of the input lands in exactly one bucket. NarrativeBytes + ArtifactBytes
// must equal len(text), and ArtifactBytes must equal the sum of its
// sub-buckets — this catches any off-by-one in the newline accounting.
func TestSegmentExactByteAccounting(t *testing.T) {
	text := "Here is some prose.\n" +
		"```go\nfunc main() {}\n```\n" +
		"More prose explaining things.\n" +
		"```\nuntagged shown block\n```\n" +
		"```bash\necho hi\n```\n" +
		"```xyzzy\nmystery\n```\n" +
		"End."

	vb := SegmentVisible(text)

	if got := vb.NarrativeBytes + vb.ArtifactBytes; got != int64(len(text)) {
		t.Fatalf("narrative+artifact = %d, want len(text) = %d", got, len(text))
	}
	subSum := sumMap(vb.ArtifactLang) + vb.ArtifactUntaggedBytes + sumMap(vb.ArtifactUnknownTags)
	if subSum != vb.ArtifactBytes {
		t.Fatalf("artifact sub-buckets = %d, want ArtifactBytes = %d", subSum, vb.ArtifactBytes)
	}

	if vb.ArtifactLang["go"] == 0 {
		t.Error("expected go artifact bytes")
	}
	if vb.ArtifactLang["bash"] == 0 {
		t.Error("expected bash artifact bytes (shell = code)")
	}
	if vb.ArtifactUntaggedBytes == 0 {
		t.Error("expected untagged artifact bytes")
	}
	if vb.ArtifactUnknownTags["xyzzy"] == 0 {
		t.Error("expected unknown-tag ledger entry for xyzzy")
	}

	cat := vb.ArtifactByCategory()
	if cat[Code] != vb.ArtifactLang["go"]+vb.ArtifactLang["bash"] {
		t.Errorf("category Code = %d, want go+bash = %d", cat[Code], vb.ArtifactLang["go"]+vb.ArtifactLang["bash"])
	}
	if cat[Prose] != vb.ArtifactUntaggedBytes {
		t.Errorf("category Prose = %d, want untagged = %d", cat[Prose], vb.ArtifactUntaggedBytes)
	}
	if cat[Unknown] != vb.ArtifactUnknownTags["xyzzy"] {
		t.Errorf("category Unknown = %d, want xyzzy = %d", cat[Unknown], vb.ArtifactUnknownTags["xyzzy"])
	}
}

func TestSegmentPureNarrative(t *testing.T) {
	text := "Just an explanation with `inline code` and no fences.\nSecond line."
	vb := SegmentVisible(text)
	if vb.ArtifactBytes != 0 {
		t.Errorf("ArtifactBytes = %d, want 0 (inline code stays prose)", vb.ArtifactBytes)
	}
	if vb.NarrativeBytes != int64(len(text)) {
		t.Errorf("NarrativeBytes = %d, want %d", vb.NarrativeBytes, len(text))
	}
}

func TestSegmentUnterminatedFence(t *testing.T) {
	// A fence opened but never closed: the remainder is all artifact.
	text := "prose\n```go\nfunc x() {}\nstill in fence"
	vb := SegmentVisible(text)
	if vb.NarrativeBytes != int64(len("prose")+1) { // "prose\n"
		t.Errorf("NarrativeBytes = %d, want %d", vb.NarrativeBytes, len("prose")+1)
	}
	if vb.ArtifactLang["go"] == 0 || vb.ArtifactLang["go"] != vb.ArtifactBytes {
		t.Errorf("unterminated go fence: ArtifactLang[go]=%d ArtifactBytes=%d", vb.ArtifactLang["go"], vb.ArtifactBytes)
	}
}

func TestSegmentTildeFence(t *testing.T) {
	text := "~~~python\nprint(1)\n~~~\n"
	vb := SegmentVisible(text)
	if vb.ArtifactLang["python"] == 0 {
		t.Errorf("expected python artifact bytes from tilde fence, got %+v", vb)
	}
}
