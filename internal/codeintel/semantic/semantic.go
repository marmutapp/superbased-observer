package semantic

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"
)

// Dim is the embedding vector dimensionality. Raised 256 → 512 (W4, the
// embedder-tuning pass): at 256 the feature-hash collision rate on short
// identifier+signature token sets polluted `related` ranking with
// hash-collision neighbours; a measured variant sweep showed 512 is the
// recall/storage knee (related recall@5 39%→45%, recall@10 58%→67% over a
// labelled sibling set, vs no further reliable gain past 512 at ~2× the
// storage of 1024). Pure-TF only — the same sweep showed IDF weighting
// *hurt* recall here (it downweights the shared domain tokens, e.g.
// `CodeIntel*`, that actually bind true siblings). `dim` is recorded per
// row in `codeintel_embeddings`, so a re-index migrates the vectors;
// neighbours within a project are always one consistent dim. See
// docs/codeintel/decisions.md ADR-0008.
const Dim = 512

// Embedder turns text into a fixed-width vector. The default
// implementation is the feature-hashed TF embedder; a neural backend can
// be swapped in behind this interface without touching callers (ADR-0003,
// ADR-0004). Embed takes a context so a future backend that calls out
// (ONNX-via-wazero, a local model server) fits the seam; the default
// ignores it and never errors.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// TFIDFEmbedder is the default CGO-free embedder: feature-hashed term
// frequency, L2-normalized, 256-dim. (Named TF-IDF for continuity with
// the plan; the shipping scheme is feature-hashed TF — no corpus IDF —
// which is good enough for v1 per ADR-0003.)
type TFIDFEmbedder struct{}

// Embed implements Embedder.
func (TFIDFEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return Vectorize(text), nil
}

// Dim implements Embedder.
func (TFIDFEmbedder) Dim() int { return Dim }

// SplitIdentifier breaks a programming identifier into lowercase word
// pieces across camelCase, PascalCase, snake_case, kebab-case, dotted,
// and letter/digit boundaries: "handleHTTPRequest" -> [handle http
// request], "user_id" -> [user id], "Editor.handleClick" -> [editor
// handle click]. Acronyms are kept whole (HTTPS -> https). Returns the
// original lowercased token too so an exact match still scores.
func SplitIdentifier(id string) []string {
	if id == "" {
		return nil
	}
	// First split on non-alphanumeric separators.
	fields := strings.FieldsFunc(id, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]struct{}{}
	var out []string
	add := func(w string) {
		w = strings.ToLower(w)
		if w == "" {
			return
		}
		if _, dup := seen[w]; dup {
			return
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	for _, f := range fields {
		add(f) // whole field (lowercased)
		for _, piece := range splitCamel(f) {
			add(piece)
		}
	}
	return out
}

// splitCamel splits a single separator-free field on case and
// letter/digit boundaries, keeping runs of capitals (acronyms) together.
func splitCamel(s string) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var pieces []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			// camelCase boundary: handle|Click
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			// acronym end before a new word: HTTP|Request
			boundary = true
		case unicode.IsDigit(prev) != unicode.IsDigit(cur):
			// letter/digit transition: utf|8, base|64
			boundary = true
		}
		if boundary {
			pieces = append(pieces, string(runes[start:i]))
			start = i
		}
	}
	pieces = append(pieces, string(runes[start:]))
	return pieces
}

// Tokenize produces the searchable token stream for a blob of identifier-
// bearing text (name + fqn + signature). It splits on non-identifier
// runes, then identifier-splits each piece, de-duplicating.
func Tokenize(text string) []string {
	if text == "" {
		return nil
	}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	seen := map[string]struct{}{}
	var out []string
	for _, f := range fields {
		for _, w := range SplitIdentifier(f) {
			if _, dup := seen[w]; dup {
				continue
			}
			seen[w] = struct{}{}
			out = append(out, w)
		}
	}
	return out
}

// TokenStream returns the space-joined Tokenize output — the value stored
// in codeintel_fts.tokens so the unicode61 tokenizer indexes our pre-split
// words.
func TokenStream(text string) string {
	return strings.Join(Tokenize(text), " ")
}

// Vectorize produces a 256-dim feature-hashed, L2-normalized TF vector
// from text (identical scheme to internal/compression/indexing).
func Vectorize(text string) []float32 {
	vec := make([]float32, Dim)
	toks := Tokenize(text)
	if len(toks) == 0 {
		return vec
	}
	for _, w := range toks {
		vec[hashWord(w)%uint32(Dim)]++
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vec
}

// Pack serializes a vector to a little-endian []float32 BLOB.
func Pack(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// Unpack reverses Pack. Returns nil on a malformed/empty blob.
func Unpack(blob []byte) []float32 {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(blob)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

// Cosine returns the cosine similarity of two equal-length vectors
// (0 when lengths differ or either is zero).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// --- MinHash + LSH (SIMILAR_TO near-clone) ---------------------------

const (
	// numHashes is the MinHash signature length. Bands × rows == numHashes.
	numHashes = 48
	// Bands is the number of LSH bands; two items sharing any band bucket
	// are clone candidates. More bands = higher recall (lower threshold).
	Bands = 12
	rows  = numHashes / Bands // 4 rows per band

	mhPrime = uint64(0xFFFFFFFFFFFFFFC5) // largest 64-bit prime
)

// mhSeeds are the (a,b) coefficients for the numHashes universal hash
// functions h_i(x) = a_i*x + b_i mod prime. Deterministic (derived from a
// fixed FNV walk) so signatures are reproducible across runs/machines.
var mhA, mhB = genSeeds()

func genSeeds() (a, b [numHashes]uint64) {
	h := fnv.New64a()
	for i := range numHashes {
		h.Reset()
		_, _ = h.Write([]byte{byte(i), 'a'})
		a[i] = h.Sum64() | 1 // odd, non-zero
		h.Reset()
		_, _ = h.Write([]byte{byte(i), 'b'})
		b[i] = h.Sum64()
	}
	return a, b
}

// Signature computes the MinHash signature of a token set. Empty input
// yields a nil signature (no buckets).
func Signature(tokens []string) []uint64 {
	if len(tokens) == 0 {
		return nil
	}
	// De-dup to a set — MinHash is over the SET of tokens.
	set := map[string]struct{}{}
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	sig := make([]uint64, numHashes)
	for i := range sig {
		sig[i] = math.MaxUint64
	}
	for t := range set {
		x := fnv64(t)
		for i := range numHashes {
			hv := mhA[i]*x + mhB[i] // mod 2^64 is fine for LSH bucketing
			if hv%mhPrime < sig[i] {
				sig[i] = hv % mhPrime
			}
		}
	}
	return sig
}

// LSHBuckets returns one bucket hash per band for a token set — the values
// persisted to codeintel_minhash. Two nodes sharing a (band, hash) bucket
// are near-clone candidates. Length == Bands (nil for empty input).
func LSHBuckets(tokens []string) []uint64 {
	sig := Signature(tokens)
	if sig == nil {
		return nil
	}
	out := make([]uint64, Bands)
	for b := range Bands {
		h := fnv.New64a()
		var buf [8]byte
		for r := range rows {
			binary.LittleEndian.PutUint64(buf[:], sig[b*rows+r])
			_, _ = h.Write(buf[:])
		}
		out[b] = h.Sum64()
	}
	return out
}

// shingleK is the body k-gram width for near-clone MinHash. k consecutive
// raw tokens form one shingle, so copy-pasted bodies share shingles even
// when surrounding code differs.
const shingleK = 3

// BodyShingleBuckets returns the LSH band buckets of a code body's k-gram
// shingles — the near-clone signature persisted to codeintel_minhash for
// SIMILAR_TO (W3). It shingles the ORDER-PRESERVING, lowercased raw token
// stream (so structure/repetition matters, unlike the identifier SET used
// for search), then MinHashes the shingle set. A body with fewer than k
// tokens falls back to its token set; an empty body yields nil.
func BodyShingleBuckets(body string) []uint64 {
	toks := bodyShingleTokens(body)
	if len(toks) == 0 {
		return nil
	}
	var shingles []string
	if len(toks) < shingleK {
		shingles = toks
	} else {
		shingles = make([]string, 0, len(toks)-shingleK+1)
		for i := 0; i+shingleK <= len(toks); i++ {
			shingles = append(shingles, strings.Join(toks[i:i+shingleK], "\x1f"))
		}
	}
	return LSHBuckets(shingles)
}

// bodyShingleTokens splits a code body into its lowercased raw token stream,
// preserving order AND duplicates (the k-shingle basis). Distinct from
// Tokenize, which de-dups and camel-splits for search.
func bodyShingleTokens(text string) []string {
	if text == "" {
		return nil
	}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.ToLower(f)
	}
	return out
}

// EstimateJaccard estimates the Jaccard similarity of two token sets from
// their MinHash signatures (fraction of equal positions). Used in tests
// and as an optional confirm step; SIMILAR_TO ranking in the store uses
// shared-bucket count, which is the LSH proxy.
func EstimateJaccard(a, b []uint64) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	eq := 0
	for i := range a {
		if a[i] == b[i] {
			eq++
		}
	}
	return float64(eq) / float64(n)
}

// Jaccard is the exact Jaccard of two token sets (test/ground-truth aid).
func Jaccard(a, b []string) float64 {
	sa, sb := map[string]struct{}{}, map[string]struct{}{}
	for _, t := range a {
		sa[t] = struct{}{}
	}
	for _, t := range b {
		sb[t] = struct{}{}
	}
	if len(sa) == 0 && len(sb) == 0 {
		return 1
	}
	inter := 0
	for t := range sa {
		if _, ok := sb[t]; ok {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func hashWord(w string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(w))
	return h.Sum32()
}

func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// SortedTokens returns Tokenize(text) sorted — handy for deterministic
// test assertions.
func SortedTokens(text string) []string {
	t := Tokenize(text)
	sort.Strings(t)
	return t
}
