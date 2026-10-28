package dnfbridge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	"longheng.io/server/internal/modules/dnf/progression"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestAlignedStackableUseResolverAuthorizesPVFWasteAndCompletePetFeedOnly(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                        "",
		"equipment/equipment.lst":                    "",
		"stackable/stackable.lst":                    "24 `cash/creature/creature_food.stk`\n25 `cash/creature/broken_food.stk`\n26 `event/percent_capsule.stk`\n27 `storybook/chapter.stk`\n30 `transform/town_monster.stk`\n31 `transform/random_town.stk`\n32 `transform/dungeon_riding.stk`\n33 `transform/dungeon_ridable.stk`\n34 `transform/town_celebrate.stk`\n35 `forgotten_land/potion.stk`\n1031 `book_skill1.stk`\n1038 `book_skill2.stk`\n1204 `book_fskill1.stk`\n1205 `book_fskill2.stk`\n1206 `book_fskillreset.stk`\n1253 `book_fskillreset_event.stk`\n6002 `projectile/boomerang.stk`\n6009 `projectile/shuriken.stk`\n6012 `projectile/bomb.stk`\n80003 `extention/test_book_skill1.stk`\n2683638 `event/random_level_up_ticket.stk`\n10003634 `event/level_up_ticket.stk`\n10164630 `10164001/10164630.stk`\n490701712 `cash/chn_490701712.stk`\n490701713 `cash/chn_490701713.stk`\n",
		"stackable/cash/creature/creature_food.stk":  "[stackable type] `[feed]` 1\n[int data]\n300 -20 30\n[/int data]\n",
		"stackable/cash/creature/broken_food.stk":    "[stackable type] `[feed]` 1\n[int data]\n300 -20\n[/int data]\n",
		"stackable/event/percent_capsule.stk":        "[stackable type] `[etc]`\n[increase status type] `[expUpByPercent]` 15\n",
		"stackable/storybook/chapter.stk":            "[stackable type] `[waste]`\n[minimum level] 85\n[maximum level] 99\n[action type] `[story book]` 3142\n[/action type]\n",
		"stackable/transform/town_monster.stk":       "[stackable type] `[disguise]`\n[string data] `monster`\n[int data] 61584 300\n",
		"stackable/transform/random_town.stk":        "[stackable type] `[random disguise]`\n[string data] `monster` `creature`\n[int data] 64703 300 191 300\n",
		"stackable/transform/dungeon_riding.stk":     "[stackable type] `[waste]`\n[action type] `[riding]` 107000902 300000\n[action usable place] `[dungeon]`\n[action usable dungeon] 100 101\n",
		"stackable/transform/dungeon_ridable.stk":    "[stackable type] `[waste]`\n[action type] `[ridable item]` 64734\n[action usable place] `[dungeon]` `[dont use assult]`\n[limit dungeon] 200 201\n",
		"stackable/transform/town_celebrate.stk":     "[stackable type] `[etc]`\n[action type] `[celebrate big transition]` 1 2 3\n[action usable place] `[village]` `[seria room]`\n[int data] 300\n",
		"stackable/forgotten_land/potion.stk":        "[stackable type] `[waste]`\n[action type] `[hp recovery]` `%` 50 500 `myself`\n[action usable place] `[dungeon]`\n[action usable dungeon] 4000\n",
		"stackable/book_skill1.stk":                  "[name] `SP+5 skill book`\n[minimum level] 1\n[stackable type] `[etc]` 8\n",
		"stackable/book_skill2.stk":                  "[name] `SP+20 skill book`\n[minimum level] 1\n[stackable type] `[etc]` 8\n",
		"stackable/book_fskill1.stk":                 "[name] `TP+1 skill book`\n[minimum level] 50\n[stackable type] `[etc]` 8\n",
		"stackable/book_fskill2.stk":                 "[name] `TP+5 skill book`\n[minimum level] 50\n[stackable type] `[etc]` 8\n",
		"stackable/book_fskillreset.stk":             "[name] `TP reset book`\n[minimum level] 50\n[stackable type] `[etc]` 8\n",
		"stackable/book_fskillreset_event.stk":       "[name] `event TP reset book`\n[minimum level] 50\n[stackable type] `[etc]` 8\n",
		"stackable/projectile/boomerang.stk":         "[stackable type] `[throw]`\n[int data] 101 2 3 4 5 6\n[string data] `particle/a.ptl`\n",
		"stackable/projectile/shuriken.stk":          "[stackable type] `[throw]`\n[int data] 102 2 3 4 5 6\n",
		"stackable/projectile/bomb.stk":              "[stackable type] `[set]`\n[int data] 103 2 3 4 5 6\n[action usable place] `[dungeon]`\n",
		"stackable/extention/test_book_skill1.stk":   "[name] `extension SP+5 skill book`\n[minimum level] 1\n[stackable type] `[etc]` 8\n",
		"stackable/10164001/10164630.stk":            "[stackable type] `[etc]` 1\n[minimum level] 90\n[maximum level] 90\n[action type] `[gm quest clear]`\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n",
		"stackable/event/level_up_ticket.stk":        "[stackable type] `[etc]` 1\n[minimum level] 1\n[maximum level] 79\n[levelup range] 1\n[action type] `[level up ticket]`\n[/action type]\n",
		"stackable/event/random_level_up_ticket.stk": "[stackable type] `[etc]` 1\n[minimum level] 55\n[maximum level] 82\n[levelup range] 3\n[action type] `[level up ticket]`\n[/action type]\n[booster info]\n[etc]\n1 1 50000 1 1 30000 2 1 10000 3\n[/etc]\n[/booster info]\n",
		"stackable/cash/chn_490701712.stk":           "[stackable type] `[waste]`\n",
		"stackable/cash/chn_490701713.stk":           "[stackable type] `[material]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	waste, err := resolver(490701712)
	if err != nil || waste.ItemID != 490701712 || waste.PVFPath != "stackable/cash/chn_490701712.stk" || waste.StackableType != "[waste]" {
		t.Fatalf("waste resolution=%+v err=%v", waste, err)
	}
	for itemID, wantAction := range map[int64]string{
		30: "[disguise]",
		31: "[random disguise]",
		32: "[riding]",
		33: "[ridable item]",
		34: "[celebrate big transition]",
	} {
		got, resolveErr := resolver(itemID)
		if resolveErr != nil || got.Transformation == nil || got.Transformation.ActionType != wantAction || len(got.Transformation.FormIDs) == 0 {
			t.Fatalf("transformation item=%d resolution=%+v err=%v", itemID, got, resolveErr)
		}
	}
	if town, _ := resolver(30); !town.Transformation.VillageAllowed || !town.Transformation.SeriaRoomAllowed || town.Transformation.DungeonAllowed || town.Transformation.DurationSeconds != 300 {
		t.Fatalf("town disguise contract=%+v", town.Transformation)
	}
	if riding, _ := resolver(32); !riding.Transformation.DungeonAllowed || riding.Transformation.VillageAllowed || riding.Transformation.DurationSeconds != 300 || fmt.Sprint(riding.Transformation.ActionUsableDungeonIDs) != "[100 101]" {
		t.Fatalf("dungeon riding contract=%+v", riding.Transformation)
	}
	if ridable, _ := resolver(33); !ridable.Transformation.DungeonAllowed || !ridable.Transformation.DisallowAssault || fmt.Sprint(ridable.Transformation.LimitDungeonIDs) != "[200 201]" {
		t.Fatalf("dungeon ridable contract=%+v", ridable.Transformation)
	}
	forgottenPotion, err := resolver(35)
	if err != nil || forgottenPotion.UsePlace == nil || !forgottenPotion.UsePlace.DungeonAllowed ||
		forgottenPotion.UsePlace.VillageAllowed || fmt.Sprint(forgottenPotion.UsePlace.ActionUsableDungeonIDs) != "[4000]" {
		t.Fatalf("forgotten-land potion resolution=%+v err=%v", forgottenPotion, err)
	}
	feed, err := resolver(24)
	if err != nil || feed.ItemID != 24 || feed.PVFPath != "stackable/cash/creature/creature_food.stk" || feed.StackableType != "[feed]" ||
		feed.PetFeedEffectDurationSeconds != 300 || feed.PetFeedSkillChargePercent != -20 || feed.PetFeedSatietyDelta != 30 {
		t.Fatalf("feed resolution=%+v err=%v", feed, err)
	}
	if broken, err := resolver(25); err == nil || broken != (alignedcmd.StackableUseResolution{}) {
		t.Fatalf("broken feed resolution=%+v err=%v", broken, err)
	}
	percentCapsule, err := resolver(26)
	if err != nil || percentCapsule.DirectExperience == nil || percentCapsule.DirectExperience.EffectType != "[exppercentup]" ||
		len(percentCapsule.DirectExperience.EffectValues) != 1 || percentCapsule.DirectExperience.EffectValues[0] != 15 {
		t.Fatalf("percent capsule resolution=%+v err=%v", percentCapsule, err)
	}
	storyBook, err := resolver(27)
	if err != nil || storyBook.StoryBook == nil || storyBook.StoryBook.ActionType != "[story book]" ||
		storyBook.StoryBook.QuestID != 3142 || storyBook.StoryBook.MinimumLevel != 85 || storyBook.StoryBook.MaximumLevel != 99 {
		t.Fatalf("story-book resolution=%+v err=%v", storyBook, err)
	}
	bookCases := map[int64]alignedcmd.TPBookResolution{
		1031:  {Kind: alignedcmd.SkillPointBookAddSP, Delta: 5, MinimumLevel: 1},
		1038:  {Kind: alignedcmd.SkillPointBookAddSP, Delta: 20, MinimumLevel: 1},
		1204:  {Kind: alignedcmd.SkillPointBookAddTP, Delta: 1, MinimumLevel: 50},
		1205:  {Kind: alignedcmd.SkillPointBookAddTP, Delta: 5, MinimumLevel: 50},
		1206:  {Kind: alignedcmd.SkillPointBookResetTP, MinimumLevel: 50},
		1253:  {Kind: alignedcmd.SkillPointBookResetTP, MinimumLevel: 50},
		80003: {Kind: alignedcmd.SkillPointBookAddSP, Delta: 5, MinimumLevel: 1},
	}
	for itemID, want := range bookCases {
		book, resolveErr := resolver(itemID)
		if resolveErr != nil || book.ItemID != itemID || book.StackableType != "[etc]" ||
			book.TPBook == nil || *book.TPBook != want {
			t.Fatalf("SP/TP book %d resolution=%+v want=%+v err=%v", itemID, book, want, resolveErr)
		}
	}
	for itemID, wantType := range map[int64]string{6002: "[throw]", 6009: "[throw]", 6012: "[set]"} {
		projectile, resolveErr := resolver(itemID)
		if resolveErr != nil || projectile.Projectile == nil || projectile.Projectile.StackableType != wantType ||
			projectile.Projectile.EffectID <= 0 || len(projectile.Projectile.IntData) != 6 || !projectile.Projectile.DungeonAllowed {
			t.Fatalf("projectile %d resolution=%+v err=%v", itemID, projectile, resolveErr)
		}
	}
	material, err := resolver(490701713)
	if err != nil || material != (alignedcmd.StackableUseResolution{}) {
		t.Fatalf("material resolution=%+v err=%v", material, err)
	}
	actionResolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackableAction)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := actionResolver(10164630)
	if err != nil || ticket.ItemID != 10164630 || ticket.QuestCompletionTicket == nil ||
		ticket.QuestCompletionTicket.ActionType != "[gm quest clear]" || !ticket.QuestCompletionTicket.VillageOnly {
		t.Fatalf("op515 quest ticket resolution=%+v err=%v", ticket, err)
	}
	eventLevelUpResolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketRequestEventServerLevelUp)
	if err != nil || eventLevelUpResolver == nil {
		t.Fatalf("op418 resolver=%v err=%v", eventLevelUpResolver, err)
	}
	eventLevelUpTicket, err := eventLevelUpResolver(10003634)
	if err != nil || eventLevelUpTicket.DirectExperience == nil || eventLevelUpTicket.QuestCompletionTicket == nil ||
		eventLevelUpTicket.DirectExperience.EffectType != "[level up ticket]" {
		t.Fatalf("op418 level-up ticket resolution=%+v err=%v", eventLevelUpTicket, err)
	}
	levelUpTicket, err := resolver(10003634)
	if err != nil || levelUpTicket.ItemID != 10003634 || levelUpTicket.DirectExperience == nil ||
		levelUpTicket.QuestCompletionTicket == nil || levelUpTicket.DirectExperience.EffectType != "[level up ticket]" ||
		len(levelUpTicket.DirectExperience.EffectValues) != 1 || levelUpTicket.DirectExperience.EffectValues[0] != 1 ||
		levelUpTicket.DirectExperience.MinimumLevel != 1 || levelUpTicket.DirectExperience.MaximumLevel != 79 ||
		levelUpTicket.QuestCompletionTicket.ActionType != "[quest clear]" {
		t.Fatalf("level-up ticket resolution=%+v err=%v", levelUpTicket, err)
	}
	randomDefinition, err := catalog.ResolveItem(2683638)
	if err != nil || len(randomDefinition.LevelUpOutcomes) != 3 ||
		randomDefinition.LevelUpOutcomes[0] != (levelUpTicketOutcome{Range: 1, Weight: 50000}) ||
		randomDefinition.LevelUpOutcomes[1] != (levelUpTicketOutcome{Range: 2, Weight: 30000}) ||
		randomDefinition.LevelUpOutcomes[2] != (levelUpTicketOutcome{Range: 3, Weight: 10000}) {
		t.Fatalf("random level-up definition=%+v err=%v", randomDefinition, err)
	}
	randomLevelUpTicket, err := resolver(2683638)
	if err != nil || randomLevelUpTicket.DirectExperience == nil || len(randomLevelUpTicket.DirectExperience.EffectValues) != 1 ||
		randomLevelUpTicket.DirectExperience.EffectValues[0] < 1 || randomLevelUpTicket.DirectExperience.EffectValues[0] > 3 {
		t.Fatalf("random level-up ticket resolution=%+v err=%v", randomLevelUpTicket, err)
	}
}

func TestAlignedStackableUseResolverRecognizesFatiguePotionPVFContract(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":             "",
		"equipment/equipment.lst":         "",
		"stackable/stackable.lst":         "9001 `cash/fatigue.stk`\n9002 `event/fatigue_one.stk`\n",
		"stackable/cash/fatigue.stk":      "[name] `fatigue 50`\n[explain] `疲劳值低于106时才能使用；每日限用3个`\n[minimum level] 1\n[stackable type] `[expert town potion]` 0\n[action type] `[add fatigue]` 50\n[/action type]\n[total usable count] 3\n[action usable place] `[village]` `[seria room]` `[dont use assult]`\n[/action usable place]\n",
		"stackable/event/fatigue_one.stk": "[name] `fatigue 1`\n[explain] `fatigue below 155, daily 3`\n[minimum level] 1\n[stackable type] `[expert town potion]` 0\n[action type] `[add fatigue]` 1\n[/action type]\n[total usable count] 3\n[action usable place] `[village]`\n[/action usable place]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackableAction)
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolver(9001)
	if err != nil || first.ItemID != 9001 || first.FatigueRecovery != 50 || first.FatigueMaximumRemaining != 106 || first.DailyUseLimit != 3 {
		t.Fatalf("50-point fatigue resolution=%+v err=%v", first, err)
	}
	second, err := resolver(9002)
	if err != nil || second.FatigueRecovery != 1 || second.FatigueMaximumRemaining != 155 || second.DailyUseLimit != 3 {
		t.Fatalf("1-point fatigue resolution=%+v err=%v", second, err)
	}
}

// [total usable count] 不是疲劳药专属：任何 stackable（例如 [waste] 成长秘药）
// 声明它都必须解析出每日上限，否则服务端永远不会拒绝第 N+1 次使用；没有该字段的
// 道具必须保持 0（= 无上限、也不多做任何事）。
func TestAlignedStackableUseResolverReadsTotalUsableCountForAnyStackable(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "10153501 `10153001/10153501.stk`\n10153502 `10153001/10153502.stk`\n",
		"stackable/10153001/10153501.stk": "[name] `远古精灵的秘药`\n[stackable type] `[waste]`\n[minimum level] 1\n" +
			"[total usable count] 10\n",
		"stackable/10153001/10153502.stk": "[name] `无上限材料`\n[stackable type] `[waste]`\n[minimum level] 1\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	capped, err := resolver(10153501)
	if err != nil || capped.ItemID != 10153501 || capped.DailyUseLimit != 10 {
		t.Fatalf("capped waste resolution=%+v err=%v", capped, err)
	}
	uncapped, err := resolver(10153502)
	if err != nil || uncapped.ItemID != 10153502 || uncapped.DailyUseLimit != 0 {
		t.Fatalf("uncapped waste resolution=%+v err=%v", uncapped, err)
	}
}

func TestAlignedStackableUseResolverRecognizesDungeonAdmissionTickets(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "10158692 `10158001/10158692.stk`\n490701703 `cash/chn_490701703.stk`\n",
		"stackable/10158001/10158692.stk": "[stackable type] `[waste]`\n[minimum level] 70\n[action type] `[add dimension dungeon enter count]` 5\n" +
			"[action usable place] `[village]` `[seria room]`\n",
		"stackable/cash/chn_490701703.stk": "[stackable type] `[waste]`\n[minimum level] 90\n[action type] `[up layer despair dungeon]` 5115\n" +
			"[action usable place] `[village]`\n[total usable count] 2\n[use action packet] 1\n",
	}
	itemCatalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	admissionCatalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: itemCatalog, dungeonAdmissionCatalog: admissionCatalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	dimension, err := resolver(10158692)
	if err != nil || dimension.DungeonAdmissionTicket == nil || len(dimension.DungeonAdmissionTicket.Grants) != 1 ||
		dimension.DungeonAdmissionTicket.Grants[0].Policy.Key != "dimension_gate.character.daily" ||
		dimension.DungeonAdmissionTicket.Grants[0].Amount != 5 || dimension.DungeonAdmissionTicket.Grants[0].MaximumBonus != 5 {
		t.Fatalf("dimension ticket=%+v err=%v", dimension, err)
	}
	tower, err := resolver(490701703)
	if err != nil || tower.DungeonAdmissionTicket == nil || tower.DungeonAdmissionTicket.TargetDungeonID != 5115 ||
		tower.DungeonAdmissionTicket.ActionPacket != 1 || len(tower.DungeonAdmissionTicket.Grants) != 2 || tower.DungeonAdmissionTicket.UseLimit.Limit != 2 ||
		tower.DungeonAdmissionTicket.Grants[0].Amount != 1 || tower.DungeonAdmissionTicket.Grants[1].Amount != 1 {
		t.Fatalf("tower ticket=%+v err=%v", tower, err)
	}
}

func TestAlignedStackableUseResolverRealPVFRecognizesDungeonAdmissionTickets(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify dungeon admission tickets")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	itemCatalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	admissionCatalog, err := dnfdungeonadmission.LoadCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: itemCatalog, dungeonAdmissionCatalog: admissionCatalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, action := range map[int64]string{
		10158692:  "[add dimension dungeon enter count]",
		490701703: "[up layer despair dungeon]",
		10099882:  "[up layer despair dungeon]",
		10158917:  "[up layer despair dungeon]",
		10302670:  "[up layer despair dungeon]",
		10161291:  "[up layer despair dungeon]",
	} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.DungeonAdmissionTicket == nil || resolution.DungeonAdmissionTicket.ActionType != action ||
			(action == "[up layer despair dungeon]" && resolution.DungeonAdmissionTicket.ActionPacket != 1) ||
			(action == "[add dimension dungeon enter count]" && resolution.DungeonAdmissionTicket.ActionPacket != 0) {
			t.Fatalf("item=%d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFCoversPeaceWingStoryBooks(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime story books")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, questID := range map[int64]int64{
		10149084: 3140,
		10149085: 3141,
		10149086: 3142,
		10149087: 3143,
		10149088: 3144,
	} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.StoryBook == nil || resolution.StoryBook.QuestID != questID ||
			resolution.StoryBook.ActionType != "[story book]" || resolution.StackableType != "[waste]" {
			t.Fatalf("story-book item=%d quest=%d resolution=%+v err=%v", itemID, questID, resolution, resolveErr)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFCoversForgottenLandWaste(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify Forgotten Land consumables")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{
		10092913, 10092914, 10092915, 10092916, 10092917, 10092918,
		10093345, 10093346, 10093469, 10093470, 10093471, 10093472,
	} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.ItemID != itemID || resolution.StackableType != "[waste]" ||
			resolution.UsePlace == nil || !resolution.UsePlace.DungeonAllowed || resolution.UsePlace.VillageAllowed ||
			fmt.Sprint(resolution.UsePlace.ActionUsableDungeonIDs) != "[4000]" {
			t.Fatalf("Forgotten Land item=%d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFCoversEveryTransformationConsumable(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every runtime transformation consumable")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve transformation candidate item=%d: %v", itemID, resolveErr)
		}
		transformation, transformErr := currentPVFTransformationUseResolution(definition)
		if transformErr != nil {
			t.Fatalf("parse transformation item=%d path=%s: %v", itemID, definition.PVFPath, transformErr)
		}
		if transformation == nil {
			continue
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil || resolution.Transformation == nil || resolution.Transformation.ActionType != transformation.ActionType {
			t.Fatalf("resolve transformation item=%d path=%s resolution=%+v err=%v", itemID, definition.PVFPath, resolution, resolveErr)
		}
		counts[transformation.ActionType]++
	}
	want := map[string]int{
		"[disguise]":                 267,
		"[random disguise]":          1,
		"[riding]":                   53,
		"[ridable item]":             27,
		"[celebrate big transition]": 1,
	}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("transformation census=%v want=%v", counts, want)
	}
}

func TestAlignedStackableUseResolverRealPVFRecognizesEverySkillPointBook(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime SP/TP point books")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for itemID, want := range map[int64]struct {
		path    string
		kind    alignedcmd.SkillPointBookKind
		delta   int64
		minimum int64
	}{
		1031:  {path: "stackable/book_skill1.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 5, minimum: 1},
		1038:  {path: "stackable/book_skill2.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 20, minimum: 1},
		1204:  {path: "stackable/book_fskill1.stk", kind: alignedcmd.SkillPointBookAddTP, delta: 1, minimum: 50},
		1205:  {path: "stackable/book_fskill2.stk", kind: alignedcmd.SkillPointBookAddTP, delta: 5, minimum: 50},
		1206:  {path: "stackable/book_fskillreset.stk", kind: alignedcmd.SkillPointBookResetTP, minimum: 50},
		1253:  {path: "stackable/book_fskillreset_event.stk", kind: alignedcmd.SkillPointBookResetTP, minimum: 50},
		80003: {path: "stackable/extention/test_book_skill1.stk", kind: alignedcmd.SkillPointBookAddSP, delta: 5, minimum: 1},
	} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.PVFPath != want.path || resolution.TPBook == nil ||
			resolution.TPBook.Kind != want.kind || resolution.TPBook.Delta != want.delta ||
			resolution.TPBook.MinimumLevel != want.minimum {
			t.Fatalf("runtime SP/TP book %d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFCoversEveryProjectile(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every runtime throw/set item")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	invalid := make([]string, 0)
	skippedNoop := make([]string, 0)
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve projectile candidate item=%d: %v", itemID, resolveErr)
		}
		stackableType := normalizeDungeonDropStackableType(definition.StackableType)
		if stackableType != "[throw]" && stackableType != "[set]" {
			continue
		}
		projectile, projectileErr := currentPVFProjectileUseResolution(definition)
		if projectileErr != nil {
			invalid = append(invalid, fmt.Sprintf("item=%d path=%s type=%s ints=%v err=%v", itemID, definition.PVFPath, stackableType, definition.ActionIntData, projectileErr))
			continue
		}
		if projectile == nil {
			skippedNoop = append(skippedNoop, definition.PVFPath)
			continue
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil || resolution.Projectile == nil || resolution.Projectile.EffectID != projectile.EffectID ||
			resolution.Projectile.StackableType != stackableType || !resolution.Projectile.DungeonAllowed {
			t.Fatalf("resolve projectile item=%d path=%s resolution=%+v err=%v", itemID, definition.PVFPath, resolution, resolveErr)
		}
		counts[stackableType]++
	}
	if len(invalid) != 0 {
		t.Fatalf("invalid runtime projectile contracts (%d): %v", len(invalid), invalid)
	}
	if fmt.Sprint(skippedNoop) != "[stackable/throw/dummy_throwing.stk]" {
		t.Fatalf("unexpected no-op projectiles=%v", skippedNoop)
	}
	want := map[string]int{"[throw]": 134, "[set]": 35}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("projectile census=%v want=%v", counts, want)
	}
}

func TestNormalizeDirectExperienceTypeKeepsLevelUpTicketAction(t *testing.T) {
	if got := normalizeDirectExperienceType("`[LEVEL UP TICKET]`"); got != "[level up ticket]" {
		t.Fatalf("normalized type=%q", got)
	}
}

func TestNormalizeDirectExperienceTypeCanonicalizesPercentCapsuleAlias(t *testing.T) {
	if got := normalizeDirectExperienceType("`[ExpUpByPercent]`"); got != "[exppercentup]" {
		t.Fatalf("normalized type=%q", got)
	}
}

func TestAlignedStackableUseResolverRealPVFCoversEveryDirectExperienceConsumable(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every runtime direct-experience consumable")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve stackable definition %d: %v", itemID, resolveErr)
		}
		effectType := normalizeDirectExperienceType(definition.DirectExperienceType)
		if effectType == "" {
			continue
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil {
			t.Fatalf("resolve direct-experience item=%d path=%s type=%q: %v", itemID, definition.PVFPath, definition.DirectExperienceType, resolveErr)
		}
		if resolution.DirectExperience == nil || resolution.DirectExperience.EffectType != effectType ||
			resolution.ItemID != int64(itemID) || resolution.PVFPath != definition.PVFPath ||
			resolution.DirectExperience.MinimumLevel != definition.MinimumLevel ||
			resolution.DirectExperience.MaximumLevel != definition.MaximumLevel ||
			fmt.Sprint(resolution.DirectExperience.EffectValues) != fmt.Sprint(definition.DirectExperienceValues) {
			t.Fatalf("direct-experience item=%d definition=%+v resolution=%+v", itemID, definition, resolution)
		}
		counts[effectType]++
	}
	for _, effectType := range []string{"[expup]", "[exppercentup]", "[expupbytable]", "[levelupbytable]"} {
		if counts[effectType] == 0 {
			t.Fatalf("runtime PVF type %q has no covered item: counts=%v", effectType, counts)
		}
	}
	if total := counts["[expup]"] + counts["[exppercentup]"] + counts["[expupbytable]"] + counts["[levelupbytable]"]; total != 522 {
		t.Fatalf("covered direct-experience total=%d want=522 counts=%v", total, counts)
	}
	for _, itemID := range []int64{690001552, 690001553, 690001554, 690001555} {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil || resolution.DirectExperience == nil {
			t.Fatalf("experience book item=%d resolution=%+v err=%v", itemID, resolution, resolveErr)
		}
	}
	t.Logf("covered runtime direct-experience consumables: %v", counts)
}

func TestDirectExperienceGainRealPVFCoversCapsulesAndExperienceBooks(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime direct-experience formulas")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := progression.Load(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		itemID   int64
		level    int
		wantGain uint32
	}{
		{name: "fixed experience book", itemID: 690001552, level: 20, wantGain: 1000},
		{name: "table experience capsule", itemID: 10158716, level: 50, wantGain: 422400},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolution, resolveErr := resolver(test.itemID)
			if resolveErr != nil || resolution.DirectExperience == nil {
				t.Fatalf("resolution=%+v err=%v", resolution, resolveErr)
			}
			gain, gainErr := directExperienceGain(tables, test.level, 0, *resolution.DirectExperience)
			if gainErr != nil || gain != test.wantGain {
				t.Fatalf("gain=%d want=%d err=%v resolution=%+v", gain, test.wantGain, gainErr, resolution)
			}
		})
	}

	percent, err := resolver(690001555)
	if err != nil || percent.DirectExperience == nil {
		t.Fatalf("percentage experience book resolution=%+v err=%v", percent, err)
	}
	upper, err := tables.ThresholdToNext(20)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := tables.ThresholdToNext(19)
	if err != nil {
		t.Fatal(err)
	}
	percentGain, err := directExperienceGain(tables, 20, lower, *percent.DirectExperience)
	if err != nil || percentGain != (upper-lower)/100 {
		t.Fatalf("percentage gain=%d want=%d err=%v", percentGain, (upper-lower)/100, err)
	}

	levelTable, err := resolver(10158963)
	if err != nil || levelTable.DirectExperience == nil {
		t.Fatalf("target-level capsule resolution=%+v err=%v", levelTable, err)
	}
	current, err := tables.ThresholdToNext(49)
	if err != nil {
		t.Fatal(err)
	}
	target, err := tables.ThresholdToNext(81)
	if err != nil {
		t.Fatal(err)
	}
	levelGain, err := directExperienceGain(tables, 50, current, *levelTable.DirectExperience)
	if err != nil || levelGain != target-current {
		t.Fatalf("target-level gain=%d want=%d err=%v", levelGain, target-current, err)
	}
}

func TestAlignedStackableUseResolverRealPVFCoversEveryLevelUpTicket(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every runtime level-up ticket")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketRequestEventServerLevelUp)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	ranges := make(map[int64]int)
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve stackable definition %d: %v", itemID, resolveErr)
		}
		if normalizeLevelUpTicketAction(definition.ActionType) == "" {
			continue
		}
		count++
		ranges[definition.LevelUpRange]++
		if definition.LevelUpRange > 1 {
			t.Logf("multi-level ticket item=%d range=%d name=%q path=%s", itemID, definition.LevelUpRange, definition.Name, definition.PVFPath)
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil {
			t.Fatalf("resolve level-up ticket %d path=%s: %v", itemID, definition.PVFPath, resolveErr)
		}
		if resolution.DirectExperience == nil || resolution.QuestCompletionTicket == nil ||
			resolution.DirectExperience.EffectType != "[level up ticket]" ||
			len(resolution.DirectExperience.EffectValues) != 1 || resolution.DirectExperience.EffectValues[0] <= 0 ||
			resolution.DirectExperience.EffectValues[0] > definition.LevelUpRange ||
			definition.LevelUpRange <= 0 {
			t.Fatalf("level-up ticket %d definition=%+v resolution=%+v", itemID, definition, resolution)
		}
	}
	if count == 0 {
		t.Fatal("runtime PVF has no [level up ticket] documents")
	}
	t.Logf("covered runtime level-up tickets: %d ranges=%v", count, ranges)
}

func TestDirectExperienceGainRealPVFLevelUpTicketTargetsExactLevels(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify level-up ticket progression thresholds")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := progression.Load(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		level        int
		rangeValue   int64
		currentTotal uint32
	}{
		{name: "one level", level: 1, rangeValue: 1},
		{name: "random maximum", level: 55, rangeValue: 3},
		{name: "ten levels", level: 1, rangeValue: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			targetTotal, thresholdErr := tables.ThresholdToNext(test.level + int(test.rangeValue) - 1)
			if thresholdErr != nil {
				t.Fatal(thresholdErr)
			}
			gain, gainErr := directExperienceGain(tables, test.level, test.currentTotal, alignedcmd.DirectExperienceConsumableResolution{
				EffectType:   "[level up ticket]",
				EffectValues: []int64{test.rangeValue},
			})
			if gainErr != nil {
				t.Fatal(gainErr)
			}
			if gain != targetTotal-test.currentTotal {
				t.Fatalf("level=%d range=%d gain=%d want=%d", test.level, test.rangeValue, gain, targetTotal-test.currentTotal)
			}
		})
	}
}

func TestAlignedStackableUseResolverRealPVFAuthorizesBothPetFeeds(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime pet feeds")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{24, 490701301} {
		resolution, err := resolver(itemID)
		if err != nil {
			t.Fatalf("resolve feed %d: %v", itemID, err)
		}
		if resolution.ItemID != itemID || resolution.StackableType != "[feed]" ||
			resolution.PetFeedEffectDurationSeconds != 300 ||
			resolution.PetFeedSkillChargePercent != -20 ||
			resolution.PetFeedSatietyDelta != 30 {
			t.Fatalf("feed %d resolution = %+v", itemID, resolution)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFRecognizesQuestTicketFamilies(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime quest tickets")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[int64]string{
		10164630: "[gm quest clear]",
		10301410: "[lv clear]",
		10303917: "[any quest clear]",
		10158730: "[achievement quest clear]",
		10162220: "[side quest clear]",
		10006007: "[quest clear]",
		10003197: "[quest clear coupon]",
		10164042: "[first awaken clear]",
		10303235: "[second awaken clear]",
	}
	for itemID, wantAction := range wants {
		resolution, resolveErr := resolver(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve ticket %d: %v", itemID, resolveErr)
		}
		if resolution.ItemID != itemID || resolution.QuestCompletionTicket == nil ||
			resolution.QuestCompletionTicket.ActionType != wantAction {
			t.Fatalf("ticket %d resolution=%+v want_action=%q", itemID, resolution, wantAction)
		}
	}
}

func TestAlignedStackableUseResolverRealPVFCoversEveryQuestTicketActionDocument(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every runtime quest ticket")
	}
	source, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, itemID := range catalog.stackableIDs {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve stackable definition %d: %v", itemID, resolveErr)
		}
		action := normalizeQuestCompletionTicketAction(definition.ActionType)
		if action == "" {
			continue
		}
		resolution, resolveErr := resolver(int64(itemID))
		if resolveErr != nil {
			t.Fatalf("resolve quest ticket %d action=%q: %v", itemID, action, resolveErr)
		}
		if resolution.QuestCompletionTicket == nil || resolution.QuestCompletionTicket.ActionType != action {
			t.Fatalf("quest ticket %d action=%q resolution=%+v", itemID, action, resolution)
		}
		counts[action]++
	}
	for _, action := range []string{
		"[any quest clear]", "[achievement quest clear]", "[first awaken clear]",
		"[second awaken clear]", "[quest clear]", "[quest clear coupon]",
		"[lv clear]", "[gm quest clear]", "[side quest clear]",
	} {
		if counts[action] == 0 {
			t.Fatalf("runtime PVF action %q has no covered document: counts=%v", action, counts)
		}
	}
	t.Logf("covered runtime quest ticket documents: %v", counts)
}

func TestAlignedStackableUseResolverRecognizesEveryQuestCompletionAction(t *testing.T) {
	actions := []struct {
		tag  string
		ints string
	}{
		{tag: "[any quest clear]", ints: "674 649"},
		{tag: "[achievement quest clear]"},
		{tag: "[first awaken clear]", ints: "1 1"},
		{tag: "[second awaken clear]", ints: "0 1"},
		{tag: "[quest clear]"},
		{tag: "[quest clear coupon]"},
		{tag: "[lv clear]", ints: "90"},
		{tag: "[gm quest clear]"},
		{tag: "[side quest clear]"},
	}
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
	}
	list := ""
	for index, action := range actions {
		itemID := 1000 + index
		path := fmt.Sprintf("stackable/ticket_%d.stk", itemID)
		list += fmt.Sprintf("%d `ticket_%d.stk`\n", itemID, itemID)
		source[path] = "[name] `ticket`\n[stackable type] `[etc]` 1\n[minimum level] 50\n[maximum level] 90\n[action type] `" +
			action.tag + "` " + action.ints +
			"\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n"
	}
	source["stackable/stackable.lst"] = list
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range actions {
		resolution, resolveErr := resolver(int64(1000 + index))
		if resolveErr != nil {
			t.Fatalf("resolve %q: %v", want.tag, resolveErr)
		}
		if resolution.QuestCompletionTicket == nil || resolution.QuestCompletionTicket.ActionType != strings.ToLower(want.tag) ||
			resolution.QuestCompletionTicket.MinimumLevel != 50 || resolution.QuestCompletionTicket.MaximumLevel != 90 ||
			!resolution.QuestCompletionTicket.VillageOnly {
			t.Fatalf("action %q resolution=%+v", want.tag, resolution)
		}
	}
}

// 扩容券三兄弟（[upgrade inventory] / [upgrade cargo] / [upgrade account cargo]）。
//
// 现场 2026-10-03（服主报，测试x = char 11）：490701690/490701691/490701692 使用后
// 没效果。抓包证明客户端其实发了 op430（物品栏券）、op408（金库券）和 op515 action 146
// （帐号金库工具），而服务端这三条路一条都没接。这个用例锁住解析侧的判据：种类只看 PVF
// 自己的 [action type]，所以同族的 100 件道具（40/47/13）都适用，换一件券也能用。
func TestAlignedStackableUseResolverRecognizesContainerUpgradeVouchers(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                     "",
		"equipment/equipment.lst":                 "",
		"stackable/stackable.lst":                 "490701690 `etc/upgrade_inventory.stk`\n490701691 `etc/upgrade_cargo.stk`\n490701692 `etc/upgrade_account_cargo.stk`\n490701693 `etc/upgrade_no_place.stk`\n490701694 `etc/upgrade_wrong_type.stk`\n",
		"stackable/etc/upgrade_inventory.stk":     "[stackable type] `[etc]`\n[action type] `[upgrade inventory]`\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n",
		"stackable/etc/upgrade_cargo.stk":         "[stackable type] `[etc]`\n[action type] `[upgrade cargo]`\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n",
		"stackable/etc/upgrade_account_cargo.stk": "[stackable type] `[etc]`\n[action type] `[upgrade account cargo]`\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n",
		// 反证一：声明了 [upgrade inventory] 却没有 [action usable place] [village]。
		"stackable/etc/upgrade_no_place.stk": "[stackable type] `[etc]`\n[action type] `[upgrade inventory]`\n[/action type]\n[action usable place] `[dungeon]`\n[/action usable place]\n",
		// 反证二：只有 action type、stackable type 不是 [etc]。
		"stackable/etc/upgrade_wrong_type.stk": "[stackable type] `[waste]`\n[action type] `[upgrade cargo]`\n[/action type]\n[action usable place] `[village]`\n[/action usable place]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	want := map[int64]alignedcmd.ContainerUpgradeKind{
		490701690: alignedcmd.ContainerUpgradeInventory,
		490701691: alignedcmd.ContainerUpgradeCargo,
		490701692: alignedcmd.ContainerUpgradeAccountCargo,
	}
	// 三个 opcode 都必须拿到解析器：op430/op408 的请求体里只有槽位，没有 item id，
	// owner 只能靠这个解析器判"槽上那件东西是不是扩容券、是哪一种"。
	for _, opcode := range []dnfenum.CmdPacket{
		dnfenum.CmdPacketUseStackableAction,
		dnfenum.CmdPacketUpgradeInventory,
		dnfenum.CmdPacketUpgradeCargo,
	} {
		resolver, resolverErr := service.alignedStackableUseResolverForCommand(opcode)
		if resolverErr != nil || resolver == nil {
			t.Fatalf("opcode %d resolver=%v err=%v", opcode, resolver, resolverErr)
		}
		for itemID, wantKind := range want {
			resolution, resolveErr := resolver(itemID)
			upgrade := resolution.ContainerUpgrade
			if resolveErr != nil || upgrade == nil || upgrade.Kind != wantKind || upgrade.ItemID != itemID ||
				strings.TrimSpace(resolution.StackableType) != "[etc]" {
				t.Fatalf("opcode %d item %d resolution=%+v err=%v", opcode, itemID, resolution, resolveErr)
			}
		}
		if _, resolveErr := resolver(490701693); resolveErr == nil {
			t.Fatalf("opcode %d: [upgrade inventory] without [action usable place] [village] must fail closed", opcode)
		}
		if _, resolveErr := resolver(490701694); resolveErr == nil {
			t.Fatalf("opcode %d: [upgrade cargo] without a bare [etc] stackable must fail closed", opcode)
		}
	}
}

// 城镇自施放增益药剂（史诗Buff药剂一族）：`[stackable type] [expert town potion]`
// + **没有** `[action type]` + `[stat change duration]`。
//
// 现场 2026-10-03（服主报，测试x = char 11）：490002458 连点 15 次全是 op44 error 17，
// 因为 useStackableWithResolvers 只认 [waste]，这一族整族用不了（全库 710 件
// [expert town potion]，其中约 30 件是这种"说明里写了限时增益"的）。
func TestAlignedStackableUseResolverRecognizesStatChangePotions(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "490002458 `event/epic_buff.stk`\n490006963 `event/ouhuang.stk`\n490005358 `event/lantern_big.stk`\n10008220 `cash/reinforce.stk`\n490000999 `event/fatigue.stk`\n490000998 `event/dungeon_only.stk`\n490000997 `event/no_place.stk`\n",
		// 史诗Buff药剂：说明里写了"掉落率变为2倍" ⇒ 200%/600s。
		"stackable/event/epic_buff.stk": "[name] `epic buff potion`\n[explain] `使用后， 深渊派对里的史诗装备掉落率变为2倍， 效果持续10分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n[action usable place] `[village]` `[seria room]`\n",
		// 欧皇药水：说明里有"掉落率"但没有倍数 ⇒ 同样按家族基准 200。
		"stackable/event/ouhuang.stk": "[name] `lucky potion`\n[explain] `使用后， 可以增加深渊派对里的史诗装备掉落率， 效果持续10分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n[action usable place] `[village]` `[seria room]`\n",
		// 小灯笼的大瓶：说明里同样只有"增加…掉落率"。
		"stackable/event/lantern_big.stk": "[name] `lantern big`\n[explain] `使用后， 增加在深渊派对中史诗道具掉落率， 效果持续10分钟。 效果不与其他史诗Buff药剂叠加。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n[action usable place] `[village]` `[seria room]`\n",
		// 反证一：同族但效果不是掉落率的（强化秘药一类）⇒ 能正常用，但不给倍率。
		"stackable/cash/reinforce.stk": "[name] `reinforce potion`\n[explain] `使用后， 强化成功率提升。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n[action usable place] `[village]`\n",
		// 反证二：有 [action type] 的同族道具必须继续走各自的 action 通道（这里是疲劳药）。
		"stackable/event/fatigue.stk": "[name] `fatigue 30`\n[explain] `疲劳值低于106时才能使用；每日限用3个`\n[stackable type] `[expert town potion]` 0\n[action type] `[add fatigue]` 30\n[/action type]\n[total usable count] 3\n[action usable place] `[village]`\n",
		// 反证三：同族里没写 [action usable place] 的（全库 9 件：赛丽亚的幸运*药剂、
		// 强化秘药、和平的呐喊…）也必须能正常使用 —— 地点字段是"限制"，不是"证据"。
		"stackable/event/no_place.stk": "[name] `seria lucky drop`\n[explain] `使用后， 可以获得深渊派对史诗道具掉落率提升Buff， 效果持续10分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n",
		// 反证四：只能在地下图用的同类增益同样是这一族（客户端按同一份 PVF 判地点）。
		"stackable/event/dungeon_only.stk": "[name] `dungeon only buff`\n[explain] `使用后， 增加深渊派对里的史诗装备掉落率。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n[action usable place] `[dungeon]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil || resolver == nil {
		t.Fatalf("op44 resolver=%v err=%v", resolver, err)
	}

	// 这些夹具在 PVF 里都写 `[stat change duration] 600000`（600 秒），窗口长度一律按
	// PVF 口径（2026-10-03 那次"硬编码 1 分钟"的过期验证已经按服主要求撤掉了）。
	const wantSeconds = int64(600)
	for _, tc := range []struct {
		itemID      int64
		wantPercent int64
		wantSeconds int64
	}{
		{itemID: 490002458, wantPercent: 200, wantSeconds: wantSeconds},
		{itemID: 490006963, wantPercent: 200, wantSeconds: wantSeconds},
		{itemID: 490005358, wantPercent: 200, wantSeconds: wantSeconds},
		{itemID: 10008220, wantPercent: 0, wantSeconds: wantSeconds},
	} {
		resolution, resolveErr := resolver(tc.itemID)
		potion := resolution.StatChangePotion
		if resolveErr != nil || potion == nil ||
			potion.EpicDropRatePercent != tc.wantPercent ||
			potion.EffectDurationSeconds != tc.wantSeconds {
			t.Fatalf("item %d resolution=%+v err=%v", tc.itemID, resolution, resolveErr)
		}
	}

	// 反证二：有 [action type] 的道具不能被这条分支抢走（疲劳药必须仍然是 op515 的活）。
	fatigue, err := resolver(490000999)
	if err != nil {
		t.Fatal(err)
	}
	if fatigue.StatChangePotion != nil {
		t.Fatalf("an [add fatigue] item must keep its own action path: %+v", fatigue.StatChangePotion)
	}
	// 反证三：只声明了 [action usable place] 缺字段或 [dungeon] 的同类增益**也要放行** ——
	// 地点字段是"使用地点的限制"，不是"这是增益药剂"的证据；客户端按同一份 PVF
	// 自己判地点。全库有 9 件同族连这个字段都没写（赛丽亚的幸运*药剂、强化秘药…），
	// 把它们拒掉就是服主报的"没反应"。
	for _, itemID := range []int64{490000998, 490000997} {
		admitted, resolveErr := resolver(itemID)
		if resolveErr != nil || admitted.StatChangePotion == nil ||
			admitted.StatChangePotion.EpicDropRatePercent != 200 ||
			admitted.StatChangePotion.EffectDurationSeconds != wantSeconds {
			t.Fatalf("item %d must still be admitted: %+v err=%v", itemID, admitted, resolveErr)
		}
	}
}

// 强化/增幅成功率药剂（服主 2026-10-03："看看增幅的秘密 强化秘药这类增加强化增幅成功率
// 的道具 也一并修复了"）。判据全部来自 PVF，不看 id 不看名字：
//
//	说明文字写"强化成功率"（强化秘药那一支）            → reinforce + 固定常量 10
//	[stackable type] `[item upgrad buff usual upgrade]`   → reinforce（强化的奥秘）
//	[stackable type] `[item upgrad buff amplify upgrade]` → amplify（永恒的增幅的秘密）
//	buff 表里有 `item_usual_upgrade_success_rate` 候选    → 按权重掷点（赛丽亚 10/20/30）
//
// 同时钉住"两族互不串味"：史诗掉落率药剂不能被判成强化药剂，反之亦然。
func TestAlignedStackableUseResolverRecognizesUpgradeRatePotions(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "490002458 `event/epic_buff.stk`\n10008220 `cash/seria_reinforce.stk`\n" +
			"490007878 `event/mystery_upgrade.stk`\n490001534 `event/eternal_amplify.stk`\n" +
			"490004594 `professional/secret_reinforcement.stk`\n",
		// 史诗Buff药剂：只影响掉落率，不该被判成强化药剂。
		"stackable/event/epic_buff.stk": "[name] `epic buff potion`\n[explain] `使用后， 深渊派对里的史诗装备掉落率变为2倍， 效果持续10分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n",
		// 赛丽亚的幸运强化药剂：概率表在 etc 里，服务端掷点。
		"stackable/cash/seria_reinforce.stk": "[name] `seria lucky reinforce`\n[explain] `使用后， 可以获得强化成功率提升Buff， 效果持续10分钟。 将会从下级、 中级、 上级Buff中随机发放1种。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 600000 `myself`\n",
		// 强化的奥秘：专用 stackable type，PVF 里没有数字。
		"stackable/event/mystery_upgrade.stk": "[name] `mystery of upgrade`\n[explain] `使用后， 可以提升强化成功率， 效果持续30分钟。`\n[stackable type] `[item upgrad buff usual upgrade]` 1000\n[stat change duration] 1800000 `myself`\n",
		// 永恒的增幅的秘密：增幅版，同样没有数字。
		"stackable/event/eternal_amplify.stk": "[name] `eternal secret amplify`\n[explain] `使用后， 可以增加增幅成功几率， 效果持续30分钟。`\n[stackable type] `[item upgrad buff amplify upgrade]` 1000\n[stat change duration] 1800000 `myself`\n",
		// 强化秘药：副职业 [expert town potion]，说明里只有"提升强化成功率"。
		"stackable/professional/secret_reinforcement.stk": "[name] `secret reinforcement`\n[explain] `使用后， 可以提升强化成功率， 效果持续30分钟。`\n[stackable type] `[expert town potion]` 0\n[stat change duration] 1800000 `myself`\n",
		"etc/variousbuffstackablelist.etc": "[index]\n10008220 \n[probability buff list]\n1097 60 1098 30 1099 10 \n[/index]\n" +
			"[index]\n490002458 \n[probability buff list]\n1136 100 \n[/index]\n",
		"etc/variousbufflist.etc": "[index]\n1097 \n[icon index]\n3085 \n[tooltip text]\n`强化成功率提升 (下级)` \n[effect]\n[name]\n`item_usual_upgrade_success_rate` \n[param]\n10 \n[/param]\n[/effect]\n[/index]\n" +
			"[index]\n1098 \n[icon index]\n3080 \n[tooltip text]\n`强化成功率提升 (中级)` \n[effect]\n[name]\n`item_usual_upgrade_success_rate` \n[param]\n20 \n[/param]\n[/effect]\n[/index]\n" +
			"[index]\n1099 \n[icon index]\n3075 \n[tooltip text]\n`强化成功率提升 (上级)` \n[effect]\n[name]\n`item_usual_upgrade_success_rate` \n[param]\n30 \n[/param]\n[/effect]\n[/index]\n" +
			"[index]\n1136 \n[icon index]\n183 \n[tooltip text]\n`史诗道具掉落率提升` \n[effect]\n[name]\n`epic_rarity_item_drop_rate` \n[param]\n100 \n[/param]\n[/effect]\n[/index]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil || resolver == nil {
		t.Fatalf("op44 resolver=%v err=%v", resolver, err)
	}

	for _, tc := range []struct {
		itemID       int64
		wantKind     string
		wantPercent  int64
		wantRolls    int
		wantDropRate int64
	}{
		{itemID: 490004594, wantKind: currentUpgradeRatePotionReinforce, wantPercent: 10},
		{itemID: 490007878, wantKind: currentUpgradeRatePotionReinforce, wantPercent: 10},
		{itemID: 490001534, wantKind: currentUpgradeRatePotionAmplify, wantPercent: 10},
		{itemID: 10008220, wantKind: currentUpgradeRatePotionReinforce, wantRolls: 3},
		{itemID: 490002458, wantKind: "", wantDropRate: 0},
	} {
		resolution, resolveErr := resolver(tc.itemID)
		potion := resolution.StatChangePotion
		if resolveErr != nil || potion == nil {
			t.Fatalf("item %d resolution=%+v err=%v", tc.itemID, resolution, resolveErr)
		}
		if potion.UpgradeKind != tc.wantKind || potion.UpgradeBonusPercent != tc.wantPercent ||
			len(potion.UpgradeRolls) != tc.wantRolls {
			t.Fatalf("item %d upgrade kind=%q bonus=%d rolls=%+v (want kind=%q bonus=%d rolls=%d)",
				tc.itemID, potion.UpgradeKind, potion.UpgradeBonusPercent, potion.UpgradeRolls,
				tc.wantKind, tc.wantPercent, tc.wantRolls)
		}
		// 掉落率那一族不能被强化药剂污染：反过来也一样（490002458 只有 BuffRolls）。
		if tc.itemID == 490002458 {
			if len(potion.BuffRolls) != 1 || potion.BuffRolls[0].BuffID != 1136 ||
				potion.BuffRolls[0].BonusPercent != 100 {
				t.Fatalf("epic drop potion rolls=%+v", potion.BuffRolls)
			}
			if len(potion.UpgradeRolls) != 0 || potion.UpgradeBonusPercent != 0 {
				t.Fatalf("epic drop potion must not look like an upgrade potion: %+v", potion)
			}
			continue
		}
		if len(potion.BuffRolls) != 0 || potion.EpicDropRatePercent != tc.wantDropRate {
			t.Fatalf("item %d must not carry drop-rate data: %+v", tc.itemID, potion)
		}
	}

	// 概率表逐档核对：赛丽亚的幸运强化药剂 = 1097(60,+10) / 1098(30,+20) / 1099(10,+30)。
	resolution, err := resolver(10008220)
	if err != nil {
		t.Fatal(err)
	}
	want := []alignedcmd.StatChangePotionWeightedRoll{
		{BuffID: 1097, Weight: 60, BonusPercent: 10},
		{BuffID: 1098, Weight: 30, BonusPercent: 20},
		{BuffID: 1099, Weight: 10, BonusPercent: 30},
	}
	if len(resolution.StatChangePotion.UpgradeRolls) != len(want) {
		t.Fatalf("seria rolls=%+v", resolution.StatChangePotion.UpgradeRolls)
	}
	for i := range want {
		if resolution.StatChangePotion.UpgradeRolls[i] != want[i] {
			t.Fatalf("seria roll %d=%+v want %+v", i, resolution.StatChangePotion.UpgradeRolls[i], want[i])
		}
	}
}
