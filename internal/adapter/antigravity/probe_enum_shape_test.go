package antigravity

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// TestProbeEnumShape clusters per-step (1.2[]) entries by their
// 1.2.1 step-type enum value and reports the modal sub-shape for
// each cluster. Used to map the unknown enum values
// (5/8/14/15/17/21/23/28/65/81/82/83/90/98 on FB48) to
// CORTEX_STEP_TYPE_* names extracted from the language_server
// binary.
//
// Set ANTIGRAVITY_PROBE_FILE=/tmp/probe-getct.bin to run.
func TestProbeEnumShape(t *testing.T) {
	path := os.Getenv("ANTIGRAVITY_PROBE_FILE")
	if path == "" {
		t.Skip("set ANTIGRAVITY_PROBE_FILE=/tmp/probe-getct.bin to run")
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	type stepProfile struct {
		idx      int
		typeEnum uint64
		subKeys  map[string]bool
	}
	var profiles []*stepProfile
	var current *stepProfile

	_ = protowire.Walk(buf, func(f protowire.Field) error {
		// New step entry under 1.2 → start a profile.
		if pathEq(f.Path, 1, 2) && f.WireType == protowire.WireBytes {
			current = &stepProfile{
				idx:     len(profiles),
				subKeys: map[string]bool{},
			}
			profiles = append(profiles, current)
			return nil
		}
		if current == nil || len(f.Path) < 3 || f.Path[0] != 1 || f.Path[1] != 2 {
			return nil
		}
		// Capture the enum value at 1.2.1.
		if pathEq(f.Path, 1, 2, 1) && f.WireType == protowire.WireVarint {
			current.typeEnum = f.Varint
			return nil
		}
		// Record the immediate sub-field number under 1.2[] (i.e.
		// f.Path[2]). That's the discriminator the dashboard cares
		// about — it tells us whether the step is a file-view
		// (1.2.14), an artifact (1.2.10), an ephemeral (1.2.103),
		// a user message (1.2.19), etc.
		current.subKeys[fmt.Sprintf("1.2.%d", f.Path[2])] = true
		return nil
	})

	// Cluster by enum value.
	type cluster struct {
		typeEnum uint64
		count    int
		shapes   map[string]int // sub-shape signature → count
		sample   []int          // step indices
	}
	clusters := map[uint64]*cluster{}
	for _, p := range profiles {
		c, ok := clusters[p.typeEnum]
		if !ok {
			c = &cluster{typeEnum: p.typeEnum, shapes: map[string]int{}}
			clusters[p.typeEnum] = c
		}
		c.count++
		keys := make([]string, 0, len(p.subKeys))
		for k := range p.subKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sig := strings.Join(keys, ",")
		c.shapes[sig]++
		if len(c.sample) < 3 {
			c.sample = append(c.sample, p.idx)
		}
	}

	keys := make([]uint64, 0, len(clusters))
	for k := range clusters {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	t.Logf("=== %d total steps, %d distinct 1.2.1 enum values ===", len(profiles), len(clusters))
	for _, k := range keys {
		c := clusters[k]
		t.Logf("enum=%d  n=%d  samples=%v", k, c.count, c.sample)
		sigKeys := make([]string, 0, len(c.shapes))
		for sig := range c.shapes {
			sigKeys = append(sigKeys, sig)
		}
		sort.Slice(sigKeys, func(i, j int) bool { return c.shapes[sigKeys[i]] > c.shapes[sigKeys[j]] })
		for _, sig := range sigKeys {
			t.Logf("    sub-shape %q  ×%d", sig, c.shapes[sig])
		}
	}
}
