package protowire

import (
	"errors"
	"testing"
)

func TestWalkSimple(t *testing.T) {
	// Synthesize: field 1 (varint) = 42, field 2 (bytes) = "hello".
	var buf []byte
	buf = AppendVarintField(buf, 1, 42)
	buf = AppendBytesField(buf, 2, []byte("hello"))

	var fields []Field
	err := Walk(buf, func(f Field) error {
		// Capture by copy — slice references aliasing buf is valid until
		// Walk returns, but we want to inspect after.
		fields = append(fields, Field{
			Path:        append([]int(nil), f.Path...),
			Depth:       f.Depth,
			FieldNumber: f.FieldNumber,
			WireType:    f.WireType,
			Bytes:       append([]byte(nil), f.Bytes...),
			Varint:      f.Varint,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].FieldNumber != 1 || fields[0].WireType != WireVarint || fields[0].Varint != 42 {
		t.Errorf("field[0] mismatch: %+v", fields[0])
	}
	if fields[1].FieldNumber != 2 || fields[1].WireType != WireBytes || string(fields[1].Bytes) != "hello" {
		t.Errorf("field[1] mismatch: %+v", fields[1])
	}
}

func TestWalkRecursesIntoSubmessage(t *testing.T) {
	// Outer field 1 contains inner field 5 (varint=99) + field 6 (string).
	var inner []byte
	inner = AppendVarintField(inner, 5, 99)
	inner = AppendBytesField(inner, 6, []byte("nested"))
	var outer []byte
	outer = AppendBytesField(outer, 1, inner)

	var paths [][]int
	err := Walk(outer, func(f Field) error {
		paths = append(paths, append([]int(nil), f.Path...))
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := [][]int{
		{1},
		{1, 5},
		{1, 6},
	}
	if len(paths) != len(want) {
		t.Fatalf("got paths %v, want %v", paths, want)
	}
	for i := range want {
		if !equalInts(paths[i], want[i]) {
			t.Errorf("path[%d]: got %v want %v", i, paths[i], want[i])
		}
	}
}

func TestWalkRejectsInvalidWireType(t *testing.T) {
	// Tag with wire type 3 (deprecated start group).
	buf := []byte{0x0b} // (1<<3) | 3
	err := Walk(buf, func(Field) error { return nil })
	if !errors.Is(err, ErrInvalidWireType) {
		t.Fatalf("got err %v, want ErrInvalidWireType", err)
	}
}

func TestWalkTruncated(t *testing.T) {
	// Tag for bytes field 1 + length 100, but buffer truncated.
	buf := []byte{0x0a, 0x64, 0x68}
	err := Walk(buf, func(Field) error { return nil })
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("got err %v, want ErrTruncated", err)
	}
}

func TestWalkYieldErrorPropagates(t *testing.T) {
	var buf []byte
	buf = AppendVarintField(buf, 1, 1)
	sentinel := errors.New("stop")
	err := Walk(buf, func(Field) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("yield error did not propagate: %v", err)
	}
}

func TestValidatesAsProto(t *testing.T) {
	// Build a 5-record message.
	var buf []byte
	for i := 1; i <= 5; i++ {
		buf = AppendVarintField(buf, i, uint64(i*100))
	}
	if got := ValidatesAsProto(buf, 10); got != 5 {
		t.Fatalf("ValidatesAsProto: got %d records, want 5", got)
	}

	// Random bytes should validate at most a few records before failing.
	random := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if got := ValidatesAsProto(random, 10); got > 1 {
		t.Errorf("random bytes parsed %d records, expected ≤1", got)
	}
}

func TestFindProtobufStart(t *testing.T) {
	// Pad with 3 zero bytes, then a valid varint tag (field 1, wire 0).
	buf := []byte{0x00, 0x00, 0x00, 0x08, 0x42}
	skip, ok := FindProtobufStart(buf, 4)
	if !ok || skip != 3 {
		t.Fatalf("FindProtobufStart: skip=%d ok=%v, want skip=3 ok=true", skip, ok)
	}

	// All-zero input should fail.
	zeroes := make([]byte, 8)
	if _, ok := FindProtobufStart(zeroes, 4); ok {
		t.Fatal("FindProtobufStart accepted all-zero input")
	}
}

func TestEncodeRoundtrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16384, 1 << 32, 1<<63 - 1} {
		enc := AppendVarint(nil, v)
		got, n, err := decodeVarint(enc)
		if err != nil || got != v || n != len(enc) {
			t.Errorf("varint(%d): enc=%x got=%d n=%d err=%v", v, enc, got, n, err)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
