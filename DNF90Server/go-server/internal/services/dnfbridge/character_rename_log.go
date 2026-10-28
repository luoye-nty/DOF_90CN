package dnfbridge

import (
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// currentCharacterRenameLogKeyPrefix namespaces one rename record inside the
	// account metadata key/value store (the same store the old-name reservation
	// already uses). The key carries the character id and the rename instant so
	// records sort without a new table or schema migration:
	//
	//	character_rename_log:<characterID>:<unixSeconds> = <serverID>|<previous name>
	currentCharacterRenameLogKeyPrefix = "character_rename_log:"

	// currentCharacterRenameLogLimit bounds the history kept per character. The
	// window renders a short list; older records are pruned when a new one is
	// appended so account metadata cannot grow without limit.
	currentCharacterRenameLogLimit = 12

	// currentCharacterRenameLogValueSeparator splits the recorded 大区 index from
	// the previous name inside the metadata value.
	currentCharacterRenameLogValueSeparator = "|"
)

// currentCharacterRenameLogEntry is one committed rename of one character.
type currentCharacterRenameLogEntry struct {
	// Unix is the rename instant in seconds. The client feeds this straight to
	// localtime() when it renders the record's date column.
	Unix int64
	// ServerID is the 大区 the rename happened on, as an index into the client's
	// own 大区 table. HasServerID is false for records written before the index
	// became part of the record; those fall back to the configured region.
	ServerID    byte
	HasServerID bool
	// OldName is the name the character carried before that rename. The current
	// 90CN row layout carries no name field, so this is metadata for operators
	// and for any later use -- it is deliberately not sent on the wire.
	OldName string
}

func currentCharacterRenameLogKey(characterID string, unix int64) string {
	return currentCharacterRenameLogKeyPrefix + strings.TrimSpace(characterID) + ":" + strconv.FormatInt(unix, 10)
}

func parseCurrentCharacterRenameLogKey(key, characterID string) (int64, bool) {
	if !strings.HasPrefix(key, currentCharacterRenameLogKeyPrefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(key, currentCharacterRenameLogKeyPrefix)
	separator := strings.LastIndex(rest, ":")
	if separator <= 0 || strings.TrimSpace(rest[:separator]) != strings.TrimSpace(characterID) {
		return 0, false
	}
	unix, err := strconv.ParseInt(strings.TrimSpace(rest[separator+1:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return unix, true
}

// parseCurrentCharacterRenameLogValue splits "<serverID>|<previous name>".
//
// A value without the separator is treated as a bare previous name (records
// written before the 大区 index became part of the record still load) and
// reports ok=false so the projection can fall back to the configured region.
func parseCurrentCharacterRenameLogValue(value string) (byte, string, bool) {
	trimmed := strings.TrimSpace(value)
	if separator := strings.Index(trimmed, currentCharacterRenameLogValueSeparator); separator > 0 {
		raw := strings.TrimSpace(trimmed[:separator])
		if parsed, err := strconv.ParseUint(raw, 10, 8); err == nil {
			return byte(parsed), strings.TrimSpace(trimmed[separator+1:]), true
		}
	}
	return 0, trimmed, false
}

func encodeCurrentCharacterRenameLogValue(serverID byte, oldName string) string {
	return strconv.FormatUint(uint64(serverID), 10) + currentCharacterRenameLogValueSeparator + oldName
}

// appendCurrentCharacterRenameLog records one rename on the account record that
// the rename transaction is already saving. Callers must invoke it inside that
// transaction so a rolled-back rename never leaves a record behind.
func appendCurrentCharacterRenameLog(account *dnfrepo.AccountRecord, characterID, oldName string, serverID byte, now time.Time) {
	characterID = strings.TrimSpace(characterID)
	oldName = strings.TrimSpace(oldName)
	if account == nil || characterID == "" || oldName == "" {
		return
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string, 4)
	}
	unix := now.Unix()
	account.Metadata[currentCharacterRenameLogKey(characterID, unix)] = encodeCurrentCharacterRenameLogValue(serverID, oldName)

	// Prune beyond the kept limit, oldest first. A same-second rename overwrites
	// its own key, so the log never holds two records for one instant.
	entries := currentCharacterRenameLogEntries(*account, characterID)
	if len(entries) <= currentCharacterRenameLogLimit {
		return
	}
	for _, entry := range entries[currentCharacterRenameLogLimit:] {
		delete(account.Metadata, currentCharacterRenameLogKey(characterID, entry.Unix))
	}
}

// currentCharacterRenameLogEntries returns one character's committed renames,
// newest first.
func currentCharacterRenameLogEntries(account dnfrepo.AccountRecord, characterID string) []currentCharacterRenameLogEntry {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || len(account.Metadata) == 0 {
		return nil
	}
	entries := make([]currentCharacterRenameLogEntry, 0, 4)
	for key, value := range account.Metadata {
		unix, ok := parseCurrentCharacterRenameLogKey(key, characterID)
		if !ok {
			continue
		}
		serverID, oldName, hasServerID := parseCurrentCharacterRenameLogValue(value)
		entries = append(entries, currentCharacterRenameLogEntry{
			Unix:        unix,
			ServerID:    serverID,
			HasServerID: hasServerID,
			OldName:     oldName,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Unix > entries[j].Unix })
	return entries
}
