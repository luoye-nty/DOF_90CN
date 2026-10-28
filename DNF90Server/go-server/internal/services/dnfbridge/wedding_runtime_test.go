package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/wedding"
)

func TestWeddingDungeonRequiresActualPairAndKeepsBothResults(t *testing.T) {
	s, repos, a, b := weddingTestService(t)
	ctx := context.Background()
	_, err := wedding.Commit(ctx, repos, s.weddingCatalog, wedding.Command{ActorID: "1", PartnerID: "2", AccountID: "dnf:1", Operation: 1, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	owner := &runtimePartyDungeonOwner{entryMemberCount: 2, members: []runtimePartyDungeonMember{{userID: 1, generation: a.characterGeneration}, {userID: 2, generation: b.characterGeneration}}}
	runtime := &runtimeDungeonState{Character: dnfrepo.CharacterRecord{CharacterID: "1"}, partyDungeonOwner: owner}
	for _, mode := range []string{"solo", "three", "practice", "unrelated", "pair"} {
		r := *runtime
		switch mode {
		case "solo":
			r.partyDungeonOwner = nil
		case "three":
			r.partyDungeonOwner = &runtimePartyDungeonOwner{entryMemberCount: 3, members: owner.members}
		case "practice":
			r.ProtocolMode = currentDungeonProtocolModePractice
		case "unrelated":
			r.Character.CharacterID = "3"
		}
		got, e := s.currentWeddingDungeonCouple(ctx, &r)
		if e != nil || got != (mode == "pair") {
			t.Fatalf("%s: %t %v", mode, got, e)
		}
		owner.weddingClearKnown = false
	}
	if ok, e := s.currentWeddingDungeonCouple(ctx, runtime); e != nil || !ok {
		t.Fatalf("pair: %t %v", ok, e)
	}
	owner.members = owner.members[1:]
	runtime.Character.CharacterID = "2"
	if ok, e := s.currentWeddingDungeonCouple(ctx, runtime); e != nil || !ok {
		t.Fatalf("second result after first leaves: %t %v", ok, e)
	}
}

func TestWeddingCeremonyTimerAndPrivateRoom(t *testing.T) {
	s, repos, a, b := weddingTestService(t)
	ctx := context.Background()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	s.gameplayTimers = queue
	s.weddingCatalog.EndSeconds = 60
	s.weddingCatalog.Ceremonies[0] = wedding.Ceremony{Gift: wedding.Item{ID: 10002629, Count: 1}}
	items, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst": "", "equipment/equipment.lst": "", "stackable/stackable.lst": "10002629 `gift.stk`",
		"stackable/gift.stk": "[name] `Wedding Gift`\n[stackable type] `[material]`\n[stack limit] 100\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.pvfItemCatalog = items
	start := queue.Now().Add(300 * time.Second).Unix()
	for _, id := range []string{"1", "2"} {
		c, _, _ := repos.Character.Load(ctx, id)
		partner := int64(2)
		if id == "2" {
			partner = 1
		}
		c.Stats = map[string]int64{"wedding_state": 2, "wedding_id": 65538, "wedding_partner_id": partner, "wedding_start_at": start, "wedding_room": 8}
		if err := repos.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
		if _, ok := weddingTownInstance(c, 9, 0); ok {
			t.Fatal("wrong room admitted")
		}
	}
	for _, peer := range []*gameSession{a, b} {
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: peer.selectedCharacterID, TownID: 39, AreaID: 8, InstanceKey: 65538, Session: peer})
	}
	c, _, _ := repos.Character.Load(ctx, "1")
	if err := s.scheduleWeddingCeremony(c); err != nil {
		t.Fatal(err)
	}
	first := queue.task(t, 0)
	if first.delay != 300*time.Second {
		t.Fatalf("start delay=%s", first.delay)
	}
	queue.mu.Lock()
	queue.now = time.Unix(start, 0)
	queue.mu.Unlock()
	queue.fire(first, false)
	for _, id := range []string{"1", "2"} {
		c, _, _ := repos.Character.Load(ctx, id)
		if c.Stats["wedding_state"] != 3 {
			t.Fatal("ceremony not started")
		}
	}
	second := queue.task(t, 1)
	if second.delay != 60*time.Second {
		t.Fatalf("end delay=%s", second.delay)
	}
	queue.mu.Lock()
	queue.now = time.Unix(start+60, 0)
	queue.mu.Unlock()
	queue.fire(second, false)
	for _, id := range []string{"1", "2"} {
		c, _, _ := repos.Character.Load(ctx, id)
		if c.Stats["wedding_state"] != 4 {
			t.Fatal("ceremony not completed")
		}
		bag, _, _ := repos.Inventory.Load(ctx, id)
		count := int64(0)
		for _, item := range bag.Slots {
			if item.ItemID == 10002629 {
				count += item.Count
			}
		}
		if count != 1 {
			t.Fatalf("%s gifts=%d", id, count)
		}
	}
	if err := s.advanceWeddingCeremony("1", 65538, start); err != nil {
		t.Fatal(err)
	}
	for _, peer := range []*gameSession{a, b} {
		packets := peer.conn.(*bufferConn).write.Bytes()
		seen := map[uint16]int{}
		for len(packets) > 0 {
			packet, rest := splitLongHengGameServerUpperPacket(t, packets)
			packets = rest
			seen[packet.Header.MsgID]++
			if packet.Header.MsgID == 1007 && len(packet.Body) != 68 {
				t.Fatal("invalid cinema start")
			}
			if packet.Header.MsgID == 999 && len(packet.Body) != 71 {
				t.Fatal("invalid cinema end")
			}
		}
		if seen[1007] != 1 || seen[999] != 1 {
			t.Fatalf("cinema notifications: %v", seen)
		}
	}
}

func weddingTestService(t *testing.T) (*Service, dnfrepo.Group, *gameSession, *gameSession) {
	t.Helper()
	repos := memory.NewMemoryGroup()
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":    "39 `wedding.twn`",
		"town/wedding.twn": "[area]\n0 `wedding.map` `[normal]`\n[/area]",
		"map/wedding.map":  "[NPC]\n20013 `[left]` 100 200 0\n[/NPC]",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain, gameUpperHeader: gameUpperHeaderServer16}, onlinePlayers: newOnlinePlayerManager(), townCatalog: table, weddingCatalog: &wedding.Catalog{MinimumLevel: 30, RemarryDays: 1, MaxLove: 999}, repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
	sessions := []*gameSession{{conn: &bufferConn{}, accountID: "dnf:1"}, {conn: &bufferConn{}, accountID: "dnf:2"}}
	for i, c := range []dnfrepo.CharacterRecord{{CharacterID: "1", AccountID: "dnf:1", Name: "first", Job: "0", Level: 30}, {CharacterID: "2", AccountID: "dnf:2", Name: "second", Job: "1", Level: 30}} {
		if err := repos.Character.Save(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		peer := sessions[i]
		id := uint16(i + 1)
		s.bindGameSessionCharacter(peer, id)
		peer.townSceneReadyCharacterID = id
		peer.townPositionSnapshot = currentTownPositionSnapshot{CharacterID: id, TownID: 39, AreaID: 0, PositionX: 100, PositionY: 200, PositionValid: true}
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: id, TownID: 39, AreaID: 0, Session: peer})
		t.Cleanup(func() { s.clearWeddingInvitations(peer) })
	}
	return s, repos, sessions[0], sessions[1]
}

func weddingTestRequest(target uint32, op byte, decision ...byte) []byte {
	body := make([]byte, 20+len(decision))
	binary.LittleEndian.PutUint32(body[13:17], target)
	body[17] = op
	if len(decision) > 0 {
		body[20] = decision[0]
	}
	return body
}

func TestWeddingInvitationAndDecision(t *testing.T) {
	for _, decision := range []byte{0, 1, 2} {
		for _, upper := range []bool{false, true} {
			t.Run(string(rune('0'+decision))+map[bool]string{false: "legacy", true: "upper"}[upper], func(t *testing.T) {
				s, repos, a, b := weddingTestService(t)
				queue := newFakeCurrentDungeonDeathTimerQueue()
				s.gameplayTimers = queue
				send := func(peer *gameSession, opcode uint16, body []byte) {
					t.Helper()
					var err error
					m := weddingGameplayModule()
					req := gameplayRequest{Body: body, Classification: dnfproto.DefaultChannelClassification}
					if upper {
						err = m.UpperHandlers[opcode](s, peer, req)
					} else {
						req.Body = m.LegacyNormalizers[opcode](body)
						err = m.LegacyHandlers[opcode](s, peer, req)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				send(a, 1032, weddingTestRequest(2, 1))
				ack, tail := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
				if ack.Header.Classification != 1 || ack.Header.MsgID != 1032 || !bytes.Equal(ack.Body, []byte{1, 0, 0, 0, 0, 2, 0, 0, 0, 1, 0, 0}) || len(tail) != 0 {
					t.Fatalf("request ack: %+v %x", ack.Header, ack.Body)
				}
				invite, tail := splitLongHengGameServerUpperPacket(t, b.conn.(*bufferConn).write.Bytes())
				if invite.Header.Classification != 0 || invite.Header.MsgID != 997 || !bytes.Equal(invite.Body, []byte{0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0}) || len(tail) != 0 {
					t.Fatalf("invite: %+v %x", invite.Header, invite.Body)
				}
				for _, id := range []string{"1", "2"} {
					c, _, _ := repos.Character.Load(context.Background(), id)
					if c.Stats["wedding_state"] != 0 {
						t.Fatal("proposal persisted before consent")
					}
				}
				a.conn.(*bufferConn).write.Reset()
				b.conn.(*bufferConn).write.Reset()
				if got := s.weddingInvitations[1].expires.Sub(queue.Now()); got != 6*time.Second {
					t.Fatalf("native invitation lifetime: %s", got)
				}
				queue.mu.Lock()
				queue.now = queue.now.Add(5500 * time.Millisecond)
				queue.mu.Unlock()
				target := uint32(1)
				if decision != 0 {
					target = 0 // Native decline and timeout omit the proposer.
				}
				send(b, 1033, weddingTestRequest(target, 1, decision))
				response, _ := splitLongHengGameServerUpperPacket(t, b.conn.(*bufferConn).write.Bytes())
				if response.Header.MsgID != 1033 || len(response.Body) != 13 || response.Body[0] != 1 || response.Body[12] != decision {
					t.Fatalf("response: %x", response.Body)
				}
				notice, _ := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
				if notice.Header.Classification != 0 || notice.Header.MsgID != 998 || len(notice.Body) != 12 || notice.Body[11] != decision {
					t.Fatalf("result notice: %+v %x", notice.Header, notice.Body)
				}
				want := int64(0)
				if decision == 0 {
					want = 1
				}
				for _, id := range []string{"1", "2"} {
					c, _, _ := repos.Character.Load(context.Background(), id)
					if c.Stats["wedding_state"] != want {
						t.Fatalf("decision %d state %+v", decision, c.Stats)
					}
				}
				if len(s.weddingInvitations) != 0 {
					t.Fatal("invitation retained after decision")
				}
				if decision == 0 {
					for _, peer := range []*gameSession{a, b} {
						seen := map[uint16]int{}
						for raw := peer.conn.(*bufferConn).write.Bytes(); len(raw) > 0; {
							packet, rest := splitLongHengGameServerUpperPacket(t, raw)
							raw = rest
							seen[packet.Header.MsgID]++
						}
						if seen[1000] != 1 || seen[1013] != 1 || seen[232] != 0 || seen[245] != 0 {
							t.Fatalf("relationship notifications: %v", seen)
						}
					}
					send(a, 1032, weddingTestRequest(0, 5))
					for _, id := range []string{"1", "2"} {
						c, _, _ := repos.Character.Load(context.Background(), id)
						if c.Stats["wedding_state"] != 0 {
							t.Fatal("breakup not reciprocal")
						}
					}
				}
			})
		}
	}
}

func TestWeddingRejectsStaleAndInvalidSessions(t *testing.T) {
	for _, mode := range []string{"disconnect", "switch", "replace", "timeout", "left_area", "other_channel", "spoof", "self", "short", "bad_decision", "zero_accept", "wrong_target"} {
		t.Run(mode, func(t *testing.T) {
			s, repos, a, b := weddingTestService(t)
			if mode == "self" {
				if err := s.handleWeddingRequest(a, weddingTestRequest(1, 1)); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := s.handleWeddingRequest(a, weddingTestRequest(2, 1)); err != nil {
				t.Fatal(err)
			}
			response := weddingTestRequest(1, 1, 0)
			switch mode {
			case "disconnect":
				s.unbindGameSession(a)
			case "switch":
				s.bindGameSessionCharacter(a, 3)
			case "replace":
				replacement := &gameSession{conn: &bufferConn{}, accountID: "dnf:1", selectedCharacterID: 1}
				s.promoteResidentGameSession(replacement, 1)
			case "timeout":
				s.expireWeddingInvitation(s.weddingInvitations[1])
			case "left_area":
				// 运营口径（2026-10-04）：求婚只要求"同频道 + 同区域"，
				// 所以"同区域内走远"不再算失效（见 TestWeddingProposeOnlyRequiresSameArea）；
				// 真正让它失效的是对方离开了这个场景实例。
				s.onlinePlayers.LeaveArea(2)
			case "other_channel":
				b.channel.ID = 42
			case "spoof":
				b.accountID = "dnf:999"
			case "short":
				response = response[:18]
			case "bad_decision":
				response[20] = 3
			case "zero_accept":
				binary.LittleEndian.PutUint32(response[13:17], 0)
			case "wrong_target":
				binary.LittleEndian.PutUint32(response[13:17], 3)
			}
			if err := s.handleWeddingResponse(b, response); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"1", "2"} {
				c, _, _ := repos.Character.Load(context.Background(), id)
				if c.Stats["wedding_state"] != 0 {
					t.Fatalf("%s persisted invalid consent", mode)
				}
			}
		})
	}
}

// 运营口径（2026-10-04）：求婚只要求"同频道 + 同区域 + 双方场景就绪"，
// 不再要求双方贴着神父 NPC。
//
// 背景：神父站在城镇 39 主街 y≈156，而角色自然站位在 y≈240（客户端视觉上已经是
// "站在神父面前"），旧口径沿用任务会面的 ±120/±80 容差，实测差 5 像素就被拒，
// 客户端只会弹"未能搜索到该玩家"，看起来就像功能坏了。
// 这个用例把"同区域内离神父很远也能求婚"钉住，防止以后被改回去。
// 想恢复旧口径：把 wedding_runtime.go 的 currentWeddingRequiresPriest 改成 true。
func TestWeddingProposeOnlyRequiresSameArea(t *testing.T) {
	s, _, a, b := weddingTestService(t)
	// 两人都远离测试地图里摆在 (100,200) 的神父，但仍同频道同区域。
	a.townPositionSnapshot.PositionX, a.townPositionSnapshot.PositionY = 2000, 600
	b.townPositionSnapshot.PositionX, b.townPositionSnapshot.PositionY = 2100, 640
	if err := s.handleWeddingRequest(a, weddingTestRequest(2, 1)); err != nil {
		t.Fatal(err)
	}
	if len(s.weddingInvitations) == 0 {
		t.Fatal("same-area proposal far from the priest was rejected")
	}
	if s.weddingInvitations[1] == nil || s.weddingInvitations[2] == nil {
		t.Fatalf("invitation not registered for both peers: %+v", s.weddingInvitations)
	}
}

func TestWeddingStateWire(t *testing.T) {
	single := weddingStateBody(wedding.State{})
	if binary.LittleEndian.Uint32(single[:4]) != 0xffffffff || single[14] != 0xff {
		t.Fatalf("single sentinels: %x", single)
	}
	body := weddingStateBody(wedding.State{PairID: 0x01020304, Status: 4, Ceremony: 2, Ring: 5, Date: 0x11223344, Love: 999})
	if !bytes.Equal(body, []byte{4, 3, 2, 1, 4, 2, 0x44, 0x33, 0x22, 0x11, 0xe7, 3, 0, 0, 5}) {
		t.Fatalf("state: %x", body)
	}
}

func TestWeddingEnterKeepsPersistedTownAndRejectsOtherChannel(t *testing.T) {
	s, repos, a, _ := weddingTestService(t)
	ctx := context.Background()
	table, err := dnftown.Load(ctx, townMoveTestSource{
		"town/town.lst":    "39 `wedding.twn`",
		"town/wedding.twn": "[area]\n0 `priest.map` `[normal]`\n[/area]\n[area]\n8 `Church/Church.map` `[normal]`\n[/area]",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.townCatalog = table
	s.weddingCatalog.Positions = [2][3]int64{{670, 330, 0}, {685, 360, 0}}
	c, _, _ := repos.Character.Load(ctx, "1")
	c.Stats = map[string]int64{"town_id": 39, "area_id": 0, "pos_x": 100, "pos_y": 200, "direction": 5, "area_state": 3, "wedding_state": 2, "wedding_id": 65538, "wedding_partner_id": 2, "wedding_start_at": time.Now().Add(300 * time.Second).Unix(), "wedding_room": 8}
	if err := repos.Character.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	enableTownMoveSkillProjection(t, s, repos, "1")
	body := make([]byte, 22)
	binary.LittleEndian.PutUint32(body[13:17], 65538)
	binary.LittleEndian.PutUint32(body[17:21], 8)
	a.channel.ID = 19
	if err := s.handleWeddingEnter(a, body); err != nil {
		t.Fatal(err)
	}
	if a.townPositionSnapshot.AreaID != 0 {
		t.Fatal("entered ceremony in wrong channel")
	}
	a.channel.ID = 0
	if err := s.handleWeddingEnter(a, body); err != nil {
		t.Fatal(err)
	}
	if a.townPositionSnapshot.AreaID != 8 || a.townPositionSnapshot.PositionX != 670 {
		t.Fatalf("entry snapshot: %+v", a.townPositionSnapshot)
	}
	presence, ok := s.onlinePlayers.PlayerForCharacter(1)
	if !ok || presence.InstanceKey != 65538 {
		t.Fatalf("private presence: %+v", presence)
	}
	stored, _, _ := repos.Character.Load(ctx, "1")
	if stored.Stats["area_id"] != 0 || stored.Stats["pos_x"] != 100 {
		t.Fatal("church overwrote reconnect location")
	}
	if err := s.handleTownSetUserArea(a, buildTownMoveRequest(39, 0, 100, 230, 5)); err != nil {
		t.Fatal(err)
	}
	if a.townPositionSnapshot.AreaID != 0 {
		t.Fatal("native priest exit failed")
	}
}
