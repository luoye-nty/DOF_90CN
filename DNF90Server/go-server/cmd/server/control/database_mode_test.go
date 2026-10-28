package main

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestSelectDatabaseModePreservesInstallationAndClient(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Database = databaseConfig{
		Mode: databaseModeSQLite,
		Path: "data/dnf90.db",
		Name: "dnf_local",
	}
	cfg.Client.Directory = `D:\Games\DNF`
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	controller := newController(paths, &output, &output)
	if err := controller.selectDatabaseMode(context.Background(), "mysql", false); err != nil {
		t.Fatalf("select MySQL: %v", err)
	}
	selected, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Database.Mode != databaseModePortable ||
		selected.Database.Host != "127.0.0.1" ||
		selected.Database.Port != 13306 ||
		selected.Database.User != "root" ||
		selected.Database.Password == "" {
		t.Fatalf("portable database profile = %+v", selected.Database)
	}
	if selected.InstallationID != cfg.InstallationID ||
		selected.Server.AccountID != cfg.Server.AccountID ||
		selected.Client.Directory != cfg.Client.Directory {
		t.Fatalf("mode switch changed installation identity: %+v", selected)
	}

	password := selected.Database.Password
	if err := controller.selectDatabaseMode(context.Background(), "sqlite", false); err != nil {
		t.Fatalf("select SQLite: %v", err)
	}
	selected, err = decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Database.Mode != databaseModeSQLite || selected.Database.Path != "data/dnf90.db" {
		t.Fatalf("SQLite database profile = %+v", selected.Database)
	}
	if selected.Database.Password != password {
		t.Fatal("portable MySQL credential was not retained for switching back")
	}
}

func TestNormalizeDatabaseModeAcceptsPublishedNames(t *testing.T) {
	for input, want := range map[string]string{
		"SQLite": databaseModeSQLite,
		"SQLiet": databaseModeSQLite,
		"MySQL":  databaseModePortable,
	} {
		got, err := normalizeDatabaseMode(input)
		if err != nil || got != want {
			t.Fatalf("normalizeDatabaseMode(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := normalizeDatabaseMode("unknown"); err == nil {
		t.Fatal("unknown database mode was accepted")
	}
}

func TestSelectDatabaseModeRejectsUnknownModeWithoutWriting(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.instance, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := newController(paths, &bytes.Buffer{}, &bytes.Buffer{})
	if err := controller.selectDatabaseMode(context.Background(), "unknown", false); err == nil {
		t.Fatal("unknown database mode was accepted")
	}
	data, err := os.ReadFile(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sentinel" {
		t.Fatalf("invalid selection changed instance.json: %q", data)
	}
}
