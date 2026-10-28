//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"longheng.io/server/internal/platform/wineenv"
)

type liveProcess struct {
	running    bool
	executable string
	createdAt  time.Time
}

func inspectProcess(pid int) (liveProcess, error) {
	if pid <= 0 {
		return liveProcess{}, fmt.Errorf("invalid PID %d", pid)
	}
	handle, exists, err := openProcessForControl(
		pid,
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
	)
	if err != nil {
		return liveProcess{}, fmt.Errorf("open PID %d: %w", pid, err)
	}
	if !exists {
		return liveProcess{}, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return liveProcess{}, fmt.Errorf("query PID %d state: %w", pid, err)
	}
	if event == windows.WAIT_OBJECT_0 {
		return liveProcess{}, nil
	}
	if event != uint32(windows.WAIT_TIMEOUT) {
		return liveProcess{}, fmt.Errorf("unexpected wait state 0x%x for PID %d", event, pid)
	}
	executable, err := processExecutablePath(handle)
	if err != nil {
		if exited, waitErr := processHandleExitedWithin(handle, 1_000); waitErr == nil && exited {
			return liveProcess{}, nil
		}
		return liveProcess{}, fmt.Errorf("query PID %d executable: %w", pid, err)
	}
	var creationTime windows.Filetime
	var exitTime windows.Filetime
	var kernelTime windows.Filetime
	var userTime windows.Filetime
	if err := windows.GetProcessTimes(
		handle,
		&creationTime,
		&exitTime,
		&kernelTime,
		&userTime,
	); err != nil {
		if exited, waitErr := processHandleExitedWithin(handle, 1_000); waitErr == nil && exited {
			return liveProcess{}, nil
		}
		return liveProcess{}, fmt.Errorf("query PID %d creation time: %w", pid, err)
	}
	return liveProcess{
		running:    true,
		executable: executable,
		createdAt:  time.Unix(0, creationTime.Nanoseconds()).UTC(),
	}, nil
}

func forceTerminateProcess(pid int, expectedExecutable string, expectedCreatedAt time.Time) error {
	handle, exists, err := openProcessForControl(
		pid,
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
	)
	if err != nil {
		return fmt.Errorf("open PID %d for termination: %w", pid, err)
	}
	if !exists {
		return nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	actual, err := processExecutablePath(handle)
	if err != nil {
		if exited, waitErr := processHandleExitedWithin(handle, 1_000); waitErr == nil && exited {
			return nil
		}
		return fmt.Errorf("query PID %d executable before termination: %w", pid, err)
	}
	if !sameExecutable(actual, expectedExecutable) {
		return fmt.Errorf(
			"refusing to terminate PID %d: executable %s does not match %s",
			pid,
			actual,
			expectedExecutable,
		)
	}
	if !expectedCreatedAt.IsZero() {
		var creationTime windows.Filetime
		var exitTime windows.Filetime
		var kernelTime windows.Filetime
		var userTime windows.Filetime
		if err := windows.GetProcessTimes(
			handle,
			&creationTime,
			&exitTime,
			&kernelTime,
			&userTime,
		); err != nil {
			if exited, waitErr := processHandleExitedWithin(handle, 1_000); waitErr == nil && exited {
				return nil
			}
			return fmt.Errorf("query PID %d creation time before termination: %w", pid, err)
		}
		actualCreatedAt := time.Unix(0, creationTime.Nanoseconds()).UTC()
		if !actualCreatedAt.Equal(expectedCreatedAt) {
			return fmt.Errorf(
				"refusing to terminate PID %d: creation time %s does not match %s",
				pid,
				actualCreatedAt.Format(time.RFC3339Nano),
				expectedCreatedAt.Format(time.RFC3339Nano),
			)
		}
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		if exited, waitErr := processHandleExitedWithin(handle, 1_000); waitErr == nil && exited {
			return nil
		}
		return fmt.Errorf("terminate PID %d: %w", pid, err)
	}
	event, err := windows.WaitForSingleObject(handle, 10_000)
	if err != nil {
		return fmt.Errorf("wait for terminated PID %d: %w", pid, err)
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf(
			"terminated PID %d did not exit within 10 seconds (wait state 0x%x)",
			pid,
			event,
		)
	}
	return nil
}

func processExecutablePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	if wineenv.Detected() {
		// Wine implements QueryFullProcessImageName from the server handle's
		// process identity, while PSAPI module enumeration may require VM_READ
		// or fail for a process backed by a Unix loader. Prefer the limited-right
		// API so START/STATUS/STOP retain executable ownership checks.
		if path, err := queryFullProcessImageName(handle, buffer); err == nil {
			return path, nil
		}
	}
	// QueryFullProcessImageName returned one trailing non-path code unit for the
	// managed server on this Windows build. The primary module path is the same
	// authoritative executable identity and is NUL-terminated by this API.
	if err := windows.GetModuleFileNameEx(handle, 0, &buffer[0], uint32(len(buffer))); err == nil {
		if path := windows.UTF16ToString(buffer); path != "" {
			return path, nil
		}
	}

	return queryFullProcessImageName(handle, buffer)
}

func queryFullProcessImageName(handle windows.Handle, buffer []uint16) (string, error) {
	if len(buffer) == 0 {
		return "", errors.New("process executable path buffer is empty")
	}
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	path := windows.UTF16ToString(buffer[:size])
	if path == "" {
		return "", errors.New("empty process executable path")
	}
	return path, nil
}

func processHandleExited(handle windows.Handle) (bool, error) {
	return processHandleExitedWithin(handle, 0)
}

func processHandleExitedWithin(
	handle windows.Handle,
	timeoutMilliseconds uint32,
) (bool, error) {
	event, err := windows.WaitForSingleObject(handle, timeoutMilliseconds)
	if err != nil {
		return false, err
	}
	switch event {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait state 0x%x", event)
	}
}

func processParentPID(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid PID %d", pid)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("snapshot processes for PID %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, fmt.Errorf("read process snapshot for PID %d: %w", pid, err)
	}
	for {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return 0, fmt.Errorf("scan process snapshot for PID %d: %w", pid, err)
		}
	}
	return 0, fmt.Errorf("PID %d was not found in the process snapshot", pid)
}

func processExistsInSnapshot(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid PID %d", pid)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, fmt.Errorf("snapshot processes for PID %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false, fmt.Errorf("read process snapshot for PID %d: %w", pid, err)
	}
	for {
		if int(entry.ProcessID) == pid {
			return true, nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return false, nil
			}
			return false, fmt.Errorf("scan process snapshot for PID %d: %w", pid, err)
		}
	}
}

func openProcessForControl(
	pid int,
	desiredAccess uint32,
) (windows.Handle, bool, error) {
	const (
		attempts  = 21
		retryWait = 25 * time.Millisecond
	)
	for attempt := 0; attempt < attempts; attempt++ {
		access := desiredAccess | windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ
		if wineenv.Detected() {
			// QueryFullProcessImageName and GetProcessTimes need only the limited
			// rights already requested by callers. Wine can reject VM_READ for a
			// perfectly controllable process, so do not turn that optional PSAPI
			// capability into a lifecycle failure.
			access = desiredAccess
		}
		handle, err := windows.OpenProcess(
			access,
			false,
			uint32(pid),
		)
		if err == nil {
			return handle, true, nil
		}
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, false, nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return 0, false, err
		}
		exists, snapshotErr := processExistsInSnapshot(pid)
		if snapshotErr != nil {
			return 0, false, errors.Join(err, snapshotErr)
		}
		if !exists {
			return 0, false, nil
		}
		if attempt+1 < attempts {
			time.Sleep(retryWait)
			continue
		}
		return 0, true, err
	}
	panic("unreachable")
}

func configureServerProcess(cmd *exec.Cmd) {
	configureBackgroundProcess(cmd)
}

func configureBackgroundProcess(cmd *exec.Cmd) {
	creationFlags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	if wineenv.Detected() {
		// Older Wine releases reject CREATE_NO_WINDOW for some PE console
		// processes. Redirected stdout/stderr already keeps managed processes
		// non-interactive, so the process-group boundary is sufficient here.
		creationFlags = windows.CREATE_NEW_PROCESS_GROUP
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: creationFlags,
	}
}

func configureClientProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func sameExecutable(actual, expected string) bool {
	actualAbs, actualErr := filepath.Abs(actual)
	expectedAbs, expectedErr := filepath.Abs(expected)
	if actualErr != nil || expectedErr != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(actualAbs), filepath.Clean(expectedAbs))
}
