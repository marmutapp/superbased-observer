// Package protowire walks Protocol Buffer wire-format bytes without
// requiring a .proto schema. The Antigravity adapter consumes this to
// classify decrypted .pb conversations whose schema is undocumented
// and may drift between Antigravity releases — relying on field
// numbers would cripple us; relying on wire-format shape and content
// heuristics is robust to schema change.
//
// The four wire types (per https://protobuf.dev/programming-guides/encoding/):
//
//	0  Varint            value via continuation-bit-encoded varint
//	1  64-bit fixed      8 bytes
//	2  Length-delimited  varint length, then that many bytes
//	5  32-bit fixed      4 bytes
//
// Types 3/4 (deprecated start/end groups) are not used by Antigravity
// and are rejected by the validator.
//
// Walk is recursive: any length-delimited field whose first byte is a
// valid wire-format tag is treated as a nested message and walked,
// yielding fields with a path trail.
//
// Classify helpers (IsLikelyText, IsLikelyUnixTimestamp,
// IsLikelyToolName, ValidatesAsProto) gate emission decisions in the
// adapter without committing to specific field-number knowledge.
package protowire
