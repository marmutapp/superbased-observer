import { ProcessesSection } from "@/components/ProcessesSection";
import { SubAgentsSection } from "@/components/sessiondetail/SubAgentsSection";

// System tab — "what did this session actually run on this machine?".
//
// ProcessesSection is unchanged and unmoved in behaviour: it is still
// lazy-fetching (a collapsed section makes no request), still polls only while
// something is running, and still persists its own open/closed state in
// localStorage. The only change is that it now lives behind a tab.
//
// SubAgentsSection (below Processes) breaks the session's inline sub-agent
// activity out per sub-agent — same lazy disclosure contract, no request
// while collapsed.
//
// onFocusMessage crosses tabs: a process row links to the message that spawned
// it, which now lives on the Messages tab. The shell's handler switches tabs
// before focusing, so the link still lands.
export function SystemTab({
  sessionId,
  onFocusMessage,
}: {
  sessionId: string | null;
  onFocusMessage?: (messageId: string) => void;
}) {
  return (
    <div className="space-y-4">
      <ProcessesSection sessionId={sessionId} onFocusMessage={onFocusMessage} />
      <SubAgentsSection sessionId={sessionId} />
    </div>
  );
}
