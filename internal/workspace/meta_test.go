package workspace

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMarshalMetaRoundTrip(t *testing.T) {
	m := Meta{
		Source:    SourceCloneLocal,
		Origin:    "/home/user/proj",
		Branch:    "feature-x",
		RunID:     "abc123",
		CreatedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
	}
	b, err := MarshalMeta(m)
	if err != nil {
		t.Fatalf("MarshalMeta: %v", err)
	}
	var got Meta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("CreatedAt round trip mismatch: got %v, want %v", got.CreatedAt, m.CreatedAt)
	}
	got.CreatedAt = m.CreatedAt // time.Time equality via == is location-sensitive; already checked above
	if got != m {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, m)
	}
}

func TestMarshalMetaOmitsEmptyBranch(t *testing.T) {
	m := Meta{Source: SourceLive, Origin: "/home/user/proj", RunID: "abc123", CreatedAt: time.Now().UTC()}
	b, err := MarshalMeta(m)
	if err != nil {
		t.Fatalf("MarshalMeta: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["branch"]; ok {
		t.Fatalf("branch key present in output despite empty value: %s", b)
	}
	if raw["source"] != string(SourceLive) {
		t.Fatalf("source = %v, want %q", raw["source"], SourceLive)
	}
}
