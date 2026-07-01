package integration_test

import (
	"testing"

	adapterdefaults "github.com/marmutapp/superbased-observer/internal/adapter/defaults"
	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestRegistryCoversEveryRegisteredAdapter pins that every adapter in the
// canonical defaults.Adapters() list has an integration.Capability row.
// This is the guardrail the new-adapter checklist relies on: add an adapter
// without a registry row and this test goes red, forcing the capability
// declaration that init/register/MCP/doctor iterate. Lives in an external
// test package so it can import adapterdefaults without coupling the pure
// integration package to it.
func TestRegistryCoversEveryRegisteredAdapter(t *testing.T) {
	for _, a := range adapterdefaults.Adapters() {
		name := a.Name()
		if _, ok := integration.For(name); !ok {
			t.Errorf("adapter %q has no integration.Capability row — add one in internal/integration", name)
		}
	}
}

// TestRegistryHasNoOrphanRows pins the reverse: every registry row maps to a
// registered adapter (no stale rows for removed adapters).
func TestRegistryHasNoOrphanRows(t *testing.T) {
	registered := map[string]bool{}
	for _, a := range adapterdefaults.Adapters() {
		registered[a.Name()] = true
	}
	for _, c := range integration.Capabilities() {
		if !registered[c.Tool] {
			t.Errorf("registry row %q has no registered adapter — remove the stale row", c.Tool)
		}
	}
}
