package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func weddingCapsuleTestService(t *testing.T) (*Service, dnfrepo.Group, *gameSession, *gameSession) {
	t.Helper()
	s, repos, a, b := weddingTestService(t)
	source := bridgePVFSource{
		"monster/monster.lst": "", "equipment/equipment.lst": "",
		"stackable/stackable.lst": "10002613 `ring.stk`\n10002910 `capsule.stk`",
		"stackable/ring.stk":      "[stackable type] `[etc]` 1\n[stack limit] 1\n[price] 3500000\n[need material] 10002606 500\n[/need material]\n[action type] `[upgrade wedding ring]` 3\n[/action type]",
		"stackable/capsule.stk":   "[stackable type] `[waste]` 0\n[stack limit] 1000\n[attach type] `[account]`\n[minimum level] 30\n[maximum level] 84\n[increase status type] `[expUp]` 30000",
		"itemshop/itemshop.lst":   "1207 `wedding.shp`", "itemshop/wedding.shp": "[item list] 10002613\n[/item list]",
		"equipment/pricetable.tbl": "[rate] 200 150 30\n[repair cost] 0.08",
	}
	var err error
	s.pvfItemCatalog, err = newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	s.npcShopCatalog, err = newCurrentNPCShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	s.weddingCatalog.RingTickets[3] = wedding.Item{ID: 10002613, Count: 1}
	s.weddingCapsuleCatalog = &wedding.CapsuleCatalog{MaxOwn: 10, MaxTaken: 10, Gauge: 2, MaxGauge: 10, ItemID: 10002910}
	ctx := context.Background()
	for i, id := range []string{"1", "2"} {
		c, _, _ := repos.Character.Load(ctx, id)
		c.Stats = map[string]int64{"wedding_state": 4, "wedding_id": 65538, "wedding_partner_id": int64(2 - i), "wedding_date": 123, "wedding_love": 946, "wedding_ring": 2, "gold": 9000000, "wedding_capsule_pair": 65538, "wedding_capsule_date": 123, "wedding_capsule_progress": 10}
		if err := repos.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
	}
	a.townPositionSnapshot.TownID = 101
	b.townPositionSnapshot.TownID = 101
	return s, repos, a, b
}

func TestWeddingCapsuleNativeWorkflowAndRestore(t *testing.T) {
	for _, upper := range []bool{false, true} {
		t.Run(strconv.FormatBool(upper), func(t *testing.T) {
			s, repos, a, b := weddingCapsuleTestService(t)
			send := func(peer *gameSession, op uint16, code int32) []byte {
				t.Helper()
				peer.conn.(*bufferConn).write.Reset()
				m := weddingGameplayModule()
				r := gameplayRequest{Classification: dnfproto.DefaultChannelClassification}
				var err error
				if upper {
					err = m.UpperHandlers[op](s, peer, r)
				} else {
					r.Body = m.LegacyNormalizers[op]([]byte{1, 2, 3, 4})
					err = m.LegacyHandlers[op](s, peer, r)
				}
				if err != nil {
					t.Fatal(err)
				}
				ack, rest := splitLongHengGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
				if ack.Header.MsgID != op || ack.Body[0] != 1 || int32(binary.LittleEndian.Uint32(ack.Body[1:5])) != code {
					t.Fatalf("ack=%+v %x", ack.Header, ack.Body)
				}
				if code != 0 && len(ack.Body) != 5 {
					t.Fatalf("failure has success fields %x", ack.Body)
				}
				return rest
			}
			if rest := send(a, 1052, 0); len(rest) != 0 {
				t.Fatalf("make extra=%x", rest)
			}
			ack, _ := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
			if !bytes.Equal(ack.Body, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 1}) {
				t.Fatalf("make layout=%x", ack.Body)
			}
			send(a, 1052, -4)
			send(a, 1053, 0)
			pending, rest := splitLongHengGameServerUpperPacket(t, b.conn.(*bufferConn).write.Bytes())
			if pending.Header.MsgID != 1015 || !bytes.Equal(pending.Body, []byte{10, 0, 0, 0, 0, 1, 0}) || len(rest) != 0 {
				t.Fatalf("pending=%x rest=%x", pending.Body, rest)
			}
			// A new service restores solely from repositories, including the sender reminder.
			for _, peer := range []*gameSession{a, b} {
				peer.conn.(*bufferConn).write.Reset()
				c, _, _ := repos.Character.Load(context.Background(), strconv.Itoa(int(peer.selectedCharacterID)))
				restored := &Service{options: s.options, weddingCatalog: s.weddingCatalog, weddingCapsuleCatalog: s.weddingCapsuleCatalog, repositoryProvider: s.repositoryProvider}
				if err := restored.sendWeddingCapsuleState(peer, c); err != nil {
					t.Fatal(err)
				}
				noti, _ := splitLongHengGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
				if noti.Header.MsgID != 1015 || len(noti.Body) != 7 {
					t.Fatalf("restore=%+v", noti)
				}
				if peer == a && noti.Body[6] != 1 || peer == b && noti.Body[5] != 1 {
					t.Fatalf("restore state=%x", noti.Body)
				}
			}
			send(a, 1053, -4)
			rest = send(b, 1054, 0)
			item, rest := splitLongHengGameServerUpperPacket(t, rest)
			if item.Header.MsgID != 13 || len(rest) == 0 {
				t.Fatalf("take inventory order=%+v", item.Header)
			}
			bag, _, _ := repos.Inventory.Load(context.Background(), "2")
			assertCurrentNPCShopInventoryAmount(t, bag, 10002910, 1)
			send(b, 1054, -1)
		})
	}
}

func TestWeddingCapsuleRejectsMalformedClassDungeonAndFullBag(t *testing.T) {
	s, repos, a, _ := weddingCapsuleTestService(t)
	m := weddingGameplayModule()
	for _, op := range []uint16{1052, 1053, 1054} {
		if err := m.UpperHandlers[op](s, a, gameplayRequest{Classification: 0, Body: nil}); err != nil {
			t.Fatal(err)
		}
		if err := m.UpperHandlers[op](s, a, gameplayRequest{Classification: dnfproto.DefaultChannelClassification, Body: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if a.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("invalid requests sent reply")
	}
	a.dungeon.runtime = &runtimeDungeonState{}
	if err := s.weddingCapsuleCommand(a, 1052, nil); err != nil {
		t.Fatal(err)
	}
	ack, _ := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
	if int32(binary.LittleEndian.Uint32(ack.Body[1:])) != -6 {
		t.Fatalf("dungeon ack=%x", ack.Body)
	}
	a.dungeon.runtime = nil
	a.conn.(*bufferConn).write.Reset()
	c, _, _ := repos.Character.Load(context.Background(), "1")
	c.Stats["wedding_capsule_taken"] = 2
	if err := repos.Character.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	bag := dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{}}
	for slot := 1; slot <= 400; slot++ {
		bag.Slots["0:"+strconv.Itoa(slot)] = dnfrepo.ItemStack{ItemID: 999, Count: 1}
	}
	if err := repos.Inventory.Save(context.Background(), bag); err != nil {
		t.Fatal(err)
	}
	if err := s.weddingCapsuleCommand(a, 1054, nil); err != nil {
		t.Fatal(err)
	}
	ack, _ = splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
	if int32(binary.LittleEndian.Uint32(ack.Body[1:])) != -6 {
		t.Fatalf("full bag ack=%x", ack.Body)
	}
	c, _, _ = repos.Character.Load(context.Background(), "1")
	if c.Stats["wedding_capsule_taken"] != 2 {
		t.Fatal("full bag lost gift")
	}
}

func TestWeddingRingShopNativePurchase(t *testing.T) {
	for _, mode := range []string{"success", "gold", "love", "full_bag"} {
		t.Run(mode, func(t *testing.T) {
			s, repos, a, b := weddingCapsuleTestService(t)
			ctx := context.Background()
			if mode == "gold" {
				c, _, _ := repos.Character.Load(ctx, "1")
				c.Stats["gold"] = 1
				if err := repos.Character.Save(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "love" {
				for _, id := range []string{"1", "2"} {
					c, _, _ := repos.Character.Load(ctx, id)
					c.Stats["wedding_love"] = 1
					if err := repos.Character.Save(ctx, c); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "full_bag" {
				bag := dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{}}
				for slot := 1; slot <= 400; slot++ {
					bag.Slots["0:"+strconv.Itoa(slot)] = dnfrepo.ItemStack{ItemID: 999, Count: 1}
				}
				if err := repos.Inventory.Save(ctx, bag); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.handleCurrentNPCShopBuy(a, currentNPCShopBuyTestBody(10002613, 1, 1207, 20011)); err != nil {
				t.Fatal(err)
			}
			ack, rest := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
			if mode != "success" {
				want := map[string]byte{"gold": 10, "love": 1, "full_bag": 4}[mode]
				if !bytes.Equal(ack.Body, []byte{0, want}) || len(rest) != 0 {
					t.Fatalf("%s failure=%x rest=%x", mode, ack.Body, rest)
				}
				return
			}
			if ack.Body[0] != 1 || binary.LittleEndian.Uint32(ack.Body[1:]) != 5500000 {
				t.Fatalf("buy=%x", ack.Body)
			}
			update, rest := splitLongHengGameServerUpperPacket(t, rest)
			if update.Header.MsgID != 14 {
				t.Fatal("missing item update")
			}
			love, rest := splitLongHengGameServerUpperPacket(t, rest)
			if love.Header.MsgID != 1003 || binary.LittleEndian.Uint32(love.Body[4:]) != 446 || len(rest) != 0 {
				t.Fatalf("love=%x", love.Body)
			}
			peer, _ := splitLongHengGameServerUpperPacket(t, b.conn.(*bufferConn).write.Bytes())
			if peer.Header.MsgID != 1003 || binary.LittleEndian.Uint32(peer.Body[4:]) != 446 {
				t.Fatalf("peer love=%x", peer.Body)
			}
			bag, _, _ := repos.Inventory.Load(ctx, "1")
			assertCurrentNPCShopInventoryAmount(t, bag, 10002613, 1)
			assertCurrentNPCShopInventoryAmount(t, bag, 10002606, 0)
		})
	}
}

func TestWeddingCapsuleAndRingRealPVF(t *testing.T) {
	path := os.Getenv("DNF90_PVF_PATH")
	if path == "" {
		t.Skip("set DNF90_PVF_PATH for wedding capsule runtime verification")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, repos, a, b := weddingCapsuleTestService(t)
	s.weddingCatalog, err = wedding.LoadCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	s.weddingCapsuleCatalog = nil
	s.initialEquipmentArchive = archive
	rules, err := s.currentWeddingCapsuleCatalog()
	if err != nil || *rules != (wedding.CapsuleCatalog{MaxOwn: 10, MaxTaken: 10, Gauge: 2, MaxGauge: 10, ItemID: 10002910}) {
		t.Fatalf("rules=%+v %v", rules, err)
	}
	s.pvfItemCatalog, err = newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	s.npcShopCatalog, err = newCurrentNPCShopCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []uint32{10002611, 10002612, 10002613, 10002614, 10002615} {
		p, err := resolveCurrentNPCShopPricing(s.npcShopCatalog, s.pvfItemCatalog, id)
		if err != nil || p.BuyGold != []int64{500000, 1000000, 3500000, 8000000, 12000000}[i] || p.NeedMaterialCount != []int64{50, 300, 500, 800, 999}[i] || p.NeedMaterialItem != 10002606 {
			t.Fatalf("price %d=%+v %v", id, p, err)
		}
	}
	if err := s.handleCurrentNPCShopBuy(a, currentNPCShopBuyTestBody(10002613, 1, 1207, 20011)); err != nil {
		t.Fatal(err)
	}
	bag, _, _ := repos.Inventory.Load(context.Background(), "1")
	assertCurrentNPCShopInventoryAmount(t, bag, 10002613, 1)
	for _, step := range []struct {
		session *gameSession
		op      uint16
	}{{a, 1052}, {a, 1053}, {b, 1054}} {
		if err := s.weddingCapsuleCommand(step.session, step.op, nil); err != nil {
			t.Fatal(err)
		}
	}
	claimed, _, _ := repos.Inventory.Load(context.Background(), "2")
	assertCurrentNPCShopInventoryAmount(t, claimed, 10002910, 1)
	resolver, err := s.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	r, err := resolver(10002910)
	if err != nil || r.DirectExperience == nil || r.DirectExperience.MinimumLevel != 30 || r.DirectExperience.MaximumLevel != 84 {
		t.Fatalf("capsule=%+v %v", r, err)
	}
	tables, err := progression.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []int{29, 30, 84, 85} {
		exp, err := tables.ThresholdToNext(level - 1)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := s.planDirectExperienceConsumable(tables, dnfrepo.CharacterRecord{CharacterID: "1", Level: level, Stats: map[string]int64{"exp": int64(exp)}}, dnfrepo.SkillRecord{CharacterID: "1", Points: dnfrepo.SkillPointState{SyncedLevel: level}}, *r.DirectExperience, time.Now())
		if level == 29 || level == 85 {
			if err == nil {
				t.Fatalf("out-of-range %d accepted", level)
			}
			continue
		}
		if err != nil || plan.Gain != 30000 || plan.NewExperience != exp+30000 {
			t.Fatalf("use level %d=%+v %v", level, plan, err)
		}
	}
	// Exercise the bridge's real recommended-clear decision and catalog wiring,
	// not just the module's arithmetic. Replayed settlement must preserve gauge.
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	service.initialEquipmentArchive = archive
	repositories, _ := service.repositoryGroup()
	c, _, _ := repositories.Character.Load(context.Background(), "99")
	c.Level = 1
	c.Stats["exp"] = 90
	c.Stats["wedding_state"] = 4
	c.Stats["wedding_id"] = 6488164
	c.Stats["wedding_partner_id"] = 100
	c.Stats["wedding_date"] = 123
	if err := repositories.Character.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{CharacterID: "99", Points: dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = c
	// 推荐副本判据读**进图那一刻**的等级快照 runtime.entryCharacterLevel，
	// 所以这里改角色等级之后必须把快照一起刷成 1，才能落在 [1,1] 区间里。
	runtime.Character.Level = 1
	runtime.entryCharacterLevel = 1
	runtime.ordinaryFinalRoomClearAccepted = true
	runtime.Dungeon.Metadata.RecommendedLevels = []int64{1, 1}
	resources := currentDungeonSettlementTestResources(t)
	for i := 0; i < 2; i++ {
		result, err := service.commitCurrentDungeonSettlement(context.Background(), session, runtime, resources.Progression, 20)
		if err != nil || result.Character.Stats["wedding_capsule_progress"] != 2 || result.Replayed != (i == 1) {
			t.Fatalf("bridge clear=%+v err=%v", result, err)
		}
	}
}

func TestWeddingCapsuleSettlementNotificationRetriesOnce(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 73)
	plan.WeddingCapsuleBody = []byte{2, 0, 0, 0}
	runtime.settlementResultPlan = &plan
	request := make([]byte, currentDungeonPlayResultBaseSize)
	request[currentDungeonPlayResultClientRankPointOffset] = 73
	want := errors.New("capsule notification send failed")
	conn := &failNthDungeonWriteConn{failAt: 4, err: want}
	session.conn = conn
	if err := service.handleDungeonSetPlayResult(session, request); !errors.Is(err, want) {
		t.Fatalf("send=%v", err)
	}
	if runtime.settlementWeddingCapsuleSent || !runtime.settlementClearRewardSent {
		t.Fatal("failed notification marked sent")
	}
	if err := service.handleDungeonSetPlayResult(session, request); err != nil {
		t.Fatal(err)
	}
	if !runtime.settlementWeddingCapsuleSent {
		t.Fatal("notification not retried")
	}
	written := conn.write.Len()
	if err := service.handleDungeonSetPlayResult(session, request); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != written {
		t.Fatal("completed notification duplicated")
	}
}
