//go:build windows

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunGMToolLaunchTaskStartsServerBeforeTool(t *testing.T) {
	originalControl := launcherRunControl
	originalStart := launcherStartGMTool
	defer func() {
		launcherRunControl = originalControl
		launcherStartGMTool = originalStart
	}()
	var calls []string
	launcherRunControl = func(_ context.Context, args []string, password string) (string, error) {
		if password != "" {
			t.Fatalf("unexpected password %q", password)
		}
		calls = append(calls, "control:"+args[0])
		return "", nil
	}
	launcherStartGMTool = func() error {
		calls = append(calls, "gm")
		return nil
	}
	result := runGMToolLaunchTask()
	if !result.success || !reflect.DeepEqual(calls, []string{"control:start", "gm"}) {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
}

func TestRunGMToolLaunchTaskDoesNotOpenToolWhenServerStartFails(t *testing.T) {
	originalControl := launcherRunControl
	originalStart := launcherStartGMTool
	defer func() {
		launcherRunControl = originalControl
		launcherStartGMTool = originalStart
	}()
	started := false
	launcherRunControl = func(context.Context, []string, string) (string, error) {
		return "server failed", errors.New("exit status 1")
	}
	launcherStartGMTool = func() error {
		started = true
		return nil
	}
	result := runGMToolLaunchTask()
	if result.success || started {
		t.Fatalf("result=%+v started=%t", result, started)
	}
}
