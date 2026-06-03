// Package dashboard is the org server's dashboard: the /api/org/* data
// endpoints (api.go — the generated dashboard ServerInterface, role-scoped per
// request) plus the embedded web2/ React SPA (webapp/), served at root behind
// the SAML session.
//
// It replaced M1's placeholder SSO-proof HTML page in M3. Reads go through
// internal/orgserver/rollup (cached); writes (budgets, revoke, team-role) and
// the audit-log writer live in store.go / api.go. Nothing here surfaces
// content — only counts, costs, labels, and timestamps.
package dashboard
