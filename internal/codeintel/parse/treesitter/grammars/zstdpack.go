//go:build ignore

// Command zstdpack compresses every codeintel_*.wasm in this directory to a
// co-located codeintel_*.wasm.zst, using the same pure-Go zstd codec
// (klauspost/compress) the runtime decompresses with (ADR-0006). build.sh runs
// it after compiling the grammar modules; the .wasm.zst blobs are what we
// commit + embed, and the raw .wasm is gitignored build output.
//
// Usage (from the repo root, WSL with Go on PATH):
//
//	go run ./internal/codeintel/parse/treesitter/grammars/zstdpack.go
//
// It is idempotent and reports the before/after sizes plus the compressed
// total against the 20MB embed budget.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/klauspost/compress/zstd"
)

const budgetBytes = 20 * 1024 * 1024

func main() {
	here, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	// Resolve to this file's directory so the tool works from the repo root.
	dir := filepath.Join(here, "internal", "codeintel", "parse", "treesitter", "grammars")
	if _, statErr := os.Stat(dir); statErr != nil {
		dir = here // already invoked from inside the grammars dir
	}

	wasms, err := filepath.Glob(filepath.Join(dir, "codeintel_*.wasm"))
	if err != nil {
		fail(err)
	}
	if len(wasms) == 0 {
		fail(fmt.Errorf("no codeintel_*.wasm found in %s (run build.sh first)", dir))
	}
	sort.Strings(wasms)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		fail(err)
	}
	defer enc.Close()

	var rawTotal, zstTotal int64
	for _, wasm := range wasms {
		raw, err := os.ReadFile(wasm)
		if err != nil {
			fail(err)
		}
		packed := enc.EncodeAll(raw, nil)
		out := wasm + ".zst"
		if err := os.WriteFile(out, packed, 0o644); err != nil {
			fail(err)
		}
		rawTotal += int64(len(raw))
		zstTotal += int64(len(packed))
		fmt.Printf("%-32s %8d -> %8d (%.2fx)\n",
			filepath.Base(out), len(raw), len(packed), float64(len(raw))/float64(len(packed)))
	}
	fmt.Printf("--- raw %d, compressed %d (%.2fx); budget %d ---\n",
		rawTotal, zstTotal, float64(rawTotal)/float64(zstTotal), budgetBytes)
	if zstTotal > budgetBytes {
		fail(fmt.Errorf("compressed embed %d exceeds %d budget", zstTotal, budgetBytes))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "zstdpack:", err)
	os.Exit(1)
}
