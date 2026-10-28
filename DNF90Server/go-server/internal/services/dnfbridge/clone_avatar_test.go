package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentPVFDocumentHasRepeatedCloneAvatarCategory(t *testing.T) {
	doc, err := dnfpvf.Parse("clone.equ", "[item category]\n`other`\n[item category]\n`clear avatar`\n")
	if err != nil {
		t.Fatal(err)
	}
	if !currentPVFDocumentHasText(doc, "item category", "clear avatar") {
		t.Fatal("repeated clear-avatar category was not detected")
	}
}

func TestRealScriptPVFClassifiesRareCloneAndOrdinaryAvatar(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf clone-avatar smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{initialEquipmentArchive: archive}
	if isAvatar, isClone := service.currentClassifyAvatarItem(415500017); !isAvatar || !isClone {
		t.Fatalf("rare clone classification avatar=%t clone=%t", isAvatar, isClone)
	}
	if isAvatar, isClone := service.currentClassifyAvatarItem(112500035); !isAvatar || !isClone {
		t.Fatalf("female slayer clone top classification avatar=%t clone=%t", isAvatar, isClone)
	}
	if isAvatar, isClone := service.currentClassifyAvatarItem(415500058); !isAvatar || isClone {
		t.Fatalf("ordinary avatar classification avatar=%t clone=%t", isAvatar, isClone)
	}
	if isAvatar, isClone := service.currentClassifyAvatarItem(27000); isAvatar || isClone {
		t.Fatalf("ordinary equipment classification avatar=%t clone=%t", isAvatar, isClone)
	}
}

func TestCloneAvatarAppearanceProjectsMode0RosterAndMode1(t *testing.T) {
	const (
		cloneID      int64  = 415500017
		appearanceID uint32 = 415500058
	)
	entry := dnfrepo.EquipmentEntry{
		SlotIndex: 3,
		ItemID:    cloneID,
		RawEntry:  make([]byte, currentItemListEntryWireSize),
		Extra: map[string]string{
			"current_exe_equipment_type":          "3",
			dnfrepo.CloneAvatarAppearanceExtraKey: "415500058",
		},
	}
	record := dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{"3": entry}}

	mode0, err := buildCurrentActorMode0AppearanceSnapshotFromEquipment(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := currentActorMode0AppearanceGoldenItemID(t, mode0, 3); got != appearanceID {
		t.Fatalf("mode0 clone appearance=%d want=%d", got, appearanceID)
	}
	roster := equipmentRosterSummary(record)
	if len(roster) != 1 || roster[0].ItemIDOrIcon != int64(appearanceID) {
		t.Fatalf("roster clone projection=%+v", roster)
	}

	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Equipment.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	reader := &csharpLegacyUserInfoReader{
		ctx: context.Background(),
		service: &Service{repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		}},
		characterID: "19",
	}
	rows := reader.currentMode1EquipmentObjectRows()
	if len(rows) != 1 || rows[0].itemID != uint32(cloneID) || rows[0].linkedItemID != appearanceID {
		t.Fatalf("mode1 clone row=%+v", rows)
	}
}
