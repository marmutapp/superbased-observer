package verbosity

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpusParity validates that the shipped SegmentVisible reproduces the
// design spike's GLOBAL ratios on the real transcript corpus (plan §11 P1).
// It is GUARDED by the VERBOSITY_CORPUS env var (a transcript root dir) and
// SKIPS in CI — same pattern as the predict harness. Run with:
//
//	VERBOSITY_CORPUS=/mnt/c/Users/<you>/.claude/projects go test \
//	    ./internal/verbosity/ -run TestCorpusParity -v
//
// Expected (from the 2026-06-30 spike): visible prose ~89.6% / fenced
// ~10.4%, untagged ~69% of fenced bytes.
func TestCorpusParity(t *testing.T) {
	root := os.Getenv("VERBOSITY_CORPUS")
	if root == "" {
		t.Skip("set VERBOSITY_CORPUS to a transcript root to run the parity check")
	}

	var narrative, artifact, untagged int64
	codeCat := map[string]int64{}
	files, msgs := 0, 0

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		files++
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			var ln struct {
				Type    string          `json:"type"`
				Message json.RawMessage `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.Type != "assistant" {
				continue
			}
			var m struct {
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(ln.Message, &m) != nil {
				continue
			}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			msgs++
			for _, b := range blocks {
				if b.Type != "text" {
					continue
				}
				vb := SegmentVisible(b.Text)
				narrative += vb.NarrativeBytes
				artifact += vb.ArtifactBytes
				untagged += vb.ArtifactUntaggedBytes
				for lang, by := range vb.ArtifactLang {
					if CategoryOf(lang) == Code {
						codeCat[lang] += by
					}
				}
			}
		}
		return nil
	})

	visible := narrative + artifact
	if visible == 0 {
		t.Fatalf("no visible text found under %q (files=%d)", root, files)
	}
	pct := func(n, d int64) float64 { return 100 * float64(n) / float64(d) }
	t.Logf("files=%d assistant-msgs=%d", files, msgs)
	t.Logf("SPIKE-EQUIVALENT: prose %.1f%% / fenced %.1f%% (want ~89.6 / ~10.4)",
		pct(narrative, visible), pct(artifact, visible))
	if artifact > 0 {
		t.Logf("untagged: %.1f%% of fenced bytes (want ~69)", pct(untagged, artifact))
	}
	var codeArtifact int64
	for _, b := range codeCat {
		codeArtifact += b
	}
	t.Logf("NEW MODEL: category=code artifacts = %d bytes (%.1f%% of fenced) — the rest is prose-ish",
		codeArtifact, pct(codeArtifact, artifact))

	// Parity guard: prose share must be in a sane band around the spike.
	if p := pct(narrative, visible); p < 80 || p > 95 {
		t.Errorf("prose share %.1f%% outside expected 80-95%% band — segmenter drift?", p)
	}
}
