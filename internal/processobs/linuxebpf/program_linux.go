//go:build linux

package linuxebpf

import (
	"errors"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
)

// ringbufBytes is the ring-buffer size in bytes. Must be a power of two and a
// multiple of the page size. 256 KiB comfortably absorbs an exec storm (a build
// fanning out hundreds of short-lived compilers) between userspace reads.
const ringbufBytes = 1 << 18

// bpfLicense is the license string DECLARED FOR THE eBPF BYTECODE — not the
// project. The kernel only loads a tracing program whose declared license is
// GPL-compatible, AND bpf_probe_read_kernel is a GPL-only helper, so this MUST
// be GPL-compatible; "Dual MIT/GPL" is on that list and keeps the tiny embedded
// snippet permissive. Standard pattern for non-GPL userspace shipping a
// GPL-compatible BPF object.
const bpfLicense = "Dual MIT/GPL"

// newRingbufMap creates the BPF ring buffer the programs submit records to.
func newRingbufMap() (*ebpf.Map, error) {
	return ebpf.NewMap(&ebpf.MapSpec{
		Name:       "sbo_proc_rb",
		Type:       ebpf.RingBuf,
		MaxEntries: ringbufBytes,
	})
}

// startBoottimeOffset resolves the byte offset of task_struct.start_boottime
// from the running kernel's BTF. This is the "BTF-offset" technique — a
// lightweight stand-in for full CO-RE that needs no clang and no hand-emitted
// relocation records: read the offset in userspace and bake it into the program
// as an immediate before loading, so the program is correct for THIS kernel's
// layout. Falls back to real_start_time (the pre-5.5 field name) so older
// kernels still work. An error here means no BTF / unexpected layout → the
// backend fails open to poll.
func startBoottimeOffset() (uint32, error) {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return 0, err
	}
	var ts *btf.Struct
	if err := spec.TypeByName("task_struct", &ts); err != nil {
		return 0, err
	}
	for _, name := range []string{"start_boottime", "real_start_time"} {
		for _, m := range ts.Members {
			if m.Name == name {
				return uint32(m.Offset.Bytes()), nil
			}
		}
	}
	return 0, errors.New("task_struct has neither start_boottime nor real_start_time")
}

// pidnsDevIno identifies the CALLER's pid namespace by the (device, inode) of
// /proc/self/ns/pid. The program passes these to bpf_get_ns_current_pid_tgid so
// it reports pids AS SEEN IN this namespace — load-bearing on WSL2, where the
// distro runs in a child pid namespace and bpf_get_current_pid_tgid's root-ns
// pids would never match the daemon's /proc (or the poll backend's keys).
func pidnsDevIno() (dev, ino uint64, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/pid", &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// buildProgram assembles the eBPF program attached to one raw tracepoint. It
// uses only stable helpers plus a single BTF-offset read of the current task's
// start_boottime — no clang, no CO-RE relocation records, no tracefs.
//
// It reads the pid via bpf_get_ns_current_pid_tgid(nsDev, nsIno) so the reported
// pid is in the DAEMON's pid namespace (the WSL2 child namespace), matching
// /proc and the poll backend; a process not visible in that namespace yields a
// non-zero return and is skipped. The thread-group-leader filter (ns tid == ns
// tgid) drops the per-thread duplicate sched_process_exit fires (a no-op for
// exec, where de_thread() already made the caller the leader).
//
// Registers: r6–r9 are callee-saved. r8 = reported pid (ns tgid); r6 = record
// pointer. The 8-byte struct bpf_pidns_info {u32 pid; u32 tgid;} lands at
// [r10-8].
//
//	bpf_get_ns_current_pid_tgid(nsDev, nsIno, r10-8, 8)
//	if r0 != 0 goto out                 // not visible in our ns → skip
//	r9 = *(u32*)(r10-8)                 // ns tid
//	r8 = *(u32*)(r10-4)                 // ns tgid (reported pid)
//	if r9 != r8 goto out                // not the group leader → skip
//	r0 = bpf_ringbuf_reserve(&rb, 32, 0)
//	if r0 == 0 goto out
//	r6 = r0
//	*(u32*)(r6+0)  = <evType>
//	*(u32*)(r6+4)  = r8
//	r0 = bpf_get_current_task()
//	bpf_probe_read_kernel(r6+8, 8, r0 + <start_boottime offset>)
//	bpf_get_current_comm(r6+16, 16)
//	bpf_ringbuf_submit(r6, 0)
//	out: r0 = 0; return r0
func buildProgram(name string, rb *ebpf.Map, typ evType, startBoottimeOff uint32, nsDev, nsIno uint64) (*ebpf.Program, error) {
	insns := asm.Instructions{
		// bpf_get_ns_current_pid_tgid(nsDev, nsIno, r10-8, 8)
		asm.LoadImm(asm.R1, int64(nsDev), asm.DWord),
		asm.LoadImm(asm.R2, int64(nsIno), asm.DWord),
		asm.Mov.Reg(asm.R3, asm.R10),
		asm.Add.Imm(asm.R3, -8),
		asm.Mov.Imm(asm.R4, 8),
		asm.FnGetNsCurrentPidTgid.Call(),
		asm.JNE.Imm(asm.R0, 0, "out"),

		// r9 = ns tid (low struct field), r8 = ns tgid (reported pid).
		asm.LoadMem(asm.R9, asm.R10, -8, asm.Word),
		asm.LoadMem(asm.R8, asm.R10, -4, asm.Word),
		asm.JNE.Reg(asm.R9, asm.R8, "out"),

		// r0 = bpf_ringbuf_reserve(rb, recordSize, 0)
		asm.LoadMapPtr(asm.R1, rb.FD()),
		asm.Mov.Imm(asm.R2, recordSize),
		asm.Mov.Imm(asm.R3, 0),
		asm.FnRingbufReserve.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),

		// r6 = record pointer.
		asm.Mov.Reg(asm.R6, asm.R0),
		// *(u32*)(r6+0) = typ ; *(u32*)(r6+4) = tgid
		asm.StoreImm(asm.R6, offType, int64(typ), asm.Word),
		asm.StoreMem(asm.R6, offPID, asm.R8, asm.Word),

		// start_boottime: r0 = bpf_get_current_task();
		// bpf_probe_read_kernel(r6+8, 8, r0 + off)
		asm.FnGetCurrentTask.Call(),
		asm.Mov.Reg(asm.R3, asm.R0),
		asm.Add.Imm(asm.R3, int32(startBoottimeOff)),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Add.Imm(asm.R1, offStartBoottime),
		asm.Mov.Imm(asm.R2, 8),
		asm.FnProbeReadKernel.Call(),

		// bpf_get_current_comm(r6+16, commLen)
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Add.Imm(asm.R1, offComm),
		asm.Mov.Imm(asm.R2, commLen),
		asm.FnGetCurrentComm.Call(),

		// bpf_ringbuf_submit(r6, 0)
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Imm(asm.R2, 0),
		asm.FnRingbufSubmit.Call(),

		// out: r0 = 0; return
		asm.Mov.Imm(asm.R0, 0).WithSymbol("out"),
		asm.Return(),
	}

	return ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         name,
		Type:         ebpf.RawTracepoint,
		Instructions: insns,
		License:      bpfLicense,
	})
}
