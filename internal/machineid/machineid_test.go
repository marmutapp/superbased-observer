package machineid

import (
	"errors"
	"os"
	"testing"
)

// withSeams swaps the injectable I/O seams for the duration of fn and restores
// them after, so a test can drive any platform branch deterministically.
func withSeams(t *testing.T, os_ string, rf func(string) ([]byte, error), hn func() (string, error), rt func(string, ...string) ([]byte, error), fn func()) {
	t.Helper()
	origGOOS, origRF, origHN, origRT := goos, readFile, hostname, runTool
	goos, readFile, hostname, runTool = os_, rf, hn, rt
	defer func() { goos, readFile, hostname, runTool = origGOOS, origRF, origHN, origRT }()
	fn()
}

func fixedFile(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
}

func TestForOrgStableAndOrgSalted(t *testing.T) {
	files := fixedFile(map[string]string{"/etc/machine-id": "abc123\n"})
	hn := func() (string, error) { return "host", nil }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("unused") }

	var a1, a2, b string
	withSeams(t, "linux", files, hn, rt, func() {
		var err error
		if a1, err = ForOrg("org-A"); err != nil {
			t.Fatalf("ForOrg org-A: %v", err)
		}
		if a2, err = ForOrg("org-A"); err != nil {
			t.Fatalf("ForOrg org-A again: %v", err)
		}
		if b, err = ForOrg("org-B"); err != nil {
			t.Fatalf("ForOrg org-B: %v", err)
		}
	})

	if a1 == "" {
		t.Fatal("expected a non-empty identity")
	}
	if a1 != a2 {
		t.Errorf("identity not stable across calls: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Error("identity must differ across orgs for the same machine")
	}
	if len(a1) != 64 {
		t.Errorf("expected a 64-char hex SHA-256, got %d chars", len(a1))
	}
}

func TestForOrgDistinctPerRawSource(t *testing.T) {
	hn := func() (string, error) { return "host", nil }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("unused") }

	var m1, m2 string
	withSeams(t, "linux", fixedFile(map[string]string{"/etc/machine-id": "machine-one"}), hn, rt, func() {
		m1, _ = ForOrg("org")
	})
	withSeams(t, "linux", fixedFile(map[string]string{"/etc/machine-id": "machine-two"}), hn, rt, func() {
		m2, _ = ForOrg("org")
	})
	if m1 == "" || m2 == "" {
		t.Fatal("expected non-empty identities")
	}
	if m1 == m2 {
		t.Error("different machines must produce different identities")
	}
}

func TestForOrgLinuxFileOrderPrefersMachineID(t *testing.T) {
	hn := func() (string, error) { return "host", nil }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("unused") }
	files := fixedFile(map[string]string{
		"/etc/machine-id":          "primary",
		"/var/lib/dbus/machine-id": "secondary",
	})
	var pref, dbusOnly string
	withSeams(t, "linux", files, hn, rt, func() { pref, _ = ForOrg("org") })
	withSeams(t, "linux", fixedFile(map[string]string{"/var/lib/dbus/machine-id": "secondary"}), hn, rt, func() {
		dbusOnly, _ = ForOrg("org")
	})
	// The identity built from /etc/machine-id="primary" must differ from the
	// one built from the dbus fallback="secondary", proving /etc wins when both
	// are present.
	if pref == dbusOnly {
		t.Error("expected /etc/machine-id to be preferred over the dbus fallback")
	}
}

func TestForOrgHostnameFallback(t *testing.T) {
	// No files readable; hostname is the only source.
	noFiles := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	hn := func() (string, error) { return "the-host\n", nil }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("no tool") }

	var got string
	withSeams(t, "linux", noFiles, hn, rt, func() { got, _ = ForOrg("org") })
	if got == "" {
		t.Fatal("expected the hostname fallback to yield an identity")
	}
	// Confirm it is actually the hostname source (matches a direct hash).
	want := hashIdentity("org", "the-host")
	if got != want {
		t.Errorf("hostname fallback = %q, want %q", got, want)
	}
}

func TestForOrgEmptyWhenNoSource(t *testing.T) {
	noFiles := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	noHost := func() (string, error) { return "", errors.New("no hostname") }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("no tool") }

	var got string
	var err error
	withSeams(t, "linux", noFiles, noHost, rt, func() { got, err = ForOrg("org") })
	if err != nil {
		t.Errorf("a merely-absent source must not be an error, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty identity when no source is available, got %q", got)
	}
}

func TestForOrgEmptyOrgIsUnbindable(t *testing.T) {
	files := fixedFile(map[string]string{"/etc/machine-id": "abc"})
	hn := func() (string, error) { return "host", nil }
	rt := func(string, ...string) ([]byte, error) { return nil, errors.New("unused") }
	var got string
	withSeams(t, "linux", files, hn, rt, func() { got, _ = ForOrg("") })
	if got != "" {
		t.Errorf("empty orgID must yield an empty (unbindable) identity, got %q", got)
	}
}

func TestDarwinPlatformUUIDParsing(t *testing.T) {
	noFiles := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	hn := func() (string, error) { return "mac-host", nil }
	ioreg := `+-o IOPlatformExpertDevice  <class IOPlatformExpertDevice>
    "IOPlatformUUID" = "11112222-3333-4444-5555-666677778888"
    "IOPlatformSerialNumber" = "C02XYZ"`
	rt := func(name string, _ ...string) ([]byte, error) {
		if name != "ioreg" {
			return nil, errors.New("unexpected tool")
		}
		return []byte(ioreg), nil
	}
	var got string
	withSeams(t, "darwin", noFiles, hn, rt, func() { got, _ = ForOrg("org") })
	want := hashIdentity("org", "11112222-3333-4444-5555-666677778888")
	if got != want {
		t.Errorf("darwin UUID parse = %q, want hash of the IOPlatformUUID %q", got, want)
	}
}

func TestWindowsMachineGUIDParsing(t *testing.T) {
	noFiles := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	hn := func() (string, error) { return "win-host", nil }
	regOut := "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Cryptography\r\n    MachineGuid    REG_SZ    aaaabbbb-cccc-dddd-eeee-ffff00001111\r\n"
	rt := func(name string, _ ...string) ([]byte, error) {
		if name != "reg" {
			return nil, errors.New("unexpected tool")
		}
		return []byte(regOut), nil
	}
	var got string
	withSeams(t, "windows", noFiles, hn, rt, func() { got, _ = ForOrg("org") })
	want := hashIdentity("org", "aaaabbbb-cccc-dddd-eeee-ffff00001111")
	if got != want {
		t.Errorf("windows GUID parse = %q, want hash of the MachineGuid %q", got, want)
	}
}

func TestHashIdentityDomainSeparation(t *testing.T) {
	// (org="a", raw="bc") must not collide with (org="ab", raw="c").
	if hashIdentity("a", "bc") == hashIdentity("ab", "c") {
		t.Error("hashIdentity must domain-separate the org salt from the raw id")
	}
}
