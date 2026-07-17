//go:build windows

package termsession

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// newOSSpawner returns the native-Windows ConPTY-backed spawner. It launches
// an observer launcher inside a pseudoconsole (ConPTY, Win10 1809+) and reaps
// the whole child tree through a job object with KILL_ON_JOB_CLOSE — the
// Windows analog of the unix Setsid→kill(-pgid) tree reap. CGO-free: every
// call goes through golang.org/x/sys/windows.
func newOSSpawner() Spawner { return windowsSpawner{} }

// conPTYAvailable reports whether kernel32 exposes CreatePseudoConsole. It is
// present on Windows 10 1809 (build 17763) and later; on older Windows the
// probe fails and the launch seam stays unwired (the "Launch here" button is
// hidden — the honest fallback). Computed once.
var conPTYAvailable = sync.OnceValue(func() bool {
	return windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole").Find() == nil
})

// ptySupported reports whether this Windows host can host the embedded
// terminal. True on a ConPTY-capable host — so cmd wires the launch seam and
// the dashboard shows the "Launch here" button natively, no WSL required.
func ptySupported() bool { return conPTYAvailable() }

type windowsSpawner struct{}

// Spawn builds a pseudoconsole around a server-derived launcher argv, starts
// the process attached to it inside a kill-on-close job object, and returns a
// conPTY. On failure every allocated handle is released before returning.
func (windowsSpawner) Spawn(spec Spec) (PTY, error) {
	if !conPTYAvailable() {
		return nil, ErrPlatformUnsupported
	}

	cols, rows := spec.Cols, spec.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	// Two pipes: one feeds the child's input (we keep the write end), one
	// drains its output (we keep the read end). ConPTY takes the opposite
	// ends and holds its own references, so we close our copies of those
	// after CreatePseudoConsole.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("termsession: create input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("termsession: create output pipe: %w", err)
	}

	var hpc windows.Handle
	err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inRead, outWrite, 0, &hpc)
	// ConPTY now holds its own references to the ends it consumes.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, fmt.Errorf("termsession: create pseudoconsole: %w", err)
	}

	// cleanup releases everything allocated so far; used on every error path
	// after the ConPTY exists but before we hand ownership to a conPTY.
	cleanup := func() {
		windows.ClosePseudoConsole(hpc)
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
	}

	// STARTUPINFOEX carrying the pseudoconsole as a proc-thread attribute.
	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("termsession: alloc attribute list: %w", err)
	}
	defer attrList.Delete()
	// The pseudoconsole attribute's value IS the HPCON handle value itself
	// (MS's own ConPTY sample passes hPC directly as lpValue, not &hPC). It is
	// an opaque OS handle, not a Go memory pointer, so the uintptr→Pointer
	// conversion carries no GC-safety hazard — vet's generic "possible misuse"
	// heuristic is a false positive here. This file is windows-only; the
	// linux CI vet/lint never parses it.
	//nolint:govet // opaque handle passed as attribute value, not a heap pointer
	if err := attrList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(uintptr(hpc)),
		unsafe.Sizeof(hpc),
	); err != nil {
		cleanup()
		return nil, fmt.Errorf("termsession: set pseudoconsole attribute: %w", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrList.List()

	// Job object: TerminateJobObject (or closing the last handle) reaps the
	// whole `observer <tool>` → `<tool>` tree — the unix kill(-pgid) analog.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("termsession: create job object: %w", err)
	}
	var jeli windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	jeli.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&jeli)),
		uint32(unsafe.Sizeof(jeli)),
	); err != nil {
		_ = windows.CloseHandle(job)
		cleanup()
		return nil, fmt.Errorf("termsession: configure job object: %w", err)
	}

	// Command line from the validated, server-derived argv (never client
	// argv). ComposeCommandLine applies Windows quoting.
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(spec.argv()))
	if err != nil {
		_ = windows.CloseHandle(job)
		cleanup()
		return nil, fmt.Errorf("termsession: build command line: %w", err)
	}

	flags := uint32(windows.CREATE_SUSPENDED | windows.EXTENDED_STARTUPINFO_PRESENT)
	var env *uint16
	if spec.Env != nil {
		block, err := makeEnvBlock(spec.Env)
		if err != nil {
			_ = windows.CloseHandle(job)
			cleanup()
			return nil, fmt.Errorf("termsession: build environment: %w", err)
		}
		env = block
		flags |= windows.CREATE_UNICODE_ENVIRONMENT
	}

	// Start suspended so we can assign the child to the job BEFORE it runs —
	// no descendant can escape the tree kill. cwd nil inherits the daemon's
	// working directory (matches the unix path); the launcher resolves the
	// project dir from the session itself.
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, env, nil, &si.StartupInfo, &pi); err != nil {
		_ = windows.CloseHandle(job)
		cleanup()
		return nil, fmt.Errorf("termsession: create process for observer %s: %w", spec.Subcommand, err)
	}

	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		_ = windows.CloseHandle(job)
		cleanup()
		return nil, fmt.Errorf("termsession: assign job object: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		_ = windows.CloseHandle(job)
		cleanup()
		return nil, fmt.Errorf("termsession: resume thread: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)

	return &conPTY{
		hpc:     hpc,
		job:     job,
		process: pi.Process,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
	}, nil
}

// conPTY wraps a ConPTY (hpc) + its job object + the launcher process. The
// pipe ends are wrapped as *os.File so Read/Write get the runtime's poller
// (concurrent Read/Close is safe). Handle ownership is split to avoid any
// double-close: Wait closes the process handle (it runs exactly once, from
// the manager's waitExit); Kill terminates+closes the job; the ConPTY and
// pipes close once via teardownOnce (shared by Kill and Close).
type conPTY struct {
	hpc     windows.Handle
	job     windows.Handle
	process windows.Handle
	in      *os.File // child stdin (we write keystrokes here)
	out     *os.File // child stdout (we read terminal output here)

	killOnce     sync.Once
	teardownOnce sync.Once
	procOnce     sync.Once
}

func (p *conPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *conPTY) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *conPTY) Resize(rows, cols uint16) error {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait blocks until the launcher process exits and returns its exit code. It
// is the sole owner of the process handle close (the manager calls it exactly
// once); TerminateJobObject in Kill unblocks it by killing the process.
func (p *conPTY) Wait() (int, error) {
	defer p.procOnce.Do(func() { _ = windows.CloseHandle(p.process) })
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return -1, fmt.Errorf("termsession: wait: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return -1, fmt.Errorf("termsession: exit code: %w", err)
	}
	return int(code), nil
}

// closeMaster releases the pseudoconsole and both pipe ends exactly once.
// Closing the ConPTY signals the child EOF; closing the pipes unblocks any
// pending Read. Shared by Kill and Close.
func (p *conPTY) closeMaster() {
	p.teardownOnce.Do(func() {
		windows.ClosePseudoConsole(p.hpc)
		_ = p.in.Close()
		_ = p.out.Close()
	})
}

// Kill force-reaps the whole child tree and releases the job + master
// handles. Idempotent. The process handle itself is closed by Wait.
func (p *conPTY) Kill() error {
	p.killOnce.Do(func() {
		_ = windows.TerminateJobObject(p.job, 1)
		_ = windows.CloseHandle(p.job)
	})
	p.closeMaster()
	return nil
}

// Close releases the master handles without force-killing the child (used
// when detaching); Kill is the authoritative teardown.
func (p *conPTY) Close() error {
	p.closeMaster()
	return nil
}

// makeEnvBlock builds a double-NUL-terminated UTF-16 environment block from a
// KEY=VALUE slice, for the rare caller that overrides the child environment
// (the dashboard passes nil → inherit, so this is exercised only by direct
// callers of the Spawner).
func makeEnvBlock(env []string) (*uint16, error) {
	var buf []uint16
	for _, e := range env {
		u, err := windows.UTF16FromString(e)
		if err != nil {
			return nil, err
		}
		buf = append(buf, u...) // includes the trailing NUL
	}
	buf = append(buf, 0) // extra NUL terminates the block
	if len(buf) < 2 {
		buf = []uint16{0, 0} // an empty block is still two NULs
	}
	return &buf[0], nil
}
