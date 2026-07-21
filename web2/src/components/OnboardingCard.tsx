import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Rocket, UserPlus, Users, Wallet, BellRing } from "lucide-react";

// OnboardingCard — first-run welcome + checklist for a fresh org server (G11).
// Mirrors web/'s OnboardingCard, re-scoped to the admin/governance day-one
// path: the org server has just started, no agent has enrolled yet, so the
// Overview is all zeros. Rather than a dead "no data" first screen, teach the
// four steps that light the dashboard up, each linking to the real page that
// does it. Dismissible permanently (node-local localStorage flag).
//
// Shown only while the org has no enrolled activity (active developers AND
// sessions both zero). Once the first agent pushes, the card is gone.

const KEY = "sb_org_onboarding_dismissed";

function dismissed(): boolean {
  try {
    return localStorage.getItem(KEY) === "1";
  } catch {
    return false;
  }
}

type Step = {
  icon: typeof Rocket;
  to: string;
  cta: string;
  body: ReactNode;
};

const STEPS: Step[] = [
  {
    icon: UserPlus,
    to: "/invite",
    cta: "Enroll your first agent",
    body: (
      <>
        Mint an enrolment token and run{" "}
        <code className="font-mono text-fg-3">observer-org enroll</code> on a
        developer's machine — the node then pushes its aggregated activity here.
      </>
    ),
  },
  {
    icon: Users,
    to: "/invite",
    cta: "Invite teammates",
    body: (
      <>
        Add the rest of your team from your SCIM/SAML directory so their spend
        and sessions roll up under the right people and teams.
      </>
    ),
  },
  {
    icon: Wallet,
    to: "/settings",
    cta: "Set a spend budget",
    body: (
      <>
        Cap monthly spend per team or project and get a webhook alert as you
        cross each threshold — before the invoice, not after.
      </>
    ),
  },
  {
    icon: BellRing,
    to: "/trajectories/alerts",
    cta: "Configure alerts",
    body: (
      <>
        Add threshold rules over cost, error rate, and latency that post to a
        Slack/PagerDuty webhook when a metric breaches.
      </>
    ),
  },
];

export function OnboardingCard({
  activeDevelopers,
  sessions,
}: {
  activeDevelopers: number;
  sessions: number;
}) {
  const [hidden, setHidden] = useState(dismissed);
  if (hidden || activeDevelopers > 0 || sessions > 0) return null;

  const dismiss = () => {
    try {
      localStorage.setItem(KEY, "1");
    } catch {
      // ignore
    }
    setHidden(true);
  };

  return (
    <section className="relative overflow-hidden rounded-3 border border-accent/30 bg-bg-2 p-5">
      <span
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{
          background:
            "radial-gradient(circle at 100% 0%, var(--accent-soft), transparent 55%)",
        }}
      />
      <div className="relative flex items-start gap-4">
        <span className="grid h-11 w-11 shrink-0 place-items-center rounded-2 border border-accent/40 bg-accent-soft text-accent">
          <Rocket size={20} />
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="text-[15px] font-semibold text-fg-0">
            Welcome to your org dashboard.
          </h2>
          <p className="mt-1 max-w-2xl text-[12.5px] leading-relaxed text-fg-2">
            Nothing's enrolled yet — so every tile reads zero. Once an agent
            enrolls and pushes, spend, sessions, and governance signals appear
            here on their own. Four steps to a warm dashboard:
          </p>
          <div className="mt-4 grid gap-2.5 sm:grid-cols-2">
            {STEPS.map((s, i) => (
              <div
                key={i}
                className="flex items-start gap-2.5 rounded-2 border border-line-2 bg-bg-1 p-3"
              >
                <span className="grid h-7 w-7 shrink-0 place-items-center rounded-2 border border-line-2 bg-bg-2 text-fg-3">
                  <s.icon size={14} />
                </span>
                <div className="min-w-0">
                  <Link
                    to={s.to}
                    className="text-[12.5px] font-semibold text-accent hover:underline"
                  >
                    {s.cta}
                  </Link>
                  <p className="mt-0.5 text-[11.5px] leading-snug text-fg-3">
                    {s.body}
                  </p>
                </div>
              </div>
            ))}
          </div>
          <p className="mt-3 text-[11px] text-fg-4">
            New to the org server? See{" "}
            <code className="font-mono text-fg-3">
              docs/teams-getting-started.md
            </code>{" "}
            for the full bring-up (the §0 "local in 5 minutes" quickstart).
          </p>
        </div>
        <button
          type="button"
          onClick={dismiss}
          className="relative shrink-0 text-[12px] text-fg-4 hover:text-fg-2"
          aria-label="Dismiss welcome"
        >
          ✕
        </button>
      </div>
    </section>
  );
}
