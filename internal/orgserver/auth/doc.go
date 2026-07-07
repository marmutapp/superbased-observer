// Package auth is the org server's authentication subsystem. It provides
// three independent mechanisms, each guarding a disjoint route prefix:
//
//   - Ed25519 bearer tokens (bearer.go) for the agent protocol
//     (/api/agent/*). A bearer is a JWT-shaped JSON envelope signed with the
//     server's Ed25519 key directly — no JWT library, no algorithm
//     negotiation, one key type, decoded by hand.
//   - SAML 2.0 dashboard login (saml.go) via crewjam/saml, with the
//     authenticated user carried in an HMAC-signed cookie (session.go).
//   - A static SCIM token (checked in middleware.go) for the IdP's SCIM
//     provisioning client (/scim/v2/*).
//
// The three never overlap: middleware.go exposes RequireBearer,
// RequireSAMLSession, and RequireSCIMToken, applied per prefix by the
// orgserver wiring.
//
// No secret material is embedded in code: the Ed25519 signing key, the HMAC
// session key, and the SCIM token are all read from configured file paths at
// boot.
package auth
