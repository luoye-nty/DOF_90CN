package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"testing"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func warOperationSource() dungeonDropCatalogTestSource {
	text := "[guardian list]\n[guardian]\n1\n[ap]\n50000\n[gp]\n1000\n"
	for i := 1; i <= 5; i++ {
		text += fmt.Sprintf("[skill_%d]\n0 1 5\n", i)
	}
	text += "[/guardian]\n[/guardian list]\n[guardian exp]\n1 18 0 2 83 0 3 237 20\n[/guardian exp]\n[guardian skill]\n1 1500 0 2 15000 200 3 50000 1000 4 100000 5000\n[/guardian skill]\n[shop list]\n201 202 211 212\n[/shop list]\n[building upgrade]\n[building]\n0 0 1000 0\n[/building]\n[mecenary]\n0 0 1000 0\n[/mecenary]\n[/building upgrade]"
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst": "", "equipment/equipment.lst": "",
		"etc/agitwar.etc":         text,
		"npc/npc.lst":             "390 `workshop.npc` 389 `pub.npc`",
		"npc/workshop.npc":        "[guild agit shop npc]\n`defence`\n[upgrade]\n`guild agit` 1 421 2 422\n[/upgrade]",
		"npc/pub.npc":             "[guild agit shop npc]\n`mercenary`\n[upgrade]\n`guild agit` 1 411 2 412\n[/upgrade]",
		"itemshop/itemshop.lst":   "201 `workshop.shp` 202 `workshop_2.shp` 211 `pub.shp` 212 `pub_2.shp`",
		"stackable/stackable.lst": "10161523 `cannon.stk` 10161524 `cannon2.stk`",
		"stackable/cannon.stk":    "[name]\n`cannon`\n[stackable type]\n`[expert town potion]` 0\n[stack limit]\n2\n[require ap]\n7\n[minimum level]\n17",
		"stackable/cannon2.stk":   "[name]\n`cannon2`\n[stackable type]\n`[expert town potion]` 0\n[stack limit]\n2\n[require ap]\n9\n[minimum level]\n17",
	}
	for _, row := range []struct {
		path      string
		npc, item int
	}{{"workshop.shp", 421, 10161523}, {"workshop_2.shp", 422, 10161524}, {"pub.shp", 411, 10161523}, {"pub_2.shp", 412, 10161524}} {
		source["itemshop/"+row.path] = fmt.Sprintf("[NPC]\n%d\n[type]\n`[agitwar build shop]`\n[item list]\n%d\n[/item list]", row.npc, row.item)
	}
	return source
}

func TestCurrentGuildWarOperationsCapturedRequests(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			fail := &guildFailSettlement{base: repos.CharacterSettlement}
			repos.CharacterSettlement = fail
			master := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "master", Level: 90, Stats: map[string]int64{"gold": 10000}}
			if err := repos.Character.Save(ctx, master); err != nil {
				t.Fatal(err)
			}
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "16", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			r := dnfguild.NewRegistry()
			g, err := r.Create(master, "war", serviceTestNow())
			if err != nil {
				t.Fatal(err)
			}
			g.Guardians = []dnfguild.Guardian{{ID: 1, Level: 1}}
			g.ActiveGuardian = 1
			g.GuardianAP = 10000
			g.GuardianGP = 2000
			g.Mark = 13
			if err = dnfguild.Save(ctx, repos.Settings, r, serviceTestNow()); err != nil {
				t.Fatal(err)
			}
			items, err := newPVFDungeonDropCatalog(warOperationSource())
			if err != nil {
				t.Fatal(err)
			}
			ss := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: master.AccountID, initialTownRouteCharacterID: 16, initialTownRouteStage: currentInitialTownRoutePlayerStateSent}
			s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, pvfItemCatalog: items, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
			load := func() (*dnfguild.Guild, dnfrepo.InventoryRecord) {
				t.Helper()
				r, e := dnfguild.Load(ctx, repos.Settings)
				if e != nil {
					t.Fatal(e)
				}
				g, _ := r.GuildByID(g.ID)
				inv, ok, e := repos.Inventory.Load(ctx, "16")
				if e != nil || !ok {
					t.Fatal(e)
				}
				return g, inv
			}
			send := func(op uint16, body []byte, code byte) []byte {
				t.Helper()
				ss.conn.(*bufferConn).write.Reset()
				h := guildGameplayModule().UpperHandlers[op]
				if legacy {
					h = guildGameplayModule().LegacyHandlers[op]
				}
				if e := h(s, ss, gameplayRequest{Opcode: op, Classification: 1, Body: body}); e != nil {
					t.Fatal(e)
				}
				ack, rest := splitGameServerUpperPacket(t, ss.conn.(*bufferConn).write.Bytes())
				want := []byte{1}
				if code != 0 {
					want = []byte{0, code}
				}
				if ack.Header.MsgID != op || ack.Header.Classification != 1 || !bytes.Equal(ack.Body, want) {
					t.Fatalf("op%d ACK %x want %x", op, ack.Body, want)
				}
				if code != 0 && len(rest) > 0 {
					t.Fatal("failure sent state")
				}
				return rest
			}
			words := func(v ...uint32) []byte {
				b := make([]byte, 4*len(v))
				for i, n := range v {
					binary.LittleEndian.PutUint32(b[i*4:], n)
				}
				return b
			}
			feed, _ := hex.DecodeString("0000000001000000000000000b000000")
			skill, _ := hex.DecodeString("02000000010000000000000001000000")
			upgrade, _ := hex.DecodeString("438bdf8180f71a00b8f61a00c4860100000100000002000000")
			buy, _ := hex.DecodeString("730d9b0001000000c9000000a5010000")
			send(1350, feed, 0)
			send(1350, feed, 0)
			a, _ := load()
			if a.GuardianAP != 9989 || a.Guardians[0].APExp != 11 || a.Guardians[0].Level != 1 {
				t.Fatal("partial feed/retry")
			}
			send(1350, words(0, 1, 11, 7), 0)
			a, _ = load()
			if a.GuardianAP != 9982 || a.Guardians[0].APExp != 18 || a.Guardians[0].Level != 2 {
				t.Fatal("level transition lost cumulative experience")
			}
			send(1350, skill, 0)
			send(1350, skill, 0)
			a, _ = load()
			if a.Guardians[0].Skills[0] != 1 || a.GuardianAP != 9982 {
				t.Fatal("free initial skill unlock/retry")
			}
			send(1350, words(2, 1, 1, 2), 0)
			send(1350, words(2, 1, 1, 2), 0)
			a, _ = load()
			if a.GuardianAP != 8482 || a.Guardians[0].Skills[0] != 2 {
				t.Fatal("skill level cost/retry")
			}
			send(1350, words(3, 1, 0, 1), 3)     // second skill needs the second star
			send(1350, words(0, 1, 0, 1), 3)     // stale experience
			send(1350, words(0, 1, 18, 1000), 3) // past current threshold
			send(1350, words(2, 1, 2, 3), 22)    // actual AP/GP cost exceeds balance
			before, invBefore := load()
			fail.fail = true
			send(1354, buy, 3)
			fail.fail = false
			a, inv := load()
			if !reflect.DeepEqual(a, before) || !reflect.DeepEqual(inv, invBefore) {
				t.Fatal("rolled-back purchase changed guild or inventory")
			}
			rest := send(1354, buy, 0)
			itemPacket, rest := splitGameServerUpperPacket(t, rest)
			state, tail := splitGameServerUpperPacket(t, rest)
			if itemPacket.Header.MsgID != 14 || state.Header.MsgID != 1297 || len(tail) != 0 {
				t.Fatal("purchase refresh order")
			}
			a, inv = load()
			count := int64(0)
			for _, item := range inv.Slots {
				if item.ItemID == 10161523 {
					count += item.Count
				}
			}
			if a.GuardianAP != 8475 || count != 1 {
				t.Fatal("purchase AP debit/grant")
			}
			// Failed upgrade rolls back level and balance; retry succeeds once.
			before, _ = load()
			fail.fail = true
			send(840, upgrade, 3)
			fail.fail = false
			a, _ = load()
			if !reflect.DeepEqual(a, before) {
				t.Fatal("failed upgrade persisted")
			}
			rest = send(840, upgrade, 0)
			state, rest = splitGameServerUpperPacket(t, rest)
			notice, tail := splitGameServerUpperPacket(t, rest)
			if state.Header.MsgID != 1297 || binary.LittleEndian.Uint16(state.Body[2166:]) != 2 || binary.LittleEndian.Uint16(state.Body[2168:]) != 1 || notice.Header.MsgID != 778 || !bytes.Equal(notice.Body, []byte{0, 2, 0, 0, 0}) || len(tail) != 0 {
				t.Fatal("native merchant level projection")
			}
			send(840, upgrade, 0)
			a, _ = load()
			if a.GuardianAP != 7475 || a.WarShopLevel(0) != 2 || a.Mark != 13 {
				t.Fatal("upgrade retry or guild state")
			}
			send(1354, buy, 3) // stale level-1 shop after upgrade
			send(1354, words(10161524, 3, 202, 422), 0)
			a, inv = load()
			count = 0
			for _, item := range inv.Slots {
				if item.ItemID == 10161524 {
					count += item.Count
					if item.Count > 2 {
						t.Fatal("stack overflow")
					}
				}
			}
			if a.GuardianAP != 7448 || count != 3 {
				t.Fatal("upgraded shop item grant")
			}
			// A partial existing stack must roll back too if the remainder has
			// no room. The failure ACK releases the native purchase-busy flag.
			inv.Slots = fullCurrentNPCShopWasteInventory()
			inv.Slots[currentCeraShopInventorySlotKey(0, 65)] = dnfrepo.ItemStack{ItemID: 10161524, Count: 1}
			if e := repos.Inventory.Save(ctx, inv); e != nil {
				t.Fatal(e)
			}
			before, invBefore = load()
			send(1354, words(10161524, 3, 202, 422), 136)
			a, inv = load()
			if !reflect.DeepEqual(a, before) || !reflect.DeepEqual(inv, invBefore) {
				t.Fatal("full backpack purchase changed guild or partial stack")
			}
			for _, body := range [][]byte{nil, upgrade[:24], append(append([]byte{}, upgrade...), 0)} {
				send(840, body, 3)
			}
			send(1354, words(10161524, 0, 202, 422), 3)
			send(1354, words(10161524, 1, 202, 411), 3)
			ss.accountID = "dnf:wrong"
			send(1354, words(10161524, 1, 202, 422), 3)
			ss.accountID = master.AccountID
			stored, e := dnfguild.Load(ctx, repos.Settings)
			if e != nil {
				t.Fatal(e)
			}
			storedGuild, _ := stored.GuildByID(g.ID)
			member := storedGuild.Members["16"]
			member.Grade = 5
			storedGuild.Members["16"] = member
			if e = dnfguild.Save(ctx, repos.Settings, stored, serviceTestNow()); e != nil {
				t.Fatal(e)
			}
			send(1354, words(10161524, 1, 202, 422), 36)
			send(840, upgrade, 36)
			c, _, e := repos.Character.Load(ctx, "16")
			if e != nil || c.Stats["gold"] != 10000 {
				t.Fatal("guild shop changed gold")
			}
		})
	}
}

func TestCurrentGuildWarRealPVFCatalog(t *testing.T) {
	name := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if name == "" {
		t.Skip("real PVF not selected")
	}
	archive, err := platformpvf.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	growth, err := dnfguild.LoadGuardianGrowth(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(growth.Experience) != 49 || growth.Experience[1] != (dnfguild.GuardianCost{AP: 18}) || growth.Experience[20] != (dnfguild.GuardianCost{AP: 66247, GP: 604}) || growth.Skills[1].AP != 1500 || growth.Skills[0] != (dnfguild.GuardianCost{}) || len(growth.SkillLimits) != 2 {
		t.Fatal("actual guardian rules")
	}
	shops, err := dnfguild.LoadWarShops(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for kind, merchant := range shops {
		if len(merchant.Levels) != 10 || merchant.Costs[2].AP != 1000 || merchant.Costs[10] != (dnfguild.GuardianCost{AP: 95000, GP: 5000}) {
			t.Fatalf("actual merchant%d", kind)
		}
		for _, shop := range merchant.Levels {
			for item := range shop.Items {
				def, e := items.ResolveItem(item)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = dnfguild.LoadWarProductCost(archive, def.PVFPath); e != nil {
					t.Fatalf("product%d: %v", item, e)
				}
			}
		}
	}
	if !shops[0].Levels[1].Items[10161523] || shops[0].Levels[1].NPC != 421 || shops[0].Levels[1].ID != 201 {
		t.Fatal("captured cannon context")
	}
	cost, err := dnfguild.LoadWarProductCost(archive, "stackable/10161001/10161523.stk")
	if err != nil || cost != (dnfguild.GuardianCost{AP: 7}) {
		t.Fatal("actual cannon price", cost, err)
	}
}
