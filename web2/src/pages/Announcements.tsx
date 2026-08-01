import { useState } from "react";
import {
  api,
  ApiError,
  type OrgAnnouncement,
  type OrgAnnouncementCurrent,
  type OrgAnnouncementDraft,
} from "@/lib/api";
import { useApi } from "@/lib/useApi";
import { dateTime } from "@/lib/format";
import { Badge, Button, Card, ErrorState, PageHeader, Spinner } from "@/components/ui";

// Limits mirror internal/announce (MaxTitleChars / MaxBodyChars). They
// are duplicated here ONLY to drive the live counters — the server
// re-validates every field through announce.Validate before signing, so
// a drifted constant here degrades to a confusing counter, never to an
// over-long banner reaching the fleet.
const MAX_TITLE = 120;
const MAX_BODY = 280;

const SEVERITIES: OrgAnnouncement["severity"][] = ["info", "notice", "critical"];

const SEVERITY_HELP: Record<OrgAnnouncement["severity"], string> = {
  info: "Neutral. The default — “here is something that changed”.",
  notice: "Accent. Something the team should act on eventually.",
  critical: "Red. Reserved for security advisories — overusing it is how a banner surface dies.",
};

// slugId builds the stable dismissal key: a date-prefixed slug of the
// title, matching the convention the release rail uses
// ("2026-07-31-example"). Ids must be UNIQUE over time — every
// dashboard remembers dismissed ids in localStorage, so reusing one
// ships a banner that is pre-dismissed for everyone who saw the old
// one. The date prefix makes accidental reuse essentially impossible
// unless the same title is published twice in one day, which is also
// the one case where suppressing it is the right answer.
function slugId(title: string, now: Date): string {
  const day = now.toISOString().slice(0, 10);
  const slug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40);
  return slug ? `${day}-${slug}` : day;
}

// Counter renders "n / max", turning red once the server would refuse.
function Counter({ n, max }: { n: number; max: number }) {
  return (
    <span className={`font-mono text-[11px] ${n > max ? "text-danger" : "text-fg-3"}`}>
      {n} / {max}
    </span>
  );
}

const inputClass =
  "w-full rounded border border-line-2 bg-bg-3 px-3 py-2 text-sm text-fg-1 placeholder:text-fg-3 focus:border-accent focus:outline-none";

// CurrentPanel shows exactly what enrolled nodes are being served right
// now, and is the only place Retract lives — retraction is an action on
// a specific published thing, not a mode of the composer.
function CurrentPanel({
  current,
  onChanged,
}: {
  current: OrgAnnouncementCurrent;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const retract = () => {
    setBusy(true);
    setError(null);
    api
      .orgAnnouncementRetract()
      .then(onChanged)
      .catch((e: unknown) => setError(friendly(e)))
      .finally(() => setBusy(false));
  };

  if (!current.published) {
    return (
      <Card>
        <div className="text-sm text-fg-2">
          Nothing published. Enrolled dashboards show only the announcements compiled into their
          own release.
        </div>
      </Card>
    );
  }

  return (
    <Card>
      <div className="mb-2 flex items-center gap-2">
        <div className="text-sm font-medium text-fg-1">Currently published</div>
        <Badge tone="accent">v{current.version}</Badge>
        {current.retracted && <Badge tone="muted">retracted</Badge>}
      </div>
      {current.retracted ? (
        <div className="text-sm text-fg-2">
          The newest version is a retraction — nodes clear the banner on their next poll (they
          check on the org push cycle they already run, roughly every 15 minutes).
        </div>
      ) : (
        <>
          <div className="space-y-2">
            {current.announcements.map((a) => (
              <div key={a.id} className="rounded border border-line-2 bg-bg-3 p-3">
                <div className="flex items-center gap-2">
                  <Badge tone={a.severity === "critical" ? "bad" : a.severity === "notice" ? "accent" : "muted"}>
                    {a.severity}
                  </Badge>
                  <span className="text-sm font-medium text-fg-1">{a.title}</span>
                </div>
                <div className="mt-1 text-sm text-fg-2">{a.body}</div>
                <div className="mt-2 flex flex-wrap items-center gap-3 text-[11px] text-fg-3">
                  <span className="font-mono">{a.id}</span>
                  <span>expires {dateTime(a.expires_at)}</span>
                  {a.url && (
                    <a
                      href={a.url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-accent hover:text-accent-strong"
                    >
                      {a.url}
                    </a>
                  )}
                </div>
              </div>
            ))}
          </div>
          <div className="mt-3 flex items-center gap-2">
            <Button
              onClick={retract}
              disabled={busy}
              title="Publishes an empty (signed, versioned) document — nodes clear the banner on their next poll. Audited."
            >
              {busy ? "Retracting…" : "Retract"}
            </Button>
            <span className="text-[11px] text-fg-3">
              Announcements also self-retire at their expiry; retract only when you need it gone
              sooner.
            </span>
          </div>
        </>
      )}
      {error && <div className="mt-2 text-sm text-danger">{error}</div>}
    </Card>
  );
}

// friendly maps the two failure modes an admin can actually hit onto
// plain language: no admin role, or a body the server refused.
function friendly(e: unknown): string {
  if (e instanceof ApiError && e.status === 403) {
    return "Publishing requires an org admin session ([dashboard].admin_emails).";
  }
  if (e instanceof ApiError && e.status === 400) {
    return `Refused: ${e.message}`;
  }
  return String(e);
}

// ComposePanel is the authoring surface. Publishing signs the document
// with the org's Ed25519 key and bumps its version; agents verify it
// against the key they pinned at enrolment.
function ComposePanel({ onPublished }: { onPublished: () => void }) {
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [severity, setSeverity] = useState<OrgAnnouncement["severity"]>("info");
  const [url, setUrl] = useState("");
  const [days, setDays] = useState(14);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [published, setPublished] = useState<number | null>(null);

  const expiresAt = new Date(Date.now() + days * 86400_000);
  const id = slugId(title, new Date());
  const tooLong = title.length > MAX_TITLE || body.length > MAX_BODY;
  const ready = title.trim() !== "" && body.trim() !== "" && days > 0 && !tooLong;

  const publish = () => {
    const draft: OrgAnnouncementDraft = {
      id,
      severity,
      title: title.trim(),
      body: body.trim(),
      expires_at: expiresAt.toISOString().replace(/\.\d{3}Z$/, "Z"),
      ...(url.trim() ? { url: url.trim() } : {}),
    };
    setBusy(true);
    setError(null);
    api
      .orgAnnouncementPublish(draft)
      .then((r) => {
        setPublished(r.version);
        setTitle("");
        setBody("");
        setUrl("");
        onPublished();
      })
      .catch((e: unknown) => setError(friendly(e)))
      .finally(() => setBusy(false));
  };

  return (
    <Card>
      <div className="mb-3 text-sm font-medium text-fg-1">Compose an announcement</div>

      <div className="mb-1 flex items-center justify-between">
        <label className="text-xs font-medium text-fg-2">Title</label>
        <Counter n={title.length} max={MAX_TITLE} />
      </div>
      <input
        value={title}
        onChange={(e) => setTitle(e.target.value)}
        placeholder="One line, plain text"
        className={inputClass}
      />

      <div className="mb-1 mt-3 flex items-center justify-between">
        <label className="text-xs font-medium text-fg-2">Body</label>
        <Counter n={body.length} max={MAX_BODY} />
      </div>
      <textarea
        value={body}
        onChange={(e) => setBody(e.target.value)}
        rows={3}
        placeholder="Plain text. No markdown, no HTML — banners are a one-glance surface; put detail behind the link."
        className={inputClass}
      />

      <div className="mt-3 grid gap-3 sm:grid-cols-3">
        <div>
          <label className="mb-1 block text-xs font-medium text-fg-2">Severity</label>
          <select
            value={severity}
            onChange={(e) => setSeverity(e.target.value as OrgAnnouncement["severity"])}
            className={inputClass}
          >
            {SEVERITIES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="mb-1 block text-xs font-medium text-fg-2">Expires in (days)</label>
          <input
            type="number"
            min={1}
            max={365}
            value={days}
            onChange={(e) => setDays(Number(e.target.value))}
            className={inputClass}
          />
        </div>
        <div>
          <label className="mb-1 block text-xs font-medium text-fg-2">Link (optional)</label>
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://…"
            className={inputClass}
          />
        </div>
      </div>
      <div className="mt-1 text-[11px] text-fg-3">{SEVERITY_HELP[severity]}</div>

      <div className="mt-3 rounded border border-line-2 bg-bg-3 p-3 text-[11px] text-fg-3">
        <div>
          Dismissal id: <span className="font-mono text-fg-2">{id}</span> — derived from the title
          and today's date. Ids are remembered per dashboard, so reusing one hides the banner from
          anyone who dismissed the old one.
        </div>
        <div className="mt-1">
          Expires <span className="text-fg-2">{dateTime(expiresAt.toISOString())}</span>. Every
          announcement self-retires; links must be https.
        </div>
      </div>

      <div className="mt-3 flex items-center gap-2">
        <Button
          variant="primary"
          onClick={publish}
          disabled={!ready || busy}
          title="Validate + sign + version in one transaction, then serve it to enrolled agents on their existing poll. Audited."
        >
          {busy ? "Publishing…" : "Publish (audited)"}
        </Button>
        {published !== null && <Badge tone="good">published v{published}</Badge>}
        {tooLong && <span className="text-xs text-danger">Over the length limit.</span>}
      </div>
      {error && <div className="mt-2 text-sm text-danger">{error}</div>}
    </Card>
  );
}

export function AnnouncementsPage() {
  const { data, error, loading, reload } = useApi(() => api.orgAnnouncement(), []);

  return (
    <>
      <PageHeader
        title="Announcements"
        subtitle="A one-way, dismissible banner on every enrolled node's own dashboard. Signed and versioned like the policy registry, delivered on the poll agents already make — no new connection, no read receipts, and each node operator can silence the rail locally."
      />
      {error ? (
        <ErrorState message={error} onRetry={reload} />
      ) : loading || !data ? (
        <Spinner />
      ) : (
        <div className="space-y-5">
          <CurrentPanel current={data} onChanged={reload} />
          <ComposePanel onPublished={reload} />
        </div>
      )}
    </>
  );
}
