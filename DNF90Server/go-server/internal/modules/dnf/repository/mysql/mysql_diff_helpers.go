package mysql

import (
	"bytes"
	"context"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

// Helpers for the diffing collection writers (replaceItemStackCollection,
// replaceEquipmentEntries): compare a row as it would be stored and delete
// rows by entry key in bounded batches.

// storedTimeEqual compares two times as the DATETIME(6) columns keep them:
// zero is NULL, everything else UTC at microsecond precision. MySQL rounds the
// sub-microsecond part (a live check stored .123456789 as .123457), so round
// rather than truncate, or such a value would look changed on every save.
func storedTimeEqual(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.UTC().Round(time.Microsecond).Equal(b.UTC().Round(time.Microsecond))
}

// stringMapsEqual treats nil and empty as equal, as the extra tables do.
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		other, ok := b[key]
		if !ok || other != value {
			return false
		}
	}
	return true
}

// itemStackStoredEqual reports whether writing b over a stored a would leave
// the loaded row unchanged. RawEntry compares nil and empty as equal because
// the loader returns nil for both.
func itemStackStoredEqual(a, b repository.ItemStack) bool {
	return a.ItemID == b.ItemID && a.Count == b.Count && a.Bind == b.Bind &&
		storedTimeEqual(a.ExpireAt, b.ExpireAt) && bytes.Equal(a.RawEntry, b.RawEntry) &&
		stringMapsEqual(a.Extra, b.Extra)
}

func equipmentEntryStoredEqual(a, b repository.EquipmentEntry) bool {
	return a.SlotIndex == b.SlotIndex && a.ItemID == b.ItemID && a.Bind == b.Bind &&
		storedTimeEqual(a.ExpireAt, b.ExpireAt) && bytes.Equal(a.RawEntry, b.RawEntry) &&
		stringMapsEqual(a.Extra, b.Extra)
}

func subsetItemStacks(values map[string]repository.ItemStack, keys []string) map[string]repository.ItemStack {
	out := make(map[string]repository.ItemStack, len(keys))
	for _, key := range keys {
		out[key] = values[key]
	}
	return out
}

// deleteMySQLEntryKeys deletes the owner's rows whose entry_key is in keys.
// where/args select the owner (and collection) exactly as the full-replace
// DELETE used to.
func deleteMySQLEntryKeys(ctx context.Context, database SQLDB, table, where string, args []any, keys []string) error {
	return deleteMySQLKeys(ctx, database, table, where, args, "entry_key", keys)
}

// deleteMySQLKeys is deleteMySQLEntryKeys for a table keyed by keyColumn.
func deleteMySQLKeys(ctx context.Context, database SQLDB, table, where string, args []any, keyColumn string, keys []string) error {
	for start := 0; start < len(keys); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(keys))
		chunk := keys[start:end]
		query := "DELETE FROM " + table + " WHERE " + where + " AND " + quoteSQLIdentifier(keyColumn) + " IN (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ") + ")"
		chunkArgs := make([]any, 0, len(args)+len(chunk))
		chunkArgs = append(chunkArgs, args...)
		for _, key := range chunk {
			chunkArgs = append(chunkArgs, key)
		}
		if _, err := database.ExecContext(ctx, query, chunkArgs...); err != nil {
			return err
		}
	}
	return nil
}
