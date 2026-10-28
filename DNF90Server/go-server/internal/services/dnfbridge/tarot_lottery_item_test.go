package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 塔罗牌夹具：512 是 `[stackable type]` = `[monster tarot card]` 的消耗品，
// `[lottery use cost]` 100000 金币，`[int data]` 是 (item, weight, count) 三元组。
//
// 中奖项刻意选 600/610（夹具里的 `[material]` 堆叠品）：这条用例只关心 op27 通道与
// 金币扣款，堆叠品能把这两个断言做得最干净。**混合表（装备行）由另外两条用例覆盖**：
//
//   - tarot_lottery_mixed_table_test.go：内存夹具里第一行就是装备，钉住"抽到装备也发得出去"；
//   - TestRealTarotLotteryCardsGrantEveryRow：真档四张卡 × 每一行的端到端验收。
func currentTarotLotteryTestSource() dungeonDropCatalogTestSource {
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "512 `cash/tarot_card.stk`\n"
	source["stackable/cash/tarot_card.stk"] = "[stackable type]\n`[monster tarot card]` 19\n" +
		"[stack limit]\n100\n" +
		"[lottery use cost]\n100000\n" +
		"[int data]\n600 10000 3 610 10000 1\n[/int data]\n"
	return source
}

func mustCurrentTarotLotteryTestCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(currentTarotLotteryTestSource())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// tarotLotteryTestFixture 起一个只有一个角色、背包里只有一张塔罗牌的服务。
// 槽位 70 落在 `[monster tarot card]` 自己的 PVF 页（dungeonDropStackableSlotRange
// 的 default 分支 65..120），中奖项 600 是 `[material]`，落在 121..176，两者不打架。
const (
	tarotLotteryTestCardItemID  = uint32(512)
	tarotLotteryTestCardSlot    = int16(70)
	tarotLotteryTestGold        = int64(200000)
	tarotLotteryTestGoldCost    = int64(100000)
	tarotLotteryTestRewardStack = uint32(3)
)

func newTarotLotteryTestFixture(
	t *testing.T,
	cardCount int64,
	gold int64,
) (*Service, *gameSession, *bufferConn, context.Context, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	catalog := mustCurrentTarotLotteryTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"gold": gold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:70": {ItemID: int64(tarotLotteryTestCardItemID), Count: cardCount},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	// 抽奖的随机源：0 = 落在这个权重表的第一个三元组上（600）。
	service.dungeonChoice = func(int) (int, error) { return 0, nil }
	session := &gameSession{conn: connection, connID: "tarot-lottery", selectedCharacterID: 19}
	return service, session, connection, ctx, repositories
}

func openTarotLotteryCard(t *testing.T, service *Service, session *gameSession, slot int16) {
	t.Helper()
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseLotteryItem),
		[]byte{0, 0, byte(slot), byte(slot >> 8)},
	); err != nil {
		t.Fatal(err)
	}
}

// 主用例：客户端发 class-1/op27（实机 2026-09-30T11:52:47Z 就是这个形状），服务端
// 必须把它当塔罗牌抽奖而不是"非随机罐"拒绝 —— 也就是本轮的现场问题本身。
func TestCurrentTarotLotteryItemAnswersOp27AndChargesTheDeclaredGold(t *testing.T) {
	service, session, connection, ctx, repositories := newTarotLotteryTestFixture(t, 2, tarotLotteryTestGold)

	openTarotLotteryCard(t, service, session, tarotLotteryTestCardSlot)

	result, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if result.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) {
		t.Fatalf("第一个包不是 op27: header=%+v body=%x", result.Header, result.Body)
	}
	if len(result.Body) != currentLotteryNativeResultBodySize || result.Body[0] != 1 {
		t.Fatalf("op27 结果包形状不对: len=%d want=%d body=%x", len(result.Body), currentLotteryNativeResultBodySize, result.Body)
	}
	if slot := int16(uint16(result.Body[1]) | uint16(result.Body[2])<<8); slot != tarotLotteryTestCardSlot {
		t.Fatalf("op27 结果包源槽=%d want %d", slot, tarotLotteryTestCardSlot)
	}

	// 花钱就必须把权威余额推给客户端；形状与罐子开出金币时那一个钱包包一致
	// （lottery_item.go:115-124）。
	wallet, trailing := splitGameServerUpperPacket(t, rest)
	if wallet.Header.Classification != 0 ||
		wallet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("第二个包不是钱包同步: header=%+v body=%x", wallet.Header, wallet.Body)
	}
	wantGold := tarotLotteryTestGold - tarotLotteryTestGoldCost
	if wantBody := buildCurrentGoldStateBody(wantGold); string(wallet.Body) != string(wantBody) {
		t.Fatalf("钱包包=%x want=%x", wallet.Body, wantBody)
	}
	// op27 之后**不得**出现道具表包：那会替换客户端动画仍握着的道具对象
	// （lottery_item.go:107-114 的既有教训）。钱包包是唯一允许的后续包。
	if len(trailing) != 0 {
		t.Fatalf("op27 之后多了包: %x", trailing)
	}

	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character: %v found=%t", err, found)
	}
	if got := character.Stats["gold"]; got != wantGold {
		t.Fatalf("金币=%d want %d（[lottery use cost] 必须真的扣掉）", got, wantGold)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory: %v found=%t", err, found)
	}
	if stack := inventory.Slots["0:70"]; stack.ItemID != int64(tarotLotteryTestCardItemID) || stack.Count != 1 {
		t.Fatalf("源塔罗牌槽=%+v want item=%d count=1", stack, tarotLotteryTestCardItemID)
	}
	// 中奖项 600 必须真的进背包（3 个），且落点用 Owner 写回的真实槽位。
	granted := int64(0)
	for key, stack := range inventory.Slots {
		if key == "0:70" || stack.ItemID != 600 {
			continue
		}
		granted += stack.Count
	}
	if granted != int64(tarotLotteryTestRewardStack) {
		t.Fatalf("中奖项 600 数量=%d want %d（背包=%+v）", granted, tarotLotteryTestRewardStack, inventory.Slots)
	}
}

// 金币不足：整次使用必须在动任何东西之前中止 —— 源卡不消耗、金币不变、回 op27 失败包。
func TestCurrentTarotLotteryItemFailsClosedWhenGoldIsShort(t *testing.T) {
	service, session, connection, ctx, repositories := newTarotLotteryTestFixture(t, 2, tarotLotteryTestGoldCost-1)

	openTarotLotteryCard(t, service, session, tarotLotteryTestCardSlot)

	rejected, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if rejected.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) || rejected.Body[0] != 0 {
		t.Fatalf("应回 op27 失败包: header=%+v body=%x", rejected.Header, rejected.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("被拒的使用不该产生后续包: %x", trailing)
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := character.Stats["gold"]; got != tarotLotteryTestGoldCost-1 {
		t.Fatalf("被拒的使用改了金币: %d", got)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if stack := inventory.Slots["0:70"]; stack.Count != 2 {
		t.Fatalf("被拒的使用消耗了源卡: %+v", inventory.Slots)
	}
}

// 分派判据只认 `[monster tarot card]`：罐子（`[upgradable legacy]`）必须原样落回既有
// op27 路径。这是"修 A 不坏 B"的那条断言 —— 一旦有人把判据放宽，这里先红。
func TestCurrentTarotLotteryDispatchLeavesEveryOtherOp27ItemAlone(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentTarotLotteryTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"gold": 0},
	}); err != nil {
		t.Fatal(err)
	}
	jar := int64(510) // `[upgradable legacy]`：既有的 op27 抽奖罐
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:70": {ItemID: int64(tarotLotteryTestCardItemID), Count: 1},
			"0:71": {ItemID: jar, Count: 1},
			"0:72": {ItemID: 600, Count: 1}, // 普通材料：既不是塔罗牌也不是罐子
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: &bufferConn{}, connID: "tarot-dispatch", selectedCharacterID: 19}

	for _, test := range []struct {
		name    string
		slot    int16
		handled bool
	}{
		{"tarot card is taken over", 70, true},
		{"legacy jar keeps the booster route", 71, false},
		{"ordinary material keeps the booster route", 72, false},
		{"empty slot keeps the booster route", 90, false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			handled, err := service.tryHandleCurrentTarotLotteryItem(ctx, session, catalog, false, test.slot)
			if err != nil {
				t.Fatalf("slot=%d err=%v", test.slot, err)
			}
			if handled != test.handled {
				t.Fatalf("slot=%d handled=%t want %t", test.slot, handled, test.handled)
			}
		})
	}
}

// 真档验收：真塔罗牌的 `[stackable type]` 必须命中分派判据，并且中奖项里的
// **每一行**都必须真的发得出去（不再是"装备行干净拒绝"）。
//
// 这条用例的历史：上一轮发现真档 `[int data]` 里混着 9 位数的装备 id，而 inventory 的
// grantRandomRewardItem 只接受 kind=stackable，于是"落在装备行上的抽取"发不出奖；
// 当时把它钉成"干净 fail closed"。现场 2026-10-03 服主报「塔罗牌用不了」之后追下去，
// 发现那不只是边角：`[int data]` 里装备行按权重占了大半，而且服务端**从来没真正掷过
// 骰子**（roll 恒为 0 → 永远第一行），所以这些卡 100% 落在装备行上、100% 被拒。
//
// 现在两件事都修了，于是本用例改成双向断言：
//
//  1. 每一行、每一张卡都必须回 op27 成功，并且源卡消耗 + 金币扣掉；
//  2. **装备行必须是一行真正的装备**：item_kind=equipment、数量 1、0x77 行、
//     PVF 的 [durability] 落在 +0x0B、[sealing] 落在 +0x0D，且带一个合法品级种子。
//
// 任何一行掉回"成功位=0"都会被这条用例抓住。
func TestRealTarotLotteryCardsGrantEveryRow(t *testing.T) {
	archive, err := platformpvf.Open(realLukeRewardPVFPath())
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	ctx := context.Background()
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	// 10159717 就是服主那只（暗之偶像阿努比斯 塔罗牌，stackable/10159001/10159717.stk）。
	for _, cardID := range []uint32{10159697, 10159698, 10159700, 10159717} {
		cardID := cardID
		t.Run("card", func(t *testing.T) {
			definition, err := catalog.ResolveItem(cardID)
			if err != nil {
				t.Fatalf("resolve card %d: %v", cardID, err)
			}
			if got := normalizeDungeonDropStackableType(definition.StackableType); got != currentTarotLotteryStackableType {
				t.Fatalf("card %d stackable_type=%q want %q (path=%s)",
					cardID, got, currentTarotLotteryStackableType, definition.PVFPath)
			}
			if len(definition.LotteryEntries) == 0 {
				t.Fatalf("card %d has no [int data] lottery rows (path=%s)", cardID, definition.PVFPath)
			}

			grantedRows, equipmentRows := 0, 0
			// 每一行的起始 roll：累计权重边界。
			roll := int64(0)
			for index, entry := range definition.LotteryEntries {
				rowRoll := roll
				roll += entry.Weight
				reward, resolveErr := resolveCurrentMagicBoxRewardItem(catalog, catalog.source, entry.ItemID)
				if resolveErr != nil {
					t.Errorf("card %d row %d item=%d: resolve reward: %v", cardID, index, entry.ItemID, resolveErr)
					continue
				}
				// 端到端：把 roll 钉在这一行上，走完整的 op27 使用路径。
				service, session, connection, repos := newRealTarotLotteryFixture(t, ctx, catalog, cardID, rowRoll)
				openTarotLotteryCard(t, service, session, tarotLotteryTestCardSlot)
				result, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
				if result.Header.MsgID != uint16(dnfenum.CmdPacketUseLotteryItem) || len(result.Body) == 0 {
					t.Fatalf("card %d row %d item=%d: 第一个回包不是 op27 header=%+v body=%x",
						cardID, index, entry.ItemID, result.Header, result.Body)
				}
				character, _, _ := repos.Character.Load(ctx, "19")
				inventory, _, _ := repos.Inventory.Load(ctx, "19")
				source := inventory.Slots["0:70"]
				goldAfter := character.Stats["gold"]

				if result.Body[0] != 1 {
					t.Errorf("card %d row %d item=%d kind=%q: 被拒了 body=%x（每一行都必须发得出去）",
						cardID, index, entry.ItemID, reward.Kind, result.Body)
					continue
				}
				grantedRows++
				if len(result.Body) != currentLotteryNativeResultBodySize {
					t.Errorf("card %d row %d item=%d: op27 结果包长=%d want=%d body=%x",
						cardID, index, entry.ItemID, len(result.Body), currentLotteryNativeResultBodySize, result.Body)
				}
				// 花钱就必须推权威余额；钱包包是 op27 之后**唯一**允许的后续包
				// （不能有 op14 道具行/op13 整表：那会替换客户端动画握着的对象）。
				wallet, rest := splitGameServerUpperPacket(t, trailing)
				if wallet.Header.Classification != 0 ||
					wallet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
					t.Errorf("card %d row %d item=%d: op27 之后的包不是钱包同步 header=%+v body=%x",
						cardID, index, entry.ItemID, wallet.Header, wallet.Body)
				}
				if len(rest) != 0 {
					t.Errorf("card %d row %d item=%d: op27+钱包之后还多了包 %x",
						cardID, index, entry.ItemID, rest)
				}
				if goldAfter != 0 {
					t.Errorf("card %d row %d item=%d: 成功后金币=%d want 0", cardID, index, entry.ItemID, goldAfter)
				}
				if source.ItemID != 0 || source.Count != 0 {
					t.Errorf("card %d row %d item=%d: 成功后源卡还在 %+v", cardID, index, entry.ItemID, source)
				}

				row := result.Body[3 : 3+currentItemListEntryWireSize]
				if got := binary.LittleEndian.Uint32(row[2:6]); got != uint32(entry.ItemID) {
					t.Errorf("card %d row %d: op27 行里的道具 id=%d want %d", cardID, index, got, entry.ItemID)
				}

				if strings.EqualFold(reward.Kind, "equipment") {
					equipmentRows++
					grantedKey, granted, ok := findRealTarotLotteryGrantedEquipment(inventory, int64(entry.ItemID))
					if !ok {
						// 源槽 70 是卡本身，奖励装备落在 9..64，不可能被卡挡住。
						t.Errorf("card %d row %d item=%d: 背包里找不到发出去的装备（slots=%v）",
							cardID, index, entry.ItemID, realTarotLotterySlotSummary(inventory))
						continue
					}
					if granted.Count != 1 {
						t.Errorf("card %d row %d item=%d: 装备数量=%d want 1（装备一格一件，不叠加）",
							cardID, index, entry.ItemID, granted.Count)
					}
					if got := granted.Extra["item_kind"]; got != "equipment" {
						t.Errorf("card %d row %d item=%d: item_kind=%q want equipment", cardID, index, entry.ItemID, got)
					}
					if len(granted.RawEntry) != currentItemListEntryWireSize {
						t.Errorf("card %d row %d item=%d: RawEntry 长=%d want %d",
							cardID, index, entry.ItemID, len(granted.RawEntry), currentItemListEntryWireSize)
					}
					if reward.Seal && granted.Extra["seal_flag"] != "1" {
						t.Errorf("card %d row %d item=%d: [sealing] 没写进 seal_flag（extra=%v）",
							cardID, index, entry.ItemID, granted.Extra)
					}
					if durability := int64(binary.LittleEndian.Uint16(row[0x0B:0x0D])); durability != int64(reward.Durability) {
						t.Errorf("card %d row %d item=%d: op27 行耐久=%d want %d（PVF [durability]）",
							cardID, index, entry.ItemID, durability, reward.Durability)
					}
					if seed, parseErr := strconv.ParseUint(granted.Extra["quality_seed"], 10, 32); parseErr != nil || seed == 0 {
						t.Errorf("card %d row %d item=%d: 品级种子=%q 不合法（客户端会画成最下级）",
							cardID, index, entry.ItemID, granted.Extra["quality_seed"])
					}
					t.Logf("card=%d row=%d item=%d weight=%d kind=equipment durability=%d seal=%t -> GRANTED slot=%s",
						cardID, index, entry.ItemID, entry.Weight, reward.Durability, reward.Seal, grantedKey)
				} else {
					t.Logf("card=%d row=%d item=%d weight=%d count=%d kind=%q -> GRANTED",
						cardID, index, entry.ItemID, entry.Weight, entry.Count, reward.Kind)
				}
			}
			t.Logf("card=%d rows=%d granted=%d equipment=%d",
				cardID, len(definition.LotteryEntries), grantedRows, equipmentRows)
			if grantedRows != len(definition.LotteryEntries) {
				t.Errorf("card %d: 只发出去 %d/%d 行", cardID, grantedRows, len(definition.LotteryEntries))
			}
			if equipmentRows == 0 {
				t.Errorf("card %d: 一行装备都没抽到 —— 混合表用例失去意义", cardID)
			}
		})
	}
}

// findRealTarotLotteryGrantedEquipment 在角色背包里找那件发出去的装备，返回它的行键。
func findRealTarotLotteryGrantedEquipment(inventory dnfrepo.InventoryRecord, itemID int64) (string, dnfrepo.ItemStack, bool) {
	keys := make([]string, 0, len(inventory.Slots))
	for key, stack := range inventory.Slots {
		if stack.ItemID == itemID && stack.Count > 0 {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", dnfrepo.ItemStack{}, false
	}
	sort.Strings(keys)
	return keys[0], inventory.Slots[keys[0]], true
}

func realTarotLotterySlotSummary(inventory dnfrepo.InventoryRecord) []string {
	keys := make([]string, 0, len(inventory.Slots))
	for key, stack := range inventory.Slots {
		keys = append(keys, fmt.Sprintf("%s=%d x%d", key, stack.ItemID, stack.Count))
	}
	sort.Strings(keys)
	return keys
}

// newRealTarotLotteryFixture 与内存夹具同形，但用真档目录与真塔罗牌 id，并把随机源
// 钉在指定 roll 上。注意真档卡的 `[lottery use cost]` 是 100000，所以起始金币给 100000，
// 成功之后余额必须是 0。
func newRealTarotLotteryFixture(
	t *testing.T,
	ctx context.Context,
	catalog *pvfDungeonDropCatalog,
	cardID uint32,
	roll int64,
) (*Service, *gameSession, *bufferConn, dnfrepo.Group) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"gold": 100000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:70": {ItemID: int64(cardID), Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	service.dungeonChoice = func(int) (int, error) { return int(roll), nil }
	session := &gameSession{conn: connection, connID: "tarot-real", selectedCharacterID: 19}
	return service, session, connection, repositories
}
