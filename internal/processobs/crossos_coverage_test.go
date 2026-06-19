package processobs

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// TestCrossOSBasenamesCoverEveryEnabledAdapter pins the invariant that every
// adapter the daemon scans by default has a cross-OS attribution anchor. A new
// adapter added to config.Default() without a DefaultCrossOSToolBasenames entry
// would silently get NO Windows process attribution (the map returns nil and
// cross-OS never guesses), so this test fails loudly and points the author at
// the map — the same allow-list discipline EnabledAdapters enforces elsewhere.
func TestCrossOSBasenamesCoverEveryEnabledAdapter(t *testing.T) {
	t.Parallel()
	for _, tool := range config.Default().Observer.Watch.EnabledAdapters {
		set := DefaultCrossOSToolBasenames[tool]
		if len(set) == 0 {
			t.Errorf("adapter %q is enabled by default but has no DefaultCrossOSToolBasenames entry — Windows process attribution will silently no-op for it; add a basename set (node.exe/node at minimum)", tool)
		}
	}
}
