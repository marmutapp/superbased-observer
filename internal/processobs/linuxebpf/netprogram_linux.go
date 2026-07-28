//go:build linux

package linuxebpf

import (
	"encoding/binary"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
)

// Per-process network byte accounting (docs/process-observability.md
// §"Per-process network bytes").
//
// WHAT IS COUNTED — TCP PAYLOAD BYTES ONLY. Two BPF trampoline programs
// maintain one LRU hash keyed by pid:
//
//   - fexit/tcp_sendmsg  → return value = bytes actually queued for send.
//     Taking the RETURN (not the `size` argument) is what makes a partial
//     send count honestly.
//   - fentry/tcp_cleanup_rbuf → `copied` argument = bytes actually consumed
//     by userspace. This is the receive counterpart bcc's tcptop uses; it is
//     preferred over tcp_recvmsg because tcp_recvmsg's ARITY changed across
//     kernels (5.x had a `nonblock` parameter, 6.x does not), and an fexit
//     program must know the argument count to find the return slot.
//     tcp_cleanup_rbuf(struct sock *, int copied) has been stable for years.
//
// NOT counted: UDP (so QUIC / HTTP-3 is invisible), unix-domain and raw
// sockets, IP/TCP headers, retransmits, and any traffic of a process on the
// Windows side of a WSL install (eBPF cannot see it; that needs ETW).
//
// NOT plaintext. These are BYTE COUNTERS on the socket layer. eBPF sees no
// TLS plaintext here and this code never claims to — payload capture remains
// the proxy's job.
//
// TOOLCHAIN: unchanged from the lifecycle programs — pure-Go
// github.com/cilium/ebpf, hand-written eBPF asm, no clang, no bpf2go, no
// CO-RE relocations, NO CGO. The one new kernel dependency is BPF trampolines
// (fentry/fexit, kernel ≥5.5 on x86-64/arm64 with BTF); the attach target's
// BTF id is resolved by the library from ProgramSpec.AttachTo.

// netMapMaxEntries bounds the per-pid counter map. It is an LRU hash, so a
// box with more chatty processes than this evicts the least-recently-used
// entry instead of failing an update — a consumer therefore has to treat a
// DECREASE in a cumulative counter as a reset, which it must do anyway for
// pid reuse and daemon restarts.
const netMapMaxEntries = 16384

// Byte offsets inside the 16-byte map value {u64 rx; u64 tx;}.
const (
	netValRxOff = 0
	netValTxOff = 8
	netValSize  = 16
)

// Stack layout shared by both network programs (r10-relative, all naturally
// aligned so the verifier's alignment check passes):
//
//	[r10-8  .. r10-1 ]  struct bpf_pidns_info {u32 pid; u32 tgid;}
//	[r10-32 .. r10-17]  scratch map value {u64 rx; u64 tx;}
//	[r10-36 .. r10-33]  scratch map key   (u32 pid)
const (
	netStackPidns = -8
	netStackVal   = -32
	netStackKey   = -36
)

// bpfNoExist is BPF_NOEXIST — the map-update flag that makes the insert fail
// when another CPU already created the entry, so the racing path can fall back
// to an atomic add instead of clobbering it.
const bpfNoExist = 1

// netProbe describes one attach point in the table-driven probe set
// (CLAUDE.md rule 5: a data table, not a conditional ladder).
type netProbe struct {
	// name is the loaded program's name (kernel-visible, ≤15 chars).
	name string
	// symbol is the kernel function to attach to.
	symbol string
	// attach is fentry (read an argument) or fexit (read the return value).
	attach ebpf.AttachType
	// ctxOff is the byte offset of the counted u64 slot in the trampoline
	// context: argument i is at 8*i, and for fexit the return value is at
	// 8*nargs.
	ctxOff int16
	// valOff selects the rx or tx half of the map value.
	valOff int16
}

// netProbes is the shipped probe set. Adding a protocol means adding a row.
var netProbes = []netProbe{
	{
		// tcp_sendmsg(struct sock *sk, struct msghdr *msg, size_t size) -> int
		// fexit ctx[3] = return value = bytes queued.
		name:   "sbo_net_tx",
		symbol: "tcp_sendmsg",
		attach: ebpf.AttachTraceFExit,
		ctxOff: 24,
		valOff: netValTxOff,
	},
	{
		// tcp_cleanup_rbuf(struct sock *sk, int copied)
		// fentry ctx[1] = copied = bytes consumed by userspace.
		name:   "sbo_net_rx",
		symbol: "tcp_cleanup_rbuf",
		attach: ebpf.AttachTraceFEntry,
		ctxOff: 8,
		valOff: netValRxOff,
	},
}

// newNetBytesMap creates the per-pid cumulative counter map.
func newNetBytesMap() (*ebpf.Map, error) {
	return ebpf.NewMap(&ebpf.MapSpec{
		Name:       "sbo_net_bytes",
		Type:       ebpf.LRUHash,
		KeySize:    4,
		ValueSize:  netValSize,
		MaxEntries: netMapMaxEntries,
	})
}

// buildNetProgram assembles one accounting program. Pseudocode:
//
//	r6 = ctx
//	r7 = (s64)(s32)*(u64*)(r6 + ctxOff)     // bytes moved (signed)
//	if r7 <= 0 goto out                      // error / nothing moved
//	bpf_get_ns_current_pid_tgid(nsDev, nsIno, r10-8, 8)
//	if r0 != 0 goto out                      // not visible in our pid ns
//	key = ns tgid
//	v = bpf_map_lookup_elem(&m, &key)
//	if v != 0 { lock *(u64*)(v + valOff) += r7; goto out }
//	init: zero the scratch value, put r7 in its half,
//	      bpf_map_update_elem(&m, &key, &val, BPF_NOEXIST)
//	      if that raced, re-lookup and atomically add instead
//	out: return 0
//
// The pid is read through bpf_get_ns_current_pid_tgid so it matches the
// daemon's pid namespace — load-bearing on WSL2, exactly as in buildProgram.
func buildNetProgram(p netProbe, m *ebpf.Map, nsDev, nsIno uint64) (*ebpf.Program, error) {
	return ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         p.name,
		Type:         ebpf.Tracing,
		AttachType:   p.attach,
		AttachTo:     p.symbol,
		Instructions: netProgramInstructions(p, m.FD(), nsDev, nsIno),
		License:      bpfLicense,
	})
}

// netProgramInstructions is the pure instruction stream of buildNetProgram,
// split out so it can be assembled and marshalled in a unit test WITHOUT
// CAP_BPF — the verifier itself needs privileges, but label resolution, offset
// ranges and encoding do not.
func netProgramInstructions(p netProbe, mapFD int, nsDev, nsIno uint64) asm.Instructions {
	return asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),

		// r7 = sign-extended low 32 bits of the counted slot.
		asm.LoadMem(asm.R7, asm.R6, p.ctxOff, asm.DWord),
		asm.LSh.Imm(asm.R7, 32),
		asm.ArSh.Imm(asm.R7, 32),
		asm.JSLE.Imm(asm.R7, 0, "out"),

		// bpf_get_ns_current_pid_tgid(nsDev, nsIno, r10-8, 8)
		asm.LoadImm(asm.R1, int64(nsDev), asm.DWord),
		asm.LoadImm(asm.R2, int64(nsIno), asm.DWord),
		asm.Mov.Reg(asm.R3, asm.R10),
		asm.Add.Imm(asm.R3, netStackPidns),
		asm.Mov.Imm(asm.R4, 8),
		asm.FnGetNsCurrentPidTgid.Call(),
		asm.JNE.Imm(asm.R0, 0, "out"),

		// key = ns tgid (the thread-group leader id == the pid userspace sees).
		asm.LoadMem(asm.R8, asm.R10, netStackPidns+4, asm.Word),
		asm.StoreMem(asm.R10, netStackKey, asm.R8, asm.Word),

		// v = bpf_map_lookup_elem(&m, &key)
		asm.LoadMapPtr(asm.R1, mapFD),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, netStackKey),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "init"),

		// lock *(u64*)(v + valOff) += r7
		asm.AddAtomic.Mem(asm.R0, asm.R7, asm.DWord, p.valOff),
		asm.Ja.Label("out"),

		// init: build {0,0}, place the bytes in our half, insert if absent.
		// The zeroing goes through a register because a 64-bit ST-imm is not a
		// legal eBPF encoding (the immediate field is 32 bits).
		asm.Mov.Imm(asm.R9, 0).WithSymbol("init"),
		asm.StoreMem(asm.R10, netStackVal, asm.R9, asm.DWord),
		asm.StoreMem(asm.R10, netStackVal+8, asm.R9, asm.DWord),
		asm.StoreMem(asm.R10, netStackVal+p.valOff, asm.R7, asm.DWord),
		asm.LoadMapPtr(asm.R1, mapFD),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, netStackKey),
		asm.Mov.Reg(asm.R3, asm.R10),
		asm.Add.Imm(asm.R3, netStackVal),
		asm.Mov.Imm(asm.R4, bpfNoExist),
		asm.FnMapUpdateElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),

		// Raced with another CPU's insert: add atomically to the winner.
		asm.LoadMapPtr(asm.R1, mapFD),
		asm.Mov.Reg(asm.R2, asm.R10),
		asm.Add.Imm(asm.R2, netStackKey),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"),
		asm.AddAtomic.Mem(asm.R0, asm.R7, asm.DWord, p.valOff),

		asm.Mov.Imm(asm.R0, 0).WithSymbol("out"),
		asm.Return(),
	}
}

// netAccounting holds the loaded network-accounting objects. A nil value means
// accounting is not live, and every read reports UNMEASURED rather than zero.
type netAccounting struct {
	m     *ebpf.Map
	progs []*ebpf.Program
	links []link.Link
}

// startNetAccounting loads and attaches the whole probe set. It is
// ALL-OR-NOTHING on purpose: half the probes attaching would produce a
// send-only or receive-only series that looks like real data but is a lie, so
// any failure tears everything down and returns an error the caller turns into
// a degraded (unmeasured) status. It never panics and never blocks.
func startNetAccounting(nsDev, nsIno uint64) (*netAccounting, error) {
	m, err := newNetBytesMap()
	if err != nil {
		return nil, fmt.Errorf("linuxebpf.startNetAccounting: counter map (missing CAP_BPF?): %w", err)
	}
	na := &netAccounting{m: m}
	for _, p := range netProbes {
		prog, perr := buildNetProgram(p, m, nsDev, nsIno)
		if perr != nil {
			na.close()
			return nil, fmt.Errorf("linuxebpf.startNetAccounting: load %s/%s (needs BTF + fentry/fexit support): %w", p.attach, p.symbol, perr)
		}
		na.progs = append(na.progs, prog)
		lk, lerr := link.AttachTracing(link.TracingOptions{Program: prog, AttachType: p.attach})
		if lerr != nil {
			na.close()
			return nil, fmt.Errorf("linuxebpf.startNetAccounting: attach %s/%s (missing CAP_BPF/CAP_PERFMON?): %w", p.attach, p.symbol, lerr)
		}
		na.links = append(na.links, lk)
	}
	return na, nil
}

// lookup returns the cumulative (rx, tx) byte counters for a pid, or ok=false
// when the map holds no entry for it (never sent/received, evicted, or
// cleaned up after exit).
func (n *netAccounting) lookup(pid int) (rx, tx int64, ok bool) {
	if n == nil || n.m == nil {
		return 0, 0, false
	}
	buf, err := n.m.LookupBytes(uint32(pid)) //nolint:gosec // pid is a small positive int
	if err != nil || len(buf) < netValSize {
		return 0, 0, false
	}
	return int64(binary.NativeEndian.Uint64(buf[netValRxOff:])), //nolint:gosec // counters never exceed int64
		int64(binary.NativeEndian.Uint64(buf[netValTxOff:])), true
}

// forget drops a pid's counters. Called when a pid exits (after its final
// value has been cached) and when a pid EXECs (so a reused pid never inherits
// the previous occupant's totals).
func (n *netAccounting) forget(pid int) {
	if n == nil || n.m == nil {
		return
	}
	_ = n.m.Delete(uint32(pid)) //nolint:gosec // pid is a small positive int
}

// close releases every object, in reverse order of creation. Idempotent.
func (n *netAccounting) close() {
	if n == nil {
		return
	}
	for _, l := range n.links {
		_ = l.Close()
	}
	for _, p := range n.progs {
		_ = p.Close()
	}
	if n.m != nil {
		_ = n.m.Close()
	}
	n.links, n.progs, n.m = nil, nil, nil
}
