package orgcontract

import (
	"bytes"
	"testing"
)

func TestPushSigningMessage_Deterministic(t *testing.T) {
	body := []byte("gzip-bytes-here")
	a := PushSigningMessage(1748000000, body)
	b := PushSigningMessage(1748000000, body)
	if !bytes.Equal(a, b) {
		t.Fatal("same inputs must produce the same message")
	}
}

func TestPushSigningMessage_BindsTimestampAndBody(t *testing.T) {
	body := []byte("payload")
	base := PushSigningMessage(1748000000, body)

	if bytes.Equal(base, PushSigningMessage(1748000001, body)) {
		t.Fatal("a different timestamp must change the message")
	}
	if bytes.Equal(base, PushSigningMessage(1748000000, []byte("payload2"))) {
		t.Fatal("a different body must change the message")
	}
	// Shape: "<ts>\n<64 hex chars>".
	nl := bytes.IndexByte(base, '\n')
	if nl < 0 {
		t.Fatalf("message missing newline separator: %q", base)
	}
	if got := len(base) - nl - 1; got != 64 {
		t.Fatalf("hash segment = %d chars, want 64 (sha256 hex)", got)
	}
}
