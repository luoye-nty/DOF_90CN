package dnfbridge

import (
	"os"
	"sort"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// op30 previously had no inbound route at all: the gameplay module registry did
// not own it and it was not an aligned command, so the legacy dispatcher's
// default branch logged "dnfbridge unhandled latest game command cmd=1 type=30"
// and dropped the request. Both transports must now reach the inventory domain.
func TestIncreaseStatusIsRoutedToTheInventoryDomain(t *testing.T) {
	opcode := uint16(dnfenum.CmdPacketIncreaseStatus)
	if _, owned := currentGameplayModules.legacyHandlers[opcode]; owned {
		t.Fatalf("op30 must stay on the aligned inventory route, not a gameplay module")
	}
	if _, owned := currentGameplayModules.upperHandlers[opcode]; owned {
		t.Fatalf("op30 must stay on the aligned inventory route, not a gameplay module")
	}
	decision, classified := alignedcmd.Classify(opcode)
	if !classified || decision.Domain != dnfenum.AlignedDomainInventory ||
		decision.Support != dnfenum.AlignedSupportDirect || decision.Action != alignedcmd.ActionPendingModule {
		t.Fatalf("classified=%t decision=%+v", classified, decision)
	}
}

// op30 carries no item identity, so its owner depends on exactly the same three
// PVF-backed inputs op44 uses. Each gate is opcode-keyed and must admit op30.
func TestIncreaseStatusOpensEveryPVFBackedOwnerInput(t *testing.T) {
	if !alignedSkillOwnerRulesRequired(dnfenum.CmdPacketIncreaseStatus) {
		t.Fatal("op30 SP/TP books need the active-PVF skill catalog and point baseline")
	}
	service := &Service{}
	if _, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketIncreaseStatus); err == nil {
		t.Fatal("op30 must ask for the stackable-use resolver, so a missing catalog has to be reported")
	}
	if _, err := service.alignedDirectExperienceConsumablePlannerForCommand(dnfenum.CmdPacketIncreaseStatus); err == nil {
		t.Fatal("op30 must ask for the direct-experience planner, so a missing catalog has to be reported")
	}
	// An unrelated opcode still receives no resolver and no error.
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketSortItem)
	if resolver != nil || err != nil {
		t.Fatalf("unrelated opcode resolver=%v err=%v", resolver != nil, err)
	}
}

func TestIncreaseStatusRealPVFResolvesEveryBookTheClientSendsItFor(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime op30 book families")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketIncreaseStatus)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, want := range map[int64]struct {
		path  string
		kind  alignedcmd.SkillPointBookKind
		delta int64
	}{
		1031:  {path: "stackable/book_skill1.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 5},
		1038:  {path: "stackable/book_skill2.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 20},
		1204:  {path: "stackable/book_fskill1.stk", kind: alignedcmd.SkillPointBookAddTP, delta: 1},
		1205:  {path: "stackable/book_fskill2.stk", kind: alignedcmd.SkillPointBookAddTP, delta: 5},
		80003: {path: "stackable/extention/test_book_skill1.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 5},
		1206:  {path: "stackable/book_fskillreset.stk", kind: alignedcmd.SkillPointBookResetTP},
		1253:  {path: "stackable/book_fskillreset_event.stk", kind: alignedcmd.SkillPointBookResetTP},
	} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.PVFPath != want.path || resolution.TPBook == nil ||
			resolution.TPBook.Kind != want.kind || resolution.TPBook.Delta != want.delta ||
			resolution.TPBook.MinimumLevel <= 0 {
			t.Fatalf("runtime SP/TP book %d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}

	// Every runtime [increase status type] item is [etc], the same stackable
	// family the client sends op30 for. They must reach op30 through exactly the
	// planner op44 already owns, with their amounts read from the PVF operands.
	capsules := 0
	// The six numbered character-experience books, on the other hand, declare no
	// effect at all in the active PVF: they are bare [etc] stackables whose only
	// numeric trace is the localized name. They reach the same planner through
	// the path-keyed fallback in currentPVFCharacterExperienceBook, audited
	// against that name, so no stackable/book_exp* path may stay unowned here.
	undeclaredCharacterBooks := make([]string, 0, 6)
	ownedCharacterBooks := make([]string, 0, 6)
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve stackable item=%d: %v", itemID, resolveErr)
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil {
			continue
		}
		if normalizeDirectExperienceType(definition.DirectExperienceType) != "" {
			if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" {
				t.Fatalf("increase-status item %d path=%s is %q, not [etc]", itemID, definition.PVFPath, definition.StackableType)
			}
			if resolution.DirectExperience == nil ||
				normalizeDirectExperienceType(resolution.DirectExperience.EffectType) == "" ||
				len(resolution.DirectExperience.EffectValues) == 0 {
				t.Fatalf("increase-status item %d path=%s resolution=%+v", itemID, definition.PVFPath, resolution)
			}
			capsules++
		}
		normalized := strings.ToLower(strings.ReplaceAll(definition.PVFPath, "\\", "/"))
		if !strings.HasPrefix(normalized, "stackable/book_exp") || strings.Contains(normalized, "guild") {
			continue
		}
		if resolution.DirectExperience == nil || resolution.TPBook != nil ||
			normalizeDirectExperienceType(resolution.DirectExperience.EffectType) != "[expup]" ||
			len(resolution.DirectExperience.EffectValues) != 1 ||
			resolution.DirectExperience.EffectValues[0] <= 0 {
			undeclaredCharacterBooks = append(undeclaredCharacterBooks, definition.PVFPath)
			continue
		}
		ownedCharacterBooks = append(ownedCharacterBooks, definition.PVFPath)
	}
	if capsules < 20 {
		t.Fatalf("runtime [increase status type] items reachable through op30: %d", capsules)
	}
	sort.Strings(undeclaredCharacterBooks)
	if len(undeclaredCharacterBooks) != 0 {
		t.Fatalf("op30 character-experience books still without a grant (%d): %v",
			len(undeclaredCharacterBooks), undeclaredCharacterBooks)
	}
	sort.Strings(ownedCharacterBooks)
	if len(ownedCharacterBooks) != len(currentCharacterExperienceBookAmounts) {
		t.Fatalf("op30 owns %d character-experience books, want %d: %v",
			len(ownedCharacterBooks), len(currentCharacterExperienceBookAmounts), ownedCharacterBooks)
	}
}
