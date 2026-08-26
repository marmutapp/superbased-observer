package kirocli

import (
	"context"
	"testing"
)

// TestCacheObservations_FromTokenBearingFixture proves ParseSessionFile
// wires emitCacheObservation end-to-end for the SQLite (conversations_v2)
// path, using the same synthetic non-null-token value
// TestParseStateDBTokenBearing already pins to 1 TokenEvent (real
// captures are all-null; this proves the mapping, exactly as that test's
// own comment states).
func TestCacheObservations_FromTokenBearingFixture(t *testing.T) {
	const val = `{"conversation_id":"tok-1","history":[
		{"user":{"env_context":{"env_state":{"current_working_directory":"/home/dev/project"}},
		         "content":{"Prompt":{"prompt":"hi"}},"timestamp":null},
		 "assistant":{"Response":{"message_id":"m1","content":"hello"}},
		 "request_metadata":{"request_id":"r1","message_id":"m1","model_id":"auto",
		   "request_start_timestamp_ms":1783571416209,
		   "total_tokens":150,"uncached_input_tokens":100,"output_tokens":40,
		   "cache_read_input_tokens":10,"cache_write_input_tokens":0}}
	]}`
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "tok-1", value: val, createdAt: 1, updatedAt: 100},
	}, false)
	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("precondition: got %d TokenEvents, want 1", len(res.TokenEvents))
	}
	if len(res.CacheObservations) != 1 {
		t.Fatalf("CacheObservations = %d, want 1", len(res.CacheObservations))
	}

	obs := res.CacheObservations[0]
	if obs.Usage.NetInputTokens != 100 {
		t.Errorf("obs.Usage.NetInputTokens = %d, want 100", obs.Usage.NetInputTokens)
	}
	if obs.Usage.OutputTokens != 40 {
		t.Errorf("obs.Usage.OutputTokens = %d, want 40", obs.Usage.OutputTokens)
	}
	if obs.Usage.CacheReadTokens != 10 {
		t.Errorf("obs.Usage.CacheReadTokens = %d, want 10", obs.Usage.CacheReadTokens)
	}
	if obs.Usage.CacheCreationTokens != 0 {
		t.Errorf("obs.Usage.CacheCreationTokens = %d, want 0", obs.Usage.CacheCreationTokens)
	}
	if obs.Model != "auto" {
		t.Errorf("obs.Model = %q, want auto", obs.Model)
	}
	if obs.SourceEventID != "cachetrack:"+res.TokenEvents[0].SourceEventID {
		t.Errorf("obs.SourceEventID = %q, want cachetrack:%s", obs.SourceEventID, res.TokenEvents[0].SourceEventID)
	}
	// The turn's own prompt+response content joins the accumulator before
	// this turn's Emit call, so its single non-empty delta must be
	// exactly the 2 blocks (user prompt + assistant response) — no tool
	// blocks, since this fixture's assistant carries a Response, not a
	// ToolUse.
	if len(obs.BlockHashes) != 2 {
		t.Errorf("obs.BlockHashes = %d, want 2 (prompt + response)", len(obs.BlockHashes))
	}
}

// TestCacheObservations_AllNullFixtureProducesNone proves the real
// conversations_v2-value.json fixture — whose request_metadata token
// fields are all null across every turn (see
// testdata/kirocli/conversations_v2-value.json) — produces zero
// CacheTurnObservation rows, never a fabricated one. ToolEvents still
// come out normally; only the cache side stays silent, mirroring
// TokenEvents (also always empty against this fixture).
func TestCacheObservations_AllNullFixtureProducesNone(t *testing.T) {
	dir := t.TempDir()
	val := fixtureValue(t, "conversations_v2-value.json")
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "cccccccc-3333-4f7f-860e-000000000003", value: val, createdAt: 1783571416000, updatedAt: 1783571424374},
	}, false)

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) == 0 {
		t.Fatal("precondition: expected ToolEvents from the real fixture")
	}
	if len(res.TokenEvents) != 0 {
		t.Fatalf("precondition: got %d TokenEvents, want 0 (all-null fixture)", len(res.TokenEvents))
	}
	if len(res.CacheObservations) != 0 {
		t.Errorf("CacheObservations = %d, want 0 (all-null request_metadata must never fabricate an observation)", len(res.CacheObservations))
	}
}

// TestCacheObservations_Idempotent pins re-parse stability: a
// conversations_v2 row is a whole-row rewrite (its `value` carries the
// FULL history every time), so re-parsing from offset 0 twice must
// reproduce byte-identical observations.
func TestCacheObservations_Idempotent(t *testing.T) {
	const val = `{"conversation_id":"tok-2","history":[
		{"user":{"env_context":{"env_state":{"current_working_directory":"/home/dev/project"}},
		         "content":{"Prompt":{"prompt":"first"}},"timestamp":null},
		 "assistant":{"Response":{"message_id":"a1","content":"first reply"}},
		 "request_metadata":{"request_id":"r1","message_id":"a1","model_id":"auto",
		   "request_start_timestamp_ms":1,
		   "uncached_input_tokens":50,"output_tokens":20,
		   "cache_read_input_tokens":5,"cache_write_input_tokens":0}},
		{"user":{"env_context":{"env_state":{"current_working_directory":"/home/dev/project"}},
		         "content":{"Prompt":{"prompt":"second"}},"timestamp":null},
		 "assistant":{"Response":{"message_id":"a2","content":"second reply"}},
		 "request_metadata":{"request_id":"r2","message_id":"a2","model_id":"auto",
		   "request_start_timestamp_ms":2,
		   "uncached_input_tokens":80,"output_tokens":30,
		   "cache_read_input_tokens":40,"cache_write_input_tokens":10}}
	]}`
	dir := t.TempDir()
	path := newStateDB(t, dir, []convInsert{
		{key: "/home/dev/project", conversationID: "tok-2", value: val, createdAt: 1, updatedAt: 100},
	}, false)
	a := NewWithOptions(nil, dir)

	first, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(first.CacheObservations) != 2 || len(second.CacheObservations) != 2 {
		t.Fatalf("want 2 observations each parse, got %d and %d", len(first.CacheObservations), len(second.CacheObservations))
	}
	for i := range first.CacheObservations {
		a, b := first.CacheObservations[i], second.CacheObservations[i]
		if a.SourceEventID != b.SourceEventID {
			t.Errorf("obs %d: SourceEventID changed: %q vs %q", i, a.SourceEventID, b.SourceEventID)
		}
		if len(a.BlockHashes) != len(b.BlockHashes) {
			t.Errorf("obs %d: BlockHashes len changed: %d vs %d", i, len(a.BlockHashes), len(b.BlockHashes))
			continue
		}
		for j := range a.BlockHashes {
			if string(a.BlockHashes[j].CanonicalBytes) != string(b.BlockHashes[j].CanonicalBytes) {
				t.Errorf("obs %d block %d: canonical bytes changed", i, j)
			}
		}
	}
	// Second turn's usage differs from the first, proving both turns
	// independently drained their own (non-overlapping) block delta.
	if first.CacheObservations[1].Usage.CacheCreationTokens != 10 {
		t.Errorf("obs[1].Usage.CacheCreationTokens = %d, want 10", first.CacheObservations[1].Usage.CacheCreationTokens)
	}
}
