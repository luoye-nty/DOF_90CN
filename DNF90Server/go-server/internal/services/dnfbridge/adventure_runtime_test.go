package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestAdventureGameplayModuleOwnsLegacyAndUpperRuntimeOpcodes(t *testing.T) {
	module := adventureGameplayModule()
	for _, opcode := range []uint16{
		uint16(dnfenum.CmdPacketMercenaryInfo),
		uint16(dnfenum.CmdPacketMercenaryCompetition),
		uint16(dnfenum.CmdPacketMercenaryCompetitionCancle),
		uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest),
		uint16(dnfenum.CmdPacketMercenaryPointRecalculate),
		uint16(dnfenum.CmdPacketAdventurerShopPurchase),
		uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp),
	} {
		if module.LegacyHandlers[opcode] == nil || module.UpperHandlers[opcode] == nil {
			t.Fatalf("opcode %d is not registered for both current transports", opcode)
		}
	}
}

func TestParseCurrentAdventureExpeditionStartRequest(t *testing.T) {
	body := make([]byte, 13)
	body[0] = 2
	binary.LittleEndian.PutUint32(body[1:5], 6*60*60)
	binary.LittleEndian.PutUint16(body[5:7], 2)
	copy(body[7:], []byte{1, 0, 2, 3, 1, 4})
	request, err := parseCurrentAdventureExpeditionStartRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.Area != 2 || request.DurationSeconds != 6*60*60 || len(request.Members) != 2 ||
		request.Members[1] != (currentAdventureExpeditionMemberRequest{Selector: 3, Job: 1, GrowType: 4}) {
		t.Fatalf("request=%+v", request)
	}
	if _, err := parseCurrentAdventureExpeditionStartRequest(body[:12]); err == nil {
		t.Fatal("truncated request accepted")
	}
}

func TestBuildCurrentAdventureExpeditionStateBodyMatchesCurrentReader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := adventuregroup.RuntimeState{
		Expeditions: map[string]adventuregroup.Expedition{
			"2": {
				Area:       2,
				State:      1,
				StartedAt:  now.Unix() - 10,
				EndsAt:     now.Unix() + 10,
				Attributes: []byte{4, 7},
				Members: []adventuregroup.ExpeditionMember{{
					CharacterID: 3,
					Name:        "牛牛",
					Level:       90,
					Job:         1,
					GrowType:    2,
				}},
			},
		},
	}
	body := buildCurrentAdventureExpeditionStateBody(state, now)
	offset := 0
	if body[offset] != 1 {
		t.Fatalf("area count=%d", body[offset])
	}
	offset++
	if body[offset] != 2 || body[offset+1] != 1 {
		t.Fatalf("area/state=%x", body[offset:offset+2])
	}
	offset += 2 + 4 + 4
	if body[offset] != 2 || body[offset+1] != 4 || body[offset+2] != 7 {
		t.Fatalf("attributes=%x", body[offset:offset+3])
	}
	offset += 3
	if binary.LittleEndian.Uint16(body[offset:offset+2]) != 1 {
		t.Fatalf("member count=%x", body[offset:offset+2])
	}
	offset += 2
	if binary.LittleEndian.Uint16(body[offset:offset+2]) != 3 {
		t.Fatalf("member id=%x", body[offset:offset+2])
	}
	offset += 2
	nameLength := int(binary.LittleEndian.Uint32(body[offset : offset+4]))
	if nameLength != 4 {
		t.Fatalf("GB18030 name length=%d body=%x", nameLength, body)
	}
}

func TestResolveCurrentAdventureShopProductAcceptsCurrentItemIDAndZeroBasedIndex(t *testing.T) {
	category := adventuregroup.ShopCategory{Products: []adventuregroup.ShopProduct{
		{ItemID: 100},
		{ItemID: 200},
	}}
	if product, ok := resolveCurrentAdventureShopProduct(category, 200); !ok || product.ItemID != 200 {
		t.Fatalf("item-id selector product=%+v ok=%v", product, ok)
	}
	if product, ok := resolveCurrentAdventureShopProduct(category, 0); !ok || product.ItemID != 100 {
		t.Fatalf("index selector product=%+v ok=%v", product, ok)
	}
}

// 冒险团商店直接卖传说灵魂（PVF [adventurer shop purchase info]：
// `10099775 15 70 3 3`），它的家是账号共享仓的 364 格。刷新报文必须报这一格 ——
// 只落库不发这一行，客户端要等下一次列表刷新才看得到买到的灵魂。
func TestSendCurrentAdventureInventoryUpdatesReportsAccountSharedSlot(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, connID: "adventure-shop-ack", selectedCharacterID: 22}
	result := currentAdventureShopCommit{
		Inventory: dnfrepo.InventoryRecord{CharacterID: "22", Slots: map[string]dnfrepo.ItemStack{}},
		AccountInventory: dnfrepo.AccountInventoryRecord{
			AccountID: "account-1",
			Slots:     map[string]dnfrepo.ItemStack{"0:364": {ItemID: 10099775, Count: 15}},
		},
		AccountSlots: []uint16{364},
	}
	if err := sendCurrentAdventureInventoryUpdates(service, session, result); err != nil {
		t.Fatal(err)
	}
	packets := splitAllFinishBridgePackets(t, connection.write.Bytes())
	if len(packets) != 1 || packets[0].Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("adventure shop update packets=%v", finishBridgePacketIDs(packets))
	}
	body := packets[0].Body
	if len(body) != 3+currentItemListEntryWireSize || body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(body[1:3]) != 1 {
		t.Fatalf("adventure shop update body=%x", body)
	}
	row := body[3:]
	if slot := binary.LittleEndian.Uint16(row[0:2]); slot != 364 {
		t.Fatalf("adventure shop update slot=%d want the account soul cell 364", slot)
	}
	if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != 10099775 {
		t.Fatalf("adventure shop update item=%d want 10099775", itemID)
	}
	if count := binary.LittleEndian.Uint32(row[6:10]); count != 15 {
		t.Fatalf("adventure shop update count=%d want 15", count)
	}
}

// 客户端 0x00C79940 的结果码跳转表只认 19/22/23/204/235；其余（含过去一律回的 1）
// 落到 default = 什么都不弹。这条用例把"超限没提示"钉死。
func TestCurrentAdventureShopFailureCodesMatchTheClientResultTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want byte
	}{
		{"purchase limit", adventuregroup.ErrPurchaseLimitReached, currentAdventureShopPurchaseLimitCode},
		{"insufficient points", adventuregroup.ErrInsufficientShopPoints, currentAdventureShopInsufficientCode},
		{"inventory full", errDungeonPickupInventoryFull, currentAdventureShopInventoryFullCode},
		{"wrapped limit", fmt.Errorf("shop: %w", adventuregroup.ErrPurchaseLimitReached), currentAdventureShopPurchaseLimitCode},
		{"unknown stays silent", errCurrentAdventureStateInvalid, currentAdventureGenericFailureCode},
		{"plain state invalid stays silent", adventuregroup.ErrRuntimeStateInvalid, currentAdventureGenericFailureCode},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := currentAdventureShopFailureCode(test.err); got != test.want {
				t.Fatalf("failure code=%d want=%d", got, test.want)
			}
		})
	}
	if currentAdventureShopPurchaseLimitCode != 19 || currentAdventureShopInsufficientCode != 22 ||
		currentAdventureShopLevelTooLowCode != 235 || currentAdventureShopInventoryFullCode != 204 {
		t.Fatalf("client result codes changed without re-reading 0x00C79940")
	}
}

// 商店点数的"进度"字段只由冒险团状态决定：骁勇结晶 = 距下一个结晶的余数，
// 成长胶囊页签 = 当前这一格的进度（累计值 mod 单格容量），其余分类恒 0
// （它们没有经验窗口，绝不能回落成早期那种"分类号当进度"的伪值）。
func TestAdventureShopPointProgressFollowsRuntimeState(t *testing.T) {
	projection := adventuregroup.Projection{
		Runtime: adventuregroup.RuntimeState{
			BraveExperience: 1_912_720,
			// 一格 = 116,914,950；这里正好一格 + 半格，进度必须是那"半格"。
			GrowthExperience: 175_372_425,
		},
		CapsuleGauge: 116_914_950,
	}
	if got := currentAdventureInfoShopPointProgress(int(adventuregroup.ShopPointBrave), projection); got != 1_912_720 {
		t.Fatalf("brave progress=%d", got)
	}
	if got := currentAdventureInfoShopPointProgress(currentAdventureInfoCapsuleProgressEntry, projection); got != 58_457_475 {
		t.Fatalf("capsule progress=%d want the current gauge remainder", got)
	}
	for _, index := range []int{int(adventuregroup.ShopPointGlory), int(adventuregroup.ShopPointPure), 4} {
		if got := currentAdventureInfoShopPointProgress(index, projection); got != 0 {
			t.Fatalf("progress[%d]=%d want 0", index, got)
		}
	}
	// 正好一格（旧行为下会卡死的那一点）：进度必须是 0 而不是 100%，
	// 而且再攒一点就应该立刻动起来 —— 现场"胶囊不累计"就是这个位置。
	projection.Runtime.GrowthExperience = 116_914_950
	if got := currentAdventureInfoShopPointProgress(currentAdventureInfoCapsuleProgressEntry, projection); got != 0 {
		t.Fatalf("exactly-one-gauge progress=%d want 0", got)
	}
	projection.Runtime.GrowthExperience += 1_000_000
	if got := currentAdventureInfoShopPointProgress(currentAdventureInfoCapsuleProgressEntry, projection); got != 1_000_000 {
		t.Fatalf("progress after one gauge=%d want 1000000", got)
	}
	// 缺 PVF 分母时不猜：必须回 0。
	projection.CapsuleGauge = 0
	if got := currentAdventureInfoShopPointProgress(currentAdventureInfoCapsuleProgressEntry, projection); got != 0 {
		t.Fatalf("progress without a gauge=%d want 0", got)
	}
}
