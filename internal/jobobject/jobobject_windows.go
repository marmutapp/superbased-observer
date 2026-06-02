//go:build windows

package jobobject

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AttachProcess creates an anonymous Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and assigns cmd's underlying
// process to it. cmd MUST already have been Start()'d — Process is
// nil otherwise.
//
// The returned Closer holds the Job Object handle. When the calling
// observer process exits (cleanly OR otherwise), Windows
// automatically closes all its handles; KILL_ON_JOB_CLOSE then fires
// for the codex.exe child. Callers should also defer Close() so a
// graceful exit closes the handle promptly rather than waiting for
// process tear-down.
//
// Returns an error when the Job Object can't be created, the limit
// can't be set, or assigning the process to the job fails. The
// caller's exec.Cmd is unaffected — a returned error means the V7-1
// protection is absent for this spawn but the child is still running.
//
// V7-1.
func AttachProcess(cmd *exec.Cmd) (io.Closer, error) {
	if cmd == nil {
		return nil, errors.New("jobobject.AttachProcess: cmd is nil")
	}
	if cmd.Process == nil {
		return nil, errors.New("jobobject.AttachProcess: cmd.Process is nil — call cmd.Start() first")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("jobobject.AttachProcess: CreateJobObject: %w", err)
	}
	closer := &jobHandle{handle: job}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = closer.Close()
		return nil, fmt.Errorf("jobobject.AttachProcess: SetInformationJobObject: %w", err)
	}

	// cmd.Process.Pid is sufficient — OpenProcess + AssignProcessToJobObject
	// requires PROCESS_SET_QUOTA + PROCESS_TERMINATE rights, but
	// Windows.AssignProcessToJobObject takes an HANDLE that the runtime
	// can derive from PID via the documented OpenProcess flags.
	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = closer.Close()
		return nil, fmt.Errorf("jobobject.AttachProcess: OpenProcess(pid=%d): %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(processHandle) //nolint:errcheck // best-effort

	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		_ = closer.Close()
		return nil, fmt.Errorf("jobobject.AttachProcess: AssignProcessToJobObject: %w", err)
	}
	return closer, nil
}

// jobHandle owns the Job Object handle and closes it on demand.
// Closing the handle is what fires KILL_ON_JOB_CLOSE on every
// assigned process — so callers wanting an explicit kill (vs.
// waiting for process exit) can call Close() directly.
type jobHandle struct {
	handle windows.Handle
}

func (j *jobHandle) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	if err != nil {
		return fmt.Errorf("jobobject.Close: CloseHandle: %w", err)
	}
	return nil
}
