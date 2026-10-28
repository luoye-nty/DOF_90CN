package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type guildFailSettlement struct {
	base dnfrepo.CharacterSettlementUnitOfWork
	fail bool
}

func (f *guildFailSettlement) WithinCharacterSettlement(ctx context.Context, id string, apply func(dnfrepo.Group) error) error {
	return f.base.WithinCharacterSettlement(ctx, id, func(tx dnfrepo.Group) error {
		if err := apply(tx); err != nil {
			return err
		}
		if f.fail {
			return errors.New("injected guild transaction failure")
		}
		return nil
	})
}

func TestCurrentGuildCosmeticsContentsLifecycle(t *testing.T) {
	ctx := context.Background()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst": "", "equipment/equipment.lst": "",
		"stackable/stackable.lst":         "490008321 `rename.stk` 490001385 `contract.stk`",
		"stackable/rename.stk":            "[name]\n`rename`\n[stackable type]\n`[waste]` 0\n[stack limit]\n1",
		"stackable/contract.stk":          "[name]\n`contract`\n[stackable type]\n`[booster random]` 0\n[stack limit]\n1",
		"etc/(r)guild.etc":                "[guild contents list]\n16 6 500 -1 0 10153531 0 100000 `new`\n1 5 169 -1 0 10150602 0 36000 `hot`\n[/guild contents list]\n[additional guild contents list]\n22 20 -1 0 0 490008321 0 10000000 `` 1\n23 21 300 1 0 490001373 0 3000000 `` 1\n[/additional guild contents list]\n[guild contracts]\n21 490001385 1\n[/guild contracts]",
		"etc/guild/insigniashapelist.etc": "[insignia mark]\n`mark0` `mark1` `mark2`\n[/insignia mark]\n[insignia frame]\n`frame0` `frame1` `frame2` `frame3`\n[/insignia frame]",
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	fail := &guildFailSettlement{base: repositories.CharacterSettlement}
	repositories.CharacterSettlement = fail
	master := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "master", Level: 90, Stats: map[string]int64{"gold": 30000000}}
	guest := dnfrepo.CharacterRecord{CharacterID: "3", AccountID: "dnf:guest", Name: "guest", Level: 90, Stats: map[string]int64{"gold": 10000000}}
	for _, c := range []dnfrepo.CharacterRecord{master, guest} {
		if err := repositories.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: c.CharacterID, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(guild.ID, guest, "", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecideApplication(guild.ID, "16", "3", true, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	guild.MemberLimit, guild.Mileage = 100, 200000
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	owner := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: master.AccountID}
	member := &gameSession{conn: &bufferConn{}, selectedCharacterID: 3, accountID: guest.AccountID}
	for _, session := range []*gameSession{owner, member} {
		session.channel.ID = 253
		session.initialTownRouteCharacterID = session.selectedCharacterID
		session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, pvfItemCatalog: items, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }, gameSessions: map[uint16]*gameSession{16: owner, 3: member}}
	send := func(session *gameSession, opcode uint16, body []byte, code byte) {
		t.Helper()
		session.conn.(*bufferConn).write.Reset()
		handler := guildGameplayModule().UpperHandlers[opcode]
		if handler == nil {
			t.Fatalf("missing route %d", opcode)
		}
		if err := handler(service, session, gameplayRequest{Opcode: opcode, Classification: 1, Body: body}); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
		want := []byte{1}
		if code != 0 {
			want = []byte{0, code}
		}
		if packet.Header.MsgID != opcode || !bytes.Equal(packet.Body, want) {
			t.Fatalf("op%d: %x want %x", opcode, packet.Body, want)
		}
	}
	load := func() (*dnfguild.Guild, dnfrepo.CharacterRecord, dnfrepo.InventoryRecord) {
		t.Helper()
		r, err := dnfguild.Load(ctx, repositories.Settings)
		if err != nil {
			t.Fatal(err)
		}
		g, _ := r.GuildByID(guild.ID)
		c, _, err := repositories.Character.Load(ctx, "16")
		if err != nil {
			t.Fatal(err)
		}
		i, _, err := repositories.Inventory.Load(ctx, "16")
		if err != nil {
			t.Fatal(err)
		}
		return g, c, i
	}
	request := func(id uint32) []byte { body := make([]byte, 8); binary.LittleEndian.PutUint32(body, id); return body }
	send(owner, 787, []byte{4}, 0)
	send(owner, 832, []byte{2, 0, 0, 0, 3, 0, 0, 0}, 0)
	send(member, 832, []byte{0, 0, 0, 0, 1, 0, 0, 0}, currentGuildGenericError)
	send(owner, 832, []byte{3, 0, 0, 0, 0, 0, 0, 0}, currentGuildGenericError)
	send(owner, 832, []byte{0}, 19)
	g, _, _ := load()
	if g.Mark != 4 || g.AuraMark != 2 || g.AuraFrame != 3 {
		t.Fatalf("cosmetic state=%+v", g)
	}
	member.conn.(*bufferConn).write.Reset()
	if err := service.publishCurrentGuildManagement(owner, g); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, member.conn.(*bufferConn).write.Bytes())
	if packet.Header.MsgID != 70 || !bytes.Equal(packet.Body, buildCurrentGuildManagementBody(g, 2, service.gameplayNow(), dnfguild.DefaultMemberGrade)) {
		t.Fatal("online member did not receive saved cosmetics")
	}
	send(member, 761, request(23), currentGuildContentErrorPermission)
	send(owner, 761, append(request(23), 0), 13)
	send(owner, 761, request(999), 13)
	fail.fail = true
	send(owner, 761, request(22), 13)
	fail.fail = false
	_, c, inv := load()
	if c.Stats["gold"] != 30000000 || len(inv.Slots) != 0 {
		t.Fatal("failed purchase committed assets")
	}
	definition, err := items.ResolveItem(490008321)
	if err != nil {
		t.Fatal(err)
	}
	inv.Slots = make(map[string]dnfrepo.ItemStack)
	for slot := definition.SlotStart; slot <= definition.SlotEnd; slot++ {
		inv.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{ItemID: 999, Count: 1}
	}
	if err := repositories.Inventory.Save(ctx, inv); err != nil {
		t.Fatal(err)
	}
	// A full bag has no code of its own in op761's table, so it answers the
	// generic refusal (13 购买后公会仓库将超过上限).
	send(owner, 761, request(22), currentGuildContentErrorGeneric)
	_, c, inv = load()
	if c.Stats["gold"] != 30000000 {
		t.Fatal("full backpack charged gold")
	}
	inv.Slots = make(map[string]dnfrepo.ItemStack)
	if err := repositories.Inventory.Save(ctx, inv); err != nil {
		t.Fatal(err)
	}
	send(owner, 761, request(22), 0)
	g, c, inv = load()
	if c.Stats["gold"] != 20000000 || currentQuestDropHeldCount(inv, 490008321) != 1 {
		t.Fatal("rename purchase debit/delivery")
	}
	send(owner, 761, request(22), currentGuildContentErrorAlreadyOwned)
	send(owner, 761, request(23), 0)
	g, c, _ = load()
	if c.Stats["gold"] != 17000000 || !g.Benefits[21].Active(service.gameplayNow()) {
		t.Fatal("contract purchase")
	}
	send(owner, 761, request(23), currentGuildContentErrorAlreadyOwned)
	claim := make([]byte, 17)
	binary.LittleEndian.PutUint32(claim[13:], 21)
	fail.fail = true
	send(owner, 1244, claim, 19)
	fail.fail = false
	g, _, inv = load()
	if g.Benefits[21].Claimed["16"] || currentQuestDropHeldCount(inv, 490001385) != 0 {
		t.Fatal("failed claim committed")
	}
	send(owner, 1244, claim, 0)
	send(owner, 1244, claim, 19)
	send(member, 1244, claim, 0)
	g, _, inv = load()
	if !g.Benefits[21].Claimed["16"] || !g.Benefits[21].Claimed["3"] || currentQuestDropHeldCount(inv, 490001385) != 1 {
		t.Fatal("contract once per member")
	}
	send(owner, 761, request(16), 0)
	send(owner, 761, request(16), currentGuildContentErrorAlreadyOwned)
	g, _, inv = load()
	if g.MemberLimit != 500 || g.Mileage != 100000 {
		t.Fatal("capacity purchase")
	}
	var renameSlot int16
	for key, stack := range inv.Slots {
		if stack.ItemID == 490008321 {
			_, renameSlot, _ = parseSceneInventorySlotKey(key)
		}
	}
	var rename packetWriter
	rename.writeUint16(uint16(renameSlot))
	rename.writeByte(0)
	writeClientNameDstr(&rename, "renamed")
	send(owner, 179, rename.bytes(), 0)
	g, c, inv = load()
	if g.Name != "renamed" || currentQuestDropHeldCount(inv, 490008321) != 0 || c.Stats["gold"] != 17000000 {
		t.Fatal("rename ticket consumption")
	}
	c.Stats["gold"] = 0
	if err := repositories.Character.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	send(owner, 761, request(22), currentGuildContentErrorGold)
	registry, err = dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	g, _ = registry.GuildByID(guild.ID)
	g.Mileage = 0
	b := g.Benefits[21]
	b.ExpiresAt = time.Now().Add(-time.Second)
	b.Claimed = nil
	g.Benefits[21] = b
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	send(owner, 761, request(16), currentGuildContentErrorMileage)
	send(owner, 1244, claim, 19)
}

func TestCurrentGuildCatalogRuntimePVF(t *testing.T) {
	path := os.Getenv("DNF90_PVF_PATH")
	if path == "" {
		t.Skip("set DNF90_PVF_PATH for runtime guild catalog validation")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfguild.LoadCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Contents) != 23 || catalog.AuraMarks != 25 || catalog.AuraFrames != 25 || catalog.ContractItem != 490001385 || catalog.ContractCount != 1 {
		t.Fatalf("runtime catalog=%+v", catalog)
	}
	if c := catalog.Contents[22]; !c.Gold || c.Price != 10000000 || c.Type != 20 || c.ItemID != 490008321 {
		t.Fatalf("rename row=%+v", c)
	}
	if c := catalog.Contents[23]; !c.Gold || c.Price != 3000000 || c.Days != 1 || c.Value != 300 {
		t.Fatalf("contract row=%+v", c)
	}
}
