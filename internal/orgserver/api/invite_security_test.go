// invite_security_test.go — revert-proof anchors for the codex security
// findings on the Tier-3 delegated-invite loop (Arc 2 of
// docs/plans/tier3-local-contract-and-teams-invite-plan-2026-07-31.md).
//
// Each test names the property it pins and the mutation that fires it, so a
// future refactor that reintroduces the hole fails here rather than shipping.

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// TestInviteCapIsAtomicUnderConcurrency is the MUTATION-PROOF anchor for
// finding 1 (HIGH): the monthly cap used to be a COUNT on one connection
// followed by an INSERT on another, with ~60ms of argon2id in between. N
// concurrent mints all read "0 of 3 used" and all inserted, overshooting the
// cap by N-1.
//
// The property: however many requests arrive at once, EXACTLY capLimit tokens
// exist afterwards and every other request is refused with 429. Moving the
// count back out of store.mintInviteAtomically's BEGIN IMMEDIATE transaction
// fires this test.
func TestInviteCapIsAtomicUnderConcurrency(t *testing.T) {
	const (
		capLimit   = 3
		concurrent = 8
	)
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: capLimit})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	start := make(chan struct{})
	codes := make([]int, concurrent)
	bodies := make([]string, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`)
			<-start // release the herd together
			h.MintEnrolmentToken(rec, req)
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	created, refused := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			refused++
		default:
			t.Errorf("request %d: unexpected code %d body=%s", i, c, bodies[i])
		}
	}
	if created != capLimit {
		t.Errorf("201 responses = %d, want exactly %d — the cap was overshot by concurrent mints", created, capLimit)
	}
	if refused != concurrent-capLimit {
		t.Errorf("429 responses = %d, want %d", refused, concurrent-capLimit)
	}

	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens WHERE minted_by = 'member-1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != capLimit {
		t.Errorf("token rows = %d, want exactly %d — COUNT and INSERT must be one transaction", rows, capLimit)
	}
	if n := countAudit(t, h, "member-1"); n != capLimit {
		t.Errorf("invite_minted audit rows = %d, want %d (one per landed mint)", n, capLimit)
	}
}

// TestInviteTTLDaysBounded pins finding 3 (MEDIUM): ttl_days was unbounded,
// so {"ttl_days":100000} minted a 273-year enrolment token and a large enough
// value overflowed time.Duration into a PAST expiry. Both mint surfaces now
// accept only [1, 90], and an EXPLICIT 0 is refused rather than silently
// meaning "server default" (that is what omitting the key is for).
func TestInviteTTLDaysBounded(t *testing.T) {
	bad := []string{"0", "-1", "91", "100000", "2147483647", "-2147483648"}

	t.Run("saml", func(t *testing.T) {
		h, d := newTestHandlers(t)
		h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 50})
		seedMember(t, d, "member-1", "member@acme.example")
		seedMember(t, d, "user-1", "dev@acme.example")

		for _, v := range bad {
			rec := httptest.NewRecorder()
			h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1",
				fmt.Sprintf(`{"user_id":"user-1","ttl_days":%s}`, v)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("ttl_days=%s: code=%d body=%s, want 400", v, rec.Code, rec.Body.String())
			}
		}
		var rows int
		if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("out-of-range ttl_days minted %d tokens, want 0", rows)
		}

		// The bound itself is accepted, and it means 90 days.
		rec := httptest.NewRecorder()
		h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1",
			`{"user_id":"user-1","ttl_days":90}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("ttl_days=90: code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}
		var resp mintTokenResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		exp, err := time.Parse(time.RFC3339, resp.ExpiresAt)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Until(exp); d < 89*24*time.Hour || d > 91*24*time.Hour {
			t.Errorf("ttl_days=90 produced expiry in %v, want ~90 days", d)
		}

		// Omitting the key keeps the server default (7 days here), NOT 400.
		rec = httptest.NewRecorder()
		h.MintEnrolmentToken(rec, mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("omitted ttl_days: code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}
	})

	t.Run("agent", func(t *testing.T) {
		h, d := newTestHandlers(t)
		h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 50})
		seedMember(t, d, "member-1", "member@acme.example")
		seedMember(t, d, "user-1", "dev@acme.example")

		for _, v := range bad {
			rec := httptest.NewRecorder()
			h.MintInviteToken(rec, agentInviteRequestFor("member-1",
				fmt.Sprintf(`{"email":"dev@acme.example","ttl_days":%s}`, v)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("ttl_days=%s: code=%d body=%s, want 400", v, rec.Code, rec.Body.String())
			}
		}
		var rows int
		if err := d.QueryRow(`SELECT COUNT(*) FROM enrolment_tokens`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("out-of-range ttl_days minted %d tokens, want 0", rows)
		}

		rec := httptest.NewRecorder()
		h.MintInviteToken(rec, agentInviteRequestFor("member-1",
			`{"email":"dev@acme.example","ttl_days":90}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("ttl_days=90: code=%d body=%s, want 201", rec.Code, rec.Body.String())
		}
	})
}

// TestInviteTTLUnitBoundary pins the pure resolver directly, including the
// absent-vs-explicit distinction the *int exists for.
func TestInviteTTLUnitBoundary(t *testing.T) {
	def := 7 * 24 * time.Hour
	if got, err := inviteTTL(nil, def); err != nil || got != def {
		t.Errorf("absent ttl_days = (%v, %v), want (%v, nil)", got, err, def)
	}
	for _, days := range []int{minInviteTTLDays, 7, 30, maxInviteTTLDays} {
		d := days
		got, err := inviteTTL(&d, def)
		if err != nil {
			t.Errorf("ttl_days=%d: %v", days, err)
			continue
		}
		if want := time.Duration(days) * 24 * time.Hour; got != want {
			t.Errorf("ttl_days=%d = %v, want %v", days, got, want)
		}
	}
	for _, days := range []int{0, -1, maxInviteTTLDays + 1, 100000} {
		d := days
		if _, err := inviteTTL(&d, def); err == nil {
			t.Errorf("ttl_days=%d was accepted, want an error", days)
		}
	}
}

// TestInviteAttemptBudgetBoundsTheEmailOracle pins finding 4 (MEDIUM): the
// mint answers 404 for an unknown/inactive address and 201 for an active one,
// which distinguishes active org members — and a MISS consumed no allowance
// at all, so the probe was free and unbounded.
//
// The distinct statuses are kept (a member who typo'd deserves to be told);
// what is bounded is the RATE. inviteAttemptBudget failed resolutions per
// inviteAttemptWindow, after which EVERY invite from that inviter is 429 —
// hit or miss — so the oracle stops answering at all.
func TestInviteAttemptBudgetBoundsTheEmailOracle(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 1000})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	// The budget's worth of misses are answered honestly.
	for i := 0; i < inviteAttemptBudget; i++ {
		rec := httptest.NewRecorder()
		h.MintInviteToken(rec, agentInviteRequestFor("member-1",
			fmt.Sprintf(`{"email":"probe-%d@acme.example"}`, i)))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("miss %d: code=%d body=%s, want 404", i, rec.Code, rec.Body.String())
		}
	}

	// The next one is refused, and so is the one after it.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"over@acme.example"}`))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("miss past budget (%d): code=%d body=%s, want 429", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "invite_attempts_exceeded") {
			t.Errorf("over-budget body = %s, want the invite_attempts_exceeded code", rec.Body.String())
		}
	}

	// ...and so is a request for a REAL member: once the budget is spent the
	// endpoint stops discriminating, which is the whole point.
	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("hit past budget: code=%d body=%s, want 429 (a 201 here restores the oracle)", rec.Code, rec.Body.String())
	}

	// A DIFFERENT inviter has their own budget.
	seedMember(t, d, "member-2", "member2@acme.example")
	rec = httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-2", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("second inviter: code=%d body=%s, want 201 (the budget is per-actor)", rec.Code, rec.Body.String())
	}

	// The window rolls: age the recorded attempts past it and member-1 works
	// again.
	old := time.Now().UTC().Add(-inviteAttemptWindow - time.Minute).Format(time.RFC3339Nano)
	if _, err := d.Exec(`UPDATE invite_attempts SET created_at = ? WHERE actor_user_id = 'member-1'`, old); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("after the window rolled: code=%d body=%s, want 201", rec.Code, rec.Body.String())
	}
	// The stale rows were pruned in the same transaction, so the table stays
	// bounded without a sweeper.
	var stale int
	if err := d.QueryRow(`SELECT COUNT(*) FROM invite_attempts WHERE created_at < ?`, old).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("%d attempt rows older than the window survived the mint", stale)
	}
}

// TestInviteHitsDoNotConsumeAttemptBudget is the other half of finding 4: the
// budget must charge FAILED resolutions only, or a legitimate inviter would
// be throttled long before their monthly cap.
func TestInviteHitsDoNotConsumeAttemptBudget(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: inviteAttemptBudget + 5})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	for i := 0; i < inviteAttemptBudget+3; i++ {
		rec := httptest.NewRecorder()
		h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("successful mint %d: code=%d body=%s, want 201", i, rec.Code, rec.Body.String())
		}
	}
	var attempts int
	if err := d.QueryRow(`SELECT COUNT(*) FROM invite_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("successful mints recorded %d attempt rows, want 0", attempts)
	}
}

// TestInviteAuditSourceIPIgnoresForwardedFor pins finding 8 (LOW): the audit
// row's source_ip came from X-Forwarded-For, a header any client can set, and
// this server has no trusted-proxy allow-list to tell a real hop from a
// forged one — so an inviter could point an investigation at an arbitrary
// address. The audited value is the transport peer.
func TestInviteAuditSourceIPIgnoresForwardedFor(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 10})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")

	req := mintRequest(auth.InviteRoleMember, "member-1", `{"user_id":"user-1"}`)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Forwarded-For", "9.9.9.9, 8.8.8.8")
	req.Header.Set("X-Real-IP", "9.9.9.9")

	rec := httptest.NewRecorder()
	h.MintEnrolmentToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: code=%d body=%s", rec.Code, rec.Body.String())
	}

	var sourceIP string
	if err := d.QueryRow(`SELECT COALESCE(source_ip,'') FROM audit_log WHERE action = ?`,
		rollup.ActionInviteMinted).Scan(&sourceIP); err != nil {
		t.Fatal(err)
	}
	if sourceIP != "203.0.113.7" {
		t.Errorf("audited source_ip = %q, want the transport peer 203.0.113.7 — a spoofable header must not reach the audit log", sourceIP)
	}
}

// TestInviteRefusesInactiveTargetAndChargesTheBudget pins that the
// SCIM-deactivated TARGET path is a miss for budget purposes too: it is the
// same oracle (it distinguishes "deactivated member" from "active member").
func TestInviteRefusesInactiveTargetAndChargesTheBudget(t *testing.T) {
	h, d := newTestHandlers(t)
	h.SetInviteOptions(InviteOptions{MemberInvites: true, MonthlyCap: 10})
	seedMember(t, d, "member-1", "member@acme.example")
	seedMember(t, d, "user-1", "dev@acme.example")
	if _, err := d.Exec(`UPDATE org_members SET active = 0 WHERE user_id = 'user-1'`); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.MintInviteToken(rec, agentInviteRequestFor("member-1", `{"email":"dev@acme.example"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inactive target: code=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
	var attempts int
	if err := d.QueryRow(`SELECT COUNT(*) FROM invite_attempts WHERE actor_user_id = 'member-1'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempt rows = %d, want 1 — an inactive target is a miss and must be charged", attempts)
	}
}
