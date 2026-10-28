package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnflimitedcube "longheng.io/server/internal/modules/dnf/limitedcube"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestResolveCurrentLimitedCubePolicyUsesPVFConditionsAndWeights(t *testing.T) {
	catalog := newLimitedCubeTestCatalog(t)
	policy, recognized, err := resolveCurrentLimitedCubePolicy(catalog, 9000)
	if err != nil || !recognized {
		t.Fatalf("resolve recognized=%t err=%v", recognized, err)
	}
	if policy.TicketItemID != 9000 ||
		!reflect.DeepEqual(policy.Conditions, []dnflimitedcube.Requirement{{ItemID: 9001, Count: 1}, {ItemID: 9002, Count: 1}}) ||
		!reflect.DeepEqual(policy.Materials, []dnflimitedcube.Requirement{{ItemID: 3037, Count: 10}}) ||
		len(policy.Results) != 2 || policy.Results[0].Weight != 1000 || policy.Results[1].Weight != 1000 {
		t.Fatalf("policy = %+v", policy)
	}
	if first := policy.Results[0].Stack; first.ItemID != 9001 || first.Count != 1 || first.Extra["item_kind"] != "stackable" ||
		first.Extra["pvf_path"] != "stackable/bead/power.stk" || first.Extra["stackable_type"] != "[enchant waste]" {
		t.Fatalf("first result stack = %+v", first)
	}

	for _, itemID := range []int64{9003, 3037, 9999} {
		if _, recognized, err := resolveCurrentLimitedCubePolicy(catalog, itemID); err != nil || recognized {
			t.Fatalf("item %d recognized=%t err=%v, want non-limited cube", itemID, recognized, err)
		}
	}
}

func TestRealPVFPetBeadChangeTicketsResolveExactPVFPool(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate pet-bead change ticket PVF rules")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, ticketItemID := range []int64{490007246, 490701734} {
		policy, recognized, err := resolveCurrentLimitedCubePolicy(catalog, ticketItemID)
		if err != nil || !recognized {
			t.Fatalf("ticket %d recognized=%t err=%v", ticketItemID, recognized, err)
		}
		if policy.TicketItemID != ticketItemID || len(policy.Conditions) != 91 ||
			!reflect.DeepEqual(policy.Materials, []dnflimitedcube.Requirement{{ItemID: 3037, Count: 10}}) || len(policy.Results) != 91 {
			t.Fatalf("ticket %d policy shape = %+v", ticketItemID, policy)
		}
		for offset := int64(0); offset < 91; offset++ {
			wantItemID := int64(490007152) + offset
			if condition := policy.Conditions[offset]; condition.ItemID != wantItemID || condition.Count != 1 {
				t.Fatalf("ticket %d condition %d = %+v, want item=%d count=1", ticketItemID, offset, condition, wantItemID)
			}
			if result := policy.Results[offset]; result.Stack.ItemID != wantItemID || result.Stack.Count != 1 || result.Weight != 1000 {
				t.Fatalf("ticket %d result %d = %+v, want item=%d count=1 weight=1000", ticketItemID, offset, result, wantItemID)
			}
		}
	}
}

func TestRealPVFTitleChangeCouponsResolveEquipmentPool(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate the 称号随机变更券 rules")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	// 490702315 / 490702318 / 490702379 are the three 热舞一夏称号随机变更券
	// stackables and 490700080 is 劳动节称号随机变更券. All four are
	// `[stackable type] [upgrade limit cube]` tickets whose A-condition,
	// B-condition and result items are title equipment, not stackables — the
	// family the bridge used to refuse with "result must be a positive stackable
	// item" (现场 2026-09-27 15:14:56 op338 item=490702318 result=400330118).
	for _, test := range []struct {
		ticketItemID int64
		titleItemIDs []int64
		weights      []int64
	}{
		{490702315, []int64{400330118, 400330119, 400330120, 400330121}, []int64{3000, 3000, 3000, 1000}},
		{490702318, []int64{400330118, 400330119, 400330120, 400330121}, []int64{3000, 3000, 3000, 1000}},
		{490702379, []int64{400330118, 400330119, 400330120, 400330121}, []int64{3000, 3000, 3000, 1000}},
		{490700080, []int64{400330076, 400330077, 400330078}, []int64{4750, 4750, 500}},
	} {
		policy, recognized, err := resolveCurrentLimitedCubePolicy(catalog, test.ticketItemID)
		if err != nil || !recognized {
			t.Fatalf("ticket %d recognized=%t err=%v", test.ticketItemID, recognized, err)
		}
		if policy.TicketItemID != test.ticketItemID ||
			!reflect.DeepEqual(policy.Materials, []dnflimitedcube.Requirement{{ItemID: 3037, Count: 10}}) ||
			len(policy.Conditions) != len(test.titleItemIDs) || len(policy.Results) != len(test.titleItemIDs) {
			t.Fatalf("ticket %d policy shape = %+v", test.ticketItemID, policy)
		}
		for offset, titleItemID := range test.titleItemIDs {
			if condition := policy.Conditions[offset]; condition.ItemID != titleItemID || condition.Count != 1 {
				t.Fatalf("ticket %d condition %d = %+v, want item=%d count=1", test.ticketItemID, offset, condition, titleItemID)
			}
			result := policy.Results[offset]
			if result.Stack.ItemID != titleItemID || result.Stack.Count != 1 || result.Weight != test.weights[offset] {
				t.Fatalf("ticket %d result %d = %+v, want item=%d count=1 weight=%d",
					test.ticketItemID, offset, result, titleItemID, test.weights[offset])
			}
			if result.Stack.Extra["item_kind"] != "equipment" || result.Stack.Extra["equipment_type"] != "[title name]" ||
				!validCurrentEquipmentQualitySeed(sceneInventoryExtraUint32(result.Stack.Extra, "quality_seed")) {
				t.Fatalf("ticket %d result %d stack = %+v", test.ticketItemID, offset, result.Stack)
			}
		}
	}
}

func TestRealPVFPetBeadChangeTicketConsumesCrystalWarehouseMaterial(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate account crystal material consumption")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = catalog
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:95": {ItemID: 490007240, Count: 1},
			"0:97": {ItemID: 490701734, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 100},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 95)
	binary.LittleEndian.PutUint32(request[2:6], 490007240)
	binary.LittleEndian.PutUint16(request[6:8], 97)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}
	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || len(ack.Body) != 12 || ack.Body[0] != 1 {
		t.Fatalf("op338 acknowledgement=%+v body=%x", ack.Header, ack.Body)
	}
	cleared, _ := splitGameServerUpperPacket(t, remaining)
	if cleared.Header.Classification != 0 || cleared.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(cleared.Body) != 3+currentItemListEntryWireSize || cleared.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(cleared.Body[1:3]) != 1 {
		t.Fatalf("target replacement clear=%+v body=%x", cleared.Header, cleared.Body)
	}
	if got := limitedCubeRefreshRows(t, cleared.Body)[95]; got.itemID != ^uint32(0) || got.count != 0 {
		t.Fatalf("target replacement clear row=%+v", got)
	}
	account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if got := account.Slots["0:358"].Count; got != 90 {
		t.Fatalf("account clear cube count=%d want=90", got)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	// 490007240 is one of the 91 equally weighted candidates, so the draw may
	// hand it back; the invariant is that the ack and the durable slot agree and
	// that the bead is still one member of the ticket's own pool.
	bead := inventory.Slots["0:95"]
	promptItemID := int64(binary.LittleEndian.Uint32(ack.Body[1:5]))
	if bead.ItemID != promptItemID || bead.Count != 1 || bead.Extra["source"] != "limited_cube_result" {
		t.Fatalf("changed real-PVF bead = %+v, want item=%d count=1", bead, promptItemID)
	}
	if bead.ItemID < 490007152 || bead.ItemID > 490007152+90 {
		t.Fatalf("changed real-PVF bead item=%d is outside the ticket's own pool", bead.ItemID)
	}
}

func TestRealPVFTitleChangeCouponOp338TransformsWornBagTitle(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate the 热舞一夏称号随机变更券 op338 path")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = catalog
	// Reproduces the live 2026-09-27 attempt: the 2018热舞一夏自选礼盒 reward
	// 400330118 热浪舞者 landed in bag slot 18, the ticket 490702318 sat in slot
	// 103 and the player held 无色小晶块 in slot 9.
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:103": {ItemID: 490702318, Count: 1},
			"0:18": {
				ItemID: 400330118,
				Count:  1,
				Bind:   true,
				Extra: map[string]string{
					"item_kind":       "equipment",
					"equipment_type":  "[title name]",
					"quality_seed":    "959062881",
					"enchant_card_id": "10008663",
					"value_a":         "10008663",
					"seal_flag":       "1",
				},
			},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 18)
	binary.LittleEndian.PutUint32(request[2:6], 400330118)
	binary.LittleEndian.PutUint16(request[6:8], 103)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}
	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || len(ack.Body) != 12 || ack.Body[0] != 1 {
		t.Fatalf("op338 acknowledgement=%+v body=%x", ack.Header, ack.Body)
	}
	if _, trailing := splitGameServerUpperPacket(t, remaining); len(trailing) == 0 {
		t.Fatalf("op338 committed without the target replacement and refresh packets")
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:103"]; found {
		t.Fatalf("ticket remains in inventory: %+v", inventory.Slots)
	}
	if _, found := inventory.Slots["0:9"]; found {
		t.Fatalf("十 无色小晶块 remains in inventory: %+v", inventory.Slots)
	}
	title := inventory.Slots["0:18"]
	// The draw follows the PVF weights, which list 400330118 itself at 3000, so
	// any of the four 热舞一夏 titles is a legal outcome — including the one the
	// player put in.
	switch title.ItemID {
	case 400330118, 400330119, 400330120, 400330121:
	default:
		t.Fatalf("changed title item=%d, want one of the four 热舞一夏 titles; stack=%+v", title.ItemID, title)
	}
	if title.Count != 1 || !title.Bind || title.RawEntry != nil ||
		title.Extra["pvf_path"] == "" || title.Extra["item_kind"] != "equipment" {
		t.Fatalf("changed title = %+v", title)
	}
	if title.Extra["enchant_card_id"] != "10008663" || title.Extra["seal_flag"] != "1" {
		t.Fatalf("changed title dropped the 宝珠 enchant / 封装 state: %+v", title.Extra)
	}
	if !validCurrentEquipmentQualitySeed(sceneInventoryExtraUint32(title.Extra, "quality_seed")) {
		t.Fatalf("changed title quality seed = %q", title.Extra["quality_seed"])
	}
	if promptItemID := int64(binary.LittleEndian.Uint32(ack.Body[1:5])); promptItemID != title.ItemID {
		t.Fatalf("op338 prompt item=%d want transformed title=%d", promptItemID, title.ItemID)
	}
}

func TestCurrentLimitedCubeOp338ChangesPetBeadAndRefreshesAllChangedSlots(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1, Bind: true},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 7)
	binary.LittleEndian.PutUint32(request[2:6], 9001)
	binary.LittleEndian.PutUint16(request[6:8], 2)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}

	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	promptItemID := binary.LittleEndian.Uint32(ack.Body[1:5])
	// The pool is 9001/9002 at equal weight and the PVF weights are drawn as
	// published, so both outcomes are legal; what must hold is that the ack, the
	// refreshed row and the durable slot all name the same selected template.
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) ||
		len(ack.Body) != 12 || ack.Body[0] != 1 ||
		(promptItemID != 9001 && promptItemID != 9002) ||
		binary.LittleEndian.Uint16(ack.Body[5:7]) != 1 ||
		binary.LittleEndian.Uint32(ack.Body[7:11]) != 0 || ack.Body[11] != 1 {
		t.Fatalf("op338 acknowledgement = %+v body=%x", ack.Header, ack.Body)
	}
	cleared, remaining := splitGameServerUpperPacket(t, remaining)
	if cleared.Header.Classification != 0 || cleared.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(cleared.Body) != 3+currentItemListEntryWireSize || cleared.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(cleared.Body[1:3]) != 1 {
		t.Fatalf("target replacement clear header=%+v body=%x", cleared.Header, cleared.Body)
	}
	if got := limitedCubeRefreshRows(t, cleared.Body)[7]; got.itemID != ^uint32(0) || got.count != 0 {
		t.Fatalf("target replacement clear row=%+v", got)
	}
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || refresh.Header.Classification != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) != 3+3*currentItemListEntryWireSize || refresh.Body[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(refresh.Body[1:3]) != 3 {
		t.Fatalf("op14 refresh header=%+v body=%x trailing=%x", refresh.Header, refresh.Body, trailing)
	}
	updates := limitedCubeRefreshRows(t, refresh.Body)
	if got := updates[2]; got.itemID != ^uint32(0) || got.count != 0 {
		t.Fatalf("ticket update = %+v", got)
	}
	if got := updates[7]; got.itemID != promptItemID || got.count != 1 {
		t.Fatalf("bead result update = %+v, want item=%d count=1", got, promptItemID)
	}
	if got := updates[9]; got.itemID != ^uint32(0) || got.count != 0 {
		t.Fatalf("material update = %+v", got)
	}

	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:2"]; found {
		t.Fatalf("ticket remains in inventory: %+v", inventory.Slots)
	}
	bead := inventory.Slots["0:7"]
	if bead.ItemID != int64(promptItemID) || bead.Count != 1 || !bead.Bind || bead.Extra["source"] != "limited_cube_result" {
		t.Fatalf("changed bead = %+v, want item=%d", bead, promptItemID)
	}
	if _, found := inventory.Slots["0:9"]; found {
		t.Fatalf("material remains in inventory: %+v", inventory.Slots)
	}
}

func TestCurrentLimitedCubeOp338ConsumesCrystalWarehouseMaterial(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1, Bind: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 999},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 7)
	binary.LittleEndian.PutUint32(request[2:6], 9001)
	binary.LittleEndian.PutUint16(request[6:8], 2)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}

	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || len(ack.Body) != 12 || ack.Body[0] != 1 {
		t.Fatalf("op338 acknowledgement=%+v body=%x", ack.Header, ack.Body)
	}
	cleared, remaining := splitGameServerUpperPacket(t, remaining)
	if cleared.Header.Classification != 0 || cleared.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(cleared.Body) != 3+currentItemListEntryWireSize || cleared.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(cleared.Body[1:3]) != 1 {
		t.Fatalf("target replacement clear header=%+v body=%x", cleared.Header, cleared.Body)
	}
	if got := limitedCubeRefreshRows(t, cleared.Body)[7]; got.itemID != ^uint32(0) || got.count != 0 {
		t.Fatalf("target replacement clear row=%+v", got)
	}
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || refresh.Header.Classification != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) != 3+3*currentItemListEntryWireSize || refresh.Body[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(refresh.Body[1:3]) != 3 {
		t.Fatalf("op14 refresh header=%+v body=%x trailing=%x", refresh.Header, refresh.Body, trailing)
	}
	updates := limitedCubeRefreshRows(t, refresh.Body)
	if got := updates[358]; got.itemID != 3037 || got.count != 989 {
		t.Fatalf("account crystal update=%+v want item=3037 count=989", got)
	}
	account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if got := account.Slots["0:358"].Count; got != 989 {
		t.Fatalf("persisted account crystal count=%d want=989", got)
	}
}

func TestCurrentLimitedCubeDispatcherDoesNotTreatCrystalContractAsOp338(t *testing.T) {
	service, session, repositories, connection := newCrystalContractTestRuntime(t, map[string]dnfrepo.ItemStack{
		"0:354": {ItemID: 3033, Count: 1},
	})
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := service.handleCurrentCrystalContractUpdate(session, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	session.dungeon.runtime = &runtimeDungeonState{Character: dnfrepo.CharacterRecord{CharacterID: "19"}}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 354)
	binary.LittleEndian.PutUint32(request[2:6], 3033)
	binary.LittleEndian.PutUint16(request[6:8], 1)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || !bytes.Equal(packet.Body, []byte{0, 1}) {
		t.Fatalf("crystal op338 packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	inventory, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found || inventory.Slots[dnfrepo.AccountSharedInventorySlotKey(354)].Count != 1 {
		t.Fatalf("op338 mutated crystal inventory found=%t slots=%+v err=%v", found, inventory.Slots, err)
	}
}

func TestCurrentLimitedCubeRejectsDungeonUseWithoutMutation(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9000, Count: 1},
			"0:7": {ItemID: 9001, Count: 1},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	session.dungeon.runtime = &runtimeDungeonState{Character: dnfrepo.CharacterRecord{CharacterID: "19"}}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 7)
	binary.LittleEndian.PutUint32(request[2:6], 9001)
	binary.LittleEndian.PutUint16(request[6:8], 2)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || !bytes.Equal(packet.Body, []byte{0, 1}) {
		t.Fatalf("dungeon use packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if inventory.Slots["0:2"].Count != 1 || inventory.Slots["0:7"].ItemID != 9001 || inventory.Slots["0:9"].Count != 10 {
		t.Fatalf("dungeon use mutated inventory = %+v", inventory.Slots)
	}
}

func TestResolveCurrentLimitedCubePolicyAcceptsEquipmentResultTemplates(t *testing.T) {
	catalog := newLimitedCubeTestCatalog(t)
	policy, recognized, err := resolveCurrentLimitedCubePolicy(catalog, 9010)
	if err != nil || !recognized {
		t.Fatalf("resolve recognized=%t err=%v", recognized, err)
	}
	if policy.TicketItemID != 9010 ||
		!reflect.DeepEqual(policy.Conditions, []dnflimitedcube.Requirement{{ItemID: 9101, Count: 1}, {ItemID: 9102, Count: 1}}) ||
		!reflect.DeepEqual(policy.Materials, []dnflimitedcube.Requirement{{ItemID: 3037, Count: 10}}) ||
		len(policy.Results) != 2 || policy.Results[0].Weight != 3000 || policy.Results[1].Weight != 1000 {
		t.Fatalf("policy = %+v", policy)
	}
	first := policy.Results[0].Stack
	if first.ItemID != 9101 || first.Count != 1 || first.Extra["item_kind"] != "equipment" ||
		first.Extra["pvf_path"] != "equipment/character/common/title/title_a.equ" ||
		first.Extra["equipment_type"] != "[title name]" ||
		first.Extra["source"] != "limited_cube_result" {
		t.Fatalf("first equipment result stack = %+v", first)
	}
	// raw+0x06 is the grade seed the client recomputes 品级 from; a result that
	// carried no valid seed would be repainted with the grade of seed 1.
	if seed := sceneInventoryExtraUint32(first.Extra, "quality_seed"); !validCurrentEquipmentQualitySeed(seed) {
		t.Fatalf("first result quality seed = %d (%q)", seed, first.Extra["quality_seed"])
	}
	if second := policy.Results[1].Stack; second.Extra["quality_seed"] == first.Extra["quality_seed"] {
		t.Fatalf("candidates must own independent seeds, both = %q", second.Extra["quality_seed"])
	}
	// An equipment template is a single instance; a PVF triple asking for a
	// stacked equipment result is a broken document and must stay refused.
	badCatalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":                          "",
		"equipment/equipment.lst":                      "9101 `character/common/title/title_a.equ`\n",
		"stackable/stackable.lst":                      "9011 `cash/title_change_stack.stk`\n9101 `character/common/title/title_a.equ`\n",
		"stackable/cash/title_change_stack.stk":        "[stackable type] `[upgrade limit cube]`\n[action type] `[limited cube]`\n[action usable place] `[village]`\n[A condition item]\n9101 1\n[B condition item]\n3037 10\n[result item]\n9101 2 1000\n",
		"equipment/character/common/title/title_a.equ": "[name] `Title A`\n[equipment type] `[title name]`\n[stackable type] `[material]`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCurrentLimitedCubePolicy(badCatalog, 9011); err == nil {
		t.Fatal("a stacked equipment result must be refused")
	}
}

func TestCurrentLimitedCubeOp338ChangesTitleAndKeepsBeadEnchantAndSeal(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9010, Count: 1},
			"0:7": {
				ItemID: 9101,
				Count:  1,
				Bind:   true,
				Extra: map[string]string{
					"item_kind":             "equipment",
					"equipment_type":        "[title name]",
					"quality_seed":          "12345",
					"value_a":               "10008663",
					"enchant_card_id":       "10008663",
					"byte_12":               "0",
					"enchant_upgrade_count": "0",
					"seal_flag":             "1",
				},
			},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 7)
	binary.LittleEndian.PutUint32(request[2:6], 9101)
	binary.LittleEndian.PutUint16(request[6:8], 2)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}

	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	promptItemID := binary.LittleEndian.Uint32(ack.Body[1:5])
	// The pool is title_a(3000)/title_b(1000) and the PVF weights are drawn as
	// published, so both templates are legal outcomes.
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) ||
		len(ack.Body) != 12 || ack.Body[0] != 1 ||
		(promptItemID != 9101 && promptItemID != 9102) ||
		binary.LittleEndian.Uint16(ack.Body[5:7]) != 1 ||
		binary.LittleEndian.Uint32(ack.Body[7:11]) != 0 || ack.Body[11] != 1 {
		t.Fatalf("op338 acknowledgement = %+v body=%x", ack.Header, ack.Body)
	}
	cleared, remaining := splitGameServerUpperPacket(t, remaining)
	if cleared.Header.Classification != 0 || cleared.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(cleared.Body) != 3+currentItemListEntryWireSize || cleared.Body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("target replacement clear header=%+v body=%x", cleared.Header, cleared.Body)
	}
	if got := limitedCubeRefreshRows(t, cleared.Body)[7]; got.itemID != ^uint32(0) {
		t.Fatalf("target replacement clear row=%+v", got)
	}
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) != 3+3*currentItemListEntryWireSize || refresh.Body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("op14 refresh header=%+v body=%x trailing=%x", refresh.Header, refresh.Body, trailing)
	}
	row := limitedCubeRow(t, refresh.Body, 7)
	if got := binary.LittleEndian.Uint32(row[2:6]); got != promptItemID {
		t.Fatalf("refreshed title row item=%d want %d row=%x", got, promptItemID, row)
	}
	// list 0 is an equipment container, so raw+0x06 is the grade seed, not a count.
	if seed := binary.LittleEndian.Uint32(row[6:10]); !validCurrentEquipmentQualitySeed(seed) {
		t.Fatalf("refreshed title row quality seed=%d row=%x", seed, row)
	}
	if row[0x0D] != 1 {
		t.Fatalf("refreshed title row seal flag=%d, want the 封装 mark kept; row=%x", row[0x0D], row)
	}
	if got := binary.LittleEndian.Uint32(row[0x0E:0x12]); got != 10008663 {
		t.Fatalf("refreshed title row enchant card=%d, want the 宝珠 kept; row=%x", got, row)
	}

	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:2"]; found {
		t.Fatalf("ticket remains in inventory: %+v", inventory.Slots)
	}
	if _, found := inventory.Slots["0:9"]; found {
		t.Fatalf("material remains in inventory: %+v", inventory.Slots)
	}
	title := inventory.Slots["0:7"]
	if title.ItemID != int64(promptItemID) || title.Count != 1 || !title.Bind || title.RawEntry != nil {
		t.Fatalf("changed title = %+v, want item=%d", title, promptItemID)
	}
	wantPath := map[int64]string{
		9101: "equipment/character/common/title/title_a.equ",
		9102: "equipment/character/common/title/title_b.equ",
	}[title.ItemID]
	for key, want := range map[string]string{
		"source":                         "limited_cube_result",
		"item_kind":                      "equipment",
		"pvf_path":                       wantPath,
		"equipment_type":                 "[title name]",
		"enchant_card_id":                "10008663",
		"value_a":                        "10008663",
		"seal_flag":                      "1",
		"random_title_enchant_config_id": "",
	} {
		if got := title.Extra[key]; got != want {
			t.Fatalf("changed title extra[%q] = %q want %q (extras=%+v)", key, got, want, title.Extra)
		}
	}
	if !validCurrentEquipmentQualitySeed(sceneInventoryExtraUint32(title.Extra, "quality_seed")) {
		t.Fatalf("changed title quality seed = %q", title.Extra["quality_seed"])
	}
}

// TestCurrentLimitedCubeOp338KeepsRandomTitleEnchantOnTransformedTitle covers
// the other half of 「称号变更后， 附在称号上的宝珠属性仍然有效」: a title carrying
// the random title enchant (etc/randomtitleenchant.etc config/row, raw
// +0x61/+0x63) instead of an ordinary bead. The template swap clears RawEntry,
// so the result row is synthesised from Extra and only the entry-level overlay
// can render the enchant; before that fix the transformed title came back with
// +0x61..+0x65 empty.
func TestCurrentLimitedCubeOp338KeepsRandomTitleEnchantOnTransformedTitle(t *testing.T) {
	service, session, repositories, connection := newPremiumServiceTestRuntime(t)
	service.pvfItemCatalog = newLimitedCubeTestCatalog(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:2": {ItemID: 9010, Count: 1},
			"0:7": {
				ItemID: 9101,
				Count:  1,
				Extra: map[string]string{
					"item_kind":      "equipment",
					"equipment_type": "[title name]",
					"quality_seed":   "12345",
					// setRandomTitleEnchantFields zeroes the ordinary bead half
					// of the same block when it writes the config/row pair.
					"value_a":                        "0",
					"enchant_card_id":                "0",
					"byte_12":                        "0",
					"enchant_upgrade_count":          "0",
					"random_title_enchant_config_id": "10001",
					"random_title_enchant_row_id":    "72",
				},
			},
			"0:9": {ItemID: 3037, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, currentLimitedCubeUseBodySize)
	binary.LittleEndian.PutUint16(request[0:2], 7)
	binary.LittleEndian.PutUint32(request[2:6], 9101)
	binary.LittleEndian.PutUint16(request[6:8], 2)
	if err := service.handleCurrentLimitedCubeUseRequest(session, request); err != nil {
		t.Fatal(err)
	}
	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseLimitCube) || len(ack.Body) != 12 || ack.Body[0] != 1 {
		t.Fatalf("op338 acknowledgement = %+v body=%x", ack.Header, ack.Body)
	}
	_, remaining = splitGameServerUpperPacket(t, remaining)
	refresh, trailing := splitGameServerUpperPacket(t, remaining)
	if len(trailing) != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("op14 refresh header=%+v body=%x trailing=%x", refresh.Header, refresh.Body, trailing)
	}
	row := limitedCubeRow(t, refresh.Body, 7)
	if got := binary.LittleEndian.Uint16(row[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset]); got != 10001 {
		t.Fatalf("transformed title row config=%d want 10001; row=%x", got, row)
	}
	if got := binary.LittleEndian.Uint16(row[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd]); got != 72 {
		t.Fatalf("transformed title row id=%d want 72; row=%x", got, row)
	}

	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	title := inventory.Slots["0:7"]
	for key, want := range map[string]string{
		"random_title_enchant_config_id": "10001",
		"random_title_enchant_row_id":    "72",
		"enchant_card_id":                "0",
	} {
		if got := title.Extra[key]; got != want {
			t.Fatalf("changed title extra[%q] = %q want %q (extras=%+v)", key, got, want, title.Extra)
		}
	}
}

func limitedCubeRow(t *testing.T, body []byte, slot uint16) []byte {
	t.Helper()
	for offset := 3; offset+currentItemListEntryWireSize <= len(body); offset += currentItemListEntryWireSize {
		row := body[offset : offset+currentItemListEntryWireSize]
		if binary.LittleEndian.Uint16(row[0:2]) == slot {
			return row
		}
	}
	t.Fatalf("no refresh row for slot %d in body=%x", slot, body)
	return nil
}

type limitedCubeRefreshRow struct {
	itemID uint32
	count  uint32
}

func limitedCubeRefreshRows(t *testing.T, body []byte) map[uint16]limitedCubeRefreshRow {
	t.Helper()
	rows := make(map[uint16]limitedCubeRefreshRow)
	for offset := 3; offset+currentItemListEntryWireSize <= len(body); offset += currentItemListEntryWireSize {
		row := body[offset : offset+currentItemListEntryWireSize]
		rows[binary.LittleEndian.Uint16(row[0:2])] = limitedCubeRefreshRow{
			itemID: binary.LittleEndian.Uint32(row[2:6]),
			count:  binary.LittleEndian.Uint32(row[6:10]),
		}
	}
	return rows
}

func newLimitedCubeTestCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                          "",
		"equipment/equipment.lst":                      "9101 `character/common/title/title_a.equ`\n9102 `character/common/title/title_b.equ`\n",
		"stackable/stackable.lst":                      "9000 `cash/pet_bead_change.stk`\n9001 `bead/power.stk`\n9002 `bead/element.stk`\n9003 `material/plain.stk`\n3037 `material/clear_crystal.stk`\n9010 `cash/title_change.stk`\n",
		"stackable/cash/pet_bead_change.stk":           "[stackable type] `[upgrade limit cube]`\n[action type] `[limited cube]`\n[action usable place] `[village]`\n[A condition item]\n9001 1\n9002 1\n[B condition item]\n3037 10\n[result item]\n9001 1 1000\n9002 1 1000\n",
		"stackable/cash/title_change.stk":              "[stackable type] `[upgrade limit cube]`\n[action type] `[limited cube]`\n[action usable place] `[village]`\n[A condition item]\n9101 1\n9102 1\n[B condition item]\n3037 10\n[result item]\n9101 1 3000\n9102 1 1000\n",
		"stackable/bead/power.stk":                     "[stackable type] `[enchant waste]`\n[stack limit] 1\n",
		"stackable/bead/element.stk":                   "[stackable type] `[enchant waste]`\n[stack limit] 1\n",
		"stackable/material/plain.stk":                 "[stackable type] `[material]`\n",
		"stackable/material/clear_crystal.stk":         "[stackable type] `[material]`\n[stack limit] 99999\n",
		"equipment/character/common/title/title_a.equ": "[name] `Title A`\n[equipment type] `[title name]`\n[rarity] 2\n[attach type] `[sealing]`\n",
		"equipment/character/common/title/title_b.equ": "[name] `Title B`\n[equipment type] `[title name]`\n[rarity] 2\n[attach type] `[sealing]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
