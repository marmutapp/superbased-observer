//go:build windows

package poll

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Resource-metric reads for the Windows poll capturer
// (docs/plans/process-obs-dashboard-enhancements-2026-06-17.md). CPU comes from
// GetProcessTimes (already in x/sys/windows); memory + disk + handle count are
// NOT wrapped there, so we bind them via LazyDLL — pure Go, no CGO. Network
// per-process metrics are NOT read here: they need ETW (spec §5.2), deferred.
var (
	modpsapi    = windows.NewLazySystemDLL("psapi.dll")
	modkernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetProcessMemoryInfo  = modpsapi.NewProc("GetProcessMemoryInfo")
	procGetProcessIoCounters  = modkernel32.NewProc("GetProcessIoCounters")
	procGetProcessHandleCount = modkernel32.NewProc("GetProcessHandleCount")
)

// processMemoryCounters mirrors PSAPI's PROCESS_MEMORY_COUNTERS. SIZE_T is
// pointer-sized (uintptr on the 64-bit targets we build). cb must be set to the
// struct size before the call.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// readProcessMetrics fills p's resource counters from an open process handle
// (PROCESS_QUERY_LIMITED_INFORMATION suffices for all three). Best-effort: a
// failed individual read leaves that field zero; HasMetrics is set whenever the
// handle was usable at all, so a genuinely-zero counter is distinguishable from
// "never read". CPU times are cumulative since process start (100ns → ms).
func readProcessMetrics(h windows.Handle, p *ProcInfo) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err == nil {
		p.CPUUserMs = filetime100ns(user) / 10000
		p.CPUSystemMs = filetime100ns(kernel) / 10000
	}

	var mc processMemoryCounters
	mc.CB = uint32(unsafe.Sizeof(mc))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); r != 0 {
		p.WorkingSetBytes = int64(mc.WorkingSetSize)
		p.MaxRSSBytes = int64(mc.PeakWorkingSetSize)
	}

	var io windows.IO_COUNTERS
	if r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
		p.ReadBytes = int64(io.ReadTransferCount)
		p.WriteBytes = int64(io.WriteTransferCount)
		p.ReadOps = int64(io.ReadOperationCount)
		p.WriteOps = int64(io.WriteOperationCount)
	}

	var handleCount uint32
	if r, _, _ := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&handleCount))); r != 0 {
		p.HandleCount = int32(handleCount)
	}

	p.HasMetrics = true
}
