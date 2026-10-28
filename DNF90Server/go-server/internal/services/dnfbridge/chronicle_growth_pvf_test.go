package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestResolveCurrentChronicleGrowthMetadata(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "10094062 `10094001/normal.stk`\n10094063 `10094001/advanced.stk`\n",
		"equipment/equipment.lst": "700 `weapon/test.equ`\n",
		currentChronicleGrowthCostPVFPath: "[consume item by rarity]\n5 3311 3 1.00\n[/consume item by rarity]\n" +
			"[weight by weapon]\n`weapon` 1.20 `coat` 1.00\n[/weight by weapon]\n" +
			"[weight by level]\n1 70 1.00 2.00 70 85 2.00 3.50 85 200 3.50 50.00\n[/weight by level]\n" +
			"[weight by upgrade]\n0 20 1.00 40.00\n[/weight by upgrade]\n" +
			"[weight by amplify]\n0 20 1.00 144.00\n[/weight by amplify]\n" +
			"[weight by seperate]\n0 7 1.00 3.00\n[/weight by seperate]\n",
		"stackable/10094001/normal.stk": "[name] `Normal`\n[stackable type] `[etc]` 1\n" +
			"[emancipate ticket] 5\n" +
			"[need material] 10093971 56 [/need material]\n[equipment level emancipate]\n" +
			"[probability] 75 100000 85 100000 [/probability]\n[equipment upgrade level] 3\n[equipment condition]\n" +
			"[rarity] 5\n[minimum level] 70\n[maximum level] 90\n[/equipment condition]\n[/equipment level emancipate]\n",
		"stackable/10094001/advanced.stk": "[name] `Advanced`\n[stackable type] `[etc]` 1\n" +
			"[equipment level emancipate]\n[probability] 75 100000 85 100000 [/probability]\n[equipment upgrade level] 5\n" +
			"[equipment condition]\n[rarity] 5\n[minimum level] 70\n[maximum level] 90\n[/equipment condition]\n[/equipment level emancipate]\n",
		"equipment/weapon/test.equ": "[name] `Test`\n[equipment type] `[weapon]` 17\n[rarity] 5\n[minimum level] 70\n[need material] 3311 169\n[/need material]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentChronicleGrowthMetadata(catalog, source, 10094062, 700)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.MaterialItemID != 3311 || resolution.UpgradeLevel != 3 || resolution.TargetMinimumLevel != 70 ||
		resolution.ConditionMinimum != 70 || resolution.ConditionMaximum != 90 || resolution.TargetRarity != 5 ||
		resolution.EquipmentType != "weapon" || resolution.CostPolicy.BaseCount != 3 || len(resolution.Probability) != 2 {
		t.Fatalf("resolution = %+v", resolution)
	}
	if resolution.CostPolicy.MaterialItemID == 10093971 || resolution.CostPolicy.BaseCount == 169 {
		t.Fatalf("[need material] was confused with op271 cost: %+v", resolution.CostPolicy)
	}
	advanced, err := resolveCurrentChronicleGrowthMetadata(catalog, source, 10094063, 700)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.UpgradeLevel != 5 || advanced.MaterialItemID != 3311 || advanced.ConditionMaximum != 90 {
		t.Fatalf("advanced resolution = %+v", advanced)
	}
}

func TestResolveCurrentEquipmentEmancipationMetadata(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":             "",
		"stackable/stackable.lst":         "10149520 `emancipate/ticket.stk`\n10149509 `material/relic.stk`\n",
		"equipment/equipment.lst":         "104000224 `weapon/source.equ`\n104000504 `weapon/output.equ`\n",
		"stackable/emancipate/ticket.stk": "[name] `Relic Conversion`\n[stackable type] `[etc]` 1\n[emancipate ticket] 8\n",
		"stackable/material/relic.stk":    "[name] `Relic Fragment`\n[stackable type] `[material]` 1\n",
		"equipment/weapon/source.equ": "[name] `Source`\n[equipment type] `[weapon]` 17\n[rarity] 3\n[minimum level] 15\n" +
			"[emancipate]\n[input] 10149509 10 10099773 12 10100116 24 10100115 36 [/input]\n" +
			"[output] 104000504 1 [/output]\n[emancipate type] 8\n[/emancipate]\n",
		"equipment/weapon/output.equ": "[name] `Relic Output`\n[equipment type] `[weapon]` 17\n[rarity] 4\n[grade] 50\n[minimum level] 15\n[durability] 35\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentChronicleGrowthMetadata(catalog, source, 10149520, 104000224)
	if err != nil {
		t.Fatal(err)
	}
	conversion := resolution.Conversion
	if conversion == nil || conversion.TicketType != 8 || conversion.OutputItemID != 104000504 || !conversion.BindOutput ||
		conversion.OutputRarity != 4 || conversion.OutputMinimumLevel != 15 || conversion.OutputDurability != 35 || len(conversion.Inputs) != 4 {
		t.Fatalf("conversion = %+v", conversion)
	}
	if conversion.Inputs[0].ItemID != 10149509 || conversion.Inputs[0].Count != 10 ||
		conversion.UpgradeMaximum != -1 || conversion.AmplifyMaximum != -1 || conversion.ForgingMaximum != -1 {
		t.Fatalf("conversion inputs/caps = %+v", conversion)
	}
}

func TestParseCurrentChronicleGrowthCostPolicies(t *testing.T) {
	document, err := dnfpvf.Parse(currentChronicleGrowthCostPVFPath, "[consume item by rarity]\n5 3311 3 1.00\n[/consume item by rarity]\n[weight by weapon]\nweapon 1.20 coat 1.00 wrisat 1.00\n[/weight by weapon]\n[weight by level]\n1 70 1.00 2.00\n[/weight by level]\n[weight by upgrade]\n0 20 1.00 40.00\n[/weight by upgrade]\n[weight by amplify]\n0 20 1.00 144.00\n[/weight by amplify]\n[weight by seperate]\n0 7 1.00 3.00\n[/weight by seperate]\n")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := parseCurrentChronicleGrowthCostPolicies(document)
	if err != nil {
		t.Fatal(err)
	}
	policy := policies[5]
	if policy.MaterialItemID != 3311 || policy.BaseCount != 3 || policy.Multiplier != 1 || policy.PartWeights["weapon"] != 1.2 || len(policy.ForgingWeights) != 1 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestResolveCurrentChronicleGrowthMetadataRealPVF(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current chronicle growth data")
	}
	source, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ticket, target int64
		upgrade        int
	}{{10094062, 116010135, 3}, {10094063, 100260612, 5}} {
		resolution, err := resolveCurrentChronicleGrowthMetadata(catalog, source, test.ticket, test.target)
		if err != nil {
			t.Fatalf("ticket=%d target=%d: %v", test.ticket, test.target, err)
		}
		if resolution.MaterialItemID != 3311 || resolution.UpgradeLevel != test.upgrade || resolution.TargetMinimumLevel != 70 ||
			resolution.ConditionMaximum != 90 || resolution.TargetRarity != 5 || resolution.EquipmentType == "" ||
			resolution.CostPolicy.BaseCount != 3 || len(resolution.Probability) != 2 {
			t.Fatalf("ticket=%d real PVF resolution = %+v", test.ticket, resolution)
		}
	}
	conversion, err := resolveCurrentChronicleGrowthMetadata(catalog, source, 10149520, 104000224)
	if err != nil {
		t.Fatalf("real PVF relic conversion: %v", err)
	}
	if conversion.Conversion == nil || conversion.Conversion.OutputItemID != 104000504 ||
		conversion.Conversion.TicketType != 8 || len(conversion.Conversion.Inputs) != 4 ||
		conversion.Conversion.Inputs[0].ItemID != 10149509 || conversion.Conversion.Inputs[0].Count != 10 {
		t.Fatalf("real PVF relic conversion = %+v", conversion.Conversion)
	}
	liveConversion, err := resolveCurrentChronicleGrowthMetadata(catalog, source, 10149529, 101030330)
	if err != nil {
		t.Fatalf("live Lv85 relic conversion: %v", err)
	}
	if liveConversion.Conversion == nil || len(liveConversion.Conversion.Inputs) != 4 ||
		liveConversion.Conversion.OutputItemID != 101030654 || liveConversion.Conversion.TicketType != 17 ||
		liveConversion.Conversion.Inputs[0].ItemID != 10149518 || liveConversion.Conversion.Inputs[0].Count != 10 ||
		liveConversion.Conversion.Inputs[1].Count != 12 || liveConversion.Conversion.Inputs[2].Count != 24 ||
		liveConversion.Conversion.Inputs[3].Count != 36 {
		t.Fatalf("live Lv85 relic conversion = %+v", liveConversion.Conversion)
	}
}

func TestSendAlignedChronicleTargetRefreshBeforeAck(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	raw[0x58] = 3
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "25", Slots: map[string]dnfrepo.ItemStack{
		"0:25": {ItemID: 116010135, Count: 1, RawEntry: raw, Extra: map[string]string{"byte_58": "3"}},
	}}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "chronicle-order", channel: channelcatalog.Channel{ID: 1}, selectedCharacterID: 25}
	ack := []byte{1, 1, 2, 0, 84, 0, 1, 0, 0, 0, 0, 160, 0, 7, 0, 0, 0}
	result := alignedcmd.Result{
		Operation:                    "upgrade_chronicle",
		PreResponseItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{{ListType: 0, SlotIndex: 25}},
		UpperResponses:               []alignedcmd.UpperResponse{{MsgID: uint16(dnfenum.CmdPacketUpgradeChronicle), Body: ack, Classification: dnfproto.DefaultChannelClassification, AllowCodec: true}},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	refresh, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || refresh.Header.Classification != 0 || refresh.Body[0] != 0 {
		t.Fatalf("first packet header=%+v body=%x", refresh.Header, refresh.Body)
	}
	response, trailing := splitLongHengGameServerUpperPacket(t, rest)
	if response.Header.MsgID != uint16(dnfenum.CmdPacketUpgradeChronicle) || !bytes.Equal(response.Body, ack) {
		t.Fatalf("second packet header=%+v body=%x", response.Header, response.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", trailing)
	}
}

func TestSendAlignedEquipmentEmancipationAckThenInvalidateThenRefresh(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	targetRaw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(targetRaw[0:2], 25)
	binary.LittleEndian.PutUint32(targetRaw[2:6], 104000504)
	binary.LittleEndian.PutUint32(targetRaw[6:10], 1)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "26", Slots: map[string]dnfrepo.ItemStack{
		"0:25": {ItemID: 104000504, Count: 1, Bind: true, RawEntry: targetRaw, Extra: map[string]string{"item_kind": "equipment"}},
		"0:84": {ItemID: 10149520, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "emancipation-order", channel: channelcatalog.Channel{ID: 1}, selectedCharacterID: 26}
	ack := []byte{1, 1, 1, 0, 84, 0, 1, 0, 0, 0}
	result := alignedcmd.Result{
		Operation: "upgrade_chronicle",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID: uint16(dnfenum.CmdPacketUpgradeChronicle), Body: ack,
			Classification: dnfproto.DefaultChannelClassification, AllowCodec: true,
		}},
		PostResponseItemSlotClears: []alignedcmd.ItemSlotRefresh{{ListType: 0, SlotIndex: 25}},
		ItemSlotRefreshes: []alignedcmd.ItemSlotRefresh{
			{ListType: 0, SlotIndex: 25},
			{ListType: 0, SlotIndex: 84},
		},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	response, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if response.Header.MsgID != uint16(dnfenum.CmdPacketUpgradeChronicle) || response.Header.Classification != 1 || !bytes.Equal(response.Body, ack) {
		t.Fatalf("first packet header=%+v body=%x", response.Header, response.Body)
	}
	clear, rest := splitLongHengGameServerUpperPacket(t, rest)
	if clear.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || clear.Header.Classification != 0 ||
		len(clear.Body) != 3+currentItemListEntryWireSize || binary.LittleEndian.Uint16(clear.Body[3:5]) != 25 ||
		binary.LittleEndian.Uint32(clear.Body[5:9]) != ^uint32(0) {
		t.Fatalf("second packet header=%+v body=%x", clear.Header, clear.Body)
	}
	refresh, trailing := splitLongHengGameServerUpperPacket(t, rest)
	if refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || refresh.Header.Classification != 0 ||
		len(refresh.Body) != 3+2*currentItemListEntryWireSize || binary.LittleEndian.Uint16(refresh.Body[3:5]) != 25 ||
		binary.LittleEndian.Uint32(refresh.Body[5:9]) != 104000504 {
		t.Fatalf("third packet header=%+v body=%x", refresh.Header, refresh.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", trailing)
	}
}
