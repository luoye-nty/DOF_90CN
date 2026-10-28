package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	mysqldriver "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	mysqlrepo "longheng.io/server/internal/modules/dnf/repository/mysql"
	sqliterepo "longheng.io/server/internal/modules/dnf/repository/sqlite"
)

const (
	bundleSchemaVersion = 1
	loginAccountTable   = "dnf_login_accounts"
)

var sqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type options struct {
	root        string
	output      string
	accountID   string
	characterID string
	username    string
	passwordIn  bool
}

type instanceFile struct {
	Database struct {
		Mode     string `json:"mode"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Name     string `json:"name"`
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"database"`
	Game struct {
		ShardID string `json:"shardId"`
	} `json:"game"`
}

type bundleInfo struct {
	SchemaVersion  int       `json:"schemaVersion"`
	GeneratedAt    time.Time `json:"generatedAt"`
	Username       string    `json:"username"`
	AccountID      string    `json:"accountId"`
	CharacterID    string    `json:"characterId"`
	CharacterName  string    `json:"characterName"`
	CharacterLevel int       `json:"characterLevel"`
	AccountCount   int       `json:"accountCount"`
	CharacterCount int       `json:"characterCount"`
	RecordCount    int       `json:"recordCount"`
}

type txDatabase struct {
	tx *sql.Tx
}

func (d txDatabase) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("save export transaction is read-only")
}

func (d txDatabase) QueryContext(ctx context.Context, query string, args ...any) (mysqlrepo.SQLRows, error) {
	return d.tx.QueryContext(ctx, query, args...)
}

func (d txDatabase) QueryRowContext(ctx context.Context, query string, args ...any) mysqlrepo.SQLRow {
	return d.tx.QueryRowContext(ctx, query, args...)
}

func (d txDatabase) PingContext(ctx context.Context) error {
	var value int
	return d.tx.QueryRowContext(ctx, "SELECT 1").Scan(&value)
}

type recordLoader[T any] interface {
	Load(context.Context, string) (T, bool, error)
}

func main() {
	var opts options
	flag.StringVar(&opts.root, "root", "", "DNF90 project root")
	flag.StringVar(&opts.output, "output", "", "output bundle directory under runtime/tmp")
	flag.StringVar(&opts.accountID, "account-id", "", "source account ID")
	flag.StringVar(&opts.characterID, "character-id", "", "source character ID")
	flag.StringVar(&opts.username, "username", "", "new public test username")
	flag.BoolVar(&opts.passwordIn, "password-stdin", false, "read the new public test password from stdin")
	flag.Parse()

	if err := run(context.Background(), opts, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, opts options, stdin io.Reader) error {
	root, err := resolveRoot(opts.root)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(opts.accountID)
	characterID := strings.TrimSpace(opts.characterID)
	if accountID == "" || characterID == "" {
		return errors.New("account-id and character-id are required")
	}
	username, err := normalizeUsername(opts.username)
	if err != nil {
		return err
	}
	if !opts.passwordIn {
		return errors.New("password-stdin is required")
	}
	password, err := readPassword(stdin)
	if err != nil {
		return err
	}
	if err := validatePassword(password); err != nil {
		return err
	}

	output, err := resolveOutput(root, opts.output)
	if err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("output already exists: %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output %s: %w", output, err)
	}

	cfg, err := readInstance(filepath.Join(root, "runtime", "config", "instance.json"))
	if err != nil {
		return err
	}
	state, character, err := exportState(ctx, cfg, accountID, characterID)
	if err != nil {
		return err
	}

	tmpRoot := filepath.Join(root, "runtime", "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return fmt.Errorf("create runtime tmp: %w", err)
	}
	stage, err := os.MkdirTemp(tmpRoot, ".save-bundle-")
	if err != nil {
		return fmt.Errorf("create save bundle staging directory: %w", err)
	}
	defer os.RemoveAll(stage)

	databasePath := filepath.Join(stage, "dnf90.db")
	if err := writeSQLiteBundle(ctx, databasePath, state, accountID, username, password); err != nil {
		return err
	}
	info := bundleInfo{
		SchemaVersion:  bundleSchemaVersion,
		GeneratedAt:    time.Now().UTC(),
		Username:       username,
		AccountID:      accountID,
		CharacterID:    characterID,
		CharacterName:  character.Name,
		CharacterLevel: character.Level,
		AccountCount:   len(state.Accounts),
		CharacterCount: len(state.Characters),
		RecordCount:    stateRecordCount(state),
	}
	if err := writeJSON(filepath.Join(stage, "save-info.json"), info); err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(stage, "guide.txt"),
		[]byte(buildGuide(info, password)),
		0o600,
	); err != nil {
		return fmt.Errorf("write save guide: %w", err)
	}
	if err := os.Rename(stage, output); err != nil {
		return fmt.Errorf("publish save bundle: %w", err)
	}

	fmt.Println("Save bundle created.")
	fmt.Println("Directory:", output)
	fmt.Printf("Character: %s (level %d, id %s)\n", character.Name, character.Level, character.CharacterID)
	fmt.Printf("Repository records: %d; login accounts: 1\n", info.RecordCount)
	fmt.Println("Password was written only to guide.txt and was not printed.")
	return nil
}

func resolveRoot(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		configured = filepath.Join(cwd, "..")
	}
	root, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go-server", "go.mod")); err != nil {
		return "", fmt.Errorf("not a DNF90 project root: %s", root)
	}
	return filepath.Clean(root), nil
}

func resolveOutput(root, configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", errors.New("output is required")
	}
	if !filepath.IsAbs(configured) {
		configured = filepath.Join(root, configured)
	}
	output, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve output: %w", err)
	}
	tmpRoot := filepath.Join(root, "runtime", "tmp")
	rel, err := filepath.Rel(tmpRoot, output)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("output must be a child directory of %s", tmpRoot)
	}
	return filepath.Clean(output), nil
}

func readInstance(path string) (instanceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return instanceFile{}, fmt.Errorf("read instance configuration: %w", err)
	}
	var cfg instanceFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return instanceFile{}, fmt.Errorf("decode instance configuration: %w", err)
	}
	if cfg.Database.Mode != "portable" {
		return instanceFile{}, fmt.Errorf("source database mode must be portable MySQL, got %q", cfg.Database.Mode)
	}
	if cfg.Database.Host != "127.0.0.1" && cfg.Database.Host != "localhost" {
		return instanceFile{}, errors.New("save export accepts only loopback MySQL")
	}
	if cfg.Database.Port < 1 || cfg.Database.Port > 65535 ||
		!sqlIdentifierPattern.MatchString(cfg.Database.Name) {
		return instanceFile{}, errors.New("source database configuration is invalid")
	}
	return cfg, nil
}

func exportState(ctx context.Context, cfg instanceFile, accountID, characterID string) (memory.State, dnfrepo.CharacterRecord, error) {
	driverCfg := mysqldriver.Config{
		User:         cfg.Database.User,
		Passwd:       cfg.Database.Password,
		Net:          "tcp",
		Addr:         net.JoinHostPort(cfg.Database.Host, strconv.Itoa(cfg.Database.Port)),
		DBName:       cfg.Database.Name,
		ParseTime:    true,
		Loc:          time.UTC,
		Timeout:      5 * time.Second,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}
	database, err := sql.Open("mysql", driverCfg.FormatDSN())
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, fmt.Errorf("open source MySQL: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	tx, err := database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, fmt.Errorf("begin consistent save snapshot: %w", err)
	}
	defer tx.Rollback()
	group, err := mysqlrepo.NewMySQLGroup(txDatabase{tx: tx}, mysqlrepo.MySQLGroupOptions{
		DatabasePlan: dnfrepo.DatabasePlan{
			ShardID:        strings.TrimSpace(cfg.Game.ShardID),
			GroupID:        "release-save-export",
			WriteDatabases: []string{cfg.Database.Name},
			ReadDatabases:  []string{cfg.Database.Name},
		},
		TablePrefix: "dnf",
	})
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}

	account, err := loadRequired(ctx, group.Account, accountID, "account")
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	character, err := loadRequired(ctx, group.Character, characterID, "character")
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if character.AccountID != accountID {
		return memory.State{}, dnfrepo.CharacterRecord{}, errors.New("selected character does not belong to selected account")
	}
	if character.Level != 90 {
		return memory.State{}, dnfrepo.CharacterRecord{}, fmt.Errorf("selected character level is %d, want 90", character.Level)
	}
	if character.Stats["delete_flag"] != 0 {
		return memory.State{}, dnfrepo.CharacterRecord{}, errors.New("selected character is deleted")
	}
	character.Slot = 0
	state := memory.State{
		Version:    memory.StateVersion,
		Accounts:   []dnfrepo.AccountRecord{account},
		Characters: []dnfrepo.CharacterRecord{character},
	}
	if err := appendOptional(ctx, group.AccountInventory, accountID, "account inventory", &state.AccountInventories); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Inventory, characterID, "inventory", &state.Inventories); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Equipment, characterID, "equipment", &state.Equipment); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Pet, characterID, "pet", &state.Pets); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Quest, characterID, "quest", &state.Quests); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Skill, characterID, "skill", &state.Skills); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.DungeonPermission, characterID, "dungeon permission", &state.DungeonPermissions); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.HellPartyPermission, characterID, "hell-party permission", &state.HellPartyPermissions); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.Mailbox, characterID, "mailbox", &state.Mailboxes); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	if err := appendOptional(ctx, group.EpicBook, accountID, "epic book", &state.EpicBooks); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}

	scopes, err := listKeys(ctx, tx, "dnf_settings", "scope")
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	accountPrefix := "account:" + accountID + ":"
	characterPrefix := "character:" + characterID + ":"
	for _, scope := range scopes {
		if !strings.HasPrefix(scope, accountPrefix) && !strings.HasPrefix(scope, characterPrefix) {
			continue
		}
		if err := appendOptional(ctx, group.Settings, scope, "settings", &state.Settings); err != nil {
			return memory.State{}, dnfrepo.CharacterRecord{}, err
		}
	}
	templateIDs, err := listKeys(ctx, tx, "dnf_packet_templates", "template_id")
	if err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, err
	}
	for _, templateID := range templateIDs {
		if err := appendOptional(ctx, group.PacketTemplate, templateID, "packet template", &state.PacketTemplates); err != nil {
			return memory.State{}, dnfrepo.CharacterRecord{}, err
		}
	}
	if _, err := memory.NewRuntimeFromState(ctx, state); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, fmt.Errorf("validate exported state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return memory.State{}, dnfrepo.CharacterRecord{}, fmt.Errorf("finish consistent save snapshot: %w", err)
	}
	return state, character, nil
}

func loadRequired[T any](ctx context.Context, loader recordLoader[T], key, label string) (T, error) {
	record, found, err := loader.Load(ctx, key)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("load %s %q: %w", label, key, err)
	}
	if !found {
		var zero T
		return zero, fmt.Errorf("%s %q was not found", label, key)
	}
	return record, nil
}

func appendOptional[T any](ctx context.Context, loader recordLoader[T], key, label string, target *[]T) error {
	record, found, err := loader.Load(ctx, key)
	if err != nil {
		return fmt.Errorf("load %s %q: %w", label, key, err)
	}
	if found {
		*target = append(*target, record)
	}
	return nil
}

func listKeys(ctx context.Context, tx *sql.Tx, table, column string) ([]string, error) {
	if !sqlIdentifierPattern.MatchString(table) || !sqlIdentifierPattern.MatchString(column) {
		return nil, errors.New("unsafe source table or column")
	}
	rows, err := tx.QueryContext(ctx, "SELECT `"+column+"` FROM `"+table+"` ORDER BY `"+column+"`")
	if err != nil {
		return nil, fmt.Errorf("list %s keys: %w", table, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan %s key: %w", table, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s keys: %w", table, err)
	}
	return keys, nil
}

func writeSQLiteBundle(ctx context.Context, path string, state memory.State, accountID, username, password string) error {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open target SQLite: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	closed := false
	defer func() {
		if !closed {
			database.Close()
		}
	}()
	if err := sqliterepo.ReplaceState(ctx, database, state); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dnf_login_accounts (
  account_id TEXT NOT NULL PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash BLOB NOT NULL,
  state TEXT NOT NULL DEFAULT 'active',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  last_login_at DATETIME NULL
)`); err != nil {
		return fmt.Errorf("create target login schema: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash target test password: %w", err)
	}
	now := time.Now().UTC()
	if _, err := database.ExecContext(ctx, `INSERT INTO dnf_login_accounts
(account_id, username, password_hash, state, created_at, updated_at)
VALUES (?, ?, ?, 'active', ?, ?)`, accountID, username, hash, now, now); err != nil {
		return fmt.Errorf("insert target test account: %w", err)
	}
	if _, err := database.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint target SQLite: %w", err)
	}
	var journalMode string
	if err := database.QueryRowContext(ctx, `PRAGMA journal_mode = DELETE`).Scan(&journalMode); err != nil {
		return fmt.Errorf("finalize target SQLite journal: %w", err)
	}
	if err := database.Close(); err != nil {
		return fmt.Errorf("close target SQLite: %w", err)
	}
	closed = true
	return verifySQLiteBundle(ctx, path, state, accountID, username, password)
}

func verifySQLiteBundle(ctx context.Context, path string, state memory.State, accountID, username, password string) error {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	var integrity string
	if err := database.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check target SQLite integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("target SQLite integrity check returned %q", integrity)
	}
	var repositoryCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM dnf_repository_records`).Scan(&repositoryCount); err != nil {
		return err
	}
	if repositoryCount != stateRecordCount(state) {
		return fmt.Errorf("target repository record count=%d want=%d", repositoryCount, stateRecordCount(state))
	}
	var storedAccountID string
	var passwordHash []byte
	if err := database.QueryRowContext(ctx, `SELECT account_id, password_hash FROM dnf_login_accounts WHERE username = ?`, username).Scan(&storedAccountID, &passwordHash); err != nil {
		return fmt.Errorf("verify target test account: %w", err)
	}
	if storedAccountID != accountID || bcrypt.CompareHashAndPassword(passwordHash, []byte(password)) != nil {
		return errors.New("target test account verification failed")
	}
	return nil
}

func stateRecordCount(state memory.State) int {
	return len(state.Accounts) + len(state.AccountInventories) + len(state.Characters) +
		len(state.Inventories) + len(state.Equipment) + len(state.Pets) +
		len(state.Quests) + len(state.Skills) + len(state.DungeonPermissions) +
		len(state.HellPartyPermissions) + len(state.PacketTemplates) + len(state.Settings) +
		len(state.Mailboxes) + len(state.EpicBooks)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func buildGuide(info bundleInfo, password string) string {
	return fmt.Sprintf(`【本存档版先看】内置测试账号与存档
==================================

本发布包已经带一个测试存档，不需要注册，直接在“一键登录.bat”中登录：

账号：%s
密码：%s
角色：%s（%d 级）

请只填写第 1 组账号并点击“登录账号 1”。不要用“注册账号”覆盖测试账号。
下面通用说明里的“首次注册”步骤只适用于不带存档的空白版本，本包请跳过注册。
这个账号密码会随发布包公开，仅用于本地一键端测试，不要用于其他网站或正式服务。


`, info.Username, password, info.CharacterName, info.CharacterLevel)
}

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	count := utf8.RuneCountInString(username)
	if !utf8.ValidString(username) || count < 3 || count > 32 {
		return "", errors.New("username must contain 3 to 32 characters")
	}
	for _, character := range username {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return "", errors.New("username contains unsupported characters")
	}
	return username, nil
}

func validatePassword(password string) error {
	if !utf8.ValidString(password) || len(password) < 8 || len(password) > 72 {
		return errors.New("password must contain 8 to 72 UTF-8 bytes")
	}
	for _, character := range password {
		if unicode.IsControl(character) {
			return errors.New("password must not contain control characters")
		}
	}
	return nil
}

func readPassword(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("password input is unavailable")
	}
	line, err := bufio.NewReader(io.LimitReader(reader, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	password = strings.TrimPrefix(password, "\ufeff")
	if password == "" {
		return "", errors.New("password is empty")
	}
	return password, nil
}
