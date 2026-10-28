package dnfbridge

import (
	"os"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealPVFRenameCardsUseTheActionType verifies the widened rename-card
// contract against the operator's actual Script.pvf.
//
// Set DNFBRIDGE_REAL_PVF_SMOKE to the runtime PVF, e.g.
//
//	DNFBRIDGE_REAL_PVF_SMOKE=runtime\data\dnf\Script.pvf
//
// Live incident 2026-09-23: the operator used a second 角色更名卡 and nothing
// happened. Two separate causes were found - a 90-day cooldown we invented, and
// recognition by two hard-coded PVF file names. This test pins the second one:
// every card that declares `[action type] [character name change]` must be
// accepted, including the event and account-bound cards, while the pet rename
// card must not be.
func TestRealPVFRenameCardsUseTheActionType(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify rename-card recognition against the runtime PVF")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatalf("build cera shop catalog: %v", err)
	}

	// The two cards the operator holds: the Cera shop bag card in main-bag slot
	// 113 and the account-bound 冒险家征集令 event card in slot 114.
	for _, itemID := range []uint32{2660698, 490007007} {
		definition, resolveErr := catalog.items.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve item %d: %v", itemID, resolveErr)
		}
		autoUse, card := catalog.characterRenameCardKind(definition)
		if !card {
			t.Fatalf("item %d (%s) is not recognized as a rename card", itemID, definition.PVFPath)
		}
		if autoUse {
			t.Fatalf("item %d (%s) must be used from the bag, not auto-used", itemID, definition.PVFPath)
		}
	}

	// Every stackable that declares the action type has to be accepted, and no
	// item outside that set may be - with exactly one deliberate exception: the
	// Cera shop's own auto-use product. Its document declares no action type at
	// all (see characterRenameCardKind), so it is recognized by its path.
	text, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		t.Fatalf("read stackable list: %v", err)
	}
	listDocument, err := dnfpvf.Parse("stackable/stackable.lst", text)
	if err != nil {
		t.Fatalf("parse stackable list: %v", err)
	}
	accepted, declared, autoUseOnly := 0, 0, 0
	for _, entry := range dnfpvf.ParseList(listDocument) {
		if entry.ID <= 0 {
			continue
		}
		docPath := "stackable/" + strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(entry.Path, "\\", "/")), "stackable/")
		if currentCharacterRenameAutoUsePVFPath(docPath) {
			// Locked separately below; it is the one card that must be accepted
			// without the declaration.
			if strings.Contains(strings.ToLower(itemTextOf(t, archive, docPath)), "[character name change]") {
				t.Fatalf("auto-use card %s now declares the action type; the path-only allowance is stale", docPath)
			}
			autoUseOnly++
			continue
		}
		announces := strings.Contains(itemTextOf(t, archive, docPath), "[character name change]")
		if announces {
			declared++
		}
		definition, resolveErr := catalog.items.ResolveItem(uint32(entry.ID))
		if resolveErr != nil {
			continue
		}
		if _, card := catalog.characterRenameCardKind(definition); card != announces {
			t.Fatalf("item %d (%s): accepted=%t but action type declared=%t",
				entry.ID, definition.PVFPath, card, announces)
		}
		_, card := catalog.characterRenameCardKind(definition)
		if card {
			accepted++
		}
	}
	// The auto-use product must actually be present and recognized; if the
	// runtime PVF ever drops it, this test says so instead of silently passing.
	autoUseDefinition, resolveErr := catalog.items.ResolveItem(2682290)
	if resolveErr != nil {
		t.Fatalf("resolve the Cera shop rename product 2682290: %v", resolveErr)
	}
	autoUse, card := catalog.characterRenameCardKind(autoUseDefinition)
	if !card || !autoUse {
		t.Fatalf("Cera shop rename product 2682290 (%s) autoUse=%t card=%t, want both true",
			autoUseDefinition.PVFPath, autoUse, card)
	}
	if autoUseOnly == 0 {
		t.Fatal("no stackable resolves to the auto-use rename-card path")
	}
	if accepted == 0 || accepted != declared {
		t.Fatalf("rename cards accepted=%d declared=%d, want both non-zero and equal", accepted, declared)
	}
	t.Logf("rename cards recognized by [character name change]: %d (+%d auto-use product)", accepted, autoUseOnly)
}

// itemTextOf reads a document or fails the test: the auto-use path assertion
// above must not silently skip when the file is unreadable.
func itemTextOf(t *testing.T, archive *platformpvf.Archive, docPath string) string {
	t.Helper()
	text, err := archive.ReadText(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	return text
}
