package semantic

import (
	"context"
	"slices"
	"testing"
)

func TestSplitIdentifier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"handleClick", []string{"handleclick", "handle", "click"}},
		{"handleHTTPRequest", []string{"handlehttprequest", "handle", "http", "request"}},
		{"user_id", []string{"user", "id"}},
		{"Editor.handleClick", []string{"editor", "handle", "click", "handleclick"}},
		{"base64Encode", []string{"base64encode", "base", "64", "encode"}},
		{"", nil},
	}
	for _, c := range cases {
		got := SplitIdentifier(c.in)
		for _, w := range c.want {
			if !slices.Contains(got, w) {
				t.Errorf("SplitIdentifier(%q) = %v, missing %q", c.in, got, w)
			}
		}
	}
}

func TestVectorizeCosine_RelatedHigherThanUnrelated(t *testing.T) {
	t.Parallel()
	q := Vectorize("handleClick mouse event handler")
	related := Vectorize("handleMouseClick event")
	unrelated := Vectorize("parseDatabaseConnectionString sql")
	simRel := Cosine(q, related)
	simUnrel := Cosine(q, unrelated)
	if simRel <= simUnrel {
		t.Errorf("related cosine %.3f should exceed unrelated %.3f", simRel, simUnrel)
	}
	if simRel <= 0 {
		t.Errorf("related cosine should be positive, got %.3f", simRel)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	t.Parallel()
	v := Vectorize("func Run() error")
	got := Unpack(Pack(v))
	if len(got) != len(v) {
		t.Fatalf("len: got %d want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("idx %d: got %v want %v", i, got[i], v[i])
		}
	}
	if Unpack(nil) != nil || Unpack([]byte{1, 2, 3}) != nil {
		t.Errorf("Unpack should reject malformed blobs")
	}
}

func TestEmbedderInterface(t *testing.T) {
	t.Parallel()
	var e Embedder = TFIDFEmbedder{}
	if e.Dim() != Dim {
		t.Errorf("Dim = %d want %d", e.Dim(), Dim)
	}
	v, err := e.Embed(context.Background(), "class Editor")
	if err != nil || len(v) != Dim {
		t.Errorf("Embed: v len %d err %v", len(v), err)
	}
}

func TestLSHBuckets_ClonesShareBuckets(t *testing.T) {
	t.Parallel()
	// Two near-identical token sets should share most LSH buckets; a
	// disjoint set should share few/none.
	a := Tokenize("func processOrder(order Order) error validate save")
	b := Tokenize("func processOrder(order Order) error validate store") // 1 token differs
	c := Tokenize("type WidgetFactory interface render dispose mount")

	ba, bb, bc := LSHBuckets(a), LSHBuckets(b), LSHBuckets(c)
	if len(ba) != Bands || len(bb) != Bands || len(bc) != Bands {
		t.Fatalf("expected %d bands; got %d/%d/%d", Bands, len(ba), len(bb), len(bc))
	}
	shared := func(x, y []uint64) int {
		n := 0
		for i := range x {
			if x[i] == y[i] {
				n++
			}
		}
		return n
	}
	abShared := shared(ba, bb)
	acShared := shared(ba, bc)
	if abShared <= acShared {
		t.Errorf("near-clone shared buckets (%d) should exceed unrelated (%d)", abShared, acShared)
	}
	if abShared == 0 {
		t.Errorf("near-clones should share at least one LSH bucket")
	}
}

func TestSignatureJaccardCorrelation(t *testing.T) {
	t.Parallel()
	a := Tokenize("alpha beta gamma delta epsilon zeta")
	b := Tokenize("alpha beta gamma delta epsilon theta") // 5/6 overlap-ish
	est := EstimateJaccard(Signature(a), Signature(b))
	exact := Jaccard(a, b)
	// MinHash is an estimator; allow a wide tolerance but they should be
	// on the same side of 0.5 for a high-overlap pair.
	if est < 0.3 {
		t.Errorf("estimated jaccard %.2f unexpectedly low (exact %.2f)", est, exact)
	}
}

func TestEmptyInputsSafe(t *testing.T) {
	t.Parallel()
	if Signature(nil) != nil || LSHBuckets(nil) != nil {
		t.Errorf("empty token set should yield nil signature/buckets")
	}
	if len(Vectorize("")) != Dim {
		t.Errorf("Vectorize(\"\") should be a zero vector of len Dim")
	}
	if TokenStream("") != "" {
		t.Errorf("TokenStream(\"\") should be empty")
	}
}

// shared counts equal band buckets between two body-shingle signatures.
func sharedBuckets(a, b []uint64) int {
	set := map[uint64]bool{}
	for _, h := range a {
		set[h] = true
	}
	n := 0
	for _, h := range b {
		if set[h] {
			n++
		}
	}
	return n
}

func TestBodyShingleBuckets(t *testing.T) {
	t.Parallel()
	if BodyShingleBuckets("") != nil {
		t.Error("empty body should yield nil buckets")
	}
	bodyA := `func alpha(x int) int { total := 0; for i := 0; i < x; i++ { total += i }; return total }`
	// A copy-paste clone: same body, only the function name changed. A
	// signature-based similarity would miss this (different name+signature);
	// body shingles must catch it.
	bodyB := `func beta(x int) int { total := 0; for i := 0; i < x; i++ { total += i }; return total }`
	// An unrelated body.
	bodyC := `func gamma(s string) bool { return len(s) > 0 && s[0] == '/' }`

	aa := BodyShingleBuckets(bodyA)
	if len(aa) != Bands {
		t.Fatalf("buckets len = %d, want %d", len(aa), Bands)
	}
	// Identical body -> all bands equal.
	if got := sharedBuckets(aa, BodyShingleBuckets(bodyA)); got != Bands {
		t.Errorf("identical bodies share %d/%d bands, want all", got, Bands)
	}
	// Rename-only clone -> shares multiple bands, so the LSH self-join
	// surfaces it as a candidate (sharing ANY band is enough; the count
	// just reflects strength).
	clone := sharedBuckets(aa, BodyShingleBuckets(bodyB))
	if clone < 2 {
		t.Errorf("rename-only clone shares %d/%d bands, want >= 2 (a robust candidate)", clone, Bands)
	}
	// Unrelated body -> fewer shared bands than the clone (discriminating).
	other := sharedBuckets(aa, BodyShingleBuckets(bodyC))
	if other >= clone {
		t.Errorf("unrelated body shares %d bands >= clone's %d — body shingles not discriminating", other, clone)
	}
}
