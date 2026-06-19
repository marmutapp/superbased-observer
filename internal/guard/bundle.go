package guard

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Org policy-bundle loading (guard spec §14.2, G13). The org layer is
// the same §4.4 TOML policy-file format as the user/project layers,
// but it arrives over the wire Ed25519-signed and is cached locally as
// the verified envelope ([guard.rules].org_bundle, default
// ~/.observer/org-policy-bundle.json). The cache is written ONLY by
// the org client (internal/orgclient.FetchPolicyBundle) after the full
// §14.2 acceptance gate: signature valid AND public key matching the
// hash pinned at enrolment AND version monotonic.
//
// Loading here re-verifies what it cheaply can:
//
//   - The envelope signature is ALWAYS re-checked against the key
//     embedded in the envelope (orgcontract.VerifyPolicyBundle). This
//     is self-contained — no DB, no network — so hook processes pay
//     only a ~64-byte Ed25519 verify on top of the file read, well
//     inside the §6.4 budget. It catches corruption and casual
//     tampering with the cached TOML.
//   - The key PIN is compared only when the composition supplied
//     Options.OrgKeyPinHash (the daemon path; guardwire reads the pin
//     from guard_policy_state). A self-consistent forged cache (new
//     key + matching signature) is caught there, and structurally at
//     the next poll, which re-fetches through the always-pinned wire
//     seam and rewrites the cache. An attacker who can rewrite files
//     under ~/.observer already owns the unsigned user policy and
//     config.toml — the signature's job is the WIRE and the server
//     impersonation case, not the local-root case (the §10.4
//     tamper-EVIDENT honesty framing).
//
// Every failure degrades to local-only policy with a LoadIssue — the
// daemon must never refuse to start over a bad bundle (the org floor
// is a hardening layer, not an availability dependency), and a
// rejected fetch never overwrites a previously good cache.

// loadOrgBundle reads + verifies + parses the cached org bundle and
// installs it as g.orgLayer. Called from New before the base engine
// builds; every failure path records an issue and leaves the layer
// nil. pinHash is Options.OrgKeyPinHash ("" skips the pin check).
func (g *Guard) loadOrgBundle(path, pinHash string) {
	raw, err := g.readFile(path)
	switch {
	case os.IsNotExist(err):
		return // not enrolled / no bundle published — the common case
	case err != nil:
		g.issues = append(g.issues, fmt.Sprintf("org bundle %s: %v", path, err))
		return
	}
	var b orgcontract.PolicyBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		g.issues = append(g.issues, fmt.Sprintf("org bundle %s: not a bundle envelope: %v — running without the org layer", path, err))
		return
	}
	pub, err := orgcontract.VerifyPolicyBundle(b)
	if err != nil {
		g.issues = append(g.issues, fmt.Sprintf("org bundle %s rejected: %v — running without the org layer", path, err))
		return
	}
	if pinHash != "" && orgcontract.PublicKeyPinHash(pub) != pinHash {
		g.issues = append(g.issues, fmt.Sprintf("org bundle %s rejected: signing key does not match the enrolment pin — running without the org layer", path))
		return
	}
	pf, perr := parsePolicyFile([]byte(b.BundleTOML), layerOrg)
	if perr != nil {
		g.issues = append(g.issues, fmt.Sprintf("org bundle %s (version %d): %v — running without the org layer", path, b.Version, perr))
		return
	}
	g.orgLayer = pf
	st := PolicyState{
		Layer:       layerOrg,
		Path:        path,
		Version:     strconv.FormatInt(b.Version, 10),
		ContentHash: sha256hex([]byte(b.BundleTOML)),
	}
	g.states = append(g.states, st)
	for i := range pf.rules {
		g.ruleCategories[pf.rules[i].ID] = pf.rules[i].Category
	}
	if g.onPolicyState != nil {
		g.onPolicyState(st)
	}
}
