package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 本文件钉住方案 A：带随机称号附魔的穿戴行，必须在 actor 生成边界用 list-3 op14 重放。
// 方案 B（mode1 内部延时更新行）实测客户端不消费，所以那条通路不足以修这个问题。

func TestCurrentWornTitleEnchantActorSlot(t *testing.T) {
	_, ok := currentWornTitleEnchantActorSlot(
		randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, true),
	)
	if !ok {
		t.Fatal("enchanted title row was not selected for the op14 replay")
	}
	_, ok = currentWornTitleEnchantActorSlot(
		randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, false),
	)
	if ok {
		t.Fatal("unenchanted title row must not be selected for the op14 replay")
	}
}

// 发包含：一条 op14、list-3、槽位 13、item 400330111、行内带 0x61..0x64 的附魔。
func TestSendSelectedWornTitleEnchantRowRefreshEmitsTheList3Row(t *testing.T) {
	repos := newRandomTitleEnchantRepositories(t, map[string]dnfrepo.EquipmentEntry{
		"13": randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, true),
	})
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	if err := service.sendSelectedWornTitleEnchantRowRefresh(session, "test"); err != nil {
		t.Fatalf("replay worn title enchant row: %v", err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("replay emitted more than one packet: trailing=%x", trailing)
	}
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("replay header=%+v", packet.Header)
	}
	if len(packet.Body) != 3+currentItemListEntryWireSize {
		t.Fatalf("replay body length=%d, want %d", len(packet.Body), 3+currentItemListEntryWireSize)
	}
	if packet.Body[0] != currentSocketListEquipment {
		t.Fatalf("replay list type=%d, want %d", packet.Body[0], currentSocketListEquipment)
	}
	if got := binary.LittleEndian.Uint16(packet.Body[3:5]); got != 13 {
		t.Fatalf("replay slot=%d, want 13", got)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[5:9]); got != randomTitleEnchantTestItemID {
		t.Fatalf("replay item id=%d, want %d", got, randomTitleEnchantTestItemID)
	}
	var row currentItemListEntry
	copy(row.data[:], packet.Body[3:3+currentItemListEntryWireSize])
	if !currentItemListEntryCarriesRandomTitleEnchant(row) {
		t.Fatalf("replay row lost the enchant: %x", row.data[:])
	}
	if got := binary.LittleEndian.Uint16(
		row.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset],
	); got != randomTitleEnchantTestConfigID {
		t.Fatalf("replay row config=%d, want %d", got, randomTitleEnchantTestConfigID)
	}
	if got := binary.LittleEndian.Uint16(
		row.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd],
	); got != randomTitleEnchantTestRowID {
		t.Fatalf("replay row row id=%d, want %d", got, randomTitleEnchantTestRowID)
	}
}

// 反向钉子：没有附魔的称号一个包都不发（不能给普通玩家多发包）。
func TestSendSelectedWornTitleEnchantRowRefreshSkipsUnenchantedTitles(t *testing.T) {
	repos := newRandomTitleEnchantRepositories(t, map[string]dnfrepo.EquipmentEntry{
		"13": randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, false),
	})
	service, session, connection := newGameplayDomainBridgeFixture(repos)
	if err := service.sendSelectedWornTitleEnchantRowRefresh(session, "test"); err != nil {
		t.Fatalf("replay worn title enchant row: %v", err)
	}
	if got := connection.write.Len(); got != 0 {
		t.Fatalf("unenchanted title produced %d bytes, want 0", got)
	}
}

// 端到端钉子：进图流程里，mode1 之后必须出现这条 list-3 op14 称号行（顺序与宝石一致）。
func TestCurrentPostStartMapReplaysWornTitleEnchantAfterFullMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		t.Fatal("dungeon entry fixture equipment repository unavailable")
	}
	title := randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, true)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries:     map[string]dnfrepo.EquipmentEntry{"13": title},
	}); err != nil {
		t.Fatalf("save dungeon worn title: %v", err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	scene := runtime.Session.Snapshot().Scene
	if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, scene, "test_title_enchant_after_mode1"); err != nil {
		t.Fatal(err)
	}

	sawMode1 := false
	sawTitleEnchant := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			sawMode1 = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == currentSocketListEquipment {
			var row currentItemListEntry
			copy(row.data[:], packet.Body[3:])
			if binary.LittleEndian.Uint32(row.data[0x02:0x06]) == randomTitleEnchantTestItemID {
				if !sawMode1 {
					t.Fatal("worn title enchant op14 preceded the dungeon full mode1")
				}
				if !currentItemListEntryCarriesRandomTitleEnchant(row) {
					t.Fatalf("dungeon worn title op14 omitted the enchant: %x", packet.Body)
				}
				sawTitleEnchant = true
			}
		}
		stream = rest
	}
	if !sawMode1 || !sawTitleEnchant {
		t.Fatalf("dungeon mode1/title-enchant replay mode1=%t title=%t", sawMode1, sawTitleEnchant)
	}
}
