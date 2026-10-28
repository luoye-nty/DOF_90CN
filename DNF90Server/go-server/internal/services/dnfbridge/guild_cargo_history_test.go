package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentGuildCargoAndListsNativeReplies(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			character := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "测试会长", Level: 90}
			if err := repos.Character.Save(ctx, character); err != nil {
				t.Fatal(err)
			}
			registry := dnfguild.NewRegistry()
			guild, err := registry.Create(character, "测试公会", serviceTestNow().AddDate(0, 0, -1))
			if err != nil {
				t.Fatal(err)
			}
			guild.Mileage = 10000000
			guild.CargoLimit = 48
			if _, err := registry.Attend("16", serviceTestNow()); err != nil {
				t.Fatal(err)
			}
			if err := dnfguild.Save(ctx, repos.Settings, registry, serviceTestNow()); err != nil {
				t.Fatal(err)
			}
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "16", Slots: map[string]dnfrepo.ItemStack{
				"0:131": {ItemID: 3151, Count: 35}, "0:0": {ItemID: 0, Count: 443},
			}}); err != nil {
				t.Fatal(err)
			}
			catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
				"monster/monster.lst": "", "equipment/equipment.lst": "",
				"stackable/stackable.lst":     "3151 `test_material.stk`\n",
				"stackable/test_material.stk": "[stackable type]\n`[material]`\n[attach type]\n`[free]`\n[stack limit]\n999\n",
			})
			if err != nil {
				t.Fatal(err)
			}
			// Upstream's harness sets townSceneReadyCharacterID here. This tree
			// tracks actor/scene readiness with the initial-town route cursor
			// instead (see currentSceneTransitionReadyForState in
			// initial_town_scene_ready.go), which is what the cargo handlers gate
			// on, so the cursor is what has to be armed. Both fields are set so the
			// harness stays valid under either model.
			session := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: character.AccountID, townSceneReadyCharacterID: 16}
			session.initialTownRouteCharacterID = 16
			session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
			service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain, regionID: 1}, pvfItemCatalog: catalog,
				repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
			handlers := guildGameplayModule().UpperHandlers
			if legacy {
				handlers = guildGameplayModule().LegacyHandlers
			}
			send := func(opcode uint16, body []byte) []byte {
				t.Helper()
				session.conn.(*bufferConn).write.Reset()
				if err := handlers[opcode](service, session, gameplayRequest{Opcode: opcode, Classification: 1, Body: body}); err != nil {
					t.Fatal(err)
				}
				return session.conn.(*bufferConn).write.Bytes()
			}
			for _, amount := range []uint32{10, 25} {
				body := make([]byte, 13)
				binary.LittleEndian.PutUint16(body[1:], 131)
				binary.LittleEndian.PutUint32(body[3:], 3151)
				binary.LittleEndian.PutUint32(body[7:], amount)
				binary.LittleEndian.PutUint16(body[11:], 28)
				ack, rest := splitGameServerUpperPacket(t, send(247, body))
				if ack.Header.MsgID != 247 || ack.Body[0] != 1 {
					t.Fatalf("存入回包=%x", ack.Body)
				}
				update, rest := splitGameServerUpperPacket(t, rest)
				wantID, wantCount := uint32(3151), uint32(25)
				if amount == 25 {
					wantID, wantCount = math.MaxUint32, 0
				}
				if update.Header.MsgID != 14 || update.Header.Classification != 0 || len(update.Body) != 3+119 ||
					binary.LittleEndian.Uint16(update.Body[3:]) != 131 || binary.LittleEndian.Uint32(update.Body[5:]) != wantID ||
					binary.LittleEndian.Uint32(update.Body[9:]) != wantCount {
					t.Fatalf("背包清空/余量错误: %x", update.Body)
				}
				cargo, rest := splitGameServerUpperPacket(t, rest)
				if cargo.Header.MsgID != 261 || len(rest) != 0 || len(cargo.Body) != 7+119 ||
					binary.LittleEndian.Uint16(cargo.Body[7:]) != 28 || binary.LittleEndian.Uint32(cargo.Body[9:]) != 3151 {
					t.Fatalf("仓库物品错误: %x", cargo.Body)
				}
			}
			inventory, _, err := repos.Inventory.Load(ctx, "16")
			if err != nil || inventory.Slots["0:131"].ItemID != 0 || inventory.Slots["0:0"].Count != 443 {
				t.Fatalf("持久化背包=%+v err=%v", inventory, err)
			}
			saved, err := dnfguild.Load(ctx, repos.Settings)
			if err != nil || saved.Guilds[guild.ID].Cargo[28].Count != 35 {
				t.Fatalf("仓库数量未保留: %v", err)
			}
			// All three opcodes are registered now that the 红包 handlers exist, so
			// the loop is back to upstream's full list and every assertion below
			// runs.
			for _, opcode := range []uint16{747, 1300, 1301} {
				first := append([]byte(nil), send(opcode, nil)...)
				packet, rest := splitGameServerUpperPacket(t, first)
				if packet.Header.Classification != 1 || packet.Body[0] != 1 || len(rest) != 0 {
					t.Fatalf("列表回包 %d: %x", opcode, packet.Body)
				}
				switch opcode {
				case 747:
					// The row OFFSETS are identical to upstream's layout; three
					// VALUES differ, because this tree's 公会积分 rules are
					// deliberately richer (report §11.1 #9):
					//   +0x22 reason: upstream hardcodes 2 (20998 公会成员每日签到).
					//         We send 0x0B for the guild master, which this client
					//         renders as 20989 公会会长每日签到 -- and this character
					//         IS the master. Our guild_runtime_test.go pins the same
					//         byte, with the 21000-byte derivation.
					//   +0x24 character id: upstream leaves it zero; we write the
					//         real id (16). Documented in currentGuildMileageHistoryBody.
					//   +0x28 points: upstream's Attend grants 1; ours grants
					//         MileageSignInPoints (3), per help text 76514
					//         完成公会每日签到，可以获得3点公会贡献。
					if len(packet.Body) != 53 || binary.LittleEndian.Uint32(packet.Body[1:]) != 1 ||
						binary.LittleEndian.Uint32(packet.Body[5:]) != uint32(serviceTestNow().Unix()) ||
						!bytes.Equal(packet.Body[9:9+len(rosterNameBytes(character.Name))], rosterNameBytes(character.Name)) ||
						packet.Body[39] != 0x0B ||
						binary.LittleEndian.Uint32(packet.Body[41:]) != 16 ||
						binary.LittleEndian.Uint32(packet.Body[45:]) != dnfguild.MileageSignInPoints {
						t.Fatalf("积分历史格式=%x", packet.Body)
					}
				case 1300:
					if !bytes.Equal(packet.Body, append([]byte{1, 1, 0}, make([]byte, 12)...)) {
						t.Fatalf("红包列表头=%x", packet.Body)
					}
				case 1301:
					if len(packet.Body) != 45 || binary.LittleEndian.Uint32(packet.Body[1:]) != 1 ||
						binary.LittleEndian.Uint32(packet.Body[5:]) != 16 || packet.Body[9] != 90 ||
						!bytes.Equal(packet.Body[10:10+len(rosterNameBytes(character.Name))], rosterNameBytes(character.Name)) ||
						binary.LittleEndian.Uint32(packet.Body[40:]) != 0 {
						t.Fatalf("红包排行混入 GM 点数: %x", packet.Body)
					}
				}
				repeat, _ := splitGameServerUpperPacket(t, send(opcode, nil))
				if !bytes.Equal(packet.Body, repeat.Body) {
					t.Fatalf("重复打开改变了 %d 数据", opcode)
				}
				bad, _ := splitGameServerUpperPacket(t, send(opcode, []byte{1}))
				if bad.Body[0] != 0 {
					t.Fatalf("接受畸形请求 %d", opcode)
				}
				if opcode != 747 && (len(bad.Body) != 6 || binary.LittleEndian.Uint32(bad.Body[2:]) != uint32(currentGuildGenericError)) {
					t.Fatalf("红包失败回包缺少 u32: %x", bad.Body)
				}
				session.selectedCharacterID = 17
				bad, _ = splitGameServerUpperPacket(t, send(opcode, nil))
				if bad.Body[0] != 0 {
					t.Fatalf("向非会员泄漏公会列表 %d", opcode)
				}
				session.selectedCharacterID = 16
			}
		})
	}
	// Upstream calls buildCurrentGuildMileageHistoryBody(guild *dnfguild.Guild)
	// here. This tree split that builder in two: the row layout is assembled from
	// pre-encoded rows by currentGuildMileageHistoryBody (see guild_protocol.go,
	// which also documents why our row layout differs from upstream's). The
	// assertion is unchanged: an empty history encodes as a bare u32 zero count.
	if b := currentGuildMileageHistoryBody(nil); !bytes.Equal(b, make([]byte, 4)) {
		t.Fatalf("空积分记录=%x", b)
	}
}
