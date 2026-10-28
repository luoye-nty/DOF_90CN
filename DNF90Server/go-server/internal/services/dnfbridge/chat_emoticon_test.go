package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseCurrentChatEmoticonCatalogFiltersLockedCashAndKeepsPVFOrder(t *testing.T) {
	entries, err := parseCurrentChatEmoticonCatalog(initialEquipmentMemSource{
		currentChatEmoticonCatalogPath: "1 `common.emo` 2 `level.emo` 3 `cash.emo`",
		currentChatEmoticonOrderPath:   "[order]\n2 1 3\n[/order]",
		"chatemoticon/common.emo":      "[type]\n`COMMON` 0\n[macro]\n`hello`",
		"chatemoticon/level.emo":       "[type]\n`LEVEL` 10\n[macro]\n`level ten`",
		"chatemoticon/cash.emo":        "[type]\n`CASH` 1234\n[macro]\n`locked`",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != 2 || entries[0].UnlockType != "LEVEL" ||
		entries[0].UnlockValue != 10 || entries[1].ID != 1 || entries[1].UnlockType != "COMMON" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestCurrentChatEmoticonConfigBodyUsesExactOp411RowsAndLevelGate(t *testing.T) {
	entries := []currentChatEmoticonEntry{
		{ID: 2, UnlockType: "LEVEL", UnlockValue: 10},
		{ID: 1, UnlockType: "COMMON"},
	}
	configuredBody := []byte{1, 0, 1, 0, 2, 0, 0, 0, 'h', 'i'}
	configured, ok := parseCurrentChatEmoticonConfiguredBody(configuredBody)
	if !ok {
		t.Fatalf("configured body rejected: %x", configuredBody)
	}
	body := buildCurrentChatEmoticonConfigBody(entries, 9, configured)
	if len(body) != 2+2+2+6 || binary.LittleEndian.Uint16(body[:2]) != 1 ||
		binary.LittleEndian.Uint16(body[2:4]) != 0 || binary.LittleEndian.Uint16(body[4:6]) != 1 ||
		!bytes.Equal(body[6:], []byte{2, 0, 0, 0, 'h', 'i'}) {
		t.Fatalf("level-9 body = %x", body)
	}
	body = buildCurrentChatEmoticonConfigBody(entries, 10, configured)
	if binary.LittleEndian.Uint16(body[:2]) != 2 || binary.LittleEndian.Uint16(body[4:6]) != 2 {
		t.Fatalf("level-10 body = %x", body)
	}
}

func TestParseCurrentChatEmoticonConfiguredBodyRejectsDuplicatesAndTrailingBytes(t *testing.T) {
	duplicate := []byte{
		2, 0,
		1, 0, 1, 0, 0, 0, 0,
		1, 0, 1, 0, 0, 0, 0,
	}
	if _, ok := parseCurrentChatEmoticonConfiguredBody(duplicate); ok {
		t.Fatalf("duplicate rows accepted: %x", duplicate)
	}
	trailing := []byte{0, 0, 0}
	if _, ok := parseCurrentChatEmoticonConfiguredBody(trailing); ok {
		t.Fatalf("trailing bytes accepted: %x", trailing)
	}
}

func TestCurrentChatEmoticonCatalogRealPVFHasVisibleEntries(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real chat-emoticon smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseCurrentChatEmoticonCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("visible emoticon entries=%d, want at least 2", len(entries))
	}
	for _, entry := range entries {
		if entry.ID == 0 || (entry.UnlockType != "COMMON" && entry.UnlockType != "LEVEL") {
			t.Fatalf("invalid visible entry: %+v", entry)
		}
	}
}
