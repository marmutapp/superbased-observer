package conversation

import (
	"bytes"
	"regexp"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// HTMLCompressor strips <script> and <style> blocks (including their
// contents) and HTML comments. The output remains valid HTML-ish enough for
// the model to read the visible structure without the weight of inline
// scripts or CSS.
type HTMLCompressor struct{}

// NewHTMLCompressor constructs an HTMLCompressor.
func NewHTMLCompressor() *HTMLCompressor { return &HTMLCompressor{} }

// Type implements Compressor.
func (HTMLCompressor) Type() types.ContentType { return types.HTML }

// reScript matches a <script ...>...</script> block, lazily matching the
// body so adjacent blocks don't swallow each other. (?is) = case-insensitive
// + dot-matches-newline. reStyle is the analog for <style>.
var (
	reScript  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	reStyle   = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// Compress implements Compressor.
func (c HTMLCompressor) Compress(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	out := body
	out = reScript.ReplaceAll(out, []byte("<!-- script stripped -->"))
	out = reStyle.ReplaceAll(out, []byte("<!-- style stripped -->"))
	// Second pass drops the stripped markers *and* any other comments to
	// keep the compressed body compact.
	out = reComment.ReplaceAll(out, nil)
	out = bytes.TrimSpace(out)
	if len(out) >= len(body) {
		return body
	}
	if endsWithNewline(body) && !endsWithNewline(out) {
		out = append(out, '\n')
	}
	return out
}
