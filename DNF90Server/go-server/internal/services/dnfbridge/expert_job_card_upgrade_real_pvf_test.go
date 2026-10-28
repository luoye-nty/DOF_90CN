package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFUpgradedHeartAndLukeCardsEnchantAfterMove(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	expert, err := dnfexpertjob.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	enchanter, ok := expert.Enchanter()
	if !ok {
		t.Fatal("active PVF has no enchanter config")
	}
	if got := sortedCurrentEnchanterQualifications(enchanter); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4, 6}) {
		t.Fatalf("active PVF enchanter qualifications=%v", got)
	}

	tests := []struct {
		name       string
		cardID     int64
		recipeID   int64
		targetID   int64
		upgrade    byte
		wantStages []int64
	}{
		{name: "Anton heart base", cardID: 10095701, recipeID: 10015132, targetID: 100351910, upgrade: 0, wantStages: []int64{0, 1, 2, 3}},
		{name: "Anton heart maximum upgrade", cardID: 10095701, recipeID: 10015132, targetID: 100351910, upgrade: 3, wantStages: []int64{0, 1, 2, 3}},
		{name: "Maker Luke Light base", cardID: 10159184, recipeID: 10159564, targetID: 100390003, upgrade: 0, wantStages: []int64{0, 1, 2}},
		{name: "Maker Luke Light maximum upgrade", cardID: 10159184, recipeID: 10159564, targetID: 100390003, upgrade: 2, wantStages: []int64{0, 1, 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, resolveErr := resolveCurrentEnchantCardMetadata(items, archive, test.cardID, test.targetID)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if !reflect.DeepEqual(resolution.UpgradeCounts, test.wantStages) ||
				!currentExpertJobStoreIntContains(resolution.UpgradeCounts, int64(test.upgrade)) ||
				resolution.CardItemID != test.cardID || resolution.TargetKind != string(dungeonDropItemEquipment) ||
				!currentExpertJobStoreStringContainsFold(resolution.AllowedEquipmentTypes, resolution.TargetEquipmentType) {
				t.Fatalf("resolution=%+v", resolution)
			}

			recipe, recipeFound := enchanter.CardRecipes[test.recipeID]
			card, cardFound := enchanter.Cards[test.cardID]
			if !recipeFound || !cardFound || recipe.Qualification != card.Qualification {
				t.Fatalf("recipe=%+v found=%t card=%+v found=%t", recipe, recipeFound, card, cardFound)
			}
			plan, planErr := expert.PlanEnchanterStore(math.MaxInt64, test.recipeID, test.cardID, enchanter.InitialEndurance, func(int) int { return 99 })
			if planErr != nil || !plan.Success {
				t.Fatalf("active PVF plan=%+v err=%v", plan, planErr)
			}

			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19", AccountID: "account-1",
				Stats: map[string]int64{
					"expert_job_type":                    int64(dnfexpertjob.EnchanterType),
					"expert_job_exp":                     math.MaxInt64,
					currentExpertJobMachineEnduranceStat: enchanter.InitialEndurance,
				},
			}); err != nil {
				t.Fatal(err)
			}
			cardStack := currentRealPVFEnchantCardStack(test.cardID, test.upgrade)
			targetRaw := make([]byte, currentItemListEntryWireSize)
			slots := map[string]dnfrepo.ItemStack{
				currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 9): {
					ItemID: test.targetID, Count: 1, RawEntry: targetRaw,
				},
				currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 240): cardStack,
			}
			if test.upgrade != 0 {
				slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 235)] = currentRealPVFEnchantCardStack(test.cardID, 0)
				slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 235)] = func() dnfrepo.ItemStack {
					base := slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 235)]
					base.Count = 3
					binary.LittleEndian.PutUint32(base.RawEntry[0x06:0x0A], 3)
					return base
				}()
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
				t.Fatal(err)
			}
			service := &Service{
				options: options{
					accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
					gameUpperBodyCodec: gameUpperBodyCodecPlain,
				},
				pvfItemCatalog:          items,
				expertJobCatalog:        expert,
				initialEquipmentArchive: archive,
				repositoryProvider: func() (dnfrepo.Group, bool) {
					return repositories, true
				},
			}
			session := &gameSession{selectedCharacterID: 19, accountID: "account-1"}
			moveOwner, ownerErr := dnfinventory.NewOwner(repositories)
			if ownerErr != nil {
				t.Fatal(ownerErr)
			}
			mergeResolver, resolverErr := service.alignedStackMergeResolverForCommand(dnfenum.CmdPacketMoveItemspace)
			if resolverErr != nil {
				t.Fatal(resolverErr)
			}
			moveOwner.SetStackMergeResolver(mergeResolver)
			move, moveErr := moveOwner.Move(ctx, dnfinventory.NewMoveCommand(alignedcmd.Request{
				AccountID: "account-1", SelectedCharacterID: 19,
			}, dnfinventory.MoveItemspaceRequest{
				SourceListType: dnfrepo.MainInventoryListType, SourceSlotIndex: 240, MoveCount: 1,
				DestinationListType: dnfrepo.MainInventoryListType, DestinationSlotIndex: 244,
			}))
			if moveErr != nil || move.Mode != "move" {
				t.Fatalf("move=%+v err=%v", move, moveErr)
			}
			if _, sortErr := moveOwner.Sort(ctx, dnfinventory.NewSortCommand(alignedcmd.Request{
				AccountID: "account-1", SelectedCharacterID: 19,
			}, dnfinventory.SortItemRequest{ListType: dnfrepo.MainInventoryListType, Category: 10})); sortErr != nil {
				t.Fatal(sortErr)
			}
			movedInventory, found, loadErr := repositories.Inventory.Load(ctx, "19")
			if loadErr != nil || !found {
				t.Fatalf("load moved inventory found=%t err=%v", found, loadErr)
			}
			cardSlot, cardFound := currentMovedEnchantCardSlot(movedInventory, test.cardID, test.upgrade)
			if !cardFound {
				t.Fatalf("moved upgraded card missing: inventory=%+v", movedInventory.Slots)
			}
			result, commitErr := service.commitCurrentExpertJobStoreEnchant(ctx, session, &currentExpertJobStore{
				OwnerCharacterID: 19, OwnerSession: session, Kind: currentExpertJobEnchantStoreKind,
				Endurance: enchanter.InitialEndurance,
			}, currentExpertJobStoreEnchantRequest{
				OwnerID: 19, RecipeID: test.recipeID,
				TargetList: dnfrepo.MainInventoryListType, TargetSlot: 9,
				CardList: dnfrepo.MainInventoryListType, CardSlot: cardSlot,
			})
			if commitErr != nil || !result.Success {
				t.Fatalf("commit result=%+v err=%v", result, commitErr)
			}
			inventory, found, loadErr := repositories.Inventory.Load(ctx, "19")
			if loadErr != nil || !found {
				t.Fatalf("load inventory found=%t err=%v", found, loadErr)
			}
			if _, remains := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, cardSlot)]; remains {
				t.Fatal("enchant success did not consume the moved card")
			}
			target := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 9)]
			if target.Extra["enchant_card_id"] != strconv.FormatInt(test.cardID, 10) ||
				currentExpertJobEnchantUpgradeCount(target) != test.upgrade ||
				binary.LittleEndian.Uint32(target.RawEntry[0x0E:0x12]) != uint32(test.cardID) || target.RawEntry[0x12] != test.upgrade {
				t.Fatalf("enchanted target=%+v raw=%x", target, target.RawEntry)
			}
		})
	}
}

func TestRealScriptPVFUpgradedHeartAndLukeCardsCreateBeadsAfterMove(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	expert, err := dnfexpertjob.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	enchanter, ok := expert.Enchanter()
	if !ok {
		t.Fatal("active PVF has no enchanter config")
	}
	for _, test := range []struct {
		name     string
		cardID   int64
		recipeID int64
		upgrade  byte
		// 【2026-09-18 批量制作宝珠】count 是这一档要制作的件数（0 视为 1）。
		// 附魔机的"制作宝珠"允许在界面里选数量，客户端把数量原样发来；服务端现在
		// 按数量循环，每次扣 1 张卡 + 一份配方材料。夹具把经验设成 MaxInt64（附魔师
		// 满级），按 [monstercard exp] 表此时各档成功率都是 100%，所以批量件数等于
		// 成功件数，断言是确定的。
		count int
	}{
		{name: "Anton heart base", cardID: 10095701, recipeID: 10015132, upgrade: 0},
		{name: "Anton heart maximum upgrade", cardID: 10095701, recipeID: 10015132, upgrade: 3},
		{name: "Maker Luke Light base", cardID: 10159184, recipeID: 10159564, upgrade: 0},
		{name: "Maker Luke Light maximum upgrade", cardID: 10159184, recipeID: 10159564, upgrade: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			wantCount := test.count
			if wantCount <= 0 {
				wantCount = 1
			}
			recipe, recipeFound := enchanter.CardRecipes[test.recipeID]
			beadID, beadFound := enchanter.BeadByCard[test.cardID]
			if !recipeFound || !beadFound {
				t.Fatalf("recipe=%+v found=%t bead=%d found=%t", recipe, recipeFound, beadID, beadFound)
			}
			beadDefinition, resolveErr := items.ResolveItem(uint32(beadID))
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			baseBeadSlot := beadDefinition.SlotStart
			baseBead, buildErr := buildCurrentDungeonPickupStack(baseBeadSlot, beadDefinition, 2)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			currentExpertJobSetEnchantUpgradeCount(&baseBead, 0)
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
			slots := map[string]dnfrepo.ItemStack{
				currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, baseBeadSlot): baseBead,
				currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 240):          currentRealPVFEnchantStack(test.cardID, int64(wantCount), test.upgrade),
			}
			if test.upgrade != 0 {
				slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 235)] = currentRealPVFEnchantCardStack(test.cardID, 0)
			}
			materialSlot := int16(65)
			for _, material := range recipe.Materials {
				stack := dnfrepo.ItemStack{ItemID: material.ItemID, Count: material.Count}
				if fixedSlot, shared := currentExpertJobSharedMaterialSlot(material.ItemID); shared {
					account.Slots[dnfrepo.AccountSharedInventorySlotKey(fixedSlot)] = stack
					continue
				}
				for {
					if _, occupied := slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, materialSlot)]; !occupied {
						break
					}
					materialSlot++
				}
				slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, materialSlot)] = stack
				materialSlot++
			}
			if err := repositories.AccountInventory.Save(ctx, account); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19", AccountID: "account-1",
				Stats: map[string]int64{
					"expert_job_type": int64(dnfexpertjob.EnchanterType),
					"expert_job_exp":  math.MaxInt64,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
				t.Fatal(err)
			}
			service := &Service{
				options: options{
					accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
					gameUpperBodyCodec: gameUpperBodyCodecPlain,
				},
				pvfItemCatalog:          items,
				expertJobCatalog:        expert,
				initialEquipmentArchive: archive,
				repositoryProvider: func() (dnfrepo.Group, bool) {
					return repositories, true
				},
			}
			moveOwner, ownerErr := dnfinventory.NewOwner(repositories)
			if ownerErr != nil {
				t.Fatal(ownerErr)
			}
			mergeResolver, resolverErr := service.alignedStackMergeResolverForCommand(dnfenum.CmdPacketMoveItemspace)
			if resolverErr != nil {
				t.Fatal(resolverErr)
			}
			moveOwner.SetStackMergeResolver(mergeResolver)
			move, moveErr := moveOwner.Move(ctx, dnfinventory.NewMoveCommand(alignedcmd.Request{
				AccountID: "account-1", SelectedCharacterID: 19,
			}, dnfinventory.MoveItemspaceRequest{
				SourceListType: dnfrepo.MainInventoryListType, SourceSlotIndex: 240, MoveCount: 1,
				DestinationListType: dnfrepo.MainInventoryListType, DestinationSlotIndex: 244,
			}))
			if moveErr != nil || move.Mode != "move" {
				t.Fatalf("move=%+v err=%v", move, moveErr)
			}
			if _, sortErr := moveOwner.Sort(ctx, dnfinventory.NewSortCommand(alignedcmd.Request{
				AccountID: "account-1", SelectedCharacterID: 19,
			}, dnfinventory.SortItemRequest{ListType: dnfrepo.MainInventoryListType, Category: 10})); sortErr != nil {
				t.Fatal(sortErr)
			}
			moved, found, loadErr := repositories.Inventory.Load(ctx, "19")
			if loadErr != nil || !found {
				t.Fatalf("load moved inventory found=%t err=%v", found, loadErr)
			}
			cardSlot, cardFound := currentMovedEnchantCardSlot(moved, test.cardID, test.upgrade)
			if !cardFound {
				t.Fatalf("moved card missing: %+v", moved.Slots)
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 19, accountID: "account-1"}
			body := make([]byte, currentExpertJobCompoundRequestSize)
			binary.LittleEndian.PutUint32(body[0:4], uint32(test.recipeID))
			binary.LittleEndian.PutUint16(body[4:6], uint16(wantCount))
			binary.LittleEndian.PutUint16(body[6:8], uint16(cardSlot))
			if handleErr := service.handleGameCommand(
				session,
				byte(dnfenum.GameCmdCommand),
				uint16(dnfenum.CmdPacketCompoundItemByExpertJob),
				body,
			); handleErr != nil {
				t.Fatal(handleErr)
			}
			ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if ack.Header.MsgID != uint16(dnfenum.CmdPacketCompoundItemByExpertJob) || ack.Header.Classification != 1 ||
				len(ack.Body) != 19 || ack.Body[0] != 1 || ack.Body[1] != 1 ||
				int64(binary.LittleEndian.Uint32(ack.Body[2:6])) != beadID ||
				binary.LittleEndian.Uint32(ack.Body[6:10]) != uint32(wantCount) ||
				binary.LittleEndian.Uint32(ack.Body[10:14]) != uint32(wantCount) ||
				binary.LittleEndian.Uint32(ack.Body[14:18]) != 0 || ack.Body[18] != 0 {
				t.Fatalf("op238 ACK header=%+v body=%x want_count=%d", ack.Header, ack.Body, wantCount)
			}
			if len(trailing) == 0 {
				t.Fatal("op238 success omitted authoritative container refresh")
			}
			inventory, found, loadErr := repositories.Inventory.Load(ctx, "19")
			if loadErr != nil || !found {
				t.Fatalf("load result inventory found=%t err=%v", found, loadErr)
			}
			var bead dnfrepo.ItemStack
			baseBeadCount := int64(0)
			for _, stack := range inventory.Slots {
				if stack.ItemID != beadID {
					continue
				}
				if currentExpertJobEnchantUpgradeCount(stack) == 0 {
					baseBeadCount += stack.Count
				}
				if currentExpertJobEnchantUpgradeCount(stack) == test.upgrade {
					bead = stack
				}
			}
			wantBaseBeadCount := int64(2)
			wantOutputStackCount := int64(1)
			if test.upgrade == 0 {
				// 初始 2 个 + 本次成功产出的 wantCount 个合进同一堆。
				wantBaseBeadCount = int64(2 + wantCount)
				wantOutputStackCount = int64(2 + wantCount)
			}
			if baseBeadCount != wantBaseBeadCount || bead.ItemID != beadID || bead.Count != wantOutputStackCount || currentExpertJobEnchantUpgradeCount(bead) != test.upgrade || bead.RawEntry[0x12] != test.upgrade {
				t.Fatalf("base_bead_count=%d upgraded_bead=%+v raw=%x", baseBeadCount, bead, bead.RawEntry)
			}
			account, _, loadErr = repositories.AccountInventory.Load(ctx, "account-1")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			for _, material := range recipe.Materials {
				if got := currentTestItemCount(inventory.Slots, material.ItemID) + currentTestItemCount(account.Slots, material.ItemID); got != 0 {
					t.Fatalf("material=%d remaining=%d", material.ItemID, got)
				}
			}
		})
	}
}

// TestRealScriptPVFBatchBeadCompoundConsumesOneCardPerCraft 覆盖 2026-09-18 的
// "批量制作宝珠"：附魔机的制作界面可以把数量调成 N（客户端原样发 {配方, 数量, 卡片槽}），
// 之前服务端只接受数量=1，其余一律回失败码 19（玩家实测被拒）。现在按数量循环：
// 每件消耗 1 张卡 + 一份配方材料，各自掷一次成功率。
//
// 夹具把附魔师经验设成 MaxInt64（满 11 级），按 [monstercard exp] 表此时各档成功率都是
// 100%，所以 5 件必然全部成功，断言是确定的：回包只带 1 条产物条目、数量 5、成功 5 失败 0，
// 卡堆被扣掉 5 张，材料按 5 份消耗，宝珠多出 5 个。
func TestRealScriptPVFBatchBeadCompoundConsumesOneCardPerCraft(t *testing.T) {
	const (
		batchCount = 5
		cardID     = int64(10095701)
		recipeID   = int64(10015132)
		cardSlot   = int16(240)
	)
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	expert, err := dnfexpertjob.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	enchanter, ok := expert.Enchanter()
	if !ok {
		t.Fatal("active PVF has no enchanter config")
	}
	recipe, recipeFound := enchanter.CardRecipes[recipeID]
	beadID, beadFound := enchanter.BeadByCard[cardID]
	if !recipeFound || !beadFound || len(recipe.Materials) == 0 {
		t.Fatalf("recipe=%+v found=%t bead=%d found=%t", recipe, recipeFound, beadID, beadFound)
	}

	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
	slots := map[string]dnfrepo.ItemStack{
		currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, cardSlot): currentRealPVFEnchantStack(cardID, batchCount, 0),
	}
	// 材料按 batchCount 份准备，这样"每件各扣一份"才跑得满 5 次。
	materialSlot := int16(65)
	for _, material := range recipe.Materials {
		stack := dnfrepo.ItemStack{ItemID: material.ItemID, Count: material.Count * batchCount}
		if fixedSlot, shared := currentExpertJobSharedMaterialSlot(material.ItemID); shared {
			account.Slots[dnfrepo.AccountSharedInventorySlotKey(fixedSlot)] = stack
			continue
		}
		for {
			if _, occupied := slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, materialSlot)]; !occupied {
				break
			}
			materialSlot++
		}
		slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, materialSlot)] = stack
		materialSlot++
	}
	if err := repositories.AccountInventory.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1",
		Stats: map[string]int64{
			"expert_job_type": int64(dnfexpertjob.EnchanterType),
			"expert_job_exp":  math.MaxInt64,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{
			accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		pvfItemCatalog:          items,
		expertJobCatalog:        expert,
		initialEquipmentArchive: archive,
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19, accountID: "account-1"}
	body := make([]byte, currentExpertJobCompoundRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], uint32(recipeID))
	binary.LittleEndian.PutUint16(body[4:6], batchCount)
	binary.LittleEndian.PutUint16(body[6:8], uint16(cardSlot))
	if handleErr := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCompoundItemByExpertJob),
		body,
	); handleErr != nil {
		t.Fatal(handleErr)
	}
	ack, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketCompoundItemByExpertJob) || ack.Header.Classification != 1 ||
		len(ack.Body) != 19 || ack.Body[0] != 1 || ack.Body[1] != 1 ||
		int64(binary.LittleEndian.Uint32(ack.Body[2:6])) != beadID ||
		binary.LittleEndian.Uint32(ack.Body[6:10]) != batchCount ||
		binary.LittleEndian.Uint32(ack.Body[10:14]) != batchCount ||
		binary.LittleEndian.Uint32(ack.Body[14:18]) != 0 || ack.Body[18] != 0 {
		t.Fatalf("op238 batch ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	if len(trailing) == 0 {
		t.Fatal("op238 batch success omitted authoritative container refresh")
	}

	inventory, found, loadErr := repositories.Inventory.Load(ctx, "19")
	if loadErr != nil || !found {
		t.Fatalf("load result inventory found=%t err=%v", found, loadErr)
	}
	cardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, cardSlot)
	if stack, stillThere := inventory.Slots[cardKey]; stillThere && stack.Count != 0 {
		t.Fatalf("card slot after batch = %+v, want the whole stack consumed", stack)
	}
	beadCount := int64(0)
	for _, stack := range inventory.Slots {
		if stack.ItemID == beadID && currentExpertJobEnchantUpgradeCount(stack) == 0 {
			beadCount += stack.Count
		}
	}
	if beadCount != batchCount {
		t.Fatalf("bead count = %d, want %d", beadCount, batchCount)
	}
	accountAfter, _, loadErr := repositories.AccountInventory.Load(ctx, "account-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	for _, material := range recipe.Materials {
		if got := currentTestItemCount(inventory.Slots, material.ItemID) + currentTestItemCount(accountAfter.Slots, material.ItemID); got != 0 {
			t.Fatalf("material=%d remaining=%d after %d crafts", material.ItemID, got, batchCount)
		}
	}
}

func sortedCurrentEnchanterQualifications(config *dnfexpertjob.EnchanterConfig) []int {
	if config == nil {
		return nil
	}
	values := make([]int, 0, len(config.CardRecipes))
	for _, recipe := range config.CardRecipes {
		values = append(values, recipe.Qualification)
	}
	slices.Sort(values)
	return values
}

func currentRealPVFEnchantCardStack(itemID int64, upgrade byte) dnfrepo.ItemStack {
	return currentRealPVFEnchantStack(itemID, 1, upgrade)
}

func currentRealPVFEnchantStack(itemID, count int64, upgrade byte) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], uint32(count))
	raw[0x12] = upgrade
	return dnfrepo.ItemStack{
		ItemID: itemID, Count: count, RawEntry: raw,
		Extra: map[string]string{
			"item_kind": "stackable", "byte_12": strconv.Itoa(int(upgrade)),
			"enchant_upgrade_count": strconv.Itoa(int(upgrade)),
		},
	}
}

func currentMovedEnchantCardSlot(inventory dnfrepo.InventoryRecord, itemID int64, upgrade byte) (int16, bool) {
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == dnfrepo.MainInventoryListType && stack.ItemID == itemID && stack.Count == 1 &&
			currentExpertJobEnchantUpgradeCount(stack) == upgrade {
			return slot, true
		}
	}
	return 0, false
}

func currentTestItemCount(slots map[string]dnfrepo.ItemStack, itemID int64) int64 {
	var total int64
	for _, stack := range slots {
		if stack.ItemID == itemID {
			total += stack.Count
		}
	}
	return total
}
