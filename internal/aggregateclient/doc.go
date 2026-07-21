// Package aggregateclient is the SOLE egress seam of the opt-in aggregate rail
// (docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md §6.3). It is the
// ONLY place in the tree that would ever transmit an aggregate.Submission over
// the network — the product's first first-party network call outside the Teams
// org-push.
//
// Two guarantees are structural, not merely conventional:
//
//   - It is impossible to Submit without consent. Submit REQUIRES a Gate
//     value, and a Gate can only be minted by Authorize, which fails unless the
//     rail is enabled AND a valid, matching consent receipt exists
//     (aggregate.CheckConsent == ConsentValid). There is no other constructor.
//
//   - The transport is hardened (finding #22): HTTPS-only, redirects refused,
//     a short timeout, a bounded response read, no cookie jar, and no
//     identifying headers beyond the unavoidable Go transport signature. The
//     destination host is validated before the first byte leaves.
//
// The package deliberately does NOT import internal/orgclient or reference
// internal/store/orgpush.go symbols — the aggregate rail and org-push must not
// entangle (design §6). That separation is pinned by
// tests/invariant/aggregate_test.go::TestAggregateEgressSeamSeparate.
package aggregateclient
