// RestartOverlay — full-screen "reconnecting" scrim shown while a
// dashboard-triggered daemon restart is in flight. Shared by the
// RestartPendingBanner (config-save path) and the Settings → Health
// "Restart daemon" control (on-demand path) via useDaemonRestart.
export function RestartOverlay({ title, body }: { title: string; body: string }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-bg-1/90 p-6 backdrop-blur-sm">
      <div className="w-full max-w-sm space-y-3 rounded-3 border border-line-2 bg-bg-2 p-6 text-center">
        <div className="text-[13px] font-semibold text-fg-1">{title}</div>
        <div className="mx-auto h-6 w-6 animate-spin rounded-full border-2 border-line-2 border-t-accent" />
        <p className="text-[12px] leading-relaxed text-fg-3">{body}</p>
      </div>
    </div>
  );
}
