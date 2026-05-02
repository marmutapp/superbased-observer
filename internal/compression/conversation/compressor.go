package conversation

import (
	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// Compressor transforms a tool_result body into a shorter form appropriate
// for its content type. Compressors are deterministic and side-effect free:
// the same input always produces the same output, and the output is safe to
// forward upstream in place of the original body. When compression would
// not reduce size, implementations may return the original body unchanged.
type Compressor interface {
	// Compress returns the compressed body. It never returns nil — an empty
	// input yields an empty output.
	Compress(body []byte) []byte
	// Type is the content type this compressor handles.
	Type() types.ContentType
}

// Registry maps a ContentType to its Compressor.
type Registry struct {
	m map[types.ContentType]Compressor
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[types.ContentType]Compressor)}
}

// DefaultRegistry builds a Registry populated with the built-in compressors
// for every content type exposed by [types.All].
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewJSONCompressor())
	r.Register(NewLogsCompressor())
	r.Register(NewTextCompressor(TextOptions{}))
	r.Register(NewDiffCompressor())
	r.Register(NewHTMLCompressor())
	r.Register(NewCodeCompressor())
	return r
}

// Register installs c under its Type. A later registration for the same type
// replaces the earlier one.
func (r *Registry) Register(c Compressor) {
	r.m[c.Type()] = c
}

// Get returns the compressor for t, or (nil, false) when none is registered.
func (r *Registry) Get(t types.ContentType) (Compressor, bool) {
	c, ok := r.m[t]
	return c, ok
}

// Compress detects the type of body (using filename as an optional hint) and
// runs the matching compressor. Returns (compressed, detected, true) on a
// successful compression, or (body, detected, false) when no compressor is
// registered for the detected type. Unknown types are returned unchanged.
func (r *Registry) Compress(body []byte, filename string) ([]byte, types.ContentType, bool) {
	t := types.Detect(body, filename)
	if t == types.Unknown {
		return body, t, false
	}
	c, ok := r.Get(t)
	if !ok {
		return body, t, false
	}
	return c.Compress(body), t, true
}
