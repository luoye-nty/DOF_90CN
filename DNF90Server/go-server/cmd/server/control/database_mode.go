package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

const (
	databaseModeSQLite   = "sqlite"
	databaseModePortable = "portable"
	databaseModeMySQL    = "mysql"
)

func (c *controller) database(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("database subcommand is required: select")
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "select":
		fs := newFlagSet("database select", c.stderr)
		mode := fs.String("mode", "", "database mode: sqlite or mysql")
		prepare := fs.Bool("prepare", false, "extract the bundled portable MySQL files after selecting MySQL")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return c.selectDatabaseMode(ctx, *mode, *prepare)
	case "configure-mysql":
		fs := newFlagSet("database configure-mysql", c.stderr)
		port := fs.Int("port", 3306, "loopback MySQL TCP port")
		name := fs.String("name", "dnf_local", "MySQL database name")
		passwordStdin := fs.Bool("password-stdin", false, "read the MySQL root password from stdin")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		if !*passwordStdin {
			return errors.New("database configure-mysql requires --password-stdin")
		}
		password, err := readLocalAccountPassword(c.stdin)
		if err != nil {
			return err
		}
		return c.configureExternalMySQL(ctx, *port, *name, password)
	default:
		return fmt.Errorf("unknown database subcommand %q", args[0])
	}
}

func normalizeDatabaseMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case databaseModeSQLite, "sqliet":
		return databaseModeSQLite, nil
	case databaseModePortable:
		return databaseModePortable, nil
	case databaseModeMySQL:
		if runtime.GOOS == "windows" {
			return databaseModePortable, nil
		}
		return databaseModeMySQL, nil
	default:
		return "", fmt.Errorf("database mode must be sqlite or mysql, got %q", value)
	}
}

// selectDatabaseMode is the single mode-switch boundary used by the Windows
// launcher and installer. It preserves installation identity, account
// namespace, client directory and all protocol settings while changing only
// the repository profile. A live server is stopped before an actual mode
// change so generated repository configuration can never disagree with the
// process that owns the game ports.
func (c *controller) selectDatabaseMode(
	ctx context.Context,
	requested string,
	prepare bool,
) error {
	targetMode, err := normalizeDatabaseMode(requested)
	if err != nil {
		return err
	}
	current, err := loadInstance(c.paths)
	if err != nil {
		return err
	}
	raw, err := decodeInstance(c.paths.instance)
	if err != nil {
		return err
	}

	if current.Database.Mode != targetMode {
		_, serverRunning, inspectErr := c.loadManagedProcess(current.InstallationID, true)
		if inspectErr != nil {
			return inspectErr
		}
		// Portable MySQL can remain alive even when the game server is down, so
		// always run the portable stop path before leaving that mode. SQLite
		// needs a checkpoint only when its server was actually running.
		if serverRunning || current.Database.Mode == databaseModePortable {
			if err := c.stop(ctx, false); err != nil {
				return fmt.Errorf("stop current database profile before switching: %w", err)
			}
		}
	}

	raw.Database.Mode = targetMode
	switch targetMode {
	case databaseModeSQLite:
		if strings.TrimSpace(raw.Database.Path) == "" {
			raw.Database.Path = "data/dnf90.db"
		}
	case databaseModePortable:
		raw.Database.Host = "127.0.0.1"
		raw.Database.Port = 13306
		if strings.TrimSpace(raw.Database.Name) == "" {
			raw.Database.Name = "dnf_local"
		}
		raw.Database.User = "root"
		if strings.TrimSpace(raw.Database.Password) == "" || raw.Database.Password == "AUTO_GENERATE" {
			raw.Database.Password, err = randomCredential("db_")
			if err != nil {
				return err
			}
		}
	case databaseModeMySQL:
		raw.Database.Host = "127.0.0.1"
		raw.Database.Port = 3306
		if strings.TrimSpace(raw.Database.Name) == "" {
			raw.Database.Name = "dnf_local"
		}
		raw.Database.User = "root"
		if strings.TrimSpace(raw.Database.Password) == "" || raw.Database.Password == "AUTO_GENERATE" {
			raw.Database.Password, err = randomCredential("db_")
			if err != nil {
				return err
			}
		}
	}

	validated := raw
	if strings.EqualFold(strings.TrimSpace(validated.Server.AdvertiseIP), autoDetectAdvertiseIP) {
		validated.Server.AdvertiseIP = current.Server.AdvertiseIP
	}
	if err := validateInstance(validated); err != nil {
		return fmt.Errorf("validate selected database profile: %w", err)
	}
	data, err := marshalInstance(raw)
	if err != nil {
		return err
	}
	if err := writeFile(c.paths.instance, data, 0o600); err != nil {
		return err
	}

	if prepare && targetMode == databaseModePortable {
		if err := preparePortableMySQL(ctx, c.paths, c.stdout); err != nil {
			return fmt.Errorf("install bundled portable MySQL: %w", err)
		}
	}
	label := "SQLite"
	if targetMode == databaseModePortable || targetMode == databaseModeMySQL {
		label = "MySQL"
	}
	fmt.Fprintf(c.stdout, "Database profile selected: %s\n", label)
	return nil
}

func (c *controller) configureExternalMySQL(
	ctx context.Context,
	port int,
	name string,
	password string,
) error {
	if runtime.GOOS == "windows" {
		return errors.New("external system MySQL configuration is available only in the Linux release")
	}
	cfg, err := loadInstance(c.paths)
	if err != nil {
		return err
	}
	if _, found, err := c.loadManagedProcess(cfg.InstallationID, true); err != nil {
		return err
	} else if found {
		return errors.New("stop DNF90 before changing the MySQL profile")
	}
	raw, err := decodeInstance(c.paths.instance)
	if err != nil {
		return err
	}
	raw.Database.Mode = databaseModeMySQL
	raw.Database.Host = "127.0.0.1"
	raw.Database.Port = port
	raw.Database.Name = strings.TrimSpace(name)
	raw.Database.User = "root"
	raw.Database.Password = password
	validated := raw
	if strings.EqualFold(strings.TrimSpace(validated.Server.AdvertiseIP), autoDetectAdvertiseIP) {
		validated.Server.AdvertiseIP = cfg.Server.AdvertiseIP
	}
	if err := validateInstance(validated); err != nil {
		return err
	}
	if err := ensureExternalMySQLDatabase(ctx, validated); err != nil {
		return err
	}
	data, err := marshalInstance(raw)
	if err != nil {
		return err
	}
	if err := writeFile(c.paths.instance, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "External loopback MySQL configured: 127.0.0.1:%d/%s\n", port, raw.Database.Name)
	return nil
}
