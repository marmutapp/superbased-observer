package shell

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Rule is one step in a filter chain. Push receives each input line and may
// call emit zero or more times to output transformed lines. Flush is called
// once at end-of-stream so rules can release buffered state (trailing dedup
// counts, truncate tails, etc.).
type Rule interface {
	Push(line string, emit func(string))
	Flush(emit func(string))
}

// LineDrop drops lines matching its regex.
type LineDrop struct{ re *regexp.Regexp }

// Push implements Rule.
func (r *LineDrop) Push(line string, emit func(string)) {
	if !r.re.MatchString(line) {
		emit(line)
	}
}

// Flush implements Rule. LineDrop holds no state.
func (r *LineDrop) Flush(emit func(string)) {}

func newLineDrop(s RuleSpec) (Rule, error) {
	re, err := regexp.Compile(s.Pattern)
	if err != nil {
		return nil, fmt.Errorf("line_drop: %w", err)
	}
	return &LineDrop{re: re}, nil
}

// LineKeep keeps only lines matching its regex.
type LineKeep struct{ re *regexp.Regexp }

// Push implements Rule.
func (r *LineKeep) Push(line string, emit func(string)) {
	if r.re.MatchString(line) {
		emit(line)
	}
}

// Flush implements Rule. LineKeep holds no state.
func (r *LineKeep) Flush(emit func(string)) {}

func newLineKeep(s RuleSpec) (Rule, error) {
	re, err := regexp.Compile(s.Pattern)
	if err != nil {
		return nil, fmt.Errorf("line_keep: %w", err)
	}
	return &LineKeep{re: re}, nil
}

// DedupConsecutive collapses runs of identical consecutive lines into a single
// line with a " [×N]" suffix when N > 1.
type DedupConsecutive struct {
	prev    string
	count   int
	holding bool
}

// Push implements Rule.
func (r *DedupConsecutive) Push(line string, emit func(string)) {
	if r.holding && line == r.prev {
		r.count++
		return
	}
	r.flushOne(emit)
	r.prev = line
	r.count = 1
	r.holding = true
}

// Flush implements Rule. Emits any held run.
func (r *DedupConsecutive) Flush(emit func(string)) {
	r.flushOne(emit)
	r.holding = false
}

func (r *DedupConsecutive) flushOne(emit func(string)) {
	if !r.holding {
		return
	}
	if r.count == 1 {
		emit(r.prev)
	} else {
		emit(fmt.Sprintf("%s [×%d]", r.prev, r.count))
	}
}

// Truncate keeps Head leading lines and Tail trailing lines, replacing the
// middle with a single marker line when anything was elided.
type Truncate struct {
	head   int
	tail   int
	marker string

	seen       int
	headEmit   int
	tailBuffer []string
}

// Push implements Rule.
func (r *Truncate) Push(line string, emit func(string)) {
	r.seen++
	if r.headEmit < r.head {
		r.headEmit++
		emit(line)
		return
	}
	if r.tail == 0 {
		return
	}
	r.tailBuffer = append(r.tailBuffer, line)
	if len(r.tailBuffer) > r.tail {
		r.tailBuffer = r.tailBuffer[1:]
	}
}

// Flush implements Rule. Emits the marker (when anything was dropped) followed
// by the rolling tail buffer.
func (r *Truncate) Flush(emit func(string)) {
	dropped := r.seen - r.headEmit - len(r.tailBuffer)
	if dropped > 0 {
		emit(fmt.Sprintf(r.marker, dropped))
	}
	for _, l := range r.tailBuffer {
		emit(l)
	}
}

func newTruncate(s RuleSpec) (Rule, error) {
	marker := s.Marker
	if marker == "" {
		marker = "… [%d lines elided]"
	}
	return &Truncate{head: s.Head, tail: s.Tail, marker: marker}, nil
}

// SquashWhitespace collapses runs of blank lines into a single blank line.
type SquashWhitespace struct{ lastBlank bool }

// Push implements Rule.
func (r *SquashWhitespace) Push(line string, emit func(string)) {
	blank := strings.TrimSpace(line) == ""
	if blank && r.lastBlank {
		return
	}
	r.lastBlank = blank
	emit(line)
}

// Flush implements Rule. SquashWhitespace holds no cross-line buffer.
func (r *SquashWhitespace) Flush(emit func(string)) {}

func (e *Engine) buildRule(s RuleSpec) (Rule, error) {
	switch s.Type {
	case "line_drop":
		return newLineDrop(s)
	case "line_keep":
		return newLineKeep(s)
	case "dedup_consecutive":
		return &DedupConsecutive{}, nil
	case "truncate":
		return newTruncate(s)
	case "squash_whitespace":
		return &SquashWhitespace{}, nil
	case "hook":
		factory, ok := e.hooks[s.Hook]
		if !ok {
			return nil, fmt.Errorf("hook %q not registered", s.Hook)
		}
		return factory(), nil
	default:
		return nil, fmt.Errorf("unknown rule type %q", s.Type)
	}
}

func validateSpec(s FilterSpec) error {
	if strings.TrimSpace(s.Command) == "" {
		return errors.New("filter.command is required")
	}
	for i, r := range s.Rules {
		switch r.Type {
		case "line_drop", "line_keep":
			if r.Pattern == "" {
				return fmt.Errorf("rule[%d]: %s requires pattern", i, r.Type)
			}
			if _, err := regexp.Compile(r.Pattern); err != nil {
				return fmt.Errorf("rule[%d]: %s pattern: %w", i, r.Type, err)
			}
		case "truncate":
			if r.Head < 0 || r.Tail < 0 {
				return fmt.Errorf("rule[%d]: truncate head/tail must be >= 0", i)
			}
			if r.Head == 0 && r.Tail == 0 {
				return fmt.Errorf("rule[%d]: truncate requires head or tail > 0", i)
			}
			if r.Marker != "" && !strings.Contains(r.Marker, "%d") {
				return fmt.Errorf("rule[%d]: truncate marker must contain %%d", i)
			}
		case "hook":
			if r.Hook == "" {
				return fmt.Errorf("rule[%d]: hook requires name", i)
			}
		case "dedup_consecutive", "squash_whitespace":
			// no params
		case "":
			return fmt.Errorf("rule[%d]: type is required", i)
		default:
			return fmt.Errorf("rule[%d]: unknown type %q", i, r.Type)
		}
	}
	return nil
}
