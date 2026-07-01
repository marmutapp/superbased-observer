package treesitter

import (
	"io/fs"
	"strings"
	"testing"
)

// embedBudgetBytes is the ≤20MB/platform-binary embedded-grammar budget
// (plan §7.2; docs/codeintel/benchmarks.md, limitations.md). The W1 relief
// valve (ADR-0006) makes this gate measure the COMPRESSED *.wasm.zst embed —
// the bytes that actually ship in the binary — not the raw .wasm. The raw
// .wasm is gitignored build output and is never embedded.
const embedBudgetBytes = 20 * 1024 * 1024

// TestEmbeddedGrammarsWithinBudget is the CI budget gate: the total size of
// every embedded grammar artifact (the compressed *.wasm.zst blobs) must stay
// within embedBudgetBytes. Onboarding a grammar that would blow the budget
// fails here loudly. The decompressed on-disk/in-memory footprint is larger
// (~5.6x for the current set) and is documented separately in benchmarks.md;
// it never ships in the binary.
func TestEmbeddedGrammarsWithinBudget(t *testing.T) {
	var total int64
	err := fs.WalkDir(grammarFS, "grammars", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".wasm.zst") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk grammarFS: %v", err)
	}
	if total == 0 {
		t.Fatal("no embedded *.wasm.zst grammars found")
	}
	t.Logf("compressed embedded grammars: %d bytes (%.2f MB) of %d budget",
		total, float64(total)/(1024*1024), embedBudgetBytes)
	if total > embedBudgetBytes {
		t.Errorf("embedded grammar size %d bytes exceeds %d budget (%.2f MB > %.0f MB) — add a relief valve or demote a grammar",
			total, embedBudgetBytes,
			float64(total)/(1024*1024), float64(embedBudgetBytes)/(1024*1024))
	}
}
