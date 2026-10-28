package main

import (
	"context"
	"reflect"
	"testing"
)

func TestAuthenticatedClientLaunchDoesNotStopOrSwitchServerAccount(t *testing.T) {
	original := launcherRunControl
	t.Cleanup(func() {
		launcherRunControl = original
	})

	type call struct {
		args  []string
		stdin string
	}
	var calls []call
	launcherRunControl = func(
		_ context.Context,
		args []string,
		stdin string,
	) (string, error) {
		calls = append(calls, call{
			args:  append([]string(nil), args...),
			stdin: stdin,
		})
		return "", nil
	}

	if _, err := runAuthenticatedClientLaunch(
		context.Background(),
		"second-account",
		"second-password",
	); err != nil {
		t.Fatalf("runAuthenticatedClientLaunch: %v", err)
	}

	want := []call{
		{args: []string{"start"}},
		{
			args: []string{
				"launch-client",
				"--multi-instance",
				"--username",
				"second-account",
				"--password-stdin",
			},
			stdin: "second-password",
		},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("control calls = %#v, want %#v", calls, want)
	}
}

func TestPrepareLauncherDatabaseRunsBeforeServerStart(t *testing.T) {
	originalRunControl := launcherRunControl
	originalMode := globalLauncher.databaseMode
	t.Cleanup(func() {
		launcherRunControl = originalRunControl
		globalLauncher.databaseMode = originalMode
	})

	type call struct {
		args  []string
		stdin string
	}
	var calls []call
	launcherRunControl = func(
		_ context.Context,
		args []string,
		stdin string,
	) (string, error) {
		calls = append(calls, call{args: append([]string(nil), args...), stdin: stdin})
		return "", nil
	}
	globalLauncher.databaseMode = "mysql"
	result := runLauncherTask(true, "test", "test123456", false, "unused")
	if !result.success {
		t.Fatalf("login task failed: %+v", result)
	}
	want := []call{
		{args: []string{"database", "select", "--mode", "mysql"}},
		{args: []string{"start"}},
		{
			args: []string{
				"launch-client", "--multi-instance", "--username", "test", "--password-stdin",
			},
			stdin: "test123456",
		},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("control calls = %#v, want %#v", calls, want)
	}
}

func TestParseLauncherDatabaseMode(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: nil, want: ""},
		{args: []string{"--database", "MySQL"}, want: "mysql"},
		{args: []string{"--database", "SQLiet"}, want: "sqlite"},
	} {
		got, err := parseLauncherDatabaseMode(test.args)
		if err != nil || got != test.want {
			t.Fatalf("parseLauncherDatabaseMode(%v) = %q, %v", test.args, got, err)
		}
	}
	if _, err := parseLauncherDatabaseMode([]string{"--database", "bad"}); err == nil {
		t.Fatal("invalid database mode was accepted")
	}
}
