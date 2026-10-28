//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProcessExistsInSnapshot(t *testing.T) {
	exists, err := processExistsInSnapshot(os.Getpid())
	if err != nil {
		t.Fatalf("processExistsInSnapshot(current) error = %v", err)
	}
	if !exists {
		t.Fatal("current process is absent from the process snapshot")
	}

	exists, err = processExistsInSnapshot(int(^uint32(0)))
	if err != nil {
		t.Fatalf("processExistsInSnapshot(impossible) error = %v", err)
	}
	if exists {
		t.Fatal("impossible PID appears in the process snapshot")
	}
}

func TestProcessExecutablePathUsesCurrentMainModule(t *testing.T) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false,
		uint32(os.Getpid()),
	)
	if err != nil {
		t.Fatalf("open current process: %v", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	got, err := processExecutablePath(handle)
	if err != nil {
		t.Fatalf("processExecutablePath(current) error = %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("current executable path: %v", err)
	}
	if !sameExecutable(got, want) {
		t.Fatalf("process executable = %q, want %q", got, want)
	}
}

func TestManagedServerExecutableMatchesWindowsReplacementRename(t *testing.T) {
	directory := t.TempDir()
	expected := filepath.Join(directory, "DNF90Server.exe")
	renamed := expected + "~"
	content := []byte("verified managed server image")
	if err := os.WriteFile(expected, content, 0o600); err != nil {
		t.Fatalf("write expected executable: %v", err)
	}
	if err := os.WriteFile(renamed, content, 0o600); err != nil {
		t.Fatalf("write renamed executable: %v", err)
	}
	digest, err := fileSHA256(renamed)
	if err != nil {
		t.Fatalf("hash renamed executable: %v", err)
	}

	matched, err := managedServerExecutableMatches(renamed, expected, digest)
	if err != nil || !matched {
		t.Fatalf("renamed executable match = %t, %v; want true, nil", matched, err)
	}
	matched, err = managedServerExecutableMatches(expected, expected, digest)
	if err != nil || !matched {
		t.Fatalf("exact executable match = %t, %v; want true, nil", matched, err)
	}
	for _, test := range []struct {
		name   string
		actual string
		digest string
	}{
		{name: "wrong digest", actual: renamed, digest: "00"},
		{name: "empty digest", actual: renamed, digest: ""},
		{name: "backup suffix", actual: expected + ".previous-good", digest: digest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "backup suffix" {
				if err := os.WriteFile(test.actual, content, 0o600); err != nil {
					t.Fatalf("write backup executable: %v", err)
				}
			}
			matched, err := managedServerExecutableMatches(test.actual, expected, test.digest)
			if err != nil {
				t.Fatalf("managed executable check: %v", err)
			}
			if matched {
				t.Fatal("unexpected managed executable match")
			}
		})
	}
}
