// Package orgserver assembles the org server: it wires the config, the
// server DB, the auth subsystem (Ed25519 bearer, SAML SP, HMAC session,
// SCIM token), the SCIM provisioning endpoints, the agent-protocol API, and
// the placeholder dashboard into a single http.Server.
//
// Routing uses the stdlib net/http ServeMux (Go 1.22 method+path patterns);
// there is no third-party router. Auth is enforced per route prefix:
//
//	/api/agent/enroll          — public (the one-time token is the credential)
//	/api/agent/push            — Ed25519 bearer (scope-aware, via the generated wrapper)
//	/api/org/enrolment-tokens  — SAML session (admin)
//	/scim/v2/*                 — static SCIM token
//	/saml/*                    — SAML SP endpoints (public by nature)
//	/                          — SAML session (browser; redirects to SSO)
//
// Cross-cutting request-id, logging, and rate-limit middleware wrap the whole
// mux. New() does all fallible setup (open DB, load keys, fetch IdP metadata)
// so Run() is just serve-until-signal. Handler() exposes the assembled
// handler for in-process E2E tests.
//
// TLS is terminated upstream (ingress/sidecar) in the reference deployment;
// the server listens plain HTTP on its configured port. The SAML SP cert/key
// are for assertion signing, not transport.
package orgserver
