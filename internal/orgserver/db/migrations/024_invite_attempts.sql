-- Invite attempt budget — the oracle rate-limiter for the delegated-invite
-- loop (Arc 2 of
-- docs/plans/tier3-local-contract-and-teams-invite-plan-2026-07-31.md,
-- codex security finding #4).
--
-- SERVER-ONLY, with NO paired agent migration — the same shape as 008 / 009 /
-- 011 / 023. Nothing here changes the agent<->server wire: it records only
-- that some inviter asked for a target that did not resolve.
--
-- WHY: a delegated mint answers 404 for an unknown/inactive email and 201 for
-- an active one, so the endpoint distinguishes "this address is an active
-- member of the org" from "it is not". The distinct statuses are worth
-- keeping (a member who typo'd an address deserves to be told), so the
-- ORACLE RATE is bounded instead of the oracle's fidelity: only FAILED target
-- resolutions land a row here, and an inviter who exhausts the hourly budget
-- gets 429 for every subsequent invite — hit or miss — until the window
-- rolls. A legitimate inviter never sees it (the budget is an order of
-- magnitude above a human typo rate and well above the monthly mint cap).
--
-- actor_user_id is deliberately NOT a foreign key, for the same reason
-- enrolment_tokens.minted_by isn't: an ON DELETE CASCADE would let a
-- deprovision (or an attacker who can get one) silently reset the budget.
-- Rows are pruned opportunistically by the mint path (everything older than
-- the window is deleted inside the same transaction), so the table stays
-- bounded without a sweeper.
--
-- Carries no content: a user id and a timestamp. Never the attempted address
-- — recording the probed emails would BUILD the enumeration list this table
-- exists to bound.
CREATE TABLE IF NOT EXISTS invite_attempts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_user_id TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_invite_attempts_actor_time
    ON invite_attempts(actor_user_id, created_at);
