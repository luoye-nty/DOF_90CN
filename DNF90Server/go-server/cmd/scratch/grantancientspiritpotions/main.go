package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	repomysql "longheng.io/server/internal/modules/dnf/repository/mysql"
	reposqlite "longheng.io/server/internal/modules/dnf/repository/sqlite"
)

const grantSource = "manual_ancient_spirit_potion_five_20260819"

var grantExpireAt = time.Date(2037, time.December, 31, 15, 59, 59, 0, time.UTC)

type potionSpec struct {
	Multiplier string
	ItemID     int64
	PVFPath    string
}

type instanceConfig struct {
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

var potionSpecs = []potionSpec{
	{Multiplier: "1.5倍", ItemID: 490007794, PVFPath: "stackable/490007001/chn_490007794.stk"},
	{Multiplier: "2倍", ItemID: 490007817, PVFPath: "stackable/490007001/chn_490007817.stk"},
	{Multiplier: "3倍", ItemID: 10164642, PVFPath: "stackable/10164001/10164642.stk"},
	{Multiplier: "4倍", ItemID: 10005802, PVFPath: "stackable/event/chn_event_20150702_attendance_check/chn_event_super_grow_potion_30min_10005802.stk"},
	{Multiplier: "5倍", ItemID: 490701622, PVFPath: "stackable/cash/chn_490701622.stk"},
}

func main() {
	databasePath := flag.String("db", "", "offline SQLite save database")
	configPath := flag.String("config", "", "portable-MySQL instance.json")
	characterID := flag.String("character", "", "recipient character id")
	apply := flag.Bool("apply", false, "commit the five system mails")
	flag.Parse()

	if strings.TrimSpace(*characterID) == "" || (strings.TrimSpace(*databasePath) == "") == (strings.TrimSpace(*configPath) == "") {
		fmt.Fprintln(os.Stderr, "FAILED: -character and exactly one of -db or -config are required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	group, closeRepository, err := openRepository(ctx, *databasePath, *configPath)
	if err != nil {
		fail(err)
	}
	defer func() {
		if err := closeRepository(); err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: close repository:", err)
		}
	}()
	if group.Character == nil || group.Mailbox == nil || group.MailboxAssets == nil {
		fail(errors.New("repository group is unavailable"))
	}
	recipient, found, err := group.Character.Load(ctx, strings.TrimSpace(*characterID))
	if err != nil || !found {
		fail(fmt.Errorf("load recipient character: found=%t: %w", found, err))
	}
	mailbox, mailboxFound, err := group.Mailbox.Load(ctx, recipient.CharacterID)
	if err != nil {
		fail(fmt.Errorf("load recipient mailbox: %w", err))
	}

	fmt.Printf("recipient=%s name=%s apply=%t existing_batch_mails=%d mailbox_found=%t\n",
		recipient.CharacterID, recipient.Name, *apply, grantedPotionCount(mailbox), mailboxFound)
	for _, spec := range potionSpecs {
		fmt.Printf("  %s item=%d path=%s\n", spec.Multiplier, spec.ItemID, spec.PVFPath)
	}
	if !*apply {
		fmt.Println("DRY RUN: no mailbox state changed")
		return
	}

	err = group.MailboxAssets.WithinMailboxAssets(ctx, recipient.CharacterID, recipient.CharacterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		_ dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		record, exists, loadErr := mailboxes.Load(ctx, recipient.CharacterID)
		if loadErr != nil {
			return loadErr
		}
		if !exists {
			record = dnfrepo.MailboxRecord{CharacterID: recipient.CharacterID}
		} else {
			record = dnfrepo.CloneMailbox(record)
		}
		if record.Mails == nil {
			record.Mails = make(map[string]dnfrepo.MailRecord)
		}
		now := time.Now().UTC()
		existing := grantedPotionCount(record)
		if existing != 0 && existing != len(potionSpecs) {
			return fmt.Errorf("grant batch %s is incomplete: mails=%d", grantSource, existing)
		}
		if existing == 0 {
			nextID, nextErr := nextMailID(record.Mails)
			if nextErr != nil {
				return nextErr
			}
			for index, spec := range potionSpecs {
				mailID := strconv.FormatUint(nextID+uint64(index), 10)
				record.Mails[mailID] = newPotionMail(mailID, recipient, spec, now.Add(time.Duration(index)*time.Nanosecond))
			}
		} else {
			for mailID, mail := range record.Mails {
				if strings.TrimSpace(mail.Metadata["source"]) != grantSource {
					continue
				}
				spec, ok := potionSpecForMultiplier(mail.Metadata["potion_multiplier"])
				if !ok {
					return fmt.Errorf("grant mail %s has unknown multiplier %q", mailID, mail.Metadata["potion_multiplier"])
				}
				mail.Attachments = []dnfrepo.MailAttachment{newPotionAttachment(spec)}
				mail.Metadata["system_mail"] = "true"
				mail.Metadata["unlimited"] = "true"
				record.Mails[mailID] = mail
			}
		}
		record.UpdatedAt = now
		return dnfrepo.SaveMailboxFields(ctx, mailboxes, record, dnfrepo.MailboxFieldMails)
	})
	if err != nil {
		fail(err)
	}

	record, found, err := group.Mailbox.Load(ctx, recipient.CharacterID)
	if err != nil || !found {
		fail(fmt.Errorf("verify mailbox: found=%t: %w", found, err))
	}
	if err := validatePotionBatch(record); err != nil {
		fail(fmt.Errorf("verify grant: %w", err))
	}
	ids := grantedMailIDs(record)
	fmt.Printf("COMMITTED: mails=%s count=%d\n", strings.Join(ids, ","), len(ids))
}

func newPotionMail(mailID string, recipient dnfrepo.CharacterRecord, spec potionSpec, createdAt time.Time) dnfrepo.MailRecord {
	return dnfrepo.MailRecord{
		MailID:               mailID,
		SenderCharacterID:    "0",
		SenderName:           "系统",
		RecipientCharacterID: recipient.CharacterID,
		RecipientName:        recipient.Name,
		Title:                "远古精灵秘药（" + spec.Multiplier + "）",
		Body:                 "按要求发放：" + spec.Multiplier + "远古精灵秘药 1 个。",
		Attachments:          []dnfrepo.MailAttachment{newPotionAttachment(spec)},
		CreatedAt:            createdAt,
		Metadata: map[string]string{
			"source":            grantSource,
			"system_mail":       "true",
			"unlimited":         "true",
			"potion_multiplier": spec.Multiplier,
		},
	}
}

func newPotionAttachment(spec potionSpec) dnfrepo.MailAttachment {
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(spec.ItemID))
	binary.LittleEndian.PutUint32(raw[6:10], 1)
	binary.LittleEndian.PutUint16(raw[0x0b:0x0d], math.MaxUint16)
	raw[0x0d] = 1
	binary.LittleEndian.PutUint32(raw[0x38:0x3c], uint32(grantExpireAt.Unix()))
	expireUnix := strconv.FormatInt(grantExpireAt.Unix(), 10)
	return dnfrepo.MailAttachment{
		ItemID:   spec.ItemID,
		Count:    1,
		Bind:     true,
		ExpireAt: grantExpireAt,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":         "stackable",
			"pvf_path":          spec.PVFPath,
			"stackable_type":    "[waste]",
			"expire_time":       expireUnix,
			"expire_unix":       expireUnix,
			"expiration_source": "manual_long_lived_grant",
			"last_grant_source": grantSource,
		},
	}
}

func potionSpecForMultiplier(multiplier string) (potionSpec, bool) {
	for _, spec := range potionSpecs {
		if strings.TrimSpace(multiplier) == spec.Multiplier {
			return spec, true
		}
	}
	return potionSpec{}, false
}

func validatePotionBatch(record dnfrepo.MailboxRecord) error {
	if count := grantedPotionCount(record); count != len(potionSpecs) {
		return fmt.Errorf("count=%d want=%d", count, len(potionSpecs))
	}
	for mailID, mail := range record.Mails {
		if strings.TrimSpace(mail.Metadata["source"]) != grantSource {
			continue
		}
		spec, ok := potionSpecForMultiplier(mail.Metadata["potion_multiplier"])
		if !ok || len(mail.Attachments) != 1 {
			return fmt.Errorf("mail=%s has invalid multiplier or attachment count", mailID)
		}
		attachment := mail.Attachments[0]
		if attachment.ItemID != spec.ItemID || attachment.Count != 1 || !attachment.Bind ||
			!attachment.ExpireAt.Equal(grantExpireAt) || len(attachment.RawEntry) != 0x77 ||
			binary.LittleEndian.Uint16(attachment.RawEntry[0x0b:0x0d]) != math.MaxUint16 ||
			binary.LittleEndian.Uint32(attachment.RawEntry[0x38:0x3c]) != uint32(grantExpireAt.Unix()) {
			return fmt.Errorf("mail=%s attachment expiration projection is invalid", mailID)
		}
	}
	return nil
}

func openRepository(ctx context.Context, databasePath, configPath string) (dnfrepo.Group, func() error, error) {
	databasePath = strings.TrimSpace(databasePath)
	configPath = strings.TrimSpace(configPath)
	if databasePath != "" {
		database, err := sql.Open("sqlite", databasePath)
		if err != nil {
			return dnfrepo.Group{}, nil, err
		}
		component := reposqlite.NewComponent(database, databasePath)
		if err := component.Start(ctx); err != nil {
			database.Close()
			return dnfrepo.Group{}, nil, err
		}
		group, ok := component.Group()
		if !ok {
			component.Stop(context.Background())
			database.Close()
			return dnfrepo.Group{}, nil, errors.New("SQLite repository group is unavailable")
		}
		return group, func() error {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return errors.Join(component.Stop(stopCtx), database.Close())
		}, nil
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return dnfrepo.Group{}, nil, err
	}
	var config instanceConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return dnfrepo.Group{}, nil, err
	}
	host := strings.TrimSpace(config.Database.Host)
	name := strings.TrimSpace(config.Database.Name)
	if !strings.EqualFold(config.Database.Mode, "portable") ||
		(host != "127.0.0.1" && !strings.EqualFold(host, "localhost")) ||
		config.Database.Port < 1 || config.Database.Port > 65535 || name == "" || strings.ContainsAny(name, "`; /\\") {
		return dnfrepo.Group{}, nil, errors.New("portable MySQL configuration is invalid or not loopback-only")
	}
	driverConfig := mysqldriver.Config{
		User:         config.Database.User,
		Passwd:       config.Database.Password,
		Net:          "tcp",
		Addr:         net.JoinHostPort(host, strconv.Itoa(config.Database.Port)),
		DBName:       name,
		ParseTime:    true,
		Loc:          time.UTC,
		Timeout:      5 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	database, err := sql.Open("mysql", driverConfig.FormatDSN())
	if err != nil {
		return dnfrepo.Group{}, nil, err
	}
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return dnfrepo.Group{}, nil, err
	}
	group, err := repomysql.NewMySQLGroupFromDB(database, repomysql.MySQLGroupOptions{
		DatabasePlan: dnfrepo.DatabasePlan{
			ShardID:        strings.TrimSpace(config.Game.ShardID),
			GroupID:        "manual-ancient-spirit-potion-grant",
			WriteDatabases: []string{name},
			ReadDatabases:  []string{name},
		},
		TablePrefix: "dnf",
	})
	if err != nil {
		database.Close()
		return dnfrepo.Group{}, nil, err
	}
	return group, database.Close, nil
}

func grantedPotionCount(record dnfrepo.MailboxRecord) int {
	count := 0
	for _, mail := range record.Mails {
		if strings.TrimSpace(mail.Metadata["source"]) == grantSource {
			count++
		}
	}
	return count
}

func grantedMailIDs(record dnfrepo.MailboxRecord) []string {
	ids := make([]string, 0, len(potionSpecs))
	for key, mail := range record.Mails {
		if strings.TrimSpace(mail.Metadata["source"]) == grantSource {
			ids = append(ids, key)
		}
	}
	sort.Slice(ids, func(left, right int) bool {
		leftID, _ := strconv.ParseUint(ids[left], 10, 64)
		rightID, _ := strconv.ParseUint(ids[right], 10, 64)
		return leftID < rightID
	})
	return ids
}

func nextMailID(mails map[string]dnfrepo.MailRecord) (uint64, error) {
	var maxID uint64
	for key := range mails {
		value, err := strconv.ParseUint(strings.TrimSpace(key), 10, 32)
		if err == nil && value > maxID {
			maxID = value
		}
	}
	if maxID > math.MaxUint32-uint64(len(potionSpecs)) {
		return 0, errors.New("mail id range is exhausted")
	}
	return maxID + 1, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "FAILED:", err)
	os.Exit(1)
}
