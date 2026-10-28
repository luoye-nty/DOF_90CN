package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestRemoteClientIPUsesSSHConnectionSource(t *testing.T) {
	got, err := remoteClientIP("10.66.0.23 52144 10.66.0.1 22")
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.66.0.23" {
		t.Fatalf("remote IP=%q", got)
	}
}

func TestRemoteClientIPRejectsUntrustedAddresses(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "203.0.113.8", "2001:db8::1"} {
		if _, err := remoteClientIP(value + " 52144 10.66.0.1 22"); err == nil {
			t.Fatalf("remoteClientIP(%q) succeeded", value)
		}
	}
}

func TestNormalizeLocalUsername(t *testing.T) {
	tests := map[string]string{
		" Alice ": "alice",
		"测试账号":    "测试账号",
		"user-1":  "user-1",
	}
	for input, want := range tests {
		got, err := normalizeLocalUsername(input)
		if err != nil {
			t.Fatalf("normalizeLocalUsername(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeLocalUsername(%q) = %q, want %q", input, got, want)
		}
	}
	for _, invalid := range []string{"ab", "bad name", "bad/name", strings.Repeat("a", 33)} {
		if _, err := normalizeLocalUsername(invalid); err == nil {
			t.Fatalf("normalizeLocalUsername(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestSQLiteLocalAccountRegistrationAndAuthentication(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Database = databaseConfig{Mode: "sqlite", Path: "data/dnf90.db", Name: "dnf_local"}
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	controller := newController(paths, &stdout, &stdout)
	ctx := context.Background()
	if err := controller.registerLocalLoginAccount(ctx, "Alice", "correct-password", false); err != nil {
		t.Fatalf("register SQLite account: %v", err)
	}
	accountID, err := controller.authenticateRunningLocalAccount(ctx, cfg, "alice", "correct-password")
	if err != nil {
		t.Fatalf("authenticate SQLite account: %v", err)
	}
	if accountID != cfg.Server.AccountID {
		t.Fatalf("authenticated account = %q, want %q", accountID, cfg.Server.AccountID)
	}
	if _, err := controller.authenticateRunningLocalAccount(ctx, cfg, "alice", "wrong-password"); err == nil {
		t.Fatal("wrong SQLite password was accepted")
	}
	if !isRegularFile(filepath.Join(paths.runtimeRoot, filepath.FromSlash(cfg.Database.Path))) {
		t.Fatal("SQLite database file was not created")
	}
}

func TestLocalPasswordUsesBcryptAndRejectsUnsafeLengths(t *testing.T) {
	password := "correct-password"
	if err := validateLocalPassword(password); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte("wrong-password")); err == nil {
		t.Fatal("wrong password matched bcrypt hash")
	}
	for _, invalid := range []string{"short", strings.Repeat("x", 73), "bad\npassword"} {
		if err := validateLocalPassword(invalid); err == nil {
			t.Fatalf("validateLocalPassword(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestWriteActiveLocalAccountIDPreservesOtherInstanceFields(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := os.MkdirAll(filepath.Dir(paths.instance), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Server.AdvertiseIP = autoDetectAdvertiseIP
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeActiveLocalAccountID(paths, "dnf:account-two"); err != nil {
		t.Fatal(err)
	}
	updated, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Server.AccountID != "dnf:account-two" {
		t.Fatalf("active account = %q", updated.Server.AccountID)
	}
	if updated.Server.AdvertiseIP != autoDetectAdvertiseIP {
		t.Fatalf("advertise ip changed to %q", updated.Server.AdvertiseIP)
	}
	if updated.Database != cfg.Database || updated.Protocol != cfg.Protocol {
		t.Fatal("active account update changed unrelated instance configuration")
	}
}
