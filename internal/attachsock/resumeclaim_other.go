//go:build !unix

package attachsock

// ResumeClaim is a no-op on platforms without flock. Session-attach is
// Linux/WSL-only in v1 (design §6 decision 3), so the resume-claim guard is
// never exercised here; the type + Release exist only so the shared cmd-side
// call sites (the bare launcher's resume branch and the daemon's attach-resume
// spawn) compile on every target.
type ResumeClaim struct{}

// Release does nothing on platforms without flock.
func (c *ResumeClaim) Release() {}

// AcquireResumeClaim is a no-op on platforms without flock: it always succeeds
// with a no-op claim and never reports a conflict. Documented deviation: on a
// non-unix build there is no cross-process resume guard (attach itself is not
// served there), so always-succeed is the honest, compilable default.
func AcquireResumeClaim(string, string) (*ResumeClaim, bool, error) {
	return &ResumeClaim{}, true, nil
}
