// Package orgcontract is the single source of truth for the wire types
// exchanged between an enrolled agent (internal/orgclient) and the org
// server (internal/orgserver) in the Teams & Org Visibility feature.
//
// It contains plain Go structs with json:"snake_case" tags and no logic or
// I/O. Both the agent and the server depend on this package, so a
// schema change is a single edit that recompiles both sides — drift
// between the binaries is a compile error rather than a runtime
// corruption. The OpenAPI spec (docs/openapi/orgserver.yaml) references
// these types via x-go-type so the generated client/server stubs reuse
// them rather than defining a parallel set.
//
// Privacy posture (spec §1.5) is encoded structurally: the pushed row
// types carry only what cost and activity rollups need — action types,
// counts, token usage, model names, timestamps, project roots, file
// paths, and hashes. They deliberately omit prompt text, tool-output
// bodies (actions.raw_tool_output), raw tool input, preceding reasoning,
// and free-form error messages. The server cannot show what the contract
// cannot carry.
//
// Wire stability is pinned by golden_test.go; a change to any json
// encoding fails the test until the golden is intentionally regenerated
// with `go test ./internal/orgcontract -update`.
package orgcontract
