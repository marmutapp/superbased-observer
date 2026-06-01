package protowire

import (
	"regexp"
	"unicode/utf8"
)

// IsLikelyText reports whether buf looks like message body text:
// at least 8 bytes, ≥90% printable UTF-8 (ASCII printable + valid
// multi-byte runes), no embedded NULs.
//
// Used to identify message bodies / tool output blobs in walked
// proto streams without committing to specific field numbers.
func IsLikelyText(buf []byte) bool {
	if len(buf) < 8 {
		return false
	}
	printable := 0
	pos := 0
	for pos < len(buf) {
		r, n := utf8.DecodeRune(buf[pos:])
		if r == utf8.RuneError && n == 1 {
			return false
		}
		// Accept ASCII printable + standard whitespace. Other control chars
		// (BOM, etc.) survive in real text, so we neither count them toward
		// the printable ratio nor immediately reject the buffer over them.
		if (r >= 0x20 && r != 0x7f) || r == '\n' || r == '\r' || r == '\t' {
			printable += n
		}
		pos += n
	}
	return printable*10 >= len(buf)*9
}

// IsLikelyUnixTimestamp reports whether v plausibly represents a unix
// epoch time, in either seconds or milliseconds. The seconds window
// accepts 2023-08-26..2033-09-09; the milliseconds window covers the
// same range scaled. Adjust if Antigravity rolls over to nanoseconds.
func IsLikelyUnixTimestamp(v uint64) bool {
	const (
		secStart = uint64(1.7e9)
		secEnd   = uint64(2.0e9)
		msStart  = uint64(1.7e12)
		msEnd    = uint64(2.0e12)
	)
	if v >= secStart && v < secEnd {
		return true
	}
	if v >= msStart && v < msEnd {
		return true
	}
	return false
}

var toolNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{2,31}$`)

// IsLikelyToolName reports whether s matches the broad tool-naming
// convention used across Antigravity, Copilot, Gemini CLI, etc.
//
// Both camelCase (`runShellCommand`) and snake_case (`google_web_search`)
// are accepted. Length 3..32 chars, must start with a letter.
//
// Note: text bodies like "ok" or "Help me debug" don't match the
// pattern, so this works as a discriminator from message text in
// the wire-walk classifier.
func IsLikelyToolName(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	return toolNameRE.MatchString(s)
}

// ValidatesAsProto walks at most n top-level records of buf and
// reports whether every record has a valid wire-type and stays in
// bounds. Returns the count of records successfully walked (capped
// at n). Used by oscrypt.DecryptCTR's try-loop to validate plaintext
// candidates after each cipher attempt.
func ValidatesAsProto(buf []byte, n int) int {
	if len(buf) == 0 {
		return 0
	}
	matched := 0
	pos := 0
	for matched < n && pos < len(buf) {
		tag, consumed, err := decodeVarint(buf[pos:])
		if err != nil || consumed == 0 {
			return matched
		}
		pos += consumed
		fn := int(tag >> 3)
		wt := WireType(tag & 0x07)
		if fn <= 0 || (wt != WireVarint && wt != WireFixed64 && wt != WireBytes && wt != WireFixed32) {
			return matched
		}
		switch wt {
		case WireVarint:
			_, consumed, err := decodeVarint(buf[pos:])
			if err != nil {
				return matched
			}
			pos += consumed
		case WireFixed64:
			if pos+8 > len(buf) {
				return matched
			}
			pos += 8
		case WireFixed32:
			if pos+4 > len(buf) {
				return matched
			}
			pos += 4
		case WireBytes:
			length, consumed, err := decodeVarint(buf[pos:])
			if err != nil {
				return matched
			}
			pos += consumed
			if uint64(pos)+length > uint64(len(buf)) {
				return matched
			}
			pos += int(length)
		}
		matched++
	}
	return matched
}

// FindProtobufStart scans buf[0..maxSkip] and returns the first
// offset where the byte parses as a valid wire-format tag. Returns
// 0 if no valid start is found within maxSkip bytes — caller should
// fail rather than blindly use the result. Used to skip the 0–4
// alignment bytes that Antigravity sometimes prepends.
func FindProtobufStart(buf []byte, maxSkip int) (int, bool) {
	limit := maxSkip
	if limit >= len(buf) {
		limit = len(buf) - 1
	}
	for skip := 0; skip <= limit; skip++ {
		b := buf[skip]
		wt := WireType(b & 0x07)
		fn := b >> 3
		if fn == 0 {
			continue
		}
		switch wt {
		case WireVarint, WireFixed64, WireBytes, WireFixed32:
			return skip, true
		}
	}
	return 0, false
}
