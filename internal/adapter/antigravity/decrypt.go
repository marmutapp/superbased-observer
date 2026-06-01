package antigravity

import (
	"github.com/marmutapp/superbased-observer/internal/platform/oscrypt"
	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
)

// decryptOne tries every supported cipher mode against raw with the
// given secret and returns the first plaintext that validates as a
// well-formed protobuf message.
//
// "Well-formed" here is stronger than ValidatesAsProto's record
// count: random CTR garbage happens to validate ~5 records on the
// order of 1 in 100 runs (any single byte has a >50% chance of
// looking like a valid wire-format tag). The validator below adds:
//
//  1. The walk must consume the ENTIRE buffer (no trailing junk).
//     Real protobuf messages serialize cleanly to EOF; random bytes
//     bail mid-walk after a few lucky records.
//  2. Length-delimited submessages whose content claims a length
//     bigger than the remaining buffer reject the candidate. Already
//     enforced by ValidatesAsProto, included here for completeness.
//
// This catches the wrong-key case observed in pre-flight (PBKDF2-32
// + skip8 produced 290 bytes of "valid" plaintext that walked 5
// records and then collapsed; real plaintext walks to EOF).
func decryptOne(raw []byte, secret oscrypt.Secret) ([]byte, string, error) {
	validator := func(pt []byte) bool {
		if validateAsCompletePB(pt) {
			return true
		}
		if start, ok := protowire.FindProtobufStart(pt, 16); ok {
			if start < len(pt) && validateAsCompletePB(pt[start:]) {
				return true
			}
		}
		return false
	}
	res, err := oscrypt.DecryptAuto(raw, secret, validator)
	if err != nil {
		return nil, "", err
	}
	pt := res.Plaintext
	if start, ok := protowire.FindProtobufStart(pt, 16); ok && start > 0 {
		pt = pt[start:]
	}
	return pt, res.Strategy, nil
}

// validateAsCompletePB walks buf as protobuf wire format and
// reports whether the walk consumed every byte without error. This
// is a much stronger signal than "first N records are valid"
// because random byte streams almost never serialize cleanly to
// EOF — they bail on truncation or invalid wire types within tens
// of bytes.
func validateAsCompletePB(buf []byte) bool {
	if len(buf) < 8 {
		return false
	}
	pos := 0
	records := 0
	for pos < len(buf) {
		tag, n, err := readVarint(buf[pos:])
		if err != nil {
			return false
		}
		pos += n
		fn := int(tag >> 3)
		wt := tag & 0x07
		if fn <= 0 || (wt != 0 && wt != 1 && wt != 2 && wt != 5) {
			return false
		}
		switch wt {
		case 0:
			_, n, err := readVarint(buf[pos:])
			if err != nil {
				return false
			}
			pos += n
		case 1:
			if pos+8 > len(buf) {
				return false
			}
			pos += 8
		case 5:
			if pos+4 > len(buf) {
				return false
			}
			pos += 4
		case 2:
			length, n, err := readVarint(buf[pos:])
			if err != nil {
				return false
			}
			pos += n
			if uint64(pos)+length > uint64(len(buf)) {
				return false
			}
			pos += int(length)
		}
		records++
	}
	return pos == len(buf) && records >= 1
}

func readVarint(b []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		bb := b[i]
		if shift >= 64 {
			return 0, 0, errSentinel
		}
		result |= uint64(bb&0x7f) << shift
		if bb&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errSentinel
}

var errSentinel = sentinelError("varint")

type sentinelError string

func (s sentinelError) Error() string { return string(s) }
