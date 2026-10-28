package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"

	mysql "github.com/go-sql-driver/mysql"
)

func ensureExternalMySQLDatabase(ctx context.Context, cfg instanceConfig) error {
	if cfg.Database.Mode != databaseModeMySQL {
		return fmt.Errorf("external MySQL setup requires database.mode=mysql")
	}
	serverConfig := mysql.NewConfig()
	serverConfig.User = cfg.Database.User
	serverConfig.Passwd = cfg.Database.Password
	serverConfig.Net = "tcp"
	serverConfig.Addr = net.JoinHostPort(cfg.Database.Host, strconv.Itoa(cfg.Database.Port))
	serverConfig.ParseTime = true
	serverConfig.Collation = "utf8mb4_unicode_ci"
	db, err := sql.Open("mysql", serverConfig.FormatDSN())
	if err != nil {
		return fmt.Errorf("open external MySQL: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("external MySQL is not reachable on loopback: %w", err)
	}
	if _, err := db.ExecContext(
		ctx,
		"CREATE DATABASE IF NOT EXISTS `"+cfg.Database.Name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	); err != nil {
		return fmt.Errorf("create external MySQL database %s: %w", cfg.Database.Name, err)
	}
	selectedConfig := serverConfig.Clone()
	selectedConfig.DBName = cfg.Database.Name
	selected, err := sql.Open("mysql", selectedConfig.FormatDSN())
	if err != nil {
		return fmt.Errorf("open external MySQL database %s: %w", cfg.Database.Name, err)
	}
	defer selected.Close()
	if err := selected.PingContext(ctx); err != nil {
		return fmt.Errorf("select external MySQL database %s: %w", cfg.Database.Name, err)
	}
	return nil
}
