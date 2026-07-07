package antigravity

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// TestProbeWalk dumps a depth-aware summary of fields seen in a
// protobuf payload. Used during Path B reverse-engineering of
// LanguageServerService methods we don't yet have a client for.
// Skipped unless ANTIGRAVITY_PROBE_FILE points at a binary file.
func TestProbeWalk(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_PROBE_FILE")
	if path == "" {
		t.Skip("set ANTIGRAVITY_PROBE_FILE=/tmp/probe.bin to run")
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	type stat struct {
		path     []int
		count    int
		wireType protowire.WireType
		samples  []string
	}
	stats := map[string]*stat{}
	pathKey := func(p []int) string {
		parts := make([]string, len(p))
		for i, n := range p {
			parts[i] = fmt.Sprintf("%d", n)
		}
		return strings.Join(parts, ".")
	}
	_ = protowire.Walk(buf, func(f protowire.Field) error {
		k := pathKey(f.Path)
		s := stats[k]
		if s == nil {
			s = &stat{path: append([]int(nil), f.Path...), wireType: f.WireType}
			stats[k] = s
		}
		s.count++
		if len(s.samples) < 2 {
			switch f.WireType {
			case protowire.WireVarint:
				s.samples = append(s.samples, fmt.Sprintf("varint=%d", f.Varint))
			case protowire.WireFixed32, protowire.WireFixed64:
				s.samples = append(s.samples, fmt.Sprintf("fixed=%d", f.Varint))
			case protowire.WireBytes:
				if protowire.IsLikelyText(f.Bytes) {
					tx := string(f.Bytes)
					if len(tx) > 80 {
						tx = tx[:80] + "…"
					}
					s.samples = append(s.samples, fmt.Sprintf("text=%q", tx))
				} else {
					s.samples = append(s.samples, fmt.Sprintf("bytes=%dB", len(f.Bytes)))
				}
			}
		}
		return nil
	})
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("walked %d bytes, %d distinct field paths", len(buf), len(stats))
	for _, k := range keys {
		s := stats[k]
		t.Logf("  %-30s wt=%d  ×%-5d  %s", k, int(s.wireType), s.count, strings.Join(s.samples, " | "))
	}
}
