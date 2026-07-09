package antigravity

// path_inventory_test.go is a one-off diagnostic — not part of CI.
// Walks the plaintext bytes returned by a live Antigravity language
// server's GetCascadeTrajectory endpoint and emits a (path, wire_type,
// count, sample) catalog. Used to identify where the current
// Antigravity protobuf schema carries reasoning text + reasoning_tokens
// (the existing 1.2.93 / 1.2.94 / 1.3[].17 mappings fire zero hits on
// any modern session, including the FB48 fixture they were derived
// from).
//
// Skipped unless OBSERVER_AG_PATH_INVENTORY is set, since it depends on
// a running Antigravity language_server + local conversation files.
//
// Run:
//   OBSERVER_AG_PATH_INVENTORY=1 \
//     OBSERVER_AG_INVENTORY_SIDS=da973a42-...,fc8d44e5-...,162c4ab9-...,ae703782-... \
//     go test ./internal/adapter/antigravity -run TestPathInventory -v

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

func TestPathInventory(t *testing.T) {
	if os.Getenv("OBSERVER_AG_PATH_INVENTORY") == "" {
		t.Skip("set OBSERVER_AG_PATH_INVENTORY=1 + OBSERVER_AG_INVENTORY_SIDS=<csv> to run")
	}
	sidCSV := os.Getenv("OBSERVER_AG_INVENTORY_SIDS")
	if sidCSV == "" {
		t.Fatal("OBSERVER_AG_INVENTORY_SIDS env var required (csv of conversation IDs)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	a := New().WithNetworkRecovery("local")
	servers, err := discoverNativeLinux()
	if err != nil || len(servers) == 0 {
		t.Fatalf("no Linux-native language servers discovered: %v", err)
	}
	t.Logf("found %d language_server pid(s)", len(servers))

	for _, sid := range strings.Split(sidCSV, ",") {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		path := filepath.Join(home, ".gemini/antigravity/conversations", sid+".pb")
		if _, err := os.Stat(path); err != nil {
			t.Logf("[%s] skip: stat %v", sid, err)
			continue
		}
		raw, ok := fetchCascade(t, a, servers, sid)
		if !ok {
			t.Logf("[%s] skip: no server returned plaintext", sid)
			continue
		}
		t.Logf("\n========================================================")
		t.Logf("[%s] %d bytes plaintext", sid, len(raw))
		t.Logf("========================================================")
		inv := walkPathInventory(raw)
		emitInventory(t, inv)
	}
}

// TestTokenRowDump walks every per-turn 1.3.1.17.2.* sub-message and
// prints the actual varint values for every observed field. Used to
// identify which field carries reasoning_tokens (the existing wiring
// maps .1/.2/.3/.5 → input/cacheCreation/output/cacheRead but leaves
// .6/.9/.10 unmapped). Comparing per-turn values across reasoning-
// capable models (Gemini Pro / Flash with thinking on) vs non-reasoning
// flows pins which slot carries the count.
//
//	OBSERVER_AG_TOKEN_ROW_DUMP=1 \
//	OBSERVER_AG_INVENTORY_SIDS=<csv> \
//	  go test ./internal/adapter/antigravity -run TestTokenRowDump -v
func TestTokenRowDump(t *testing.T) {
	if os.Getenv("OBSERVER_AG_TOKEN_ROW_DUMP") == "" {
		t.Skip("set OBSERVER_AG_TOKEN_ROW_DUMP=1 + OBSERVER_AG_INVENTORY_SIDS=<csv> to run")
	}
	sidCSV := os.Getenv("OBSERVER_AG_INVENTORY_SIDS")
	if sidCSV == "" {
		t.Fatal("OBSERVER_AG_INVENTORY_SIDS env var required")
	}
	a := New().WithNetworkRecovery("local")
	servers, err := discoverNativeLinux()
	if err != nil || len(servers) == 0 {
		t.Fatalf("no language servers: %v", err)
	}
	for _, sid := range strings.Split(sidCSV, ",") {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		raw, ok := fetchCascade(t, a, servers, sid)
		if !ok {
			t.Logf("[%s] skip: no plaintext", sid)
			continue
		}

		// Locate the model name (1.3.3.28) so we can group dumps.
		var model string
		_ = protowire.Walk(raw, func(f protowire.Field) error {
			if pathEq(f.Path, 1, 3, 3, 28) && f.WireType == protowire.WireBytes && model == "" {
				model = string(f.Bytes)
			}
			return nil
		})

		// Group per-turn sub-messages: each new 1.3[] opens a new turn,
		// each 1.3.1.17.2.* under it lands in that turn's bag.
		type turn struct {
			fields map[int]uint64 // fieldNum (under .2) → varint value
		}
		var turns []turn
		_ = protowire.Walk(raw, func(f protowire.Field) error {
			switch {
			case pathEq(f.Path, 1, 3) && f.WireType == protowire.WireBytes:
				turns = append(turns, turn{fields: map[int]uint64{}})
			case len(f.Path) == 6 && pathPrefix(f.Path, 1, 3, 1, 17, 2) && f.WireType == protowire.WireVarint:
				if len(turns) == 0 {
					return nil
				}
				turns[len(turns)-1].fields[f.Path[5]] = f.Varint
			}
			return nil
		})

		t.Logf("\n=== [%s] model=%q — %d turns ===", sid[:8], model, len(turns))
		t.Logf("turn | .1=input | .2=cacheCr | .3=output | .5=cacheRd | .6=? | .9=? | .10=?")
		t.Logf("%s", strings.Repeat("-", 90))
		for i, tt := range turns {
			if len(tt.fields) == 0 {
				continue
			}
			get := func(n int) string {
				if v, ok := tt.fields[n]; ok {
					return fmt.Sprintf("%d", v)
				}
				return "-"
			}
			t.Logf("%4d | %8s | %9s | %9s | %10s | %5s | %5s | %5s",
				i, get(1), get(2), get(3), get(5), get(6), get(9), get(10))
			// Also show any OTHER fields we didn't expect
			var extras []string
			for fn, v := range tt.fields {
				switch fn {
				case 1, 2, 3, 5, 6, 9, 10:
				default:
					extras = append(extras, fmt.Sprintf(".%d=%d", fn, v))
				}
			}
			if len(extras) > 0 {
				sort.Strings(extras)
				t.Logf("        +extras: %s", strings.Join(extras, " "))
			}
		}
	}
}

// TestStructuredReasoningTokenCapture verifies that ParseStructuredTrajectory
// emits TokenEvents with non-zero ReasoningTokens for Gemini sessions
// (which decompose output_tokens at 1.3.1.17.2.9) and zero ReasoningTokens
// for Claude sessions (where .9 is absent). Also pins the invariant that
// the sum OutputTokens + ReasoningTokens equals the legacy .3 total —
// proving cost stays unchanged at the breakdown level.
//
//	OBSERVER_AG_REASONING_TOKEN_CHECK=1 \
//	OBSERVER_AG_INVENTORY_SIDS=<csv> \
//	  go test ./internal/adapter/antigravity -run TestStructuredReasoningTokenCapture -v
func TestStructuredReasoningTokenCapture(t *testing.T) {
	if os.Getenv("OBSERVER_AG_REASONING_TOKEN_CHECK") == "" {
		t.Skip("set OBSERVER_AG_REASONING_TOKEN_CHECK=1 + OBSERVER_AG_INVENTORY_SIDS=<csv> to run")
	}
	sidCSV := os.Getenv("OBSERVER_AG_INVENTORY_SIDS")
	if sidCSV == "" {
		t.Fatal("OBSERVER_AG_INVENTORY_SIDS env var required")
	}
	a := New().WithNetworkRecovery("local")
	servers, err := discoverNativeLinux()
	if err != nil || len(servers) == 0 {
		t.Fatalf("no language servers: %v", err)
	}
	for _, sid := range strings.Split(sidCSV, ",") {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		raw, ok := fetchCascade(t, a, servers, sid)
		if !ok {
			t.Logf("[%s] skip: no plaintext", sid)
			continue
		}
		en := ParseStructuredTrajectory(raw, sid, "[probe]", "/tmp/"+sid+".pb", nil)
		var sumOutput, sumReasoning int64
		var nonZeroReasoning int
		for _, te := range en.TokenEvents {
			sumOutput += te.OutputTokens
			sumReasoning += te.ReasoningTokens
			if te.ReasoningTokens > 0 {
				nonZeroReasoning++
			}
		}
		t.Logf("[%s] model=%q turns=%d output=%d reasoning=%d (turns_w_reasoning=%d)",
			sid[:8], en.Model, len(en.TokenEvents), sumOutput, sumReasoning, nonZeroReasoning)
		// Expectation: Gemini models emit non-zero reasoning per turn;
		// Claude models leave .9 absent → ReasoningTokens=0 universally.
		isGemini := strings.HasPrefix(en.Model, "gemini-")
		if isGemini && sumReasoning == 0 {
			t.Errorf("[%s] Gemini session reasoning_tokens total = 0; expected > 0", sid[:8])
		}
		if !isGemini && sumReasoning != 0 {
			t.Errorf("[%s] non-Gemini session has reasoning_tokens=%d; expected 0", sid[:8], sumReasoning)
		}
	}
}

// TestEmittedReasoningAndFinalSummary verifies that v1.4.51's new path
// wirings (1.2.20.3 reasoning + 1.2.30.{4,5,15} final_summary) actually
// produce ToolEvents when ParseStructuredTrajectory walks plaintext
// from live Antigravity sessions. Skipped unless explicitly enabled;
// depends on a running language_server.
//
//	OBSERVER_AG_REASONING_CHECK=1 \
//	OBSERVER_AG_INVENTORY_SIDS=<csv> \
//	  go test ./internal/adapter/antigravity -run TestEmittedReasoning -v
func TestEmittedReasoningAndFinalSummary(t *testing.T) {
	if os.Getenv("OBSERVER_AG_REASONING_CHECK") == "" {
		t.Skip("set OBSERVER_AG_REASONING_CHECK=1 + OBSERVER_AG_INVENTORY_SIDS=<csv> to run")
	}
	sidCSV := os.Getenv("OBSERVER_AG_INVENTORY_SIDS")
	if sidCSV == "" {
		t.Fatal("OBSERVER_AG_INVENTORY_SIDS env var required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	a := New().WithNetworkRecovery("local")
	servers, err := discoverNativeLinux()
	if err != nil || len(servers) == 0 {
		t.Fatalf("no language servers: %v", err)
	}
	for _, sid := range strings.Split(sidCSV, ",") {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		path := filepath.Join(home, ".gemini/antigravity/conversations", sid+".pb")
		raw, ok := fetchCascade(t, a, servers, sid)
		if !ok {
			t.Logf("[%s] skip: no plaintext", sid)
			continue
		}
		en := ParseStructuredTrajectory(raw, sid, "[probe]", path, nil)
		byRaw := map[string]int{}
		for _, e := range en.ToolEvents {
			byRaw[e.RawToolName]++
		}
		// Sample one of each new RawToolName for visual inspection.
		for _, want := range []string{"structured.reasoning", "structured.final_summary"} {
			for _, e := range en.ToolEvents {
				if e.RawToolName == want {
					t.Logf("[%s] sample %s: target=%.80q", sid[:8], want, e.Target)
					t.Logf("    output=%.200q", e.ToolOutput)
					break
				}
			}
		}
		t.Logf("[%s] reasoning=%d final_summary=%d (model=%q, %d total events)",
			sid[:8], byRaw["structured.reasoning"], byRaw["structured.final_summary"],
			en.Model, len(en.ToolEvents))
	}
}

// fetchCascade tries each discovered language_server until one returns
// the trajectory's plaintext for sid.
func fetchCascade(t *testing.T, a *Adapter, servers []LanguageServer, sid string) ([]byte, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, ls := range servers {
		for _, ep := range ls.Endpoints() {
			raw, err := callGetCascadeTrajectory(ctx, ep, ls.CSRFToken, sid, 30*time.Second)
			if err == nil && len(raw) > 0 {
				return raw, true
			}
		}
	}
	return nil, false
}

// pathStat is one row of the inventory: how often a given protobuf
// (path, wire_type) was seen + a few short samples for the bytes-wire
// case so the user can eyeball what content lives there.
type pathStat struct {
	pathKey  string
	wireType int
	count    int
	bytesLen int
	samples  []string
}

// walkPathInventory walks raw as protowire and aggregates every
// (path, wire_type) tuple it encounters into a sorted catalog.
func walkPathInventory(raw []byte) []pathStat {
	stats := map[string]*pathStat{}
	_ = protowire.Walk(raw, func(f protowire.Field) error {
		key := joinPath(f.Path) + ":" + wireTypeName(int(f.WireType))
		s, ok := stats[key]
		if !ok {
			s = &pathStat{pathKey: joinPath(f.Path), wireType: int(f.WireType)}
			stats[key] = s
		}
		s.count++
		if f.WireType == protowire.WireBytes {
			s.bytesLen += len(f.Bytes)
			if len(s.samples) < 3 && len(f.Bytes) > 0 {
				s.samples = append(s.samples, sampleString(f.Bytes))
			}
		}
		return nil
	})
	out := make([]pathStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		// Sort by path lexicographically, then wire type.
		if out[i].pathKey != out[j].pathKey {
			return out[i].pathKey < out[j].pathKey
		}
		return out[i].wireType < out[j].wireType
	})
	return out
}

// emitInventory prints the catalog grouped under the top two path
// levels so callers can scan for new candidate reasoning paths.
func emitInventory(t *testing.T, inv []pathStat) {
	t.Helper()
	t.Logf("%-22s %-8s %6s %8s   samples", "path", "wt", "count", "bytes")
	t.Logf("%s", strings.Repeat("-", 80))
	for _, s := range inv {
		samp := ""
		if len(s.samples) > 0 {
			samp = " " + s.samples[0]
		}
		t.Logf("%-22s %-8s %6d %8d  %s", s.pathKey, wireTypeShort(s.wireType), s.count, s.bytesLen, samp)
	}
}

func joinPath(p []int) string {
	parts := make([]string, len(p))
	for i, v := range p {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ".")
}

func wireTypeName(wt int) string {
	switch wt {
	case 0:
		return "varint"
	case 1:
		return "fixed64"
	case 2:
		return "bytes"
	case 5:
		return "fixed32"
	}
	return fmt.Sprintf("wt%d", wt)
}

func wireTypeShort(wt int) string {
	switch wt {
	case 0:
		return "varint"
	case 1:
		return "fix64"
	case 2:
		return "bytes"
	case 5:
		return "fix32"
	}
	return "?"
}

func sampleString(b []byte) string {
	const maxLen = 80
	// If it looks UTF-8 printable, print as quoted string; else hex.
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			printable++
		}
	}
	if len(b) > 0 && float64(printable)/float64(len(b)) > 0.85 {
		s := string(b)
		if len(s) > maxLen {
			s = s[:maxLen] + "..."
		}
		return fmt.Sprintf("%q", s)
	}
	// hex dump
	if len(b) > 24 {
		b = b[:24]
	}
	return fmt.Sprintf("hex=%x... (%d bytes)", b, len(b))
}
