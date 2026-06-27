package conversation

import (
	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// TextCompressor applies head+tail line truncation with an elision marker.
// Well-suited to free-form prose, logs that didn't match the logs sniff,
// and any tool output the conversation budget can't afford in full.
type TextCompressor struct {
	opts TextOptions
}

// TextOptions tunes TextCompressor.
type TextOptions struct {
	// MaxLines is the ceiling on the input line count before truncation
	// fires. Default 80 — chosen so short tool outputs pass through
	// unchanged.
	MaxLines int
	// Head and Tail are the line budgets kept on each end.
	Head, Tail int
	// Marker is the printf template for the elision line; must contain %d.
	Marker string
}

// NewTextCompressor constructs a TextCompressor, applying defaults for any
// zero-valued TextOptions fields.
func NewTextCompressor(opts TextOptions) *TextCompressor {
	if opts.MaxLines == 0 {
		opts.MaxLines = 80
	}
	if opts.Head == 0 {
		opts.Head = 30
	}
	if opts.Tail == 0 {
		opts.Tail = 30
	}
	if opts.Marker == "" {
		opts.Marker = "… [%d lines elided]"
	}
	return &TextCompressor{opts: opts}
}

// Type implements Compressor.
func (TextCompressor) Type() types.ContentType { return types.Text }

// Compress implements Compressor.
func (c TextCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	lines := splitLines(body)
	clipped := headTail(lines, c.opts.MaxLines, c.opts.Head, c.opts.Tail, c.opts.Marker)
	out := joinLines(clipped, endsWithNewline(body))
	if len(out) >= len(body) {
		return body
	}
	return out
}
