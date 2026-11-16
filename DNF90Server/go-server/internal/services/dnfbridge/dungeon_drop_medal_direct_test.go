package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonDropPlanSeparatesGuildMedalsFromGroundWires(t *testing.T) {
	service, runtime := prepareCurrentDungeonDirectMedalDropTest(t)
	monster := runtime.Room.Snapshot().Monsters[0]
	wires, direct, err := service.planCurrentDungeonMonsterDropsWithDirectMedals(
		runtime,
		monster,
		currentSceneActorObjectKey(99),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range wires {
		if binary.LittleEndian.Uint32(wire.Item.data[2:6]) == 100380022 {
			t.Fatalf("guild medal leaked into scene drop wires: %+v", wires)
		}
	}
	if len(direct) == 0 || direct[0].Definition.ItemID != 100380022 ||
		!currentDungeonDirectGuildMedalDrop(direct[0].Definition) ||
		direct[0].QualitySeed == 0 {
		t.Fatalf("direct medals=%+v", direct)
	}
}

func TestCurrentDungeonDirectGuildMedalDropsLandInList38AndReplayIdempotently(t *testing.T) {
	service, repositories, runtime, session, reward := currentDungeonDirectMedalTestFixture(
		t,
		map[string]dnfrepo.ItemStack{},
	)
	ctx := context.Background()

	first, err := service.grantCurrentDungeonDirectMedalDrops(
		ctx,
		session,
		runtime,
		"12:34",
		4001,
		[]currentDungeonDirectMedalDrop{reward},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.MailAttachments != 0 || first.Reused {
		t.Fatalf("first delivery=%+v", first)
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	var medalKey string
	for key, stack := range inventory.Slots {
		if strings.HasPrefix(key, "38:") && stack.ItemID == int64(reward.Definition.ItemID) {
			medalKey = key
			if stack.Count != 1 ||
				stack.Extra[currentDungeonDirectMedalGrantKeyExtra] == "" ||
				stack.Extra["source"] != currentDungeonDirectMedalDropSource ||
				currentEquipmentQualitySeedFromStack(stack) != reward.QualitySeed {
				t.Fatalf("direct medal stack key=%s value=%+v", key, stack)
			}
			break
		}
	}
	if medalKey == "" {
		t.Fatalf("medal missing from list 38: %+v", inventory.Slots)
	}

	replay, err := service.grantCurrentDungeonDirectMedalDrops(
		ctx,
		session,
		runtime,
		"12:34",
		4001,
		[]currentDungeonDirectMedalDrop{reward},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Reused || len(replay.Entries) != 1 || replay.MailAttachments != 0 {
		t.Fatalf("replay delivery=%+v", replay)
	}
	inventory, _, err = repositories.Inventory.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	medals := 0
	for key, stack := range inventory.Slots {
		if strings.HasPrefix(key, "38:") && stack.ItemID == int64(reward.Definition.ItemID) {
			medals++
		}
	}
	if medals != 1 {
		t.Fatalf("replay created %d medal rows, want 1: %+v", medals, inventory.Slots)
	}
}

func TestCurrentDungeonDirectGuildMedalDropsUseMailWhenMedalPageIsFull(t *testing.T) {
	slots := make(map[string]dnfrepo.ItemStack)
	for slot := currentGuildMedalPageSlotStart; slot <= currentGuildMedalPageSlotEnd; slot++ {
		slots[currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slot)] = dnfrepo.ItemStack{
			ItemID: 999999,
			Count:  1,
		}
	}
	service, repositories, runtime, session, reward := currentDungeonDirectMedalTestFixture(t, slots)
	ctx := context.Background()

	first, err := service.grantCurrentDungeonDirectMedalDrops(
		ctx,
		session,
		runtime,
		"12:34",
		4002,
		[]currentDungeonDirectMedalDrop{reward},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 0 || first.MailID == "" || first.MailAttachments != 1 {
		t.Fatalf("mail delivery=%+v", first)
	}

	mailbox, found, err := repositories.Mailbox.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	mail, found := mailbox.Mails[first.MailID]
	if !found || len(mail.Attachments) != 1 ||
		mail.Attachments[0].ItemID != int64(reward.Definition.ItemID) ||
		mail.Attachments[0].Extra[currentDungeonDirectMedalGrantKeyExtra] == "" ||
		mail.Attachments[0].Extra["overflow_mail"] != "true" {
		t.Fatalf("mail=%+v found=%t", mail, found)
	}

	replay, err := service.grantCurrentDungeonDirectMedalDrops(
		ctx,
		session,
		runtime,
		"12:34",
		4002,
		[]currentDungeonDirectMedalDrop{reward},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Reused || len(replay.Entries) != 0 || replay.MailID != "" {
		t.Fatalf("mail replay=%+v", replay)
	}
	mailbox, _, err = repositories.Mailbox.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Mails) != 1 {
		t.Fatalf("mail replay created %d letters, want 1: %+v", len(mailbox.Mails), mailbox.Mails)
	}
}

func TestCurrentDungeonDirectMedalClassificationOnlyMatchesFlagEquipment(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition dungeonDropItemDefinition
		want       bool
	}{
		{
			name: "guild medal",
			definition: dungeonDropItemDefinition{
				Kind:          dungeonDropItemEquipment,
				EquipmentType: "[flag]",
			},
			want: true,
		},
		{
			name: "normal equipment",
			definition: dungeonDropItemDefinition{
				Kind:          dungeonDropItemEquipment,
				EquipmentType: "[weapon]",
			},
		},
		{
			name: "guardian gem remains ordinary drop",
			definition: dungeonDropItemDefinition{
				Kind:          dungeonDropItemStackable,
				StackableType: "[flag gem]",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := currentDungeonDirectGuildMedalDrop(test.definition); got != test.want {
				t.Fatalf("direct medal=%t want=%t", got, test.want)
			}
		})
	}
}

func prepareCurrentDungeonDirectMedalDropTest(
	t *testing.T,
) (*Service, *runtimeDungeonState) {
	t.Helper()
	const medalItemID uint32 = 100380022
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] = "[map name]\n`start`\n" +
		"[dungeon]\n700\n" +
		"[type]\n`[start]`\n" +
		"[monster]\n3001 10 0 100 200 0 0 100 `[fixed]` `[normal]`\n"
	source["monster/test.gob"] += "[item]\n" + strconv.FormatUint(uint64(medalItemID), 10) + " 10\n[/item]\n"
	source[dungeonDropStackableList] = "3227 `material/test_drop.stk`\n"
	source[dungeonDropEquipmentList] = strconv.FormatUint(uint64(medalItemID), 10) +
		" `character/common/flag/test_medal.equ`\n"
	source["stackable/material/test_drop.stk"] =
		"[name]\n`Test Drop`\n" +
			"[stackable type]\n`[material]`\n" +
			"[stack limit]\n999\n" +
			"[attach type]\n`[free]`\n"
	source["equipment/character/common/flag/test_medal.equ"] =
		"[name]\n`Test Medal`\n" +
			"[equipment type]\n`[flag]`\n" +
			"[minimum level]\n50\n" +
			"[durability]\n30\n" +
			"[attach type]\n`[trade]`\n"
	source[dungeonCardDropRulePath] = currentDungeonGoldDropTestRuleText()
	source[dungeonCardGoldReferencePath] = "[gold drop ref table]\n20 100 0\n"
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 99}
	runtime, _, err := service.prepareDungeonRuntime(
		ctx,
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	return service, runtime
}

func currentDungeonDirectMedalTestFixture(
	t *testing.T,
	slots map[string]dnfrepo.ItemStack,
) (
	*Service,
	dnfrepo.Group,
	*runtimeDungeonState,
	*gameSession,
	currentDungeonDirectMedalDrop,
) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       dnfrepo.CloneInventory(dnfrepo.InventoryRecord{Slots: slots}).Slots,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	runtime := &runtimeDungeonState{
		Dungeon:        worldmap.Dungeon{ID: 700},
		MazeIndex:      2,
		lifecycleToken: 77,
	}
	session := &gameSession{selectedCharacterID: 99}
	reward := currentDungeonDirectMedalDrop{
		Definition: dungeonDropItemDefinition{
			ItemID:        100380022,
			Kind:          dungeonDropItemEquipment,
			PVFPath:       "equipment/character/common/flag/100380022.equ",
			EquipmentType: "[flag]",
			SlotStart:     9,
			SlotEnd:       64,
			Durability:    30,
		},
		Amount:      1,
		QualitySeed: 123456,
	}
	return service, repositories, runtime, session, reward
}
