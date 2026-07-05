package mcpsec

import (
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// Pin diffing (guard spec §9.2): observations vs stored pin records.
// The pins arrive as plain decoded values (the cmd layer loads
// guard_pins rows and decodes pin_hash through DecodePinHash); the
// results are policy.MCPFindings for engine evaluation plus PinUpdate
// values for the store's one-owner UpsertGuardPin. This package never
// touches the store (module rule 1).

// ClientObserved is the synthetic guard_pins.client value for servers
// seen only in proxy traffic — project-scoped registrations and
// clients outside the locate table. The proxy cannot know which
// client sent a request, and a server's tool set is a property of the
// server, not of the referencing config.
const ClientObserved = "observed"

// Pin is one decoded guard_pins row (kind=mcp_server).
type Pin struct {
	// Client / Name mirror the row identity.
	Client string
	Name   string
	// Hash is the decoded composite pin. A row whose pin_hash failed
	// DecodePinHash arrives as the zero PinHash — the diff re-pins it
	// silently (doc.go: degrade, don't false-fire).
	Hash PinHash
	// Status mirrors the row status: pinned | drifted | approved.
	Status string
}

// PinUpdate is one guard_pins upsert the diff wants persisted.
type PinUpdate struct {
	// Client / Name identify the row (kind is always mcp_server).
	Client string
	Name   string
	// Hash is the new composite pin value.
	Hash PinHash
	// Status is the new row status.
	Status string
	// First marks a first sighting: the cmd layer stamps first_seen
	// = now (UpsertGuardPin preserves first_seen on conflict, so the
	// flag only matters for messaging).
	First bool
}

// DiffConfigs compares a config-scan inventory against the stored
// pins (§9.2 cfg half):
//
//   - server with no pin row, client has NO pins at all → baseline
//     sighting: pin silently (doc.go "Baseline-quiet first scan");
//   - server with no pin row, client HAS a baseline → R-301 finding +
//     pin (status pinned — unapproved);
//   - pin exists, cfg half unchanged → touch (update keeps status;
//     last_verified advances at the store);
//   - pin exists, cfg half changed → R-305 finding + status drifted
//     (the new hash is recorded — the finding is the audit trail, and
//     re-recording keeps the rule one-shot per actual change instead
//     of once per scan).
//
// Servers present in pins but absent from the inventory are left
// untouched — history, surfaced by the list CLI as "absent".
func DiffConfigs(servers []Server, pins []Pin) ([]policy.MCPFinding, []PinUpdate) {
	type key struct{ client, name string }
	byKey := make(map[key]*Pin, len(pins))
	clientHasPins := map[string]bool{}
	for i := range pins {
		p := &pins[i]
		byKey[key{p.Client, p.Name}] = p
		clientHasPins[p.Client] = true
	}

	var findings []policy.MCPFinding
	var updates []PinUpdate
	for i := range servers {
		s := &servers[i]
		cfgHash := ConfigHash(*s)
		pin, ok := byKey[key{s.Client, s.Name}]
		switch {
		case !ok:
			updates = append(updates, PinUpdate{
				Client: s.Client, Name: s.Name,
				Hash:   PinHash{Cfg: cfgHash},
				Status: "pinned", First: true,
			})
			if clientHasPins[s.Client] {
				findings = append(findings, policy.MCPFinding{
					Kind: policy.MCPFindingNewServer, Server: s.Name, Client: s.Client,
					Detail: "appeared in " + s.Client + " config (" + s.Transport + " " + s.Command + ")",
				})
			}
		case pin.Hash.Cfg == "" || pin.Hash.Cfg == cfgHash:
			// Unchanged — or an undecodable legacy hash being silently
			// re-pinned. Touch the row so last_verified advances.
			updates = append(updates, PinUpdate{
				Client: s.Client, Name: s.Name,
				Hash:   PinHash{Cfg: cfgHash, Tools: pin.Hash.Tools},
				Status: pin.Status,
			})
		default:
			findings = append(findings, policy.MCPFinding{
				Kind: policy.MCPFindingBinaryChanged, Server: s.Name, Client: s.Client,
				Detail: "command/URL or launch shape changed (now " + s.Transport + " " + s.Command + ")",
			})
			updates = append(updates, PinUpdate{
				Client: s.Client, Name: s.Name,
				Hash:   PinHash{Cfg: cfgHash, Tools: pin.Hash.Tools},
				Status: "drifted",
			})
		}
	}
	return findings, updates
}

// DiffTools compares one server's observed tool declarations against
// the stored pins (§9.2 tools half + §9.3 poisoning):
//
//   - no pin row anywhere for the server → pin under ClientObserved
//     (+ R-301 when any baseline exists at all) and analyze;
//   - pin rows whose tools half is empty → ENRICHMENT: the first
//     observation completes the pin, silently; analyze;
//   - tools half set and unchanged → nothing;
//   - tools half changed → R-302 per pin row + status drifted;
//     analyze (the new tool set is what the model now sees).
//
// Poisoning analysis runs once per CHANGE (first observation or
// drift), never on every scan — re-flagging an unchanged description
// each session would be noise, and the pin hash is the change gate.
func DiffTools(server string, decls []ToolDecl, pins []Pin) ([]policy.MCPFinding, []PinUpdate) {
	toolsHash := ToolsHash(decls)
	if toolsHash == "" {
		return nil, nil
	}
	var matching []*Pin
	anyPins := len(pins) > 0
	for i := range pins {
		if pins[i].Name == server {
			matching = append(matching, &pins[i])
		}
	}

	var findings []policy.MCPFinding
	var updates []PinUpdate
	analyze := false
	if len(matching) == 0 {
		updates = append(updates, PinUpdate{
			Client: ClientObserved, Name: server,
			Hash:   PinHash{Tools: toolsHash},
			Status: "pinned", First: true,
		})
		if anyPins {
			findings = append(findings, policy.MCPFinding{
				Kind: policy.MCPFindingNewServer, Server: server, Client: ClientObserved,
				Detail: "observed in proxied traffic without a config pin",
			})
		}
		analyze = true
	}
	for _, pin := range matching {
		switch {
		case pin.Hash.Tools == toolsHash:
			// Unchanged — no row touch needed (tool sets re-send every
			// turn; last_verified advances on config scans instead).
		case pin.Hash.Tools == "":
			updates = append(updates, PinUpdate{
				Client: pin.Client, Name: pin.Name,
				Hash:   PinHash{Cfg: pin.Hash.Cfg, Tools: toolsHash},
				Status: pin.Status,
			})
			analyze = true
		default:
			findings = append(findings, policy.MCPFinding{
				Kind: policy.MCPFindingDescriptionDrift, Server: server, Client: pin.Client,
				Detail: "declared tool set/descriptions changed since the pin",
			})
			updates = append(updates, PinUpdate{
				Client: pin.Client, Name: pin.Name,
				Hash:   PinHash{Cfg: pin.Hash.Cfg, Tools: toolsHash},
				Status: "drifted",
			})
			analyze = true
		}
	}
	if analyze {
		for _, hit := range AnalyzeTools(decls) {
			findings = append(findings, policy.MCPFinding{
				Kind: policy.MCPFindingPoisoning, Server: hit.Server, Client: clientOf(matching),
				Detail: hit.Heuristic + " on tool " + hit.Tool + ": " + hit.Detail,
			})
		}
	}
	return findings, updates
}

// clientOf names the client for poisoning findings: the first pinned
// client, or ClientObserved when the server has no config pin.
func clientOf(matching []*Pin) string {
	if len(matching) > 0 {
		return matching[0].Client
	}
	return ClientObserved
}
