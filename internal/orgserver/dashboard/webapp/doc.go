// Package webapp embeds the org dashboard React/Vite SPA produced by the web2/
// directory at the repo root. It mirrors the agent dashboard's
// internal/intelligence/dashboard/webapp: the dist/ subtree is the committed
// output of `make web-build-org` (npm ci + vite build + copy from web2/dist)
// and is regenerated from source, not authored by hand.
//
// Handler serves the SPA shell at root with index.html fallback for client-side
// routes; the org server mounts it behind the SAML session so an unauthenticated
// browser is redirected to SSO before the app loads.
package webapp
