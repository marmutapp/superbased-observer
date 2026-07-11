//go:build windows

package poll

import (
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readProcessParameters reads a process's command line and current directory
// from its PEB — the two fields ToolHelp omits and that attribution + the
// §9.2.4 correlation need (spec §5.5). It walks
// NtQueryInformationProcess(ProcessBasicInformation) -> PebBaseAddress ->
// ReadProcessMemory(PEB) -> ProcessParameters ->
// ReadProcessMemory(RTL_USER_PROCESS_PARAMETERS) -> the CommandLine /
// CurrentDirectory NTUnicodeStrings. Pure Go, no CGO (all symbols + typed
// structs come from golang.org/x/sys/windows).
//
// The handle must carry PROCESS_VM_READ in addition to
// PROCESS_QUERY_LIMITED_INFORMATION. Same-user 64-bit processes (the AI tool's
// children, the bridge's whole point) read cleanly; anything we cannot
// VM_READ (system / another user's / higher-integrity) returns ok=false and
// the caller keeps the ToolHelp basename + start time (best-effort, never
// fatal). 32-bit (Wow64) targets are not special-cased: a failed/garbage read
// degrades to ok=false, so argv/cwd are simply absent.
func readProcessParameters(h windows.Handle) (argv []string, cwd string, ok bool) {
	params, ok := readUserProcessParameters(h)
	if !ok {
		return nil, "", false
	}

	cwd = readRemoteUTF16(h, params.CurrentDirectory.DosPath)
	if cmdline := readRemoteUTF16(h, params.CommandLine); cmdline != "" {
		// CommandLineToArgv-split so argc and the per-arg preview match the
		// Linux argv shape; fall back to the raw line if splitting fails.
		if parts, err := windows.DecomposeCommandLine(cmdline); err == nil && len(parts) > 0 {
			argv = parts
		} else {
			argv = []string{cmdline}
		}
	}
	return argv, cwd, true
}

// readUserProcessParameters walks NtQueryInformationProcess -> PebBaseAddress
// -> PEB -> ProcessParameters and returns the RTL_USER_PROCESS_PARAMETERS by
// value. It is the shared PEB hop for readProcessParameters (cmdline + cwd) and
// readProcessEnvToken (the env block). ok=false on any failed/short remote read
// (system / another user's / higher-integrity / 32-bit target / exited).
func readUserProcessParameters(h windows.Handle) (windows.RTL_USER_PROCESS_PARAMETERS, bool) {
	var zero windows.RTL_USER_PROCESS_PARAMETERS
	var pbi windows.PROCESS_BASIC_INFORMATION
	var retLen uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation,
		unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &retLen); err != nil {
		return zero, false
	}
	if pbi.PebBaseAddress == nil {
		return zero, false
	}

	var peb windows.PEB
	if !readRemote(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress)), unsafe.Pointer(&peb), unsafe.Sizeof(peb)) {
		return zero, false
	}
	if peb.ProcessParameters == nil {
		return zero, false
	}

	var params windows.RTL_USER_PROCESS_PARAMETERS
	if !readRemote(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), unsafe.Pointer(&params), unsafe.Sizeof(params)) {
		return zero, false
	}
	return params, true
}

// readProcessEnvToken reads the process's PEB environment block and returns the
// VALUE of the FIRST allowlisted key in `keys` (§5.5 P-B6 env-token). This is
// the LOAD-BEARING PRIVACY BOUNDARY: the full environment block holds secrets
// (API keys etc.), so it is parsed transiently in the capturer's memory and
// ONLY an allowlisted session-id value is ever returned — the rest is never
// emitted, shipped, or stored. The block is a double-NUL-terminated UTF-16
// KEY=VALUE sequence; EnvironmentSize bounds it (capped here against a runaway
// read). Returns "" when no allowlisted key is present or the block is
// unreadable.
func readProcessEnvToken(h windows.Handle, keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	params, ok := readUserProcessParameters(h)
	if !ok || params.Environment == nil || params.EnvironmentSize == 0 {
		return ""
	}
	size := params.EnvironmentSize
	const capBytes = 256 * 1024 // bound the read; an env block is ~10 KiB in practice
	if size > capBytes {
		size = capBytes
	}
	// Allocate a []uint16 (2-byte aligned by construction) and read the block
	// into it directly, mirroring readRemoteUTF16's pattern.
	u16 := make([]uint16, size/2)
	if len(u16) == 0 {
		return ""
	}
	var read uintptr
	if err := windows.ReadProcessMemory(h, uintptr(params.Environment),
		(*byte)(unsafe.Pointer(&u16[0])), size, &read); err != nil {
		return ""
	}
	u16 = u16[:read/2]

	start := 0
	for i := 0; i < len(u16); i++ {
		if u16[i] != 0 {
			continue
		}
		if i == start {
			break // double NUL terminates the block
		}
		entry := windows.UTF16ToString(u16[start:i])
		start = i + 1
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		if slices.Contains(keys, entry[:eq]) {
			return entry[eq+1:] // the session-id VALUE — the only field extracted
		}
	}
	return ""
}

// readRemote copies exactly size bytes from the remote process at addr into
// buf, returning false on any short or failed read.
func readRemote(h windows.Handle, addr uintptr, buf unsafe.Pointer, size uintptr) bool {
	var read uintptr
	if err := windows.ReadProcessMemory(h, addr, (*byte)(buf), size, &read); err != nil {
		return false
	}
	return read == size
}

// readRemoteUTF16 reads the remote UTF-16 buffer an NTUnicodeString points at.
// Length is a byte count (per the x/sys note), so the buffer holds Length/2
// uint16s. Length is a uint16, so it is naturally bounded to 64 KiB — no extra
// cap is needed against a runaway allocation. An empty or nil string yields "".
func readRemoteUTF16(h windows.Handle, us windows.NTUnicodeString) string {
	if us.Length == 0 || us.Buffer == nil {
		return ""
	}
	buf := make([]uint16, us.Length/2)
	var read uintptr
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(us.Buffer)),
		(*byte)(unsafe.Pointer(&buf[0])), uintptr(us.Length), &read); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf)
}
