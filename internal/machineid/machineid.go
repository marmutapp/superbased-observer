package machineid

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// readFile, hostname, and runTool are the injectable I/O seams (CLAUDE.md #1).
// Tests override them; the defaults perform the real OS reads. runTool wraps a
// platform helper (ioreg on darwin, reg.exe on windows) and returns its stdout.
var (
	readFile = os.ReadFile
	hostname = os.Hostname
	runTool  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
	// goos is a seam so a test can exercise every platform branch of
	// rawIdentity regardless of the host it runs on.
	goos = runtime.GOOS
)

// ForOrg returns the org-salted, one-way machine fingerprint for orgID, or the
// empty string when no stable OS source is available on this host. The empty
// string is a first-class value: the caller treats an unbindable node as a
// visible "unbound" managed node rather than an error, so ForOrg returns a nil
// error in that case — err is non-nil only for an unexpected I/O failure the
// caller may want to log.
//
// orgID must be non-empty; salting with it guarantees the same machine yields
// unrelated identities across orgs and that the raw OS id never leaves the host.
func ForOrg(orgID string) (string, error) {
	raw, err := rawIdentity()
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || orgID == "" {
		return "", nil
	}
	return hashIdentity(orgID, raw), nil
}

// hashIdentity is the pure salt-and-hash core: SHA-256 over a domain-separated
// (org, raw) pair, hex-encoded. Domain separation (the "\x00" delimiter between
// the salt and the raw id) prevents a different (org, raw) split from colliding.
func hashIdentity(orgID, raw string) string {
	h := sha256.New()
	h.Write([]byte("sbo-machineid\x00"))
	h.Write([]byte(orgID))
	h.Write([]byte{0})
	h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

// rawIdentity selects the most stable machine source available on this host,
// walking a per-OS ordered list and falling back to the hostname. It returns
// ("", nil) when nothing usable is found — never an error for a merely-absent
// source, so a bare container degrades to "unbindable" rather than failing.
func rawIdentity() (string, error) {
	switch goos {
	case "linux":
		// /etc/machine-id is the systemd/D-Bus stable machine id; the
		// /var/lib/dbus path is the same value on non-systemd installs. Under
		// WSL2 this is the DISTRO's id (documented in the package doc).
		if id := firstFileValue("/etc/machine-id", "/var/lib/dbus/machine-id"); id != "" {
			return id, nil
		}
	case "darwin":
		if id := darwinPlatformUUID(); id != "" {
			return id, nil
		}
	case "windows":
		if id := windowsMachineGUID(); id != "" {
			return id, nil
		}
	}
	// Hostname fallback for every OS. Weakest source (hostnames collide and
	// change), but better than an empty identity on a host without a stable id.
	if hn, err := hostname(); err == nil {
		return strings.TrimSpace(hn), nil
	}
	return "", nil
}

// firstFileValue returns the trimmed contents of the first readable, non-empty
// path, or "" if none read.
func firstFileValue(paths ...string) string {
	for _, p := range paths {
		b, err := readFile(p)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	return ""
}

// darwinPlatformUUID extracts IOPlatformUUID from `ioreg`. Returns "" on any
// failure so the caller falls back to the hostname.
func darwinPlatformUUID() string {
	out, err := runTool("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	return extractQuotedAfter(string(out), "IOPlatformUUID")
}

// windowsMachineGUID reads HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid via
// reg.exe. Returns "" on any failure so the caller falls back to the hostname.
func windowsMachineGUID() string {
	out, err := runTool("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid")
	if err != nil {
		return ""
	}
	// reg output: a line like "    MachineGuid    REG_SZ    <guid>".
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "MachineGuid") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			return fields[len(fields)-1]
		}
	}
	return ""
}

// extractQuotedAfter finds `key` in text and returns the first double-quoted
// token appearing after it on the same logical run (ioreg renders
// `"IOPlatformUUID" = "<uuid>"`).
func extractQuotedAfter(text, key string) string {
	i := strings.Index(text, key)
	if i < 0 {
		return ""
	}
	rest := text[i+len(key):]
	// Skip to the value's opening quote after the `=`.
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return ""
	}
	rest = rest[eq+1:]
	start := strings.Index(rest, "\"")
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
