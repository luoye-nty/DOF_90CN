package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	memory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFLevelAimingWeaponSelectionKeepsUpgrade(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("real PVF path unset")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Job: "0", Level: 20}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{"0:95": {ItemID: 490003551, Count: 1}}}); err != nil {
		t.Fatal(err)
	}
	service := currentBoosterTestService(items, repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "level-weapon", selectedCharacterID: 19}
	body := make([]byte, 10)
	binary.LittleEndian.PutUint16(body, 95)
	binary.LittleEndian.PutUint32(body[4:], 401000041)
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketUseBoosterItem), body); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(ack.Body) == 0 || ack.Body[0] != 1 {
		t.Fatalf("open rejected: %x", ack.Body)
	}
	inv, ok, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok := inv.Slots["0:95"]; ok {
		t.Fatal("source box was not consumed")
	}
	found := false
	for key, stack := range inv.Slots {
		if stack.ItemID != 401000041 {
			continue
		}
		found = true
		if stack.Extra["upgrade_level"] != "11" || len(stack.RawEntry) != currentItemListEntryWireSize || stack.RawEntry[0x0A] != 11 {
			t.Fatalf("native +11 reward lost reinforcement: slot=%s extra=%v raw=%x", key, stack.Extra, stack.RawEntry)
		}
	}
	if !found {
		t.Fatal("selected weapon missing")
	}
	wire := false
	for len(rest) > 0 {
		packet, tail := splitCurrentGameServerUpperPacketAuto(t, rest)
		rest = tail
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) || len(packet.Body) < 5 {
			continue
		}
		for i := 5; i+currentItemListEntryWireSize <= len(packet.Body); i += currentItemListEntryWireSize {
			row := packet.Body[i : i+currentItemListEntryWireSize]
			if binary.LittleEndian.Uint32(row[2:6]) == 401000041 {
				wire = true
				if row[0x0A] != 11 {
					t.Fatalf("wire reinforcement=%d", row[0x0A])
				}
			}
		}
	}
	if !wire {
		t.Fatal("weapon inventory update missing")
	}
}

func TestBoosterSelectionUpgradeCategoryIsolation(t *testing.T) {
	doc, err := dnfpvf.Parse("selection.stk", "[booster select category]\n0 0\n[booster equipment upgrade]\n11\n[equipment]\n100 1\n[/equipment]\n[stackable]\n200 1\n[/stackable]\n[booster select category]\n1 0\n[equipment]\n100 1\n[/equipment]\n")
	if err != nil {
		t.Fatal(err)
	}
	_, categories, err := parseCurrentBoosterSelectionCatalog(doc)
	if err != nil {
		t.Fatal(err)
	}
	if categories[0][0].UpgradeLevel != 11 || categories[0][1].UpgradeLevel != 0 || categories[1][0].UpgradeLevel != 0 {
		t.Fatalf("upgrade leaked between categories or into stackable: %+v", categories)
	}
	for _, value := range []string{"-1", "256", "11 12", ""} {
		doc, err := dnfpvf.Parse("selection.stk", "[booster select category]\n0 0\n[booster equipment upgrade]\n"+value+"\n[equipment]\n100 1\n[/equipment]\n")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := parseCurrentBoosterSelectionCatalog(doc); err == nil {
			t.Fatalf("invalid upgrade accepted: %q", value)
		}
	}
}

// Exercise the native op160 writer shapes against the candidate PVF and an
// isolated repository, including real persistence and the inventory notification.
func openLevelAimingTestBox(t *testing.T, items *pvfDungeonDropCatalog, job int, source uint32, body []byte, want map[uint32]uint32, upgrades map[uint32]byte) {
	t.Helper()
	repos := memory.NewMemoryGroup()
	ctx := context.Background()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Job: strconv.Itoa(job), Level: 90}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{"0:95": {ItemID: int64(source), Count: 1}}}); err != nil {
		t.Fatal(err)
	}
	service := currentBoosterTestService(items, repos)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "native-alljob", selectedCharacterID: 19}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketUseBoosterItem), body); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(ack.Body) == 0 || ack.Body[0] != 1 {
		t.Fatalf("source=%d ack=%x", source, ack.Body)
	}
	inv, ok, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatal(err)
	}
	got := map[uint32]uint32{}
	for key, stack := range inv.Slots {
		got[uint32(stack.ItemID)] += uint32(stack.Count)
		if level := upgrades[uint32(stack.ItemID)]; level > 0 {
			if stack.Extra["upgrade_level"] != strconv.Itoa(int(level)) || len(stack.RawEntry) != currentItemListEntryWireSize || stack.RawEntry[0x0A] != level {
				t.Fatalf("source=%d item=%d lost upgrade: %+v", source, stack.ItemID, stack)
			}
			if currentItemListStackExpire(stack) == 0 {
				t.Fatalf("temporary weapon lost expiration: %s", key)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source=%d rewards=%v want=%v", source, got, want)
	}
	seen := map[uint32]byte{}
	for len(rest) > 0 {
		packet, tail := splitCurrentGameServerUpperPacketAuto(t, rest)
		rest = tail
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) || len(packet.Body) < 5 || packet.Body[0] != 0 {
			continue
		}
		for i := 5; i+currentItemListEntryWireSize <= len(packet.Body); i += currentItemListEntryWireSize {
			row := packet.Body[i : i+currentItemListEntryWireSize]
			seen[binary.LittleEndian.Uint32(row[2:6])] = row[0x0A]
		}
	}
	for id, level := range upgrades {
		if actual, ok := seen[id]; !ok || actual != level {
			t.Fatalf("item=%d wire upgrade=%d found=%t", id, actual, ok)
		}
	}
	// A replay must not recreate the consumed reward source or duplicate contents.
	conn.write.Reset()
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketUseBoosterItem), body); err != nil {
		t.Fatal(err)
	}
	after, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv, after) {
		t.Fatal("consumed box replay changed inventory")
	}
}

func TestRealPVFLevelAimingAllJobNativeRewards(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("real PVF path unset")
	}
	a, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(a)
	if err != nil {
		t.Fatal(err)
	}
	weapons, err := resolveCurrentBoosterDefinition(items, 490003551, currentBoosterRequestSelection)
	if err != nil {
		t.Fatal(err)
	}
	if len(weapons.SelectionCategory) != 10 {
		t.Fatalf("categories=%d want=10", len(weapons.SelectionCategory))
	}
	covered := map[int]bool{}
	for category, candidates := range weapons.SelectionCategory {
		for _, c := range candidates {
			d, err := items.ResolveItem(c.ItemID)
			if err != nil {
				t.Fatal(err)
			}
			if c.UpgradeLevel != 11 || d.UsablePeriodDays != 30 {
				t.Fatalf("weapon=%d upgrade=%d days=%d", c.ItemID, c.UpgradeLevel, d.UsablePeriodDays)
			}
			doc, err := parseDungeonCardPVFDocument(a, d.PVFPath)
			if err != nil {
				t.Fatal(err)
			}
			jobs, ok := doc.Section("usable job")
			if !ok {
				t.Fatal("missing usable job")
			}
			jobText := ""
			for _, v := range jobs {
				jobText += v.Value
			}
			for job := 0; job <= jobmap.MaxID; job++ {
				if !jobmap.MatchesQuestTag(jobText, job) {
					continue
				}
				covered[job] = true
				t.Run(fmt.Sprintf("weapon/job%d/category%d/%d", job, category, c.ItemID), func(t *testing.T) {
					body := make([]byte, 10)
					binary.LittleEndian.PutUint16(body, 95)
					binary.LittleEndian.PutUint16(body[2:], category)
					binary.LittleEndian.PutUint32(body[4:], c.ItemID)
					openLevelAimingTestBox(t, items, job, 490003551, body, map[uint32]uint32{c.ItemID: 1}, map[uint32]byte{c.ItemID: 11})
				})
			}
		}
	}
	for job := 0; job <= jobmap.MaxID; job++ {
		if !covered[job] {
			t.Errorf("job %d has no native weapon", job)
		}
	}
	armor, err := resolveCurrentBoosterDefinition(items, 490003554, currentBoosterRequestSelection)
	if err != nil {
		t.Fatal(err)
	}
	if armor.SelectionRequired != 0 || len(armor.SelectionCategory) != 5 {
		t.Fatalf("armor=%+v", armor)
	}
	for category, candidates := range armor.SelectionCategory {
		t.Run(fmt.Sprintf("armor/%d", category), func(t *testing.T) {
			if len(candidates) != 5 {
				t.Fatalf("armor count=%d", len(candidates))
			}
			body := make([]byte, 6+9*len(candidates))
			binary.LittleEndian.PutUint16(body, 95)
			binary.LittleEndian.PutUint16(body[2:], category)
			want := map[uint32]uint32{}
			n := len(candidates)
			body[4+4*n] = byte(n)
			for i, c := range candidates {
				binary.LittleEndian.PutUint32(body[4+i*4:], c.ItemID)
				binary.LittleEndian.PutUint32(body[5+4*n+i*5:], c.ItemID)
				want[c.ItemID] = c.Count
			}
			openLevelAimingTestBox(t, items, 0, 490003554, body, want, nil)
		})
	}
	for stage := 0; stage < 16; stage++ {
		id := uint32(490003535 + stage)
		def, err := resolveCurrentBoosterDefinition(items, id, currentBoosterRequestRandom)
		if err != nil {
			t.Fatal(err)
		}
		want := map[uint32]uint32{}
		for _, group := range def.Random.Groups {
			if len(group.Entries) != 1 {
				t.Fatal("stage is not a fixed native reward")
			}
			v := group.Entries[0]
			want[uint32(v.ItemID)] += uint32(v.Count)
		}
		if stage == 0 && !reflect.DeepEqual(want, map[uint32]uint32{490003532: 1, 490003551: 1, 490003552: 1}) {
			t.Fatalf("Lv10=%v", want)
		}
		if stage == 2 && !reflect.DeepEqual(want, map[uint32]uint32{490003532: 1, 490003554: 1}) {
			t.Fatalf("Lv20=%v", want)
		}
		for _, source := range []uint32{id, uint32(10154405 + stage)} {
			t.Run(fmt.Sprintf("stage/%d", source), func(t *testing.T) {
				d, err := items.ResolveItem(source)
				if err != nil {
					t.Fatal(err)
				}
				doc, err := parseDungeonCardPVFDocument(a, d.PVFPath)
				if err != nil {
					t.Fatal(err)
				}
				mark := doc.Ints("icon mark")
				name, _ := doc.Text("name")
				if len(mark) != 1 || mark[0] != 34 || strings.HasPrefix(name, "name_") {
					t.Fatalf("bad native display: %s mark=%v", name, mark)
				}
				openLevelAimingTestBox(t, items, 0, source, []byte{95, 0}, want, nil)
			})
		}
	}
}
