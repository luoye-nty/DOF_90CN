package dnfbridge

import (
	"fmt"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// currentCharacterExperienceBookAmounts is the expectation the table in
// stackable_use_pvf.go has to keep reproducing, spelled out separately so a
// silent edit of one of the two cannot pass unnoticed.
var currentCharacterExperienceBookAmounts = map[int64]struct {
	path   string
	amount int64
}{
	1034: {path: "stackable/book_exp1.stk", amount: 100},
	1035: {path: "stackable/book_exp2.stk", amount: 1000},
	1036: {path: "stackable/book_exp3.stk", amount: 10000},
	1037: {path: "stackable/book_exp4.stk", amount: 100000},
	1231: {path: "stackable/book_exp5.stk", amount: 1000000},
	1232: {path: "stackable/book_exp6.stk", amount: 10000000},
}

// currentCharacterExperienceBookRefusedItems are the two families that spend a
// different experience currency. They must keep resolving to nothing so op30
// answers them with its failure ACK.
var currentCharacterExperienceBookRefusedItems = []int64{1121, 1122, 1123, 1200, 1201, 1202}

func newCharacterExperienceBookTestSource() dungeonDropCatalogTestSource {
	return dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "1034 `book_exp1.stk`\n1035 `book_exp2.stk`\n1036 `book_exp3.stk`\n1037 `book_exp4.stk`\n1231 `book_exp5.stk`\n1232 `book_exp6.stk`\n1121 `book_duelexp1.stk`\n1200 `book_guildexp1.stk`\n",
		"stackable/book_exp1.stk": "[name] `增加100经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_exp2.stk": "[name] `增加1000经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_exp3.stk": "[name] `增加10000经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_exp4.stk": "[name] `增加100000经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_exp5.stk": "[name] `增加1000000经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_exp6.stk": "[name] `增加10000000经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		// The two refused families are shaped exactly like the six above, so
		// only the table separates them.
		"stackable/book_duelexp1.stk":  "[name] `增加100决斗场经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
		"stackable/book_guildexp1.stk": "[name] `增加100公会经验值的书`\n[minimum level] 1\n[stackable type] `[etc]` 1\n",
	}
}

func TestAlignedStackableUseResolverGrantsNumberedCharacterExperienceBooks(t *testing.T) {
	catalog, err := newPVFDungeonDropCatalog(newCharacterExperienceBookTestSource())
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketIncreaseStatus)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, want := range currentCharacterExperienceBookAmounts {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.PVFPath != want.path || resolution.StackableType != "[etc]" ||
			resolution.DirectExperience == nil || resolution.TPBook != nil {
			t.Fatalf("experience book %d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
		effect := *resolution.DirectExperience
		if effect.ItemID != itemID || effect.PVFPath != want.path || effect.EffectType != "[expup]" ||
			len(effect.EffectValues) != 1 || effect.EffectValues[0] != want.amount {
			t.Fatalf("experience book %d effect=%+v want amount %d", itemID, effect, want.amount)
		}
	}
	for _, itemID := range []int64{1121, 1200} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.DirectExperience != nil || resolution.TPBook != nil {
			t.Fatalf("duel/guild experience book %d must stay unowned: resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}
}

func TestCurrentPVFCharacterExperienceBookTableMatchesDeclaredAmounts(t *testing.T) {
	for _, want := range currentCharacterExperienceBookAmounts {
		amount, found := currentPVFCharacterExperienceBook(want.path)
		if !found || amount != want.amount {
			t.Fatalf("table row %s amount=%d found=%t, want %d", want.path, amount, found, want.amount)
		}
	}
	for _, path := range []string{
		"stackable/book_duelexp1.stk",
		"stackable/book_duelexp2.stk",
		"stackable/book_duelexp3.stk",
		"stackable/book_guildexp1.stk",
		"stackable/book_guildexp2.stk",
		"stackable/book_guildexp3.stk",
		"stackable/book_fskillreset.stk",
	} {
		if amount, found := currentPVFCharacterExperienceBook(path); found {
			t.Fatalf("path %s must stay out of the table, got amount %d", path, amount)
		}
	}
}

func TestAuditCurrentPVFCharacterExperienceBookRejectsDriftedContracts(t *testing.T) {
	valid := dungeonDropItemDefinition{
		ItemID:        1034,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/book_exp1.stk",
		Name:          "增加100经验值的书",
		StackableType: "`[etc]`",
	}
	if err := auditCurrentPVFCharacterExperienceBook(valid, 100); err != nil {
		t.Fatalf("valid contract rejected: %v", err)
	}
	// A wrong declared amount stops matching the name the PVF itself carries.
	if err := auditCurrentPVFCharacterExperienceBook(valid, 1000); err == nil {
		t.Fatal("declared amount 1000 must not pass against the name 增加100经验值的书")
	}
	// The reverse direction: the shorter prefix must not slide into the longer
	// name either, or every book would validate against the smallest amount.
	thousand := valid
	thousand.Name = "增加1000经验值的书"
	if err := auditCurrentPVFCharacterExperienceBook(thousand, 100); err == nil {
		t.Fatal("declared amount 100 must not pass against the name 增加1000经验值的书")
	}
	for name, drifted := range map[string]dungeonDropItemDefinition{
		"waste stackable": func() dungeonDropItemDefinition {
			d := valid
			d.StackableType = "`[waste]`"
			return d
		}(),
		"declared action": func() dungeonDropItemDefinition {
			d := valid
			d.ActionType = "`[level up ticket]`"
			return d
		}(),
		"declared increase status type": func() dungeonDropItemDefinition {
			d := valid
			d.DirectExperienceType = "`[expUp]`"
			return d
		}(),
		"renamed entry": func() dungeonDropItemDefinition {
			d := valid
			d.Name = "增加100决斗场经验值的书"
			return d
		}(),
	} {
		if err := auditCurrentPVFCharacterExperienceBook(drifted, 100); err == nil {
			t.Fatalf("%s must fail the experience-book audit", name)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFAuditsEveryCharacterExperienceBook(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit the runtime character experience books")
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
	for itemID, want := range currentCharacterExperienceBookAmounts {
		definition, defErr := catalog.ResolveItem(uint32(itemID))
		if defErr != nil {
			t.Fatalf("resolve runtime experience book %d: %v", itemID, defErr)
		}
		if definition.PVFPath != want.path {
			t.Fatalf("runtime experience book %d path=%s, want %s", itemID, definition.PVFPath, want.path)
		}
		if normalizeDungeonDropStackableType(definition.StackableType) != "[etc]" {
			t.Fatalf("runtime experience book %d path=%s type=%q, want [etc]", itemID, definition.PVFPath, definition.StackableType)
		}
		gotName := fmt.Sprintf("%q", definition.Name)
		if auditErr := auditCurrentPVFCharacterExperienceBook(definition, want.amount); auditErr != nil {
			t.Fatalf("runtime experience book %d path=%s name=%s: %v", itemID, definition.PVFPath, gotName, auditErr)
		}
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.DirectExperience == nil || resolution.TPBook != nil ||
			resolution.DirectExperience.EffectType != "[expup]" ||
			len(resolution.DirectExperience.EffectValues) != 1 ||
			resolution.DirectExperience.EffectValues[0] != want.amount {
			t.Fatalf("runtime experience book %d name=%s resolution=%+v err=%v", itemID, gotName, resolution, resolveErr)
		}
	}
	for _, itemID := range currentCharacterExperienceBookRefusedItems {
		definition, defErr := catalog.ResolveItem(uint32(itemID))
		if defErr != nil {
			t.Fatalf("resolve runtime refused experience book %d: %v", itemID, defErr)
		}
		if _, found := currentPVFCharacterExperienceBook(definition.PVFPath); found {
			t.Fatalf("runtime item %d path=%s must stay out of the experience-book table", itemID, definition.PVFPath)
		}
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.DirectExperience != nil || resolution.TPBook != nil {
			t.Fatalf("runtime duel/guild experience book %d path=%s must stay unowned: resolution=%+v err=%v",
				itemID, definition.PVFPath, resolution, resolveErr)
		}
	}
}
