package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseCurrentDisjointAndEmblemRequests(t *testing.T) {
	disjoint := currentDisjointItemTestBody(65, 0, -1, 0x11223344)
	parsed, err := parseCurrentDisjointItemRequest(disjoint)
	if err != nil || parsed.SourceSlot != 65 || parsed.ListType != 0 || parsed.ToolSlot != -1 || parsed.Context != 0x11223344 {
		t.Fatalf("op26 request=%+v err=%v", parsed, err)
	}
	avatar, err := parseCurrentAvatarDisjointRequest([]byte{2, 0, 0x44, 0x33, 0x22, 0x11})
	if err != nil || avatar.SourceSlot != 2 || avatar.ExpectedItemID != 0x11223344 {
		t.Fatalf("op202 request=%+v err=%v", avatar, err)
	}
	emblemBody := currentEmblemCompoundTestBody([]currentEmblemCompoundInput{{ItemID: 8000, Slot: 70}, {ItemID: 8000, Slot: 71}})
	emblem, err := parseCurrentEmblemCompoundRequest(emblemBody)
	if err != nil || len(emblem.Inputs) != 2 || emblem.Inputs[1].Slot != 71 {
		t.Fatalf("op256 request=%+v err=%v", emblem, err)
	}
	repeatedEmblemBody := currentEmblemCompoundTestBody([]currentEmblemCompoundInput{{ItemID: 8000, Slot: 70}, {ItemID: 8000, Slot: 70}})
	repeatedEmblems, err := parseCurrentEmblemCompoundRequest(repeatedEmblemBody)
	if err != nil || len(repeatedEmblems.Inputs) != 2 || repeatedEmblems.Inputs[0].Slot != repeatedEmblems.Inputs[1].Slot {
		t.Fatalf("same-slot op256 request=%+v err=%v", repeatedEmblems, err)
	}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketDisjointItem), append(append([]byte(nil), disjoint...), 1, 2, 3, 4)); !bytes.Equal(got, disjoint) {
		t.Fatalf("op26 normalized=%x want=%x", got, disjoint)
	}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketCompoundEmblem), append(append([]byte(nil), emblemBody...), 1, 2, 3, 4)); !bytes.Equal(got, emblemBody) {
		t.Fatalf("op256 normalized=%x want=%x", got, emblemBody)
	}
	avatarBody := []byte{2, 0, 0x44, 0x33, 0x22, 0x11}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketDisjointAvatar), avatarBody); !bytes.Equal(got, avatarBody) {
		t.Fatalf("six-byte op202 normalized=%x want=%x", got, avatarBody)
	}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketDisjointAvatar), append(append([]byte(nil), avatarBody...), 1, 2, 3, 4)); !bytes.Equal(got, avatarBody) {
		t.Fatalf("trailed op202 normalized=%x want=%x", got, avatarBody)
	}
	for _, body := range [][]byte{disjoint[:4], append(disjoint, 0), []byte{0, 0, 1, 0, 0}} {
		if _, err := parseCurrentDisjointItemRequest(body); err == nil {
			t.Fatalf("malformed op26 accepted: %x", body)
		}
	}
	if _, err := parseCurrentEmblemCompoundRequest([]byte{1, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("one-input op256 accepted")
	}
}

func TestCurrentEquipmentDisjointAppliesPVFEquipmentSellRate(t *testing.T) {
	document, err := dnfpvf.Parse("equipment/rate_test.equ", "[rarity]\n0\n[value]\n18000\n")
	if err != nil {
		t.Fatal(err)
	}
	rewards, err := currentEquipmentDisjointRewards(currentDisjointPVFConfig{
		CubeItemID: 3037, CubeBase: 150, CubeMultipliers: []float64{1.12}, EquipmentSellRate: 200,
	}, 0, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewards) != 1 || rewards[0] != (currentDisjointReward{ItemID: 3037, Count: 26}) {
		t.Fatalf("disjoint rewards=%+v want sell-rate-adjusted 3037x26", rewards)
	}
}

func TestCurrentDisjointParsesAndAddsExactIndependentResults(t *testing.T) {
	document, err := dnfpvf.Parse("etc/disjoint.etc", "[independent result]\n[particular item]\n[input index]\n5001 5002\n[/input index]\n[output index]\n9100 1000 4\n[/output index]\n[/particular item]\n[/independent result]\n")
	if err != nil {
		t.Fatal(err)
	}
	mapping := currentDisjointParseIndependentResults(document)
	for _, itemID := range []uint32{5001, 5002} {
		if !reflect.DeepEqual(mapping[itemID], []currentDisjointReward{{ItemID: 9100, Count: 4}}) {
			t.Fatalf("item=%d independent rewards=%+v", itemID, mapping[itemID])
		}
	}
	equipment, err := dnfpvf.Parse("equipment/independent.equ", "[equipment type] `[coat]`\n[rarity] 0\n[minimum level] 10\n[value] 300\n")
	if err != nil {
		t.Fatal(err)
	}
	rewards, err := currentEquipmentDisjointRewards(currentDisjointPVFConfig{
		CubeItemID: 3037, CubeBase: 150, CubeMultipliers: []float64{1}, EquipmentSellRate: 1000,
		IndependentByItem: mapping,
	}, 5001, equipment)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rewards, []currentDisjointReward{{ItemID: 3037, Count: 2}, {ItemID: 9100, Count: 4}}) {
		t.Fatalf("independent disjoint rewards=%+v", rewards)
	}
	ordinary, err := currentEquipmentDisjointRewards(currentDisjointPVFConfig{
		CubeItemID: 3037, CubeBase: 150, CubeMultipliers: []float64{1}, EquipmentSellRate: 1000,
		IndependentByItem: mapping,
	}, 5003, equipment)
	if err != nil || !reflect.DeepEqual(ordinary, []currentDisjointReward{{ItemID: 3037, Count: 2}}) {
		t.Fatalf("ordinary rewards=%+v err=%v", ordinary, err)
	}
}

func TestCurrentEquipmentDisjointAllowsTitleWithoutSaleValue(t *testing.T) {
	title, err := dnfpvf.Parse("equipment/title/test.equ", "[equipment type] `[title name]`\n[rarity] 6\n[minimum level] 1\n")
	if err != nil {
		t.Fatal(err)
	}
	multipliers := make([]float64, 7)
	for index := range multipliers {
		multipliers[index] = 1
	}
	rewards, err := currentEquipmentDisjointRewards(currentDisjointPVFConfig{
		CubeItemID: 3037, CubeBase: 150, CubeMultipliers: multipliers, EquipmentSellRate: 1000,
	}, 5200, title)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rewards, []currentDisjointReward{{ItemID: 3037, Count: 1}}) {
		t.Fatalf("title rewards=%+v", rewards)
	}
}

func TestCurrentDisjointParsesRarityMaterialsAndSoulRows(t *testing.T) {
	additional := currentDisjointParseAdditional([]int64{
		0,
		4, 3033, 3034, 3035, 3036,
		1, 3166,
		1, 3167,
		0,
		1, 3229,
		1, 3332,
	})
	if len(additional) != 7 || len(additional[0]) != 0 || !reflect.DeepEqual(additional[1], []uint32{3033, 3034, 3035, 3036}) ||
		!reflect.DeepEqual(additional[2], []uint32{3166}) || !reflect.DeepEqual(additional[3], []uint32{3167}) ||
		len(additional[4]) != 0 || !reflect.DeepEqual(additional[5], []uint32{3229}) || !reflect.DeepEqual(additional[6], []uint32{3332}) {
		t.Fatalf("additional rarity rows=%v", additional)
	}
	consts := currentDisjointParseAdditionalConsts([]float64{
		0,
		0.30, 8.70, 8, 5,
		50, 50, 8, 10,
		0.19, 5.65, 8, 20,
		0,
		1.30, 5.50, 6, 15,
		0.19, 5.65, 8, 20,
	}, additional)
	if len(consts) != 7 || consts[1].GreatDivisor != 0.30 || consts[1].NormalDivisor != 8.70 || consts[1].GreatChance != 8 || consts[1].NormalChance != 5 ||
		consts[2].NormalChance != 10 || consts[3].NormalChance != 20 || consts[5].NormalChance != 15 || consts[6].NormalChance != 20 {
		t.Fatalf("additional const rows=%+v", consts)
	}
	expands := currentDisjointParseExpands([]int64{
		1, 10100115,
		1, 10100116,
		1, 10099773,
		1, 10099774,
		1, 10158124,
		0,
		1, 10099775,
	}, []float64{
		25, 8.70, 100,
		25, 8.70, 100,
		20, 50, 100,
		17, 0.19, 100,
		9999, 0.19, 100,
		0,
		16, 0.19, 100,
	}, len(additional))
	wantSouls := []uint32{10100115, 10100116, 10099773, 10099774, 10158124, 0, 10099775}
	if len(expands) != len(wantSouls) {
		t.Fatalf("soul rows=%+v", expands)
	}
	for rarity, itemID := range wantSouls {
		if expands[rarity].ItemID != itemID || expands[rarity].Enabled != (itemID != 0) {
			t.Fatalf("rarity=%d soul=%+v want_item=%d", rarity, expands[rarity], itemID)
		}
	}
	config := currentDisjointPVFConfig{Expands: expands}
	for rarity, itemID := range wantSouls {
		reward, granted, err := currentDisjointExpandReward(config, rarity, 90)
		if err != nil {
			t.Fatalf("rarity=%d expand error=%v", rarity, err)
		}
		if itemID == 0 {
			if granted {
				t.Fatalf("rarity=%d unexpectedly granted %+v", rarity, reward)
			}
			continue
		}
		if !granted || reward.ItemID != itemID || reward.Count == 0 {
			t.Fatalf("rarity=%d reward=%+v granted=%t want_item=%d", rarity, reward, granted, itemID)
		}
	}
}

func TestCurrentEquipmentDisjointSendsGrantedPopupThenFinalStackRefresh(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{"0:65": {ItemID: 5000, Count: 1}})
	ctx := context.Background()
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 800},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	request := currentDisjointItemTestBody(65, 0, -1, 0)
	if err := service.handleCurrentDisjointItem(&gameSession{conn: connection, selectedCharacterID: 19}, request); err != nil {
		t.Fatal(err)
	}
	source, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if source.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || source.Header.Classification != 0 ||
		len(source.Body) != 3+currentItemListEntryWireSize || source.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(source.Body[1:3]) != 1 || binary.LittleEndian.Uint16(source.Body[3:5]) != 65 ||
		binary.LittleEndian.Uint32(source.Body[5:9]) != math.MaxUint32 || binary.LittleEndian.Uint32(source.Body[9:13]) != 0 {
		t.Fatalf("op26 source refresh=%x header=%+v remaining=%d", source.Body, source.Header, len(remaining))
	}
	ack, remaining := splitGameServerUpperPacket(t, remaining)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketDisjointItem) || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) != 1+2+1+2+1+2*10 || ack.Body[0] != 1 || binary.LittleEndian.Uint16(ack.Body[1:3]) != 65 || ack.Body[3] != 0 || binary.LittleEndian.Uint16(ack.Body[4:6]) != math.MaxUint16 || ack.Body[6] != 2 {
		t.Fatalf("op26 ACK=%x header=%+v remaining=%d", ack.Body, ack.Header, len(remaining))
	}
	var clearCubeGranted uint32
	for offset := 7; offset < len(ack.Body); offset += 10 {
		if binary.LittleEndian.Uint32(ack.Body[offset+2:offset+6]) == 3037 {
			clearCubeGranted = binary.LittleEndian.Uint32(ack.Body[offset+6 : offset+10])
		}
	}
	if clearCubeGranted != 2 {
		t.Fatalf("op26 clear-cube granted=%d want this disjoint amount 2; ACK=%x", clearCubeGranted, ack.Body)
	}
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || refresh.Header.Classification != 0 ||
		len(refresh.Body) != 3+2*currentItemListEntryWireSize || refresh.Body[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(refresh.Body[1:3]) != 2 {
		t.Fatalf("op26 inventory refresh=%x header=%+v trailing=%d", refresh.Body, refresh.Header, len(trailing))
	}
	var clearCubeFinal uint32
	for offset := 3; offset < len(refresh.Body); offset += currentItemListEntryWireSize {
		if binary.LittleEndian.Uint16(refresh.Body[offset:offset+2]) == 358 && binary.LittleEndian.Uint32(refresh.Body[offset+2:offset+6]) == 3037 {
			clearCubeFinal = binary.LittleEndian.Uint32(refresh.Body[offset+6 : offset+10])
		}
	}
	if clearCubeFinal != 802 {
		t.Fatalf("op14 clear-cube final stack=%d want 802; refresh=%x", clearCubeFinal, refresh.Body)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:65"]; exists {
		t.Fatalf("equipment source was not consumed: %+v", inventory.Slots["0:65"])
	}
	assertCurrentDisjointInventoryAmount(t, inventory, 3037, 0)
	assertCurrentDisjointInventoryAmount(t, inventory, 3033, 0)
	account, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("account inventory found=%t err=%v", found, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count != 802 {
		t.Fatalf("crystal warehouse slot 358=%+v want cumulative 3037x802", stack)
	}
	if stack := account.Slots["0:354"]; stack.ItemID != 3033 || stack.Count != 2 {
		t.Fatalf("crystal warehouse slot 354=%+v want 3033x2", stack)
	}
}

func TestCurrentEquipmentDisjointConsumesTitleAndGrantsMinimumCube(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{"0:65": {ItemID: 5200, Count: 1}})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentDisjointItem(
		&gameSession{conn: connection, selectedCharacterID: 19},
		currentDisjointItemTestBody(65, 0, -1, 0),
	); err != nil {
		t.Fatal(err)
	}
	if packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes()); packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("title first response header=%+v body=%x", packet.Header, packet.Body)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("title inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:65"]; exists {
		t.Fatal("title source remains after successful disjoint")
	}
	account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("title account inventory found=%t err=%v", found, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count < 1 {
		t.Fatalf("title cube stack=%+v", stack)
	}
}

func TestCurrentEquipmentDisjointBatchCommitsAllSelectedThenSendsOneMergedResult(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:65": {ItemID: 5000, Count: 1},
		"0:66": {ItemID: 5000, Count: 1},
	})
	ctx := context.Background()
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 800},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	body := currentDisjointBatchTestBody([]int16{65, 66}, 0, -1, 317)
	if err := service.handleCurrentDisjointItem(&gameSession{conn: connection, selectedCharacterID: 19}, body); err != nil {
		t.Fatal(err)
	}

	merged, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 {
		t.Fatalf("batch trailing bytes=%d", len(trailing))
	}
	if merged.Header.MsgID != uint16(dnfenum.CmdPacketDisjointItem) ||
		merged.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(merged.Body) != 27 || merged.Body[0] != 1 ||
		binary.LittleEndian.Uint16(merged.Body[1:3]) != 65 || merged.Body[6] != 2 {
		t.Fatalf("merged batch ACK body=%x header=%+v", merged.Body, merged.Header)
	}
	mergedRewards := map[uint32]uint32{}
	for offset := 7; offset+10 <= len(merged.Body); offset += 10 {
		mergedRewards[binary.LittleEndian.Uint32(merged.Body[offset+2:offset+6])] = binary.LittleEndian.Uint32(merged.Body[offset+6 : offset+10])
	}
	if mergedRewards[3033] != 4 || mergedRewards[3037] != 4 {
		t.Fatalf("merged batch rewards=%v body=%x", mergedRewards, merged.Body)
	}
	if refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		refresh.Header.Classification != 0 || len(refresh.Body) < 3 ||
		refresh.Body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("batch refresh=%x header=%+v", refresh.Body, refresh.Header)
	}
	refreshCount := int(binary.LittleEndian.Uint16(refresh.Body[1:3]))
	if len(refresh.Body) != 3+refreshCount*currentItemListEntryWireSize {
		t.Fatalf("batch refresh body_len=%d count=%d", len(refresh.Body), refreshCount)
	}
	clearedSources := map[int16]bool{}
	for index := 0; index < refreshCount; index++ {
		start := 3 + index*currentItemListEntryWireSize
		row := refresh.Body[start : start+currentItemListEntryWireSize]
		slot := int16(binary.LittleEndian.Uint16(row[0:2]))
		if slot != 65 && slot != 66 {
			continue
		}
		if binary.LittleEndian.Uint32(row[2:6]) != math.MaxUint32 ||
			binary.LittleEndian.Uint32(row[6:10]) != 0 {
			t.Fatalf("batch consumed source slot=%d row=%x", slot, row[:10])
		}
		clearedSources[slot] = true
	}
	if !clearedSources[65] || !clearedSources[66] {
		t.Fatalf("batch refresh missing consumed sources: %+v body=%x", clearedSources, refresh.Body)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:65"]; exists {
		t.Fatal("first batch source remains")
	}
	if _, exists := inventory.Slots["0:66"]; exists {
		t.Fatal("second batch source remains")
	}
	account, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("account inventory found=%t err=%v", found, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count != 804 {
		t.Fatalf("batch cube stack=%+v want 3037x804", stack)
	}
}

func TestCurrentEquipmentDisjointBatchRollsBackEverySourceWhenOneIsStale(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:65": {ItemID: 5000, Count: 1},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	body := currentDisjointBatchTestBody([]int16{65, 66}, 0, -1, 317)
	if err := service.handleCurrentDisjointItem(&gameSession{conn: connection, selectedCharacterID: 19}, body); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketDisjointItem) ||
		!bytes.Equal(failure.Body, []byte{0, 4}) {
		t.Fatalf("batch failure=%x header=%+v trailing=%d", failure.Body, failure.Header, len(trailing))
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if stack, exists := inventory.Slots["0:65"]; !exists || stack.ItemID != 5000 || stack.Count != 1 {
		t.Fatalf("first source mutated after batch rollback: exists=%t stack=%+v", exists, stack)
	}
}

func TestCurrentEquipmentDisjointRejectsLockedSourceWithoutMutation(t *testing.T) {
	// 锁定（ALT 锁 / 解锁申请中）的装备不能被分解，而且必须报"锁定"而不是笼统的 4：
	// 客户端 op26 的码表里 4 与"物品不存在"同格，玩家看不出是锁。
	for _, state := range []string{"1", "2", "active", "locked", "unlocking", "pending_unlock"} {
		t.Run(state, func(t *testing.T) {
			repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
				"0:65": {ItemID: 5000, Count: 1, Extra: map[string]string{"equipment_lock_state": state}},
			})
			connection := &bufferConn{}
			service := currentDisjointTestService(repositories, catalog)
			if err := service.handleCurrentDisjointItem(&gameSession{conn: connection, selectedCharacterID: 19}, currentDisjointItemTestBody(65, 0, -1, 0)); err != nil {
				t.Fatal(err)
			}
			failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketDisjointItem) ||
				!bytes.Equal(failure.Body, []byte{0, currentDisjointLockedFailureCode}) {
				t.Fatalf("state=%s failure=%x header=%+v trailing=%d", state, failure.Body, failure.Header, len(trailing))
			}
			if currentDisjointLockedFailureCode == currentDisjointFailureCode {
				t.Fatalf("锁定码与通用码相同(%d)：客户端只会画'物品不存在'，锁的提示丢失", currentDisjointLockedFailureCode)
			}
			inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
			if stack, exists := inventory.Slots["0:65"]; !exists || stack.ItemID != 5000 || stack.Count != 1 ||
				stack.Extra["equipment_lock_state"] != state {
				t.Fatalf("锁定源被改动: exists=%t stack=%+v", exists, stack)
			}
		})
	}
}

func TestCurrentEquipmentDisjointBatchRejectsLockedSourceAsLocked(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:65": {ItemID: 5000, Count: 1},
		"0:66": {ItemID: 5000, Count: 1, Extra: map[string]string{"equipment_lock_state": "locked"}},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentDisjointItem(&gameSession{conn: connection, selectedCharacterID: 19}, currentDisjointBatchTestBody([]int16{65, 66}, 0, -1, 317)); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketDisjointItem) ||
		!bytes.Equal(failure.Body, []byte{0, currentDisjointLockedFailureCode}) {
		t.Fatalf("batch failure=%x header=%+v trailing=%d", failure.Body, failure.Header, len(trailing))
	}
	// 整批回滚：没被锁的第一件也不能被吃掉。
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	for _, key := range []string{"0:65", "0:66"} {
		if stack, exists := inventory.Slots[key]; !exists || stack.ItemID != 5000 || stack.Count != 1 {
			t.Fatalf("锁定批被拒后 %s 仍被改动: exists=%t stack=%+v", key, exists, stack)
		}
	}
}

func TestCurrentAvatarDisjointUsesRuntimeDisjointInfoAndPopupBody(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{"1:2": {ItemID: 5100, Count: 0}})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentAvatarDisjoint(&gameSession{conn: connection, selectedCharacterID: 19}, []byte{2, 0, 0xec, 0x13, 0, 0}); err != nil {
		t.Fatal(err)
	}
	source, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if source.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || source.Header.Classification != 0 ||
		len(source.Body) != 3+currentItemListEntrySizeForType(currentAvatarInventoryListType) || source.Body[0] != currentAvatarInventoryListType ||
		binary.LittleEndian.Uint16(source.Body[1:3]) != 1 || binary.LittleEndian.Uint16(source.Body[3:5]) != 2 ||
		binary.LittleEndian.Uint32(source.Body[5:9]) != math.MaxUint32 || binary.LittleEndian.Uint32(source.Body[9:13]) != 0 {
		t.Fatalf("op202 source refresh=%x header=%+v remaining=%d", source.Body, source.Header, len(remaining))
	}
	ack, remaining := splitGameServerUpperPacket(t, remaining)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketDisjointAvatar) || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(ack.Body) != 1+2+2+10 || ack.Body[0] != 1 || binary.LittleEndian.Uint16(ack.Body[1:3]) != 2 || binary.LittleEndian.Uint16(ack.Body[3:5]) != 1 ||
		binary.LittleEndian.Uint16(ack.Body[5:7]) != 289 || binary.LittleEndian.Uint32(ack.Body[7:11]) != 9001 || binary.LittleEndian.Uint32(ack.Body[11:15]) != 1 {
		t.Fatalf("op202 ACK=%x header=%+v remaining=%d", ack.Body, ack.Header, len(remaining))
	}
	reward, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || reward.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || reward.Header.Classification != 0 ||
		len(reward.Body) != 3+currentItemListEntryWireSize || reward.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(reward.Body[1:3]) != 1 || binary.LittleEndian.Uint16(reward.Body[3:5]) != 289 ||
		binary.LittleEndian.Uint32(reward.Body[5:9]) != 9001 || binary.LittleEndian.Uint32(reward.Body[9:13]) != 1 {
		t.Fatalf("op202 reward refresh=%x header=%+v trailing=%d", reward.Body, reward.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if _, exists := inventory.Slots["1:2"]; exists {
		t.Fatal("avatar source was not consumed")
	}
	if stack := inventory.Slots["0:289"]; stack.ItemID != 9001 || stack.Count != 1 {
		t.Fatalf("emblem page slot 289=%+v want 9001x1", stack)
	}
}

// op202 弹窗体里那个 u32 是**本次新增量**，不是合并后的整堆终值。
//
// 这条口径与 op26（disjoint_compound_protocol.go 的 buildCurrentDisjointItemSuccessBody）、
// op235（expert_job_store.go）和 op256（buildCurrentEmblemCompoundSuccessBody）完全一致，
// 而且 op26 侧早有断言钉死（TestCurrentEquipmentDisjointSendsGrantedPopupThenFinalStackRefresh：
// 账号仓先有 800 个无色、分解只出 2 个时弹窗字段必须是 2，终值 802 由随后的 0x14 增量行负责）。
//
// 时装分解漏了这一步：它发的是 reward.Count（终值）。于是玩家背包里已经有一摞同款徽章时，
// 弹窗会把老存量一起算进去 —— 现场表现就是"分解结果数量 = 本次产出 + 背包已有数量"，
// 而背包本身不多不少（终值行是对的），即纯显示错误。
func TestCurrentAvatarDisjointPopupReportsGrantedNotFinalStack(t *testing.T) {
	// 现场复现：徽章页 289 格本就有 5 个同款 9001。本次分解产出 1 个并进这一行，
	// 于是终值 6、本次新增 1 —— 这两个数字必须分别落到不同的包体字段里。
	// （存量必须放在徽章页 289..351 内：dungeonDropStackableSlotRange 只在该区间内合并，
	// 放在装备区会另开一行，弹窗数字就退化成同样看不出问题的 1 了。）
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"1:2":   {ItemID: 5100, Count: 0},
		"0:289": {ItemID: 9001, Count: 5},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentAvatarDisjoint(&gameSession{conn: connection, selectedCharacterID: 19}, []byte{2, 0, 0xec, 0x13, 0, 0}); err != nil {
		t.Fatal(err)
	}
	_, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	ack, remaining := splitGameServerUpperPacket(t, remaining)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketDisjointAvatar) ||
		len(ack.Body) != 1+2+2+10 || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint16(ack.Body[1:3]) != 2 ||
		binary.LittleEndian.Uint16(ack.Body[3:5]) != 1 ||
		binary.LittleEndian.Uint16(ack.Body[5:7]) != 289 ||
		binary.LittleEndian.Uint32(ack.Body[7:11]) != 9001 {
		t.Fatalf("op202 ACK=%x header=%+v", ack.Body, ack.Header)
	}
	if granted := binary.LittleEndian.Uint32(ack.Body[11:15]); granted != 1 {
		t.Fatalf("op202 弹窗数量=%d，必须是本次新增 1（终值 6 只能出现在 0x14 增量行里）；ACK=%x", granted, ack.Body)
	}
	// 紧随其后的 0x14 增量行才是权威终值：5 + 1 = 6。
	reward, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || reward.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || reward.Header.Classification != 0 ||
		len(reward.Body) != 3+currentItemListEntrySizeForType(dnfrepo.MainInventoryListType) || reward.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(reward.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint16(reward.Body[3:5]) != 289 ||
		binary.LittleEndian.Uint32(reward.Body[5:9]) != 9001 ||
		binary.LittleEndian.Uint32(reward.Body[9:13]) != 6 {
		t.Fatalf("op202 reward refresh=%x header=%+v trailing=%d", reward.Body, reward.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if stack := inventory.Slots["0:289"]; stack.ItemID != 9001 || stack.Count != 6 {
		t.Fatalf("emblem page slot 289=%+v want 9001x6", stack)
	}
	assertCurrentDisjointInventoryAmount(t, inventory, 9001, 6)
}

// TestCurrentAvatarDisjointUsesTheNinthEmblemRow 是"徽章栏最后一行不给用"的端到端回归。
//
// 服主现场口径：徽章页 9 行 × 7 格 = 63 格（289..351），服务端过去只认到 344，最后一行
// 345..351 从不落东西。危害不是"少 7 格"：这页只有 56 格时，被 56 种徽章占满后新徽章
// 只能进溢出邮件，而邮件领取又只能往同一页放 —— 那封邮件就永久领不出来。
//
// 用例把前 8 行 289..351 全占满，再分解一件时装；产出必须落进第 9 行的第一格 345。
// 改回 289..351 时这条会红：发放找不到落点 → 没有邮件溢出通道 → 整个分解失败。
func TestCurrentAvatarDisjointUsesTheNinthEmblemRow(t *testing.T) {
	slots := map[string]dnfrepo.ItemStack{"1:2": {ItemID: 5100, Count: 0}}
	// 前 56 格用 56 种不同道具占满，模拟"56 种徽章各占一行"的现场。
	for index := int16(0); index < currentAvatarEmblemPageSlotEnd-currentAvatarEmblemPageSlotStart+1-7; index++ {
		slot := currentAvatarEmblemPageSlotStart + index
		slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{
			ItemID: int64(2600000) + int64(index),
			Count:  1,
		}
	}
	repositories, catalog := mustCurrentDisjointTestAssets(t, slots)
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentAvatarDisjoint(&gameSession{conn: connection, selectedCharacterID: 19}, []byte{2, 0, 0xec, 0x13, 0, 0}); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	// 改回 289..351 时这里收到的是失败包 {0,4}：整段分解被拒，玩家什么都拿不到。
	if len(trailing) == 0 && failure.Header.MsgID == uint16(dnfenum.CmdPacketDisjointAvatar) && failure.Body[0] == 0 {
		t.Fatalf("徽章页第 9 行没被当落点，整段分解被拒：body=%x", failure.Body)
	}
	_, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	ack, _ := splitGameServerUpperPacket(t, remaining)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketDisjointAvatar) ||
		len(ack.Body) != 1+2+2+10 || ack.Body[0] != 1 {
		t.Fatalf("op202 ACK=%x header=%+v", ack.Body, ack.Header)
	}
	// 产出落在第 9 行（345），也就是"最后一行"的第一格。
	if slot := int16(binary.LittleEndian.Uint16(ack.Body[5:7])); slot != 345 {
		t.Fatalf("奖励落点 slot=%d，want 345（徽章页第 9 行第一格）; ACK=%x", slot, ack.Body)
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if landed := inventory.Slots["0:345"]; landed.ItemID != 9001 || landed.Count != 1 {
		t.Fatalf("第 9 行落点=%+v，want 9001 ×1", landed)
	}
	if _, exists := inventory.Slots["1:2"]; exists {
		t.Fatal("时装源行没有被消耗")
	}
}

func TestCurrentAvatarDisjointRejectsMismatchedExpectedItemWithoutMutation(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{"1:2": {ItemID: 5100, Count: 0}})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentAvatarDisjoint(&gameSession{conn: connection, selectedCharacterID: 19}, []byte{2, 0, 0xed, 0x13, 0, 0}); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketDisjointAvatar) || !bytes.Equal(failure.Body, []byte{0, 4}) {
		t.Fatalf("failure=%x header=%+v trailing=%d", failure.Body, failure.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if stack, exists := inventory.Slots["1:2"]; !exists || stack.ItemID != 5100 || stack.Count != 0 {
		t.Fatalf("avatar source mutated: exists=%t stack=%+v", exists, stack)
	}
}

func TestCurrentEmblemCompoundConsumesInputsRefreshesAndShowsRealReward(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 8000, Count: 1},
		"0:71": {ItemID: 8000, Count: 1},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentEmblemCompound(&gameSession{conn: connection, selectedCharacterID: 19}, currentEmblemCompoundTestBody([]currentEmblemCompoundInput{{ItemID: 8000, Slot: 70}, {ItemID: 8000, Slot: 71}})); err != nil {
		t.Fatal(err)
	}
	update, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || update.Header.Classification != 0 || len(update.Body) != 3+3*currentItemListEntryWireSize || update.Body[0] != 0 || binary.LittleEndian.Uint16(update.Body[1:3]) != 3 {
		t.Fatalf("op14=%x header=%+v", update.Body, update.Header)
	}
	ack, trailing := splitGameServerUpperPacket(t, rest)
	if len(ack.Body) != 10 || ack.Header.MsgID != uint16(dnfenum.CmdPacketCompoundEmblem) || ack.Header.Classification != dnfproto.DefaultChannelClassification || !bytes.Equal(ack.Body, []byte{1, 1, 0x29, 0x23, 0, 0, 1, 0, 0, 0}) {
		t.Fatalf("op256 ACK=%x header=%+v", ack.Body, ack.Header)
	}
	list, trailing := splitGameServerUpperPacket(t, trailing)
	if len(trailing) != 0 || list.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) || !bytes.Contains(list.Body, []byte{0x21, 0x01, 0x29, 0x23}) {
		t.Fatalf("full list-0 refresh=%x... header=%+v trailing=%d", list.Body[:min(16, len(list.Body))], list.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if _, exists := inventory.Slots["0:70"]; exists {
		t.Fatal("first emblem input remains")
	}
	if _, exists := inventory.Slots["0:71"]; exists {
		t.Fatal("second emblem input remains")
	}
	assertCurrentDisjointInventoryAmount(t, inventory, 9001, 1)
}

func TestCurrentEmblemCompoundConsumesRepeatedInputsFromOneStack(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 8000, Count: 2},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	body := currentEmblemCompoundTestBody([]currentEmblemCompoundInput{{ItemID: 8000, Slot: 70}, {ItemID: 8000, Slot: 70}})
	if err := service.handleCurrentEmblemCompound(&gameSession{conn: connection, selectedCharacterID: 19}, body); err != nil {
		t.Fatal(err)
	}
	update, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || update.Body[0] != 0 || binary.LittleEndian.Uint16(update.Body[1:3]) != 2 {
		t.Fatalf("same-slot op14=%x header=%+v", update.Body, update.Header)
	}
	ack, rest := splitGameServerUpperPacket(t, rest)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketCompoundEmblem) || !bytes.Equal(ack.Body, []byte{1, 1, 0x29, 0x23, 0, 0, 1, 0, 0, 0}) {
		t.Fatalf("same-slot op256 ACK=%x header=%+v", ack.Body, ack.Header)
	}
	_, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("same-slot trailing=%d", len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if _, exists := inventory.Slots["0:70"]; exists {
		t.Fatal("same-slot emblem stack remains")
	}
	assertCurrentDisjointInventoryAmount(t, inventory, 9001, 1)
}

func TestCurrentEmblemCompoundRejectsRepeatedInputsBeyondStackCount(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 8000, Count: 1},
	})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	body := currentEmblemCompoundTestBody([]currentEmblemCompoundInput{{ItemID: 8000, Slot: 70}, {ItemID: 8000, Slot: 70}})
	if err := service.handleCurrentEmblemCompound(&gameSession{conn: connection, selectedCharacterID: 19}, body); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketCompoundEmblem) || !bytes.Equal(failure.Body, []byte{0, 4}) {
		t.Fatalf("same-slot failure=%x header=%+v trailing=%d", failure.Body, failure.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if stack := inventory.Slots["0:70"]; stack.ItemID != 8000 || stack.Count != 1 {
		t.Fatalf("insufficient same-slot source mutated: %+v", stack)
	}
}

func TestCurrentDisjointFailureDoesNotConsumeSource(t *testing.T) {
	repositories, catalog := mustCurrentDisjointTestAssets(t, map[string]dnfrepo.ItemStack{"0:65": {ItemID: 5000, Count: 1}})
	connection := &bufferConn{}
	service := currentDisjointTestService(repositories, catalog)
	if err := service.handleCurrentAvatarDisjoint(&gameSession{conn: connection, selectedCharacterID: 19}, []byte{2, 0}); err != nil {
		t.Fatal(err)
	}
	failure, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || failure.Header.MsgID != uint16(dnfenum.CmdPacketDisjointAvatar) || !bytes.Equal(failure.Body, []byte{0, 4}) {
		t.Fatalf("failure=%x header=%+v trailing=%d", failure.Body, failure.Header, len(trailing))
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if stack := inventory.Slots["0:65"]; stack.ItemID != 5000 || stack.Count != 1 {
		t.Fatalf("source mutated: %+v", stack)
	}
}

func TestRealPVFDisjointAndEmblemMappings(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	config, err := loadCurrentDisjointPVFConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if config.CubeItemID != 3037 || len(config.AvatarByJob["fighter"]) != 15 || len(config.EmblemBoosters) != 13 || config.EmblemBoosters[[2]int{1, 2}] != 10095581 || config.EmblemBoosters[[2]int{4, 2}] != 10095593 {
		t.Fatalf("runtime mapping cube=%d fighter=%v boosters=%v", config.CubeItemID, config.AvatarByJob["fighter"], config.EmblemBoosters)
	}
	if !reflect.DeepEqual(config.IndependentByItem[100300212], []currentDisjointReward{{ItemID: 10093974, Count: 10}}) ||
		!reflect.DeepEqual(config.IndependentByItem[101020542], []currentDisjointReward{{ItemID: 10161538, Count: 4}}) {
		t.Fatalf("runtime independent disjoint mappings anton=%+v legendary=%+v", config.IndependentByItem[100300212], config.IndependentByItem[101020542])
	}
	if len(config.AvatarEmblemTables) < 3 || len(config.AvatarEmblemTables[2].Pools) == 0 || config.AvatarEmblemTables[2].Pools[0].PickCount != 4 {
		t.Fatalf("runtime avatar emblem tables=%d grade2 pools=%+v", len(config.AvatarEmblemTables), config.AvatarEmblemTables[2].Pools)
	}
	grade2Pool := config.AvatarEmblemTables[2].Pools[0]
	grade2Has2550046 := false
	for _, entry := range grade2Pool.Rewards {
		if entry.ItemID == 2550046 && entry.Weight == 1500 && entry.Count == 1 {
			grade2Has2550046 = true
		}
	}
	if !grade2Has2550046 {
		t.Fatalf("runtime grade2 pool missing 2550046/1500/1: %+v", grade2Pool.Rewards)
	}
	avatar, err := catalog.ResolveItem(412550012)
	if err != nil {
		t.Fatal(err)
	}
	avatarDoc, err := parseDungeonCardPVFDocument(archive, avatar.PVFPath)
	if err != nil {
		t.Fatal(err)
	}
	rewards, err := currentAvatarDisjointRewards(config, avatarDoc)
	if err != nil || len(rewards) == 0 {
		t.Fatalf("runtime avatar result=%+v err=%v", rewards, err)
	}
	avatarGrade, _ := avatarDoc.Int("grade")
	poolItems := make(map[uint32]bool)
	for _, pool := range config.AvatarEmblemTables[int(avatarGrade)].Pools {
		for _, entry := range pool.Rewards {
			poolItems[entry.ItemID] = true
		}
	}
	totalPicks := 0
	for _, pool := range config.AvatarEmblemTables[int(avatarGrade)].Pools {
		totalPicks += pool.PickCount
	}
	totalCount := uint32(0)
	for _, reward := range rewards {
		if !poolItems[reward.ItemID] || reward.Count == 0 {
			t.Fatalf("runtime avatar reward outside grade %d pool: %+v", avatarGrade, rewards)
		}
		if _, err := catalog.ResolveItem(reward.ItemID); err != nil {
			t.Fatalf("runtime avatar reward item=%d: %v", reward.ItemID, err)
		}
		totalCount += reward.Count
	}
	if totalCount < uint32(totalPicks) {
		t.Fatalf("runtime avatar rewards=%+v grade=%d want at least %d picks", rewards, avatarGrade, totalPicks)
	}
	reward, err := currentRollEmblemBoosterReward(catalog, 10095581)
	if err != nil || reward.ItemID == 0 || reward.Count == 0 {
		t.Fatalf("runtime booster result=%+v err=%v", reward, err)
	}
	if _, err := catalog.ResolveItem(reward.ItemID); err != nil {
		t.Fatalf("runtime booster reward item=%d: %v", reward.ItemID, err)
	}
	title, err := catalog.ResolveItem(400330106)
	if err != nil {
		t.Fatal(err)
	}
	titleDocument, err := parseDungeonCardPVFDocument(archive, title.PVFPath)
	if err != nil {
		t.Fatal(err)
	}
	titleRewards, err := currentEquipmentDisjointRewards(config, title.ItemID, titleDocument)
	if err != nil {
		t.Fatalf("runtime title disjoint: %v", err)
	}
	foundTitleCube := false
	for _, row := range titleRewards {
		if row.ItemID == config.CubeItemID && row.Count == 1 {
			foundTitleCube = true
		}
	}
	if !foundTitleCube {
		t.Fatalf("runtime title rewards=%+v want baseline cube=%d x1", titleRewards, config.CubeItemID)
	}
}

func TestCurrentGrantDisjointRewardsFixedWarehouseSlots(t *testing.T) {
	source := bridgePVFSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "3037 `cube.stk`\n3166 `element.stk`\n10099773 `soul.stk`\n",
		"stackable/cube.stk":      "[stackable type] `[material]`\n[stack limit] 999\n",
		"stackable/element.stk":   "[stackable type] `[material]`\n[stack limit] 999\n",
		"stackable/soul.stk":      "[stackable type] `[material]`\n[attach type] `[account]`\n[stack limit] 999\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	newOwners := func() (*dnfrepo.InventoryRecord, *dnfrepo.AccountInventoryRecord) {
		return &dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}},
			&dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
	}

	// Cube fragments and souls land on their live-tested fixed slots; element
	// crystals stay in the backpack.
	inventory, account := newOwners()
	rows, dirty, err := currentGrantDisjointRewards(inventory, account, catalog, []currentDisjointReward{{ItemID: 3037, Count: 10}, {ItemID: 3166, Count: 2}, {ItemID: 10099773, Count: 2}}, nil, 24)
	if err != nil || !dirty {
		t.Fatalf("grant rows=%+v dirty=%t err=%v", rows, dirty, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count != 10 {
		t.Fatalf("cube stack=%+v want 3037x10 at slot 358", stack)
	}
	if stack := account.Slots["0:362"]; stack.ItemID != 10099773 || stack.Count != 2 {
		t.Fatalf("soul stack=%+v want 10099773x2 at slot 362", stack)
	}
	assertCurrentDisjointInventoryAmount(t, *inventory, 3166, 2)
	if len(rows) != 3 || rows[0].Slot > rows[1].Slot || rows[1].Slot > rows[2].Slot {
		t.Fatalf("ack rows not slot-sorted: %+v", rows)
	}

	// Repeat grants merge into the same fixed slot stack.
	if _, dirty, err = currentGrantDisjointRewards(inventory, account, catalog, []currentDisjointReward{{ItemID: 3037, Count: 5}}, nil, 24); err != nil || !dirty {
		t.Fatalf("regrant dirty=%t err=%v", dirty, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count != 15 {
		t.Fatalf("merged cube stack=%+v want 3037x15", stack)
	}

	// A foreign item squatting on the fixed slot is never merged into or
	// evicted; the reward overflows to the backpack.
	inventory, account = newOwners()
	account.Slots["0:358"] = dnfrepo.ItemStack{ItemID: 3166, Count: 3}
	if _, dirty, err = currentGrantDisjointRewards(inventory, account, catalog, []currentDisjointReward{{ItemID: 3037, Count: 4}}, nil, 24); err != nil {
		t.Fatal(err)
	} else if dirty {
		t.Fatal("squatter grant must not dirty the account warehouse")
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3166 || stack.Count != 3 {
		t.Fatalf("squatter stack evicted: %+v", stack)
	}
	assertCurrentDisjointInventoryAmount(t, *inventory, 3037, 4)
}

func TestCurrentDisjointLevelDivisorCountClampsToOne(t *testing.T) {
	// 86JP CalculateLevelDivisorCount returns Math.Max(1, count): a level-1
	// source with a large divisor must still yield one item, not fail.
	for i := 0; i < 32; i++ {
		count, err := currentDisjointLevelDivisorCount(1, 8)
		if err != nil || count != 1 {
			t.Fatalf("level=1 divisor=8 count=%d err=%v", count, err)
		}
	}
	if count, err := currentDisjointLevelDivisorCount(0, 0); err != nil || count != 1 {
		t.Fatalf("zero divisor count=%d err=%v", count, err)
	}
}

func mustCurrentDisjointTestAssets(t *testing.T, slots map[string]dnfrepo.ItemStack) (dnfrepo.Group, *pvfDungeonDropCatalog) {
	t.Helper()
	source := bridgePVFSource{
		"monster/monster.lst":                     "",
		"stackable/stackable.lst":                 "3037 `cube.stk`\n3033 `additional.stk`\n2559002 `rare_red_box.stk`\n8000 `emblem.stk`\n9000 `compound_booster.stk`\n9001 `compound_result.stk`\n9100 `independent_result.stk`\n",
		"equipment/equipment.lst":                 "5000 `normal_coat.equ`\n5001 `independent_coat.equ`\n5100 `avatar_hat.equ`\n5200 `title.equ`\n",
		"stackable/cube.stk":                      "[stackable type] `[material]`\n[stack limit] 999\n",
		"stackable/additional.stk":                "[stackable type] `[material]`\n[stack limit] 999\n",
		"stackable/rare_red_box.stk":              "[stackable type] `[booster]`\n[stack limit] 999\n",
		"stackable/emblem.stk":                    "[stackable type] `[avatar emblem]`\n[grade] 1\n[stack limit] 999\n",
		"stackable/compound_booster.stk":          "[stackable type] `[booster]`\n[booster info]\n[stackable]\n1 9001 1 1\n[/stackable]\n[/booster info]\n",
		"stackable/compound_result.stk":           "[stackable type] `[avatar emblem]`\n[grade] 2\n[stack limit] 999\n",
		"stackable/independent_result.stk":        "[stackable type] `[material]`\n[stack limit] 999\n",
		"equipment/normal_coat.equ":               "[equipment type] `[coat]`\n[rarity] 0\n[minimum level] 10\n[value] 300\n",
		"equipment/independent_coat.equ":          "[equipment type] `[coat]`\n[rarity] 0\n[minimum level] 10\n[value] 300\n",
		"equipment/avatar_hat.equ":                "[equipment type] `[hat avatar]`\n[usable job] `[fighter]`\n[grade] 2\n",
		"equipment/title.equ":                     "[equipment type] `[title name]`\n[rarity] 0\n[minimum level] 1\n",
		"equipment/pricetable.tbl":                "[rate]\n1000 150 30\n",
		"etc/disjoint.etc":                        "[cube index]\n`[no element]` 3037\n[/cube index]\n[cube creation const]\n150 1\n[/cube creation const]\n[additional result]\n1 3033\n[/additional result]\n[additional result const]\n10 5 0 100\n[/additional result const]\n[additional result expand]\n0\n[/additional result expand]\n[additional result expand const]\n0\n[/additional result expand const]\n[independent result]\n[particular item]\n[input index]\n5001\n[/input index]\n[output index]\n9100 1000 4\n[/output index]\n[/particular item]\n[/independent result]\n[avatar disjoint info]\n[fighter]\n2559000 2559003 2559006 2559009 2559006 2559001 2559004 2559007 2559010 2559007 2559002 2559005 2559008 2559011 2559014\n[/fighter]\n[/avatar disjoint info]\n",
		"etc/emblemcompound.etc":                  "[emblem compound info]\n1 2 9000\n[/emblem compound info]\n",
		"etc/avatardisjoint/emblemlistinfo_0.etc": "[result info]\n1 9000 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_1.etc": "[result info]\n1 9000 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_2.etc": "[min max value]\n50 69\n[result info]\n1 9001 100 1 0\n[/result info]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentNPCShopRepositories(t, 0, slots)
	return repositories, catalog
}

func currentDisjointTestService(repositories dnfrepo.Group, catalog *pvfDungeonDropCatalog) *Service {
	return &Service{
		options:            options{accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
}

// countingDisjointSource wraps bridgePVFSource and counts ReadText calls so a
// regression can prove the disjoint config is parsed once per service.
type countingDisjointSource struct {
	bridgePVFSource
	reads atomic.Int64
}

func (s *countingDisjointSource) ReadText(relativePath string) (string, error) {
	s.reads.Add(1)
	return s.bridgePVFSource.ReadText(relativePath)
}

func newCountingDisjointSource() *countingDisjointSource {
	// Same PVF shape as mustCurrentDisjointTestAssets, restricted to the
	// documents the disjoint config loader itself reads.
	source := &countingDisjointSource{bridgePVFSource: bridgePVFSource{
		"equipment/pricetable.tbl":                "[rate]\n1000 150 30\n",
		"etc/disjoint.etc":                        "[cube index]\n`[no element]` 3037\n[/cube index]\n[cube creation const]\n150 1\n[/cube creation const]\n[additional result]\n1 3033\n[/additional result]\n[additional result const]\n10 5 0 100\n[/additional result const]\n[additional result expand]\n0\n[/additional result expand]\n[additional result expand const]\n0\n[/additional result expand const]\n[independent result]\n[particular item]\n[input index]\n5001\n[/input index]\n[output index]\n9100 1000 4\n[/output index]\n[/particular item]\n[/independent result]\n[avatar disjoint info]\n[fighter]\n2559000 2559003 2559006 2559009 2559006 2559001 2559004 2559007 2559010 2559007 2559002 2559005 2559008 2559011 2559014\n[/fighter]\n[/avatar disjoint info]\n",
		"etc/emblemcompound.etc":                  "[emblem compound info]\n1 2 9000\n[/emblem compound info]\n",
		"etc/avatardisjoint/emblemlistinfo_0.etc": "[result info]\n1 9000 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_1.etc": "[result info]\n1 9000 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_2.etc": "[min max value]\n50 69\n[result info]\n1 9001 100 1 0\n[/result info]\n",
	}}
	return source
}

// 分解配置必须按 service 记忆化：配置对同一 PVF 是不可变的，逐请求重新解析
// disjoint.etc/emblemcompound.etc/emblemlistinfo_*.etc 曾把纯 CPU 的
// tokenization 放进每个 op26/op202/op256 热路径。
func TestCurrentDisjointPVFConfigMemoizedPerService(t *testing.T) {
	source := newCountingDisjointSource()
	service := &Service{}
	first, err := service.currentDisjointPVFConfigCached(source)
	if err != nil {
		t.Fatalf("first config load: %v", err)
	}
	if first.CubeItemID != 3037 || len(first.EmblemBoosters) == 0 {
		t.Fatalf("first config cube=%d boosters=%v", first.CubeItemID, first.EmblemBoosters)
	}
	firstReads := source.reads.Load()

	for i := 0; i < 8; i++ {
		cached, err := service.currentDisjointPVFConfigCached(source)
		if err != nil {
			t.Fatalf("cached config load %d: %v", i, err)
		}
		if cached.CubeItemID != first.CubeItemID || len(cached.AvatarEmblemTables) != len(first.AvatarEmblemTables) {
			t.Fatalf("cached config drifted on call %d", i)
		}
	}
	if after := source.reads.Load(); after != firstReads {
		t.Fatalf("cached loads re-read PVF documents: before=%d after=%d", firstReads, after)
	}

	second := newCountingDisjointSource()
	if _, err := (&Service{}).currentDisjointPVFConfigCached(second); err != nil {
		t.Fatalf("fresh service load: %v", err)
	}
	if second.reads.Load() == 0 {
		t.Fatal("independent service must parse its own config")
	}
	if second.reads.Load() != firstReads {
		t.Fatalf("fresh service parse count drifted: first=%d second=%d", firstReads, second.reads.Load())
	}
}

func TestCurrentDisjointPVFConfigCacheRejectsNilInputs(t *testing.T) {
	service := &Service{}
	if _, err := service.currentDisjointPVFConfigCached(nil); err == nil {
		t.Fatal("nil PVF source accepted")
	}
	if _, err := (*Service)(nil).currentDisjointPVFConfigCached(newCountingDisjointSource()); err == nil {
		t.Fatal("nil service accepted")
	}
}

// 记忆化的失败不能在第二次请求时解引用空配置指针（旧树 287241d 修过的崩溃）。
func TestCurrentDisjointPVFConfigCacheMemoizesFailureWithoutPanic(t *testing.T) {
	service := &Service{}
	source := &countingDisjointSource{bridgePVFSource: bridgePVFSource{}}
	_, firstErr := service.currentDisjointPVFConfigCached(source)
	if firstErr == nil {
		t.Fatal("invalid PVF config unexpectedly loaded")
	}
	firstReads := source.reads.Load()

	_, secondErr := service.currentDisjointPVFConfigCached(source)
	if secondErr == nil {
		t.Fatal("cached PVF config failure unexpectedly cleared")
	}
	if secondErr.Error() != firstErr.Error() {
		t.Fatalf("cached failure changed: first=%v second=%v", firstErr, secondErr)
	}
	if after := source.reads.Load(); after != firstReads {
		t.Fatalf("cached failure re-read PVF documents: before=%d after=%d", firstReads, after)
	}
}

func currentDisjointItemTestBody(sourceSlot int16, listType byte, toolSlot int16, contextValue uint32) []byte {
	body := make([]byte, currentDisjointItemRequestSize+4)
	binary.LittleEndian.PutUint16(body[0:2], uint16(sourceSlot))
	body[2] = listType
	binary.LittleEndian.PutUint16(body[3:5], uint16(toolSlot))
	binary.LittleEndian.PutUint32(body[5:9], contextValue)
	return body
}

func currentDisjointBatchTestBody(slots []int16, listType byte, toolSlot int16, contextValue uint32) []byte {
	body := make([]byte, currentDisjointBatchHeaderSize+len(slots)*2)
	binary.LittleEndian.PutUint16(body[0:2], math.MaxUint16)
	body[2] = listType
	binary.LittleEndian.PutUint16(body[3:5], uint16(toolSlot))
	binary.LittleEndian.PutUint32(body[5:9], contextValue)
	copy(body[9:13], currentDisjointBatchMarker[:])
	body[13] = byte(len(slots))
	for index, slot := range slots {
		binary.LittleEndian.PutUint16(body[currentDisjointBatchHeaderSize+index*2:], uint16(slot))
	}
	return body
}

func currentEmblemCompoundTestBody(inputs []currentEmblemCompoundInput) []byte {
	body := make([]byte, 1+len(inputs)*currentEmblemCompoundInputSize)
	body[0] = byte(len(inputs))
	for index, input := range inputs {
		offset := 1 + index*currentEmblemCompoundInputSize
		binary.LittleEndian.PutUint32(body[offset:offset+4], input.ItemID)
		binary.LittleEndian.PutUint16(body[offset+4:offset+6], uint16(input.Slot))
	}
	return body
}

func assertCurrentDisjointInventoryAmount(t *testing.T, inventory dnfrepo.InventoryRecord, itemID int64, want int64) {
	t.Helper()
	var got int64
	for _, stack := range inventory.Slots {
		if stack.ItemID == itemID {
			got += stack.Count
		}
	}
	if got != want {
		t.Fatalf("item=%d count=%d want=%d inventory=%+v", itemID, got, want, inventory.Slots)
	}
}

// A full backpack used to abort the whole disassembly. The overflow sink now
// collects the rows that do not fit so the caller can post them as mail inside
// the same transaction; without a sink the strict behaviour is preserved,
// because consuming the source for nothing is worse than refusing.
func TestCurrentGrantDisjointRewardsRoutesFullBackpackToOverflow(t *testing.T) {
	source := bridgePVFSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "3166 `element.stk`\n",
		// A stack limit of 1 makes every unit need its own slot, so a handful
		// of units cannot fit the material page.
		"stackable/element.stk": "[stackable type] `[material]`\n[stack limit] 1\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	rewards := []currentDisjointReward{{ItemID: 3166, Count: 4000}}

	// Without a sink the grant still fails and the caller rolls back.
	inventory := &dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := &dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
	if _, _, err := currentGrantDisjointRewards(inventory, account, catalog, rewards, nil, 24); err == nil {
		t.Fatal("a full backpack with no overflow sink must still fail the disassembly")
	}

	// With a sink the grant succeeds and the remainder is reported for mail.
	inventory = &dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account = &dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
	overflow := []currentDisjointReward(nil)
	rows, _, err := currentGrantDisjointRewards(inventory, account, catalog, rewards, &overflow, 24)
	if err != nil {
		t.Fatalf("overflow sink must absorb a full backpack: %v", err)
	}
	if len(overflow) != 1 || overflow[0].ItemID != 3166 || overflow[0].Count == 0 {
		t.Fatalf("overflow=%+v", overflow)
	}
	// Everything that did fit was still granted, and the mailed remainder plus
	// the placed units account for the whole reward.
	placed := uint32(0)
	for _, row := range rows {
		placed += row.Granted
	}
	if placed+overflow[0].Count != 4000 {
		t.Fatalf("placed=%d mailed=%d want total 4000", placed, overflow[0].Count)
	}
}
