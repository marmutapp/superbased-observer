//go:build linux

package poll

// ReadProcInfo reads a single process's full /proc snapshot (the same data the
// whole-table enumerate gathers per pid). It is the seam the real-time Linux
// eBPF backend (internal/processobs/linuxebpf) uses to enrich a kernel
// exec/exit event into a complete processobs.RawEvent: the kernel ring-buffer
// record carries only identity, so the backend reads /proc/<pid>/... here to
// fill argv/cwd/uids/start-time/metrics. Reusing this reader (rather than
// forking the fiddly /proc parsing) guarantees the eBPF backend computes a
// StartTimeTicks — and therefore a ProcessKey — byte-identical to the poll
// backend's for the same process, so the two backends dedup on process_key
// when composed. Returns ok=false when the process is already gone or its stat
// is unreadable (the sub-interval race the eBPF path narrows but cannot fully
// eliminate without reading start-time in-kernel — a documented follow-up).
func ReadProcInfo(pid int) (ProcInfo, bool) { return readProc(pid) }

// PlatformBootID returns the kernel boot id stamped into every ProcessKey
// (§9.3). Exported so the eBPF backend stamps the SAME boot id as the poll
// backend, keeping their ProcessKeys in one namespace for cross-backend dedup.
func PlatformBootID() string { return platformBootID() }
