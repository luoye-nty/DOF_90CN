package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func sqliteSavePath(paths projectPaths, cfg instanceConfig) (string, error) {
	relative := filepath.Clean(filepath.FromSlash(strings.TrimSpace(cfg.Database.Path)))
	if relative == "." || filepath.IsAbs(relative) || escapesRoot(relative) {
		return "", fmt.Errorf("invalid SQLite save path %q", cfg.Database.Path)
	}
	absolute, err := filepath.Abs(filepath.Join(paths.runtimeRoot, relative))
	if err != nil {
		return "", fmt.Errorf("resolve SQLite save path: %w", err)
	}
	if !pathInside(paths.runtimeRoot, absolute) {
		return "", fmt.Errorf("SQLite save path escapes runtime: %s", absolute)
	}
	return absolute, nil
}

func finalizeSQLiteSave(ctx context.Context, paths projectPaths, cfg instanceConfig) (string, error) {
	path, err := sqliteSavePath(paths, cfg)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("open SQLite save %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("SQLite save is not a regular file: %s", path)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkpointCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return "", fmt.Errorf("open SQLite save %s: %w", path, err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()
	for _, pragma := range []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA synchronous = FULL`,
	} {
		if _, err := database.ExecContext(checkpointCtx, pragma); err != nil {
			return "", fmt.Errorf("configure SQLite save finalization: %w", err)
		}
	}
	var busy, logFrames, checkpointedFrames int
	if err := database.QueryRowContext(checkpointCtx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy,
		&logFrames,
		&checkpointedFrames,
	); err != nil {
		return "", fmt.Errorf("checkpoint SQLite save: %w", err)
	}
	if busy != 0 {
		return "", fmt.Errorf(
			"SQLite save checkpoint remained busy (log_frames=%d checkpointed_frames=%d)",
			logFrames,
			checkpointedFrames,
		)
	}
	var integrity string
	if err := database.QueryRowContext(checkpointCtx, `PRAGMA integrity_check(1)`).Scan(&integrity); err != nil {
		return "", fmt.Errorf("check SQLite save integrity: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(integrity), "ok") {
		return "", fmt.Errorf("SQLite save integrity check returned %q", integrity)
	}
	if err := database.Close(); err != nil {
		return "", fmt.Errorf("close SQLite save after checkpoint: %w", err)
	}
	if walInfo, statErr := os.Stat(path + "-wal"); statErr == nil && walInfo.Size() > 0 {
		return "", fmt.Errorf("SQLite WAL still contains %d bytes after checkpoint: %s", walInfo.Size(), path+"-wal")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect SQLite WAL after checkpoint: %w", statErr)
	}
	return path, nil
}
