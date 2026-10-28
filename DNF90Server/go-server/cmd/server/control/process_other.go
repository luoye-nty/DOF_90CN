//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type liveProcess struct {
	running    bool
	executable string
	createdAt  time.Time
}

func inspectProcess(pid int) (liveProcess, error) {
	if runtime.GOOS != "linux" {
		return liveProcess{}, fmt.Errorf("PID executable verification is unsupported on %s (PID %d)", runtime.GOOS, pid)
	}
	if pid <= 0 {
		return liveProcess{}, fmt.Errorf("invalid PID %d", pid)
	}
	stat, err := readLinuxProcessStat(pid)
	if errors.Is(err, os.ErrNotExist) {
		return liveProcess{}, nil
	}
	if err != nil {
		return liveProcess{}, err
	}
	if stat.state == "Z" || stat.state == "X" {
		return liveProcess{}, nil
	}
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if errors.Is(err, os.ErrNotExist) {
		return liveProcess{}, nil
	}
	if err != nil {
		return liveProcess{}, fmt.Errorf("read PID %d executable: %w", pid, err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return liveProcess{}, fmt.Errorf("resolve PID %d executable: %w", pid, err)
	}
	return liveProcess{
		running:    true,
		executable: filepath.Clean(executable),
		createdAt:  linuxProcessIdentity(stat.startTicks),
	}, nil
}

func forceTerminateProcess(pid int, expectedExecutable string, expectedCreatedAt time.Time) error {
	live, err := inspectProcess(pid)
	if err != nil {
		return err
	}
	if !live.running {
		return nil
	}
	if !sameExecutable(live.executable, expectedExecutable) || !live.createdAt.Equal(expectedCreatedAt) {
		return fmt.Errorf("refusing to terminate PID %d because its verified identity changed", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find PID %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate PID %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, inspectErr := inspectProcess(pid)
		if inspectErr != nil {
			return inspectErr
		}
		if !current.running {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("PID %d did not exit after SIGKILL", pid)
}

func processParentPID(pid int) (int, error) {
	stat, err := readLinuxProcessStat(pid)
	if err != nil {
		return 0, err
	}
	return stat.parentPID, nil
}

func configureServerProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func configureBackgroundProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func configureClientProcess(*exec.Cmd) {}

func sameExecutable(actual, expected string) bool {
	actualAbs, actualErr := filepath.Abs(actual)
	expectedAbs, expectedErr := filepath.Abs(expected)
	if actualErr != nil || expectedErr != nil {
		return false
	}
	return filepath.Clean(actualAbs) == filepath.Clean(expectedAbs)
}

type linuxProcessStat struct {
	state      string
	parentPID  int
	startTicks uint64
}

func readLinuxProcessStat(pid int) (linuxProcessStat, error) {
	path := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return linuxProcessStat{}, os.ErrNotExist
		}
		return linuxProcessStat{}, fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	closing := strings.LastIndexByte(text, ')')
	if closing < 0 || closing+2 >= len(text) {
		return linuxProcessStat{}, fmt.Errorf("parse %s: malformed process stat", path)
	}
	fields := strings.Fields(text[closing+1:])
	// fields starts at proc(5) field 3 (state). Linux field 22 is starttime.
	if len(fields) <= 19 {
		return linuxProcessStat{}, fmt.Errorf("parse %s: process stat has %d trailing fields", path, len(fields))
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return linuxProcessStat{}, fmt.Errorf("parse %s parent PID: %w", path, err)
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return linuxProcessStat{}, fmt.Errorf("parse %s start identity: %w", path, err)
	}
	return linuxProcessStat{state: fields[0], parentPID: parentPID, startTicks: startTicks}, nil
}

// A Linux /proc start-tick value is already unique for a PID over the lifetime
// of one boot. Store it in time.Time so the existing cross-platform process
// state can compare an exact, stable identity without relying on clock rounding.
func linuxProcessIdentity(startTicks uint64) time.Time {
	return time.Unix(0, int64(startTicks)).UTC()
}
