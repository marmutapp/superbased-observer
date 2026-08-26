package cacheobs

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestSourceEventID(t *testing.T) {
	if got := SourceEventID("msg_123"); got != "cachetrack:msg_123" {
		t.Errorf("SourceEventID = %q, want cachetrack:msg_123", got)
	}
	if got := SourceEventID(""); got != "cachetrack:" {
		t.Errorf("SourceEventID(\"\") = %q, want cachetrack:", got)
	}
}

func TestApplyImplicitCacheOverlay(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		model        string
		wantOverlay  bool
		wantBlockNil bool
	}{
		{"anthropic stays unmodified", "anthropic", "claude-sonnet-4", false, false},
		{"openai routes implicit", "openai", "gpt-5", true, true},
		{"deepseek routes implicit", "deepseek", "deepseek-v4-flash", true, true},
		{"unknown provider defaults anthropic", "", "some-model", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := models.CacheTurnObservation{
				BlockHashes: []models.CacheBlockMeta{{Kind: "text"}},
			}
			out := ApplyImplicitCacheOverlay(in, tt.provider, tt.model)
			if out.ImplicitCache != tt.wantOverlay {
				t.Errorf("ImplicitCache = %v, want %v", out.ImplicitCache, tt.wantOverlay)
			}
			gotNil := out.BlockHashes == nil
			if gotNil != tt.wantBlockNil {
				t.Errorf("BlockHashes nil = %v, want %v (BlockHashes=%v)", gotNil, tt.wantBlockNil, out.BlockHashes)
			}
		})
	}
}

func TestIsZeroUsage(t *testing.T) {
	if !IsZeroUsage(models.CacheUsage{}) {
		t.Error("zero-value CacheUsage should be zero")
	}
	nonZero := []models.CacheUsage{
		{NetInputTokens: 1},
		{OutputTokens: 1},
		{CacheReadTokens: 1},
		{CacheCreationTokens: 1},
		{CacheCreation1hTokens: 1},
	}
	for i, u := range nonZero {
		if IsZeroUsage(u) {
			t.Errorf("case %d: %+v reported zero, want non-zero", i, u)
		}
	}
}
