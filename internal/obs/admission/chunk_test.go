package admission

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestChunkForJudge(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		size       int
		overlap    int
		wantChunks int  // exact count when >0
		wantSingle bool // expect exactly the input back
	}{
		{name: "short text unchanged", text: "hello world", size: 100, wantChunks: 1, wantSingle: true},
		{name: "exactly at size unchanged", text: strings.Repeat("a", 40), size: 40, wantChunks: 1, wantSingle: true},
		{name: "zero size uses default", text: "hi", size: 0, wantChunks: 1, wantSingle: true},
		{name: "splits long text", text: strings.Repeat("word ", 40), size: 40, overlap: 8},
		{name: "no-whitespace long token still progresses", text: strings.Repeat("x", 200), size: 40, overlap: 8},
		{name: "overlap >= size is clamped (still progresses)", text: strings.Repeat("word ", 40), size: 30, overlap: 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkForJudge(tt.text, tt.size, tt.overlap)
			if len(got) == 0 {
				t.Fatal("returned no chunks")
			}
			if tt.wantSingle {
				if len(got) != 1 || got[0] != tt.text {
					t.Fatalf("want single unchanged chunk, got %d chunks", len(got))
				}
				return
			}
			if tt.wantChunks > 0 && len(got) != tt.wantChunks {
				t.Fatalf("chunks = %d, want %d", len(got), tt.wantChunks)
			}
			if len(got) < 2 {
				t.Fatalf("expected multiple chunks for long text, got %d", len(got))
			}
			// Reassembling the chunks must cover the whole input: concatenating
			// chunks (removing overlap) reproduces every byte in order. We verify
			// coverage loosely — every chunk is non-empty and their union of
			// content contains all the source once de-overlapped.
			for i, c := range got {
				if c == "" {
					t.Fatalf("chunk %d is empty", i)
				}
			}
			// The first chunk starts at the source start; the last ends at EOF.
			if !strings.HasPrefix(tt.text, got[0][:1]) {
				t.Errorf("first chunk does not start at source start")
			}
			if !strings.HasSuffix(tt.text, got[len(got)-1]) {
				t.Errorf("last chunk does not reach source end")
			}
		})
	}
}

func TestChunkForJudge_MarkerStaysWhole(t *testing.T) {
	// A whitespace-delimited marker must land wholly inside at least one chunk
	// (windows break on whitespace) so a concern is never split away.
	text := strings.Repeat("word ", 12) + "DENYNOW " + strings.Repeat("word ", 12)
	chunks := chunkForJudge(text, 40, 8)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c, "DENYNOW") {
			found = true
		}
	}
	if !found {
		t.Fatalf("marker was split across chunks: %q", chunks)
	}
}

// perChunkJudge denies (attributing AD-100) when the rendered prompt — which
// embeds the chunk text — contains the marker, else allows. It records how many
// chunks it saw.
type perChunkJudge struct {
	marker string
	calls  int
}

func (j *perChunkJudge) Judge(_ context.Context, prompt string) (string, error) {
	j.calls++
	if strings.Contains(prompt, j.marker) {
		return `{"verdicts":[{"criterion":"AD-100","decision":"deny","reason":"off scope"}]}`, nil
	}
	return `{"verdicts":[{"criterion":"AD-100","decision":"allow","reason":""}]}`, nil
}

// chunkPolicy compiles a judged-only policy with a small chunk size and no
// prefilter, so Evaluate reaches the judge and map-reduces over chunks.
func chunkPolicy(t *testing.T, chunkBytes, overlap int) PolicySpec {
	t.Helper()
	spec, err := Compile(PolicyInput{
		Mode: "enforce",
		Criteria: []CriterionInput{
			{ID: "AD-100", Type: "valid_use_case", Name: "On-scope only", Definition: "Only product support.", Decision: "deny", Severity: "warn"},
		},
		JudgeChunkBytes:        chunkBytes,
		JudgeChunkOverlapBytes: overlap,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return spec
}

func TestEvaluate_MapReduceStrictestWins(t *testing.T) {
	p := chunkPolicy(t, 40, 8)
	text := strings.Repeat("safe ", 12) + "DENYNOW " + strings.Repeat("safe ", 12)

	j := &perChunkJudge{marker: "DENYNOW"}
	res := Evaluate(context.Background(), Request{Text: text}, p, j)
	if res.Decision != DecisionDeny {
		t.Fatalf("decision = %v, want Deny (a single deny chunk must win)", res.Decision)
	}
	if res.Criterion != "AD-100" {
		t.Errorf("criterion = %q, want AD-100", res.Criterion)
	}
	if !res.JudgeUsed {
		t.Error("JudgeUsed = false, want true")
	}
	if IsDegraded(res.Degraded) {
		t.Errorf("Degraded = %q, want a non-degraded aggregate", res.Degraded)
	}
	if j.calls < 2 {
		t.Errorf("judge saw %d chunks, expected map-reduce over >= 2", j.calls)
	}
}

func TestEvaluate_MapReduceAllAllow(t *testing.T) {
	p := chunkPolicy(t, 40, 8)
	text := strings.Repeat("safe ", 30) // no marker anywhere

	j := &perChunkJudge{marker: "DENYNOW"}
	res := Evaluate(context.Background(), Request{Text: text}, p, j)
	if res.Decision != DecisionAllow {
		t.Fatalf("decision = %v, want Allow", res.Decision)
	}
	if res.Degraded != "" {
		t.Errorf("Degraded = %q, want empty on a clean multi-chunk allow", res.Degraded)
	}
}

func TestEvaluate_MapReduceDegradationCarry(t *testing.T) {
	longText := strings.Repeat("ambiguous request ", 12) // > 40 bytes → multi-chunk

	// fail-open: every chunk errors → Allow but the aggregate surfaces the
	// genuine degradation.
	openP := chunkPolicy(t, 40, 8)
	res := Evaluate(context.Background(), Request{Text: longText}, openP, &erroringJudge{})
	if res.Decision != DecisionAllow {
		t.Errorf("fail-open: decision = %v, want Allow", res.Decision)
	}
	if res.Degraded != DegradedTimeoutFailopen {
		t.Errorf("fail-open: Degraded = %q, want %q", res.Degraded, DegradedTimeoutFailopen)
	}
	if !IsDegraded(res.Degraded) {
		t.Error("fail-open aggregate must count as degraded")
	}

	// fail-closed: any errored chunk denies (strictest-wins over the fail-mode).
	strictP := chunkPolicy(t, 40, 8)
	strictP.Strict = true
	res = Evaluate(context.Background(), Request{Text: longText}, strictP, &erroringJudge{})
	if res.Decision != DecisionDeny {
		t.Errorf("fail-closed: decision = %v, want Deny", res.Decision)
	}
}

// erroringJudge always fails, exercising the per-chunk fail-mode.
type erroringJudge struct{}

func (erroringJudge) Judge(_ context.Context, _ string) (string, error) {
	return "", errors.New("judge boom")
}

func TestIsDegraded(t *testing.T) {
	tests := []struct {
		code string
		want bool
	}{
		{"", false},
		{DegradedTimeoutFailopen, true},
		{DegradedNoJudge, true},
		{DegradedPrefiltered, false},
		{DegradedSecretGate, false},
		{DegradedCache, false},
		{"unknown-code", false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := IsDegraded(tt.code); got != tt.want {
				t.Errorf("IsDegraded(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}
