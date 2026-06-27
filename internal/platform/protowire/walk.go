package protowire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WireType is the 3-bit wire type from a protobuf tag.
type WireType uint8

const (
	WireVarint  WireType = 0
	WireFixed64 WireType = 1
	WireBytes   WireType = 2
	WireFixed32 WireType = 5
)

// String renders the wire type for debug logs.
func (w WireType) String() string {
	switch w {
	case WireVarint:
		return "varint"
	case WireFixed64:
		return "fixed64"
	case WireBytes:
		return "bytes"
	case WireFixed32:
		return "fixed32"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(w))
	}
}

// Field is one wire-format record yielded by Walk.
type Field struct {
	// Path is the field-number trail from the root message to this
	// field. Length 1 = top-level field; length 2 = field inside a
	// length-delimited submessage; etc. Allocated per-field; callers
	// may retain or copy as needed.
	Path []int
	// Depth equals len(Path) - 1 for convenience.
	Depth int
	// FieldNumber is the last element of Path.
	FieldNumber int
	// WireType is the field's wire type.
	WireType WireType
	// Bytes carries the raw payload for length-delimited fields and
	// the raw 4 / 8 bytes for fixed32 / fixed64. Empty for varints.
	// The slice references the input buffer — callers must not mutate
	// or retain past the next Walk iteration unless they copy.
	Bytes []byte
	// Varint carries the decoded varint value for WireVarint, the
	// little-endian uint32 for WireFixed32 (zero-extended), and the
	// little-endian uint64 for WireFixed64. Zero for WireBytes.
	Varint uint64
}

// ErrTruncated signals that the wire stream ended mid-field.
var ErrTruncated = errors.New("protowire: truncated input")

// ErrInvalidWireType signals that an undefined wire type (3 or 4) was
// encountered, which Antigravity does not use.
var ErrInvalidWireType = errors.New("protowire: invalid wire type (deprecated start/end group)")

// Walk traverses buf and invokes yield for every field encountered.
// Length-delimited fields whose contents look like a nested message
// are recursively walked. Returning a non-nil error from yield aborts
// the walk and propagates the error up.
//
// Recurse-into-submessage decision is conservative: only when the
// content is at least 2 bytes long and its first byte parses as a
// valid wire-format tag. Falsely-recognized text-shaped data won't
// pass the deeper walk and will appear to the caller as normal
// length-delimited bytes via the parent yield call (text-shaped
// fields are also yielded — they just aren't recursed into here).
func Walk(buf []byte, yield func(Field) error) error {
	return walk(buf, nil, yield)
}

func walk(buf []byte, path []int, yield func(Field) error) error {
	pos := 0
	for pos < len(buf) {
		tag, n, err := decodeVarint(buf[pos:])
		if err != nil {
			return fmt.Errorf("protowire.Walk: tag at %d: %w", pos, err)
		}
		pos += n
		fn := int(tag >> 3)
		wt := WireType(tag & 0x07)
		if wt != WireVarint && wt != WireFixed64 && wt != WireBytes && wt != WireFixed32 {
			return ErrInvalidWireType
		}
		if fn <= 0 {
			return fmt.Errorf("protowire.Walk: invalid field number %d", fn)
		}

		field := Field{
			FieldNumber: fn,
			WireType:    wt,
		}

		switch wt {
		case WireVarint:
			v, n, err := decodeVarint(buf[pos:])
			if err != nil {
				return fmt.Errorf("protowire.Walk: varint payload at %d: %w", pos, err)
			}
			pos += n
			field.Varint = v
		case WireFixed64:
			if pos+8 > len(buf) {
				return ErrTruncated
			}
			field.Bytes = buf[pos : pos+8]
			field.Varint = binary.LittleEndian.Uint64(field.Bytes)
			pos += 8
		case WireFixed32:
			if pos+4 > len(buf) {
				return ErrTruncated
			}
			field.Bytes = buf[pos : pos+4]
			field.Varint = uint64(binary.LittleEndian.Uint32(field.Bytes))
			pos += 4
		case WireBytes:
			length, n, err := decodeVarint(buf[pos:])
			if err != nil {
				return fmt.Errorf("protowire.Walk: bytes length at %d: %w", pos, err)
			}
			pos += n
			if uint64(pos)+length > uint64(len(buf)) {
				return ErrTruncated
			}
			field.Bytes = buf[pos : pos+int(length)]
			pos += int(length)
		}

		field.Path = append(append([]int(nil), path...), fn)
		field.Depth = len(field.Path) - 1
		if err := yield(field); err != nil {
			return err
		}

		// Recurse into submessage when the payload looks like nested wire format.
		if wt == WireBytes && shouldRecurse(field.Bytes) {
			if err := walk(field.Bytes, field.Path, yield); err != nil && !errors.Is(err, ErrInvalidWireType) && !errors.Is(err, ErrTruncated) {
				// Treat invalid/truncated nested walks as soft failures —
				// the caller already saw the parent field. Hard errors
				// (yield returning) propagate.
				return err
			}
		}
	}
	return nil
}

// shouldRecurse reports whether a length-delimited payload is a
// nested protobuf message. The decision uses a single strong signal:
// "does the payload parse as proto all the way to the end?"
//
// Real submessages validate cleanly to EOF. Text bytes fail the
// wire-type check within the first few bytes — a string like "hello"
// (5 bytes) parses one varint then runs out, which is fine in
// isolation but doesn't match "consumed every byte". We track that
// distinction by walking the entire buf and comparing the final
// position against len(buf).
//
// 2-byte payloads (a single one-byte-varint) also recurse, which is
// fine — a yield call on the inner field is harmless and produces
// the same Field the parent would have anyway.
func shouldRecurse(buf []byte) bool {
	if len(buf) < 2 {
		return false
	}
	return walkConsumesAll(buf)
}

// walkConsumesAll reports whether a sequential proto walk over buf
// consumes every byte without error. It's a structural validator —
// no recursion, no field semantics. The payload's first byte being
// a valid wire-format tag is necessary but not sufficient; the
// whole buf must parse cleanly to EOF.
func walkConsumesAll(buf []byte) bool {
	pos := 0
	for pos < len(buf) {
		tag, n, err := decodeVarint(buf[pos:])
		if err != nil {
			return false
		}
		pos += n
		fn := int(tag >> 3)
		wt := WireType(tag & 0x07)
		if fn <= 0 || (wt != WireVarint && wt != WireFixed64 && wt != WireBytes && wt != WireFixed32) {
			return false
		}
		switch wt {
		case WireVarint:
			_, n, err := decodeVarint(buf[pos:])
			if err != nil {
				return false
			}
			pos += n
		case WireFixed64:
			if pos+8 > len(buf) {
				return false
			}
			pos += 8
		case WireFixed32:
			if pos+4 > len(buf) {
				return false
			}
			pos += 4
		case WireBytes:
			length, n, err := decodeVarint(buf[pos:])
			if err != nil {
				return false
			}
			pos += n
			if uint64(pos)+length > uint64(len(buf)) {
				return false
			}
			pos += int(length)
		}
	}
	return pos == len(buf)
}

// decodeVarint reads a single varint from b. Returns (value, bytes consumed, err).
func decodeVarint(b []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		bb := b[i]
		if shift >= 64 {
			return 0, 0, errors.New("protowire: varint overflow")
		}
		result |= uint64(bb&0x7f) << shift
		if bb&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, ErrTruncated
}

// EncodeTag returns the on-wire representation of (fn, wt).
func EncodeTag(fn int, wt WireType) []byte {
	return AppendVarint(nil, uint64(fn)<<3|uint64(wt))
}

// AppendVarint appends value's varint encoding to dst and returns the
// extended slice. Useful for tests that need to construct synthetic
// proto bytes.
func AppendVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

// AppendBytesField appends a length-delimited field (tag + length + bytes) to dst.
func AppendBytesField(dst []byte, fn int, payload []byte) []byte {
	dst = append(dst, EncodeTag(fn, WireBytes)...)
	dst = AppendVarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// AppendVarintField appends a varint field (tag + varint value) to dst.
func AppendVarintField(dst []byte, fn int, v uint64) []byte {
	dst = append(dst, EncodeTag(fn, WireVarint)...)
	return AppendVarint(dst, v)
}
