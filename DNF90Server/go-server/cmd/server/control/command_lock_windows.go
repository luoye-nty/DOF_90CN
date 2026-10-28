//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"longheng.io/server/internal/platform/wineenv"
)

func acquireControllerLock(ctx context.Context, projectRoot string) (func(), error) {
	if wineenv.Detected() {
		return acquireWineControllerLock(ctx, projectRoot)
	}
	lockDirectory := filepath.Join(projectRoot, "runtime", "state")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create controller lock directory: %w", err)
	}
	lockPath := filepath.Join(lockDirectory, "control.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open controller lock file: %w", err)
	}
	handle := windows.Handle(file.Fd())
	overlapped := &windows.Overlapped{}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	for {
		err = windows.LockFileEx(handle, flags, 0, 1, 0, overlapped)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
					_ = file.Close()
				})
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("lock controller state: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for another DNF90 command: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func acquireWineControllerLock(ctx context.Context, projectRoot string) (func(), error) {
	// Win32 mutex ownership belongs to an OS thread, not a goroutine. Keep the
	// controller's CLI goroutine on the acquiring thread until the deferred
	// release callback runs.
	runtime.LockOSThread()
	identity := strings.ToLower(filepath.Clean(projectRoot))
	digest := sha256.Sum256([]byte(identity))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(
		`DNF90Control-%x`,
		digest[:16],
	))
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create Wine controller mutex name: %w", err)
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create Wine controller mutex: %w", err)
	}
	for {
		event, waitErr := windows.WaitForSingleObject(mutex, 250)
		if waitErr != nil {
			_ = windows.CloseHandle(mutex)
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("wait for Wine controller mutex: %w", waitErr)
		}
		switch event {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = windows.ReleaseMutex(mutex)
					_ = windows.CloseHandle(mutex)
					runtime.UnlockOSThread()
				})
			}, nil
		case uint32(windows.WAIT_TIMEOUT):
			select {
			case <-ctx.Done():
				_ = windows.CloseHandle(mutex)
				runtime.UnlockOSThread()
				return nil, fmt.Errorf("wait for another DNF90 command: %w", ctx.Err())
			default:
			}
		default:
			_ = windows.CloseHandle(mutex)
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("unexpected Wine controller mutex wait state 0x%x", event)
		}
	}
}
