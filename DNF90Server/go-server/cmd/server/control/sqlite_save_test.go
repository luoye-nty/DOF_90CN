package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizeSQLiteSaveCheckpointsAndVerifiesDatabase(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Database = databaseConfig{Mode: "sqlite", Path: "data/dnf90.db", Name: "dnf_local"}
	path := filepath.Join(paths.runtimeRoot, "data", "dnf90.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE save_test (value TEXT NOT NULL)`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO save_test (value) VALUES ('latest')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	gotPath, err := finalizeSQLiteSave(context.Background(), paths, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Fatalf("save path = %q, want %q", gotPath, path)
	}
	if walInfo, err := os.Stat(path + "-wal"); err == nil && walInfo.Size() > 0 {
		t.Fatalf("WAL still contains %d bytes", walInfo.Size())
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	verified, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	var value string
	if err := verified.QueryRow(`SELECT value FROM save_test`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "latest" {
		t.Fatalf("saved value = %q, want latest", value)
	}
}

func TestSQLiteSavePathRejectsEscape(t *testing.T) {
	paths := newProjectPaths(t.TempDir())
	cfg := validTestInstance()
	cfg.Database = databaseConfig{Mode: "sqlite", Path: "../outside.db", Name: "dnf_local"}
	if _, err := sqliteSavePath(paths, cfg); err == nil {
		t.Fatal("escaping SQLite save path was accepted")
	}
}
