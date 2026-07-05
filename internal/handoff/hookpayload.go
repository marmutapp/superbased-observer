package handoff

import (
	"fmt"
	"strings"
)

// HookPayload budgets a rendered handover document for the SessionStart
// additionalContext lane (plan §10 inject_hook; Phase 0 finding D-P0.2).
//
// Claude Code delivers additionalContext intact only up to ~8KB; a larger
// payload it silently truncates to a ~1.9KB preview plus an output-file
// pointer, dropping the tail. So we do the budgeting ourselves,
// deterministically: when the whole doc fits within maxBytes it is
// delivered verbatim; otherwise we keep the marker + document head,
// truncate on a line boundary to leave room, and append an explicit
// pointer telling the target model to read docPath for the full handover.
// The result is guaranteed never to exceed maxBytes.
//
// It is pure string logic — reading the doc from disk and resolving the
// budget both happen at the cmd boundary.
func HookPayload(doc, docPath string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(doc) <= maxBytes {
		return doc
	}

	pointer := hookPointer(docPath)
	// Degenerate budget: even the pointer alone doesn't fit. Hard-truncate
	// the pointer so the invariant (never exceed maxBytes) still holds.
	if len(pointer) >= maxBytes {
		return truncateOnRune(pointer, maxBytes)
	}

	budget := maxBytes - len(pointer)
	// Prefer a clean line boundary so we never present a half-written line
	// to the model. The marker line is first, so it always survives when
	// the budget clears the first newline (the overwhelmingly common case).
	head := doc[:budget]
	if nl := strings.LastIndexByte(head, '\n'); nl > 0 {
		head = head[:nl+1]
	} else {
		// No newline in range — cut the FULL doc on a rune boundary at or
		// below budget (slicing head first would let n >= len(head) short-
		// circuit and return a mid-rune slice).
		head = truncateOnRune(doc, budget)
	}
	return head + pointer
}

// hookPointer is the footer appended to a truncated hook payload. It always
// fits inside maxBytes for any realistic budget and names the on-disk doc.
func hookPointer(docPath string) string {
	if docPath == "" {
		return "\n\n[Handover truncated to fit the session-start budget. The full " +
			"handover was written to disk — ask me to read the HANDOFF-*.md file " +
			"in this project before continuing.]\n"
	}
	return fmt.Sprintf("\n\n[Handover truncated to fit the session-start budget. Read the "+
		"full handover at %s before continuing.]\n", docPath)
}

// truncateOnRune returns s clipped to at most n bytes without splitting a
// trailing multi-byte UTF-8 rune.
func truncateOnRune(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	// Back up off any continuation bytes (0b10xxxxxx) so we cut on a rune
	// boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
