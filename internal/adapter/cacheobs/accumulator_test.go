package cacheobs

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func block(kind string) models.CacheBlockMeta {
	return models.CacheBlockMeta{LevelLabel: "message", Kind: kind, CanonicalBytes: []byte(`{"type":"` + kind + `"}`), Role: "assistant"}
}

func TestAccumulator_EmitProducesDeltaThenResets(t *testing.T) {
	a := New(DefaultMaxBlocksPerSession)
	a.ObserveBlocks([]models.CacheBlockMeta{block("text"), block("tool_use")})

	ts := time.Unix(1000, 0)
	usage := models.CacheUsage{NetInputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40}
	obs := a.Emit("/path/session.jsonl", "sess-1", "msg-1", "claude-sonnet-4", ts, usage, false)

	if obs.SourceFile != "/path/session.jsonl" {
		t.Errorf("SourceFile = %q", obs.SourceFile)
	}
	if obs.SourceEventID != "cachetrack:msg-1" {
		t.Errorf("SourceEventID = %q, want cachetrack:msg-1", obs.SourceEventID)
	}
	if obs.SessionID != "sess-1" || obs.MessageID != "msg-1" || obs.Model != "claude-sonnet-4" {
		t.Errorf("obs = %+v", obs)
	}
	if !obs.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", obs.Timestamp, ts)
	}
	if obs.Usage != usage {
		t.Errorf("Usage = %+v, want %+v", obs.Usage, usage)
	}
	if len(obs.BlockHashes) != 2 {
		t.Fatalf("BlockHashes = %d, want 2", len(obs.BlockHashes))
	}

	// A second Emit with no intervening ObserveBlocks call must
	// carry zero blocks — the delta was consumed by the first Emit.
	second := a.Emit("/path/session.jsonl", "sess-1", "msg-2", "claude-sonnet-4", ts, usage, false)
	if len(second.BlockHashes) != 0 {
		t.Fatalf("second.BlockHashes = %d, want 0 (delta must reset after Emit)", len(second.BlockHashes))
	}
}

func TestAccumulator_CapExceeded_EmitsNilBlockHashes(t *testing.T) {
	a := New(3)
	a.ObserveBlocks([]models.CacheBlockMeta{block("text"), block("text"), block("text"), block("text")})

	obs := a.Emit("/p", "s", "m", "model", time.Now(), models.CacheUsage{NetInputTokens: 1}, false)
	if obs.BlockHashes != nil {
		t.Errorf("BlockHashes = %v, want nil once cap exceeded", obs.BlockHashes)
	}

	// Cap stays latched: further ObserveBlocks calls are no-ops.
	a.ObserveBlocks([]models.CacheBlockMeta{block("text")})
	obs2 := a.Emit("/p", "s", "m2", "model", time.Now(), models.CacheUsage{NetInputTokens: 1}, false)
	if obs2.BlockHashes != nil {
		t.Errorf("BlockHashes after latch = %v, want nil", obs2.BlockHashes)
	}
}

func TestAccumulator_UncappedWhenMaxBlocksNonPositive(t *testing.T) {
	a := New(0)
	blocks := make([]models.CacheBlockMeta, 0, 10000)
	for i := 0; i < 10000; i++ {
		blocks = append(blocks, block("text"))
	}
	a.ObserveBlocks(blocks)
	obs := a.Emit("/p", "s", "m", "model", time.Now(), models.CacheUsage{}, false)
	if len(obs.BlockHashes) != 10000 {
		t.Fatalf("BlockHashes = %d, want 10000 (cap disabled)", len(obs.BlockHashes))
	}
}

func TestAccumulator_ObserveCompaction_ResetsAndFlags(t *testing.T) {
	a := New(DefaultMaxBlocksPerSession)
	a.ObserveBlocks([]models.CacheBlockMeta{block("text"), block("text")})
	a.ObserveCompaction()

	obs := a.Emit("/p", "s", "m", "model", time.Now(), models.CacheUsage{}, false)
	if !obs.CompactionSeen {
		t.Error("CompactionSeen = false, want true after ObserveCompaction")
	}
	if len(obs.BlockHashes) != 0 {
		t.Fatalf("BlockHashes = %d, want 0 (ObserveCompaction clears the running delta)", len(obs.BlockHashes))
	}

	// compactionSeen resets after the next Emit.
	obs2 := a.Emit("/p", "s", "m2", "model", time.Now(), models.CacheUsage{}, false)
	if obs2.CompactionSeen {
		t.Error("CompactionSeen = true on the emit after the flagged one, want false")
	}
}

func TestAccumulator_ObserveBlocks_EmptySliceIsNoop(t *testing.T) {
	a := New(DefaultMaxBlocksPerSession)
	a.ObserveBlocks(nil)
	a.ObserveBlocks([]models.CacheBlockMeta{})
	obs := a.Emit("/p", "s", "m", "model", time.Now(), models.CacheUsage{}, false)
	if len(obs.BlockHashes) != 0 {
		t.Fatalf("BlockHashes = %d, want 0", len(obs.BlockHashes))
	}
}
