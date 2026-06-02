package antigravity

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// TestProbeDump dumps the actual values of every field at a given
// path prefix. Used to inspect the structure of select sub-messages
// during Path B reverse-engineering. Set ANTIGRAVITY_PROBE_FILE +
// ANTIGRAVITY_PROBE_PREFIX (e.g. "1.2.5", "1.3.1.17") to scope.
func TestProbeDump(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_PROBE_FILE")
	if path == "" {
		t.Skip("set ANTIGRAVITY_PROBE_FILE=/tmp/probe.bin to run")
	}
	prefix := os.Getenv("ANTIGRAVITY_PROBE_PREFIX")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	type rec struct {
		path  string
		wt    protowire.WireType
		v     uint64
		bytes []byte
	}
	var recs []rec
	pathKey := func(p []int) string {
		parts := make([]string, len(p))
		for i, n := range p {
			parts[i] = fmt.Sprintf("%d", n)
		}
		return strings.Join(parts, ".")
	}
	_ = protowire.Walk(buf, func(f protowire.Field) error {
		k := pathKey(f.Path)
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			return nil
		}
		recs = append(recs, rec{k, f.WireType, f.Varint, f.Bytes})
		return nil
	})
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].path < recs[j].path })
	t.Logf("=== %d records under prefix=%q ===", len(recs), prefix)
	limit := 200
	if v := os.Getenv("ANTIGRAVITY_PROBE_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	for i, r := range recs {
		if i >= limit {
			t.Logf("... (truncated, %d more)", len(recs)-limit)
			break
		}
		switch r.wt {
		case protowire.WireVarint:
			t.Logf("  %-30s varint=%d", r.path, r.v)
		case protowire.WireFixed64:
			t.Logf("  %-30s fixed64=%d", r.path, r.v)
		case protowire.WireFixed32:
			t.Logf("  %-30s fixed32=%d", r.path, r.v)
		case protowire.WireBytes:
			if protowire.IsLikelyText(r.bytes) {
				txt := string(r.bytes)
				if len(txt) > 100 {
					txt = txt[:100] + "…"
				}
				t.Logf("  %-30s text=%q", r.path, txt)
			} else {
				t.Logf("  %-30s bytes=%dB", r.path, len(r.bytes))
			}
		}
	}
}
