package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// TestMetricSamplesJSONRoundTrip pins the ring codec including the network
// fields: everything a live chart needs must survive the DB round trip.
func TestMetricSamplesJSONRoundTrip(t *testing.T) {
	t.Parallel()
	ts := time.Unix(1_700_000_000, 0).UTC()
	in := []processobs.MetricSample{
		{T: ts, CPUMs: 10, WorkingSet: 2048, ReadBytes: 1, WriteBytes: 2},
		{
			T: ts.Add(2 * time.Second), CPUMs: 20, WorkingSet: 4096, ReadBytes: 3, WriteBytes: 4,
			NetRxBytes: 65536, NetTxBytes: 1024, NetMeasured: true,
		},
		// A measured-but-idle sample: zero bytes, but flagged measured.
		{T: ts.Add(4 * time.Second), CPUMs: 30, NetMeasured: true},
	}
	out := parseMetricSamples(marshalMetricSamples(in))
	if len(out) != len(in) {
		t.Fatalf("round trip lost samples: %d -> %d", len(in), len(out))
	}
	for i := range in {
		if !out[i].T.Equal(in[i].T) {
			t.Errorf("sample %d: timestamp %s != %s", i, out[i].T, in[i].T)
		}
		got, want := out[i], in[i]
		got.T, want.T = time.Time{}, time.Time{}
		if got != want {
			t.Errorf("sample %d: %+v != %+v", i, got, want)
		}
	}
}

// TestMetricSamplesJSONBackCompat is the migration-free guarantee: a ring
// written by a build that predates the network fields must still decode, and it
// must decode as UNMEASURED (net_measured false) rather than as measured zero.
func TestMetricSamplesJSONBackCompat(t *testing.T) {
	t.Parallel()
	// Verbatim shape of a pre-network ring (as stored today).
	const old = `[{"t":"2026-06-17T09:00:00Z","cpu_ms":150,"ws":8192,"rb":4096,"wb":512},` +
		`{"t":"2026-06-17T09:00:15Z","cpu_ms":300,"ws":9216,"rb":8192,"wb":1024}]`
	got := parseMetricSamples(old)
	if len(got) != 2 {
		t.Fatalf("old ring failed to decode: %d samples", len(got))
	}
	if got[0].CPUMs != 150 || got[1].WorkingSet != 9216 {
		t.Fatalf("old fields mis-decoded: %+v", got)
	}
	for i, s := range got {
		if s.NetMeasured || s.NetRxBytes != 0 || s.NetTxBytes != 0 {
			t.Errorf("sample %d from an old ring must read as unmeasured: %+v", i, s)
		}
	}

	// And the forward direction: a new ring stays readable by a decoder that
	// only knows the old fields (unknown keys are ignored).
	type oldSample struct {
		T          time.Time `json:"t"`
		CPUMs      int64     `json:"cpu_ms"`
		WorkingSet int64     `json:"ws"`
	}
	newRing := marshalMetricSamples([]processobs.MetricSample{
		{T: time.Unix(1, 0).UTC(), CPUMs: 7, WorkingSet: 8, NetRxBytes: 9, NetMeasured: true},
	})
	var decoded []oldSample
	if err := json.Unmarshal([]byte(newRing), &decoded); err != nil {
		t.Fatalf("new ring is not readable by an old decoder: %v", err)
	}
	if len(decoded) != 1 || decoded[0].CPUMs != 7 || decoded[0].WorkingSet != 8 {
		t.Errorf("old decoder got %+v", decoded)
	}
}

// TestMetricSamplesJSONMalformed pins the fail-soft read: a corrupt column
// yields no samples rather than breaking the whole listing.
func TestMetricSamplesJSONMalformed(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "not json", `{"t":1}`} {
		if got := parseMetricSamples(s); got != nil {
			t.Errorf("parseMetricSamples(%q) = %+v, want nil", s, got)
		}
	}
}
