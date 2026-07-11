package proxyroute

import (
	"strings"
	"testing"
)

func TestVSCodeBaseURLHint(t *testing.T) {
	h := VSCodeBaseURLHint(8820, "Cline")
	for _, want := range []string{
		"Cline",
		"OpenAI Compatible",
		"http://127.0.0.1:8820/v1",
		"observer never reads it",
		"state.vscdb",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("hint missing %q\n%s", want, h)
		}
	}
	// Empty tool falls back gracefully.
	if got := VSCodeBaseURLHint(8820, ""); !strings.Contains(got, "the extension") {
		t.Errorf("empty-tool fallback = %q", got)
	}
}
