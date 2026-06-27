package linuxebpf

import (
	"bytes"
	"encoding/binary"
)

// evType is the kind discriminator the BPF program stamps on every ring-buffer
// record. Only the two MVP lifecycle events are emitted today (§7.1); fork is
// synthesized in userspace from the exec record's PPID rather than carried as a
// separate kernel event, because the stable-helper program (no fork child-pid
// read) cannot cheaply read a fork child's pid in-kernel.
type evType uint32

const (
	evExec evType = 1
	evExit evType = 2
)

// Record wire layout (native-endian; every release target is little-endian):
//
//	struct event {
//	    __u32 type;            // offset 0
//	    __u32 pid;             // offset 4  (tgid)
//	    __u64 start_boottime;  // offset 8  (task->start_boottime, nanoseconds)
//	    char  comm[16];        // offset 16
//	};                         // 32 bytes
//
// start_boottime is read IN-KERNEL (bpf_probe_read_kernel of the current task's
// field, offset resolved from BTF) so every event is self-keyable: the
// ProcessKey no longer depends on a /proc read that a short-lived process — the
// whole reason eBPF exists — has already raced past.
const (
	recordSize       = 32
	offType          = 0
	offPID           = 4
	offStartBoottime = 8
	offComm          = 16
	commLen          = 16
	nsecPerClockTick = 10_000_000 // NSEC_PER_SEC / USER_HZ(100): matches the kernel's nsec_to_clock_t for /proc/<pid>/stat starttime
)

// kernelEvent is one decoded ring-buffer record. pid + start_boottime are the
// race-proof identity (read in-kernel); comm is the process name. argv / cwd /
// ppid / uids / metrics are filled in userspace from /proc by the translator's
// enrich step (best-effort — gone for a process that already exited).
type kernelEvent struct {
	Type            evType
	PID             int
	StartBoottimeNs uint64
	Comm            string
}

// StartTicks converts the kernel boot-time nanoseconds to the clock-tick
// starttime /proc/<pid>/stat reports (field 22), so an eBPF-keyed process and a
// poll-keyed process get a byte-identical ProcessKey and dedup. Mirrors the
// kernel's nsec_to_clock_t (integer division by NSEC_PER_SEC/USER_HZ).
func (e kernelEvent) StartTicks() int64 { return int64(e.StartBoottimeNs / nsecPerClockTick) }

// decodeEvent parses one fixed-layout ring-buffer record. Returns ok=false for
// a short read. Pure: the unit tests drive the whole translator with
// synthesized records, no kernel needed.
func decodeEvent(b []byte) (kernelEvent, bool) {
	if len(b) < recordSize {
		return kernelEvent{}, false
	}
	return kernelEvent{
		Type:            evType(binary.LittleEndian.Uint32(b[offType : offType+4])),
		PID:             int(binary.LittleEndian.Uint32(b[offPID : offPID+4])),
		StartBoottimeNs: binary.LittleEndian.Uint64(b[offStartBoottime : offStartBoottime+8]),
		Comm:            nullTerminated(b[offComm : offComm+commLen]),
	}, true
}

// nullTerminated trims a fixed-width, NUL-padded C string to its Go string.
func nullTerminated(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
