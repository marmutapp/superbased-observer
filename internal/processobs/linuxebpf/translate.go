package linuxebpf

import (
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/processobs/poll"
)

// enrichFunc resolves a pid to its full /proc snapshot. The default is
// poll.ReadProcInfo (a real /proc read, Linux only); tests inject a fake so the
// translator is exercised without a kernel or a /proc. ok=false means the
// process already vanished — but unlike the start-time (now read in-kernel), the
// argv/cwd/ppid it would have supplied are simply absent for such a process.
type enrichFunc func(pid int) (poll.ProcInfo, bool)

// translator turns the minimal kernel ring-buffer events into the same
// processobs.RawEvent stream the poll backend produces. Because start_boottime
// is read in-kernel, the translator is now STATELESS — every event self-keys
// (exec and exit carry the same kernel start-time → the same ProcessKey),
// without the exec→exit pid map the /proc-keyed version needed. The only I/O is
// the injected enrich func, used best-effort for argv/cwd/uids.
type translator struct {
	bootID string
	now    func() time.Time
	enrich enrichFunc
}

func newTranslator(bootID string, now func() time.Time, enrich enrichFunc) *translator {
	if now == nil {
		now = time.Now
	}
	return &translator{bootID: bootID, now: now, enrich: enrich}
}

// handle folds one kernel event into RawEvents:
//
//   - exec → fork + exec when /proc enrichment succeeds (full argv/cwd/uids/
//     metrics, parents-before-children shape the Attributor expects); or, when
//     the process already vanished, a single KEYED exec carrying the in-kernel
//     identity (pid + start-time + comm) so its existence is still recorded —
//     the win the /proc-keyed version threw away.
//   - exit → a single keyed exit; its start-time comes from the same in-kernel
//     read, so its ProcessKey matches the exec's with no shared state.
//
// The kernel start-time is authoritative for the key in BOTH branches: for a
// live process it equals /proc's value, and for a vanished one it is the only
// value available.
func (t *translator) handle(ev kernelEvent) []processobs.RawEvent {
	startTicks := ev.StartTicks()
	hasStart := ev.StartBoottimeNs > 0
	ts := t.now()

	switch ev.Type {
	case evExec:
		if p, ok := t.enrich(ev.PID); ok {
			// Trust the kernel start-time for the key (race-proof, and equal to
			// /proc's for a live process); take everything else from /proc.
			p.StartTicks = startTicks
			p.HasStart = hasStart
			return []processobs.RawEvent{
				forkEventFromProc(p, t.bootID, ts),
				execEventFromProc(p, t.bootID, ts),
			}
		}
		// Process gone before the /proc read: still keyed + named via comm, so
		// it counts as a real captured process (no argv/cwd/ppid). Setting
		// ExePath to comm lets the Attributor's basename checks (boundary /
		// AI-launcher / self-exclude) still see the process name.
		return []processobs.RawEvent{{
			Type: processobs.EventExec, Timestamp: ts, BootID: t.bootID,
			PID: ev.PID, StartTimeTicks: startTicks, HasStartTime: hasStart,
			ExePath: ev.Comm,
		}}
	case evExit:
		return []processobs.RawEvent{{
			Type: processobs.EventExit, Timestamp: ts, BootID: t.bootID,
			PID: ev.PID, StartTimeTicks: startTicks, HasStartTime: hasStart,
		}}
	default:
		return nil
	}
}

// forkEventFromProc mirrors poll.forkEvent: identity only, so the Attributor
// can place the process in the tree before the exec resolves its details.
func forkEventFromProc(p poll.ProcInfo, bootID string, ts time.Time) processobs.RawEvent {
	return processobs.RawEvent{
		Type: processobs.EventFork, Timestamp: ts, BootID: bootID,
		PID: p.PID, PPID: p.PPID, StartTimeTicks: p.StartTicks, HasStartTime: p.HasStart,
	}
}

// execEventFromProc mirrors poll.execEvent: the full enriched exec envelope.
// Keeping the mapping identical to poll's is deliberate — the eBPF and poll
// backends must be interchangeable to the Attributor so they dedup on
// ProcessKey when composed (the start ticks come from the same boot-time value).
func execEventFromProc(p poll.ProcInfo, bootID string, ts time.Time) processobs.RawEvent {
	return processobs.RawEvent{
		Type: processobs.EventExec, Timestamp: ts, BootID: bootID,
		PID: p.PID, PPID: p.PPID, StartTimeTicks: p.StartTicks, HasStartTime: p.HasStart,
		ExePath: p.ExePath, Argv: p.Argv, CWD: p.CWD,
		UID: p.UID, GID: p.GID, EUID: p.EUID, EGID: p.EGID,
		SeccompMode: p.SeccompMode, CapabilitiesEff: p.CapabilitiesEff,
		AppArmorLabel: p.AppArmorLabel, SELinuxLabel: p.SELinuxLabel,
		CgroupPath: p.CgroupPath, ContainerID: p.ContainerID,
		PIDNamespace: p.PIDNamespace, MountNamespace: p.MountNamespace, NetNamespace: p.NetNamespace,
		HasMetrics: p.HasMetrics,
		CPUUserMs:  p.CPUUserMs, CPUSystemMs: p.CPUSystemMs,
		MaxRSSBytes: p.MaxRSSBytes, WorkingSetBytes: p.WorkingSetBytes,
		ReadBytes: p.ReadBytes, WriteBytes: p.WriteBytes, ReadOps: p.ReadOps, WriteOps: p.WriteOps,
		ThreadCount: p.ThreadCount, HandleCount: p.HandleCount,
	}
}
