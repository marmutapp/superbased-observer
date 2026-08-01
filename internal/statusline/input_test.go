package statusline

import (
	"testing"
)

func strPtr(s string) *string   { return &s }
func f64Ptr(v float64) *float64 { return &v }

func TestParseInputFullShape(t *testing.T) {
	data := []byte(`{
		"session_id": "sess-123",
		"transcript_path": "/tmp/transcript.jsonl",
		"cwd": "/home/user/project",
		"model": {"id": "claude-opus-4-5-20260101", "display_name": "claude-opus-4-5"},
		"workspace": {"current_dir": "/home/user/project", "project_dir": "/home/user/project"},
		"cost": {"total_cost_usd": 3.42, "total_duration_ms": 60000, "total_lines_added": 12, "total_lines_removed": 4},
		"output_style": "default",
		"version": "1.28.0"
	}`)

	in, err := ParseInput(data)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}

	switch {
	case in.SessionID == nil || *in.SessionID != "sess-123":
		t.Errorf("SessionID = %v, want sess-123", in.SessionID)
	case in.TranscriptPath == nil || *in.TranscriptPath != "/tmp/transcript.jsonl":
		t.Errorf("TranscriptPath = %v", in.TranscriptPath)
	case in.CWD == nil || *in.CWD != "/home/user/project":
		t.Errorf("CWD = %v", in.CWD)
	}

	if in.Model == nil {
		t.Fatal("Model is nil")
	}
	if in.Model.ID == nil || *in.Model.ID != "claude-opus-4-5-20260101" {
		t.Errorf("Model.ID = %v", in.Model.ID)
	}
	if in.Model.DisplayName == nil || *in.Model.DisplayName != "claude-opus-4-5" {
		t.Errorf("Model.DisplayName = %v", in.Model.DisplayName)
	}

	if in.Workspace == nil {
		t.Fatal("Workspace is nil")
	}
	if in.Workspace.CurrentDir == nil || *in.Workspace.CurrentDir != "/home/user/project" {
		t.Errorf("Workspace.CurrentDir = %v", in.Workspace.CurrentDir)
	}
	if in.Workspace.ProjectDir == nil || *in.Workspace.ProjectDir != "/home/user/project" {
		t.Errorf("Workspace.ProjectDir = %v", in.Workspace.ProjectDir)
	}

	if in.Cost == nil {
		t.Fatal("Cost is nil")
	}
	if in.Cost.TotalCostUSD == nil || *in.Cost.TotalCostUSD != 3.42 {
		t.Errorf("Cost.TotalCostUSD = %v", in.Cost.TotalCostUSD)
	}
	if in.Cost.TotalDurationMS == nil || *in.Cost.TotalDurationMS != 60000 {
		t.Errorf("Cost.TotalDurationMS = %v", in.Cost.TotalDurationMS)
	}
	if in.Cost.TotalLinesAdded == nil || *in.Cost.TotalLinesAdded != 12 {
		t.Errorf("Cost.TotalLinesAdded = %v", in.Cost.TotalLinesAdded)
	}
	if in.Cost.TotalLinesRemoved == nil || *in.Cost.TotalLinesRemoved != 4 {
		t.Errorf("Cost.TotalLinesRemoved = %v", in.Cost.TotalLinesRemoved)
	}

	if in.OutputStyle == nil || *in.OutputStyle != "default" {
		t.Errorf("OutputStyle = %v", in.OutputStyle)
	}
	if in.Version == nil || *in.Version != "1.28.0" {
		t.Errorf("Version = %v", in.Version)
	}
}

func TestParseInputMissingOptionals(t *testing.T) {
	// Only session_id present — every other field must come back nil,
	// never a fabricated zero value.
	data := []byte(`{"session_id": "sess-only"}`)

	in, err := ParseInput(data)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	if in.SessionID == nil || *in.SessionID != "sess-only" {
		t.Errorf("SessionID = %v, want sess-only", in.SessionID)
	}
	if in.Model != nil {
		t.Errorf("Model = %+v, want nil", in.Model)
	}
	if in.Workspace != nil {
		t.Errorf("Workspace = %+v, want nil", in.Workspace)
	}
	if in.Cost != nil {
		t.Errorf("Cost = %+v, want nil", in.Cost)
	}
	if in.OutputStyle != nil {
		t.Errorf("OutputStyle = %v, want nil", in.OutputStyle)
	}
}

func TestParseInputPartialNestedObject(t *testing.T) {
	// cost is present but only carries total_cost_usd — the other three
	// cost sub-fields must stay nil, not zero.
	data := []byte(`{"cost": {"total_cost_usd": 1.5}}`)

	in, err := ParseInput(data)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	if in.Cost == nil {
		t.Fatal("Cost is nil")
	}
	if in.Cost.TotalCostUSD == nil || *in.Cost.TotalCostUSD != 1.5 {
		t.Errorf("Cost.TotalCostUSD = %v", in.Cost.TotalCostUSD)
	}
	if in.Cost.TotalDurationMS != nil {
		t.Errorf("Cost.TotalDurationMS = %v, want nil", in.Cost.TotalDurationMS)
	}
	if in.Cost.TotalLinesAdded != nil {
		t.Errorf("Cost.TotalLinesAdded = %v, want nil", in.Cost.TotalLinesAdded)
	}
	if in.Cost.TotalLinesRemoved != nil {
		t.Errorf("Cost.TotalLinesRemoved = %v, want nil", in.Cost.TotalLinesRemoved)
	}
}

func TestParseInputUnknownFieldsIgnored(t *testing.T) {
	// An unrecognized top-level key and an unrecognized nested key must
	// not cause an error — tolerant per ParseInput's doc comment.
	data := []byte(`{
		"session_id": "sess-x",
		"some_future_field": {"nested": true},
		"model": {"id": "gpt-99", "some_future_model_field": 42}
	}`)

	in, err := ParseInput(data)
	if err != nil {
		t.Fatalf("ParseInput: %v", err)
	}
	if in.SessionID == nil || *in.SessionID != "sess-x" {
		t.Errorf("SessionID = %v", in.SessionID)
	}
	if in.Model == nil || in.Model.ID == nil || *in.Model.ID != "gpt-99" {
		t.Errorf("Model = %+v", in.Model)
	}
}

func TestParseInputMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"truncated object", []byte(`{"session_id": "sess-123", "model": {`)},
		{"trailing garbage", []byte(`{"session_id": "sess-123"} garbage`)},
		{"empty input", []byte(``)},
		{"non-json plain text", []byte(`hello, this is not json at all`)},
		{"json array not object", []byte(`["not", "an", "object"]`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := ParseInput(tt.data)
			if err == nil {
				t.Fatalf("ParseInput(%q) returned no error, want one", tt.data)
			}
			// The caller must still get a usable zero Input, never a
			// panic and never a partially-populated struct claiming
			// data it doesn't have.
			if in.SessionID != nil || in.Model != nil || in.Cost != nil {
				t.Errorf("ParseInput on malformed input returned non-nil fields: %+v", in)
			}
		})
	}
}
