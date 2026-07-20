package shell

import (
	"bytes"
	"io"
)

// Stats summarizes input/output sizes and line counts for one filtered stream.
type Stats struct {
	BytesIn  int64
	BytesOut int64
	LinesIn  int64
	LinesOut int64
}

// ReductionRatio returns (BytesIn - BytesOut) / BytesIn. Zero when BytesIn is 0
// or when output was not smaller (negative ratios are clamped to 0).
func (s Stats) ReductionRatio() float64 {
	if s.BytesIn == 0 || s.BytesOut >= s.BytesIn {
		return 0
	}
	return float64(s.BytesIn-s.BytesOut) / float64(s.BytesIn)
}

// Stream is an io.WriteCloser that line-buffers input and pushes each complete
// line through a chain of rules. Close flushes any partial trailing line plus
// every rule's buffered state.
//
// When no filter spec matched at construction time the chain is empty; Stream
// then operates as a line-buffered pass-through. BytesOut accounting always
// reflects what was actually written to the underlying writer.
type Stream struct {
	w       io.Writer
	spec    *FilterSpec
	rules   []Rule
	partial []byte
	stats   Stats
	closed  bool
}

func newStream(w io.Writer, spec *FilterSpec) *Stream {
	return &Stream{w: w, spec: spec}
}

// Spec returns the filter spec driving the stream, or nil in pass-through mode.
func (s *Stream) Spec() *FilterSpec { return s.spec }

// Stats returns observed byte/line counts. Safe to call before Close; the
// counts advance as writes are processed.
func (s *Stream) Stats() Stats { return s.stats }

// Write implements io.Writer. Input may contain partial lines; leftover bytes
// without a trailing newline are buffered and concatenated with the next Write.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	s.stats.BytesIn += int64(len(p))
	buf := p
	if len(s.partial) > 0 {
		combined := make([]byte, 0, len(s.partial)+len(p))
		combined = append(combined, s.partial...)
		combined = append(combined, p...)
		buf = combined
		s.partial = s.partial[:0]
	}
	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			if len(buf) > 0 {
				s.partial = append(s.partial[:0], buf...)
			}
			return len(p), nil
		}
		line := string(buf[:idx])
		s.stats.LinesIn++
		if err := s.pushLine(line); err != nil {
			return 0, err
		}
		buf = buf[idx+1:]
	}
}

// Close flushes the trailing partial line (if any) and every rule's buffered
// state. Further writes return io.ErrClosedPipe. Double-close is a no-op.
//
// A trailing partial line — input that didn't end with a newline — is emitted
// with a newline appended, so BytesOut may exceed BytesIn by 1 in that case.
func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if len(s.partial) > 0 {
		s.stats.LinesIn++
		if err := s.pushLine(string(s.partial)); err != nil {
			return err
		}
		s.partial = nil
	}
	return s.flushAll()
}

func (s *Stream) pushLine(line string) error {
	var writeErr error
	emit := s.emitter(&writeErr)
	runChain(s.rules, 0, line, emit)
	return writeErr
}

func (s *Stream) flushAll() error {
	var writeErr error
	emit := s.emitter(&writeErr)
	flushChain(s.rules, 0, emit)
	return writeErr
}

func (s *Stream) emitter(writeErr *error) func(string) {
	return func(line string) {
		if *writeErr != nil {
			return
		}
		if _, err := io.WriteString(s.w, line); err != nil {
			*writeErr = err
			return
		}
		if _, err := io.WriteString(s.w, "\n"); err != nil {
			*writeErr = err
			return
		}
		s.stats.BytesOut += int64(len(line)) + 1
		s.stats.LinesOut++
	}
}

func runChain(rules []Rule, idx int, line string, out func(string)) {
	if idx >= len(rules) {
		out(line)
		return
	}
	rules[idx].Push(line, func(emitted string) {
		runChain(rules, idx+1, emitted, out)
	})
}

func flushChain(rules []Rule, idx int, out func(string)) {
	if idx >= len(rules) {
		return
	}
	rules[idx].Flush(func(emitted string) {
		runChain(rules, idx+1, emitted, out)
	})
	flushChain(rules, idx+1, out)
}
