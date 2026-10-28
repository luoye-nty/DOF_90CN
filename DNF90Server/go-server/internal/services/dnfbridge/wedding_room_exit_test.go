package dnfbridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func newWeddingRoomExitTest(t *testing.T) (*Service, *gameSession, dnfrepo.Group) {
	t.Helper()
	s, session, repos := newTownMoveTest(t)
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":      "2 `old.twn` 38 `seria.twn` 42 `underfoot.twn` 101 `couple.twn`",
		"town/old.twn":       "[area]\n5 `Town/Underfoot/Underfoot_DT_Or_Icewall.map` `[normal]`\n[/area]",
		"town/seria.twn":     "[area]\n0 `Cataclysm/Town/Elvengard/new_Elvengard.map` `[normal]`\n[/area]\n[area]\n1 `Cataclysm/Town/Elvengard/new_seria_room.map` `[gate]` 450 234\n[/area]",
		"town/underfoot.twn": "[area]\n0 `Cataclysm/Town/Underfoot/Underfoot_north.map` `[normal]`\n[/area]",
		"town/couple.twn":    "[exclude user pretown info]\n[area]\n0 `CoupleRoom/CoupleRoom.map` `[couple room]` 474 234\n[/area]",
		"map/Cataclysm/Town/Elvengard/new_seria_room.map": "[town movable area]\n858 183 30 100 38 0 49 224 34 52 101 0\n[/town movable area]",
		"map/Cataclysm/Town/Elvengard/new_Elvengard.map":  "[town movable area]\n10 196 90 142 38 2 622 196 0 0 38 1 571 129 50 50 38 1\n[/town movable area]",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.townCatalog = table
	c := weddingExitLoad(t, repos)
	c.Stats["wedding_state"], c.Stats["wedding_id"], c.Stats["wedding_partner_id"] = 4, 1900574, 30
	weddingExitSave(t, repos, c)
	peer := dnfrepo.CloneCharacter(c)
	peer.CharacterID, peer.AccountID = "30", "dnf:2"
	peer.Stats["wedding_partner_id"] = 29
	weddingExitSave(t, repos, peer)
	return s, session, repos
}

func weddingExitLoad(t *testing.T, repos dnfrepo.Group) dnfrepo.CharacterRecord {
	t.Helper()
	c, found, err := repos.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character: found=%v err=%v", found, err)
	}
	return c
}

func weddingExitSave(t *testing.T, repos dnfrepo.Group, c dnfrepo.CharacterRecord) {
	t.Helper()
	if err := repos.Character.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func weddingNativeExitPacket(t *testing.T) []byte {
	t.Helper()
	// Captured 2026-09-12 02:36:51: old Underfoot 2/5, 300/200, source 101.
	body, err := hex.DecodeString("02052c01c80005650000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestWeddingRoomExitNativeRoundTrip(t *testing.T) {
	for _, remembered := range []bool{false, true} {
		t.Run(fmt.Sprintf("native_remembered_entry_%v", remembered), func(t *testing.T) {
			s, session, repos := newWeddingRoomExitTest(t)
			c := weddingExitLoad(t, repos)
			c.Stats["town_id"], c.Stats["area_id"], c.Stats["pos_x"], c.Stats["pos_y"] = 42, 0, 497, 289
			weddingExitSave(t, repos, c)
			entry := buildTownCrossTownPortalRequest(101, 0, 900, 250, 5, 38)
			entry[9] = 1 // Live 02:57 entry carries source area 1 in opaque B.
			for _, body := range [][]byte{
				buildTownSeriaRoomPortalRequest(38, 1, 433, 311, 5, 42),
				entry,
			} {
				if err := s.handleTownSetUserArea(session, body); err != nil {
					t.Fatal(err)
				}
			}
			if got := weddingExitLoad(t, repos); got.Stats["town_id"] != 101 || got.Stats[currentTownPrevVillageTownStat] != 42 {
				t.Fatalf("entry/previous town: %+v", got.Stats)
			}
			body := weddingNativeExitPacket(t)
			if remembered {
				body[0], body[1] = 38, 1
			}
			session.conn.(*bufferConn).write.Reset()
			if err := s.handleTownSetUserArea(session, body); err != nil {
				t.Fatal(err)
			}
			_, rest := splitTownAreaTransitionSequence(t, session.conn.(*bufferConn).write.Bytes(), 29, 38, 1, 100, 250, 5, 3)
			if len(rest) != 0 {
				t.Fatalf("extra exit packets: %x", rest)
			}
			c = weddingExitLoad(t, repos)
			if c.Stats["town_id"] != 38 || c.Stats["area_id"] != 1 || c.Stats["pos_x"] != 100 || c.Stats["pos_y"] != 250 ||
				c.Stats[currentTownPrevVillageTownStat] != 42 || c.Stats[currentTownPrevVillagePosXStat] != 497 ||
				session.townPrevVillageSnapshot.TownID != 42 || c.Stats["wedding_state"] != 4 {
				t.Fatalf("exit location/previous town/wedding state: %+v %+v", c.Stats, session.townPrevVillageSnapshot)
			}
			// A replay after leaving 101 must not teleport to legacy Underfoot.
			session.conn.(*bufferConn).write.Reset()
			if err := s.handleTownSetUserArea(session, weddingNativeExitPacket(t)); err != nil {
				t.Fatal(err)
			}
			if session.conn.(*bufferConn).write.Len() != 0 {
				t.Fatal("replayed exit emitted transition")
			}
			if err := s.handleCurrentPrevVillage(session, nil); err != nil {
				t.Fatal(err)
			}
			c = weddingExitLoad(t, repos)
			if c.Stats["town_id"] != 42 || c.Stats["area_id"] != 0 || c.Stats["pos_x"] != 497 || c.Stats["pos_y"] != 289 {
				t.Fatalf("Seria door lost public origin: %+v", c.Stats)
			}
		})
	}
}

func TestWeddingRoomExitSendFailureRollsBack(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			s, session, repos := newWeddingRoomExitTest(t)
			c := weddingExitLoad(t, repos)
			c.Stats["town_id"], c.Stats["area_id"], c.Stats["pos_x"], c.Stats["pos_y"] = 101, 0, 1000, 250
			weddingExitSave(t, repos, c)
			want := errors.New("wedding exit transport failure")
			session.conn = &failNthDungeonWriteConn{failAt: failAt, err: want}
			if err := s.handleTownSetUserArea(session, weddingNativeExitPacket(t)); !errors.Is(err, want) {
				t.Fatalf("error=%v", err)
			}
			if got := weddingExitLoad(t, repos); !reflect.DeepEqual(got.Stats, c.Stats) {
				t.Fatalf("failed exit persisted location: %+v", got.Stats)
			}
			if session.townPrevVillageSnapshot.PositionValid {
				t.Fatal("failed exit committed return snapshot")
			}
			session.conn = &bufferConn{}
			if err := s.handleTownSetUserArea(session, weddingNativeExitPacket(t)); err != nil {
				t.Fatal(err)
			}
			if got := weddingExitLoad(t, repos); got.Stats["town_id"] != 38 {
				t.Fatalf("retry: %+v", got.Stats)
			}
		})
	}
}

// 工作区与上游在这一点上的锚点约定不同，这里断言**不变量**而不是上游的坐标：
//
//   - 上游的 currentWeddingPublicReturnOrigin 读的是当前城镇那扇"零尺寸反向传送门"
//     （width==0 && height==0），并另外在登录路径上擦洗被污染的 prev_village。
//   - 工作区的 town.seriaRoomPortalTrigger 只认正尺寸传送门（零尺寸行是纯触发器，
//     不构成"门"），且回城点修复收敛在 handleCurrentPrevVillage 一处。
//
// 两边都必须保证的不变量是同一个：**污染的(101)回城点绝不能被当成目的地**，
// 而且 op1425 必须给出一个真正位于城镇居民区(38/0)、不在任何传送门触发矩形里的
// 落点。下面按这个不变量断言。
func TestWeddingRoomExitRelogAndStaleOrigin(t *testing.T) {
	for _, currentTown := range []int64{38, 101} {
		for _, oldOrigin := range []byte{42, 101} {
			t.Run(fmt.Sprintf("current_%d_origin_%d", currentTown, oldOrigin), func(t *testing.T) {
				s, session, repos := newWeddingRoomExitTest(t)
				c := weddingExitLoad(t, repos)
				c.Stats["town_id"], c.Stats["area_id"] = currentTown, 0
				if currentTown == 38 {
					c.Stats["area_id"] = 1
				}
				currentTownPrevVillageApplyOriginStats(c.Stats, currentTownPositionSnapshot{CharacterID: 29, TownID: oldOrigin, AreaID: 0, PositionX: 497, PositionY: 289, MovementCode: 5, PositionValid: true})
				weddingExitSave(t, repos, c)
				// 登录仍然进赛丽亚房间，且被污染的 101 回城点不能留到会话里。
				_, town, area, row, _, err := s.currentCharacterListLoginTransition(context.Background(), session, 29, c)
				if err != nil {
					t.Fatal(err)
				}
				if town != 38 || area != 1 || row.Value1 != 450 || row.Value2 != 234 {
					t.Fatalf("login transition: town=%d area=%d row=%+v", town, area, row)
				}
				if err := s.handleCurrentPrevVillage(session, nil); err != nil {
					t.Fatal(err)
				}
				got := weddingExitLoad(t, repos)
				if got.Stats[currentTownPrevVillageTownStat] == 101 {
					t.Fatalf("wedding room survived as the previous town: %+v", got.Stats)
				}
				if oldOrigin == 42 {
					// 记忆里是一个正常的城镇 -> 就回那个城镇，不要被婚房场景干扰。
					if got.Stats["town_id"] != 42 || got.Stats["area_id"] != 0 ||
						got.Stats["pos_x"] != 497 || got.Stats["pos_y"] != 289 {
						t.Fatalf("legitimate previous town lost: %+v", got.Stats)
					}
					return
				}
				// 记忆本身就是婚房 -> 必须换成城镇居民区里的安全落点。
				if got.Stats["town_id"] != 38 || got.Stats["area_id"] != 0 {
					t.Fatalf("door landed outside the public town: %+v", got.Stats)
				}
				// 落点必须是安全站立点：不落在 38/0 的任何传送门触发矩形里。
				area38, ok := s.townArea(38, 0)
				if !ok {
					t.Fatal("town 38 area 0 missing from the fixture")
				}
				for _, portal := range area38.MovableAreas {
					if portal.Width <= 0 || portal.Height <= 0 {
						continue
					}
					if got.Stats["pos_x"] >= portal.X && got.Stats["pos_x"] <= portal.X+portal.Width &&
						got.Stats["pos_y"] >= portal.Y && got.Stats["pos_y"] <= portal.Y+portal.Height {
						t.Fatalf("landing retriggers portal %+v: %+v", portal, got.Stats)
					}
				}
			})
		}
	}
}

// 会话里残留的婚房快照同样不能被当成目的地。
func TestWeddingRoomExitStaleOnlineOrigin(t *testing.T) {
	s, session, repos := newWeddingRoomExitTest(t)
	c := weddingExitLoad(t, repos)
	stale := currentTownPositionSnapshot{CharacterID: 29, TownID: 101, AreaID: 0, PositionX: 871, PositionY: 250, MovementCode: 6, PositionValid: true}
	currentTownPrevVillageApplyOriginStats(c.Stats, stale)
	weddingExitSave(t, repos, c)
	session.townPrevVillageSnapshot = stale
	if err := s.handleCurrentPrevVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	got := weddingExitLoad(t, repos)
	if got.Stats["town_id"] != 38 || got.Stats["area_id"] != 0 {
		t.Fatalf("stale online door left the public town: %+v", got.Stats)
	}
	if got.Stats[currentTownPrevVillageTownStat] == 101 {
		t.Fatalf("stale wedding-room origin was committed: %+v", got.Stats)
	}
	if got.Stats["pos_x"] == 871 && got.Stats["pos_y"] == 250 {
		t.Fatalf("stale wedding-room position was reused verbatim: %+v", got.Stats)
	}
}

func TestWeddingRoomExitRoutingBoundaries(t *testing.T) {
	for _, variant := range []string{"normal_underfoot", "teleport_from_room", "source_mismatch", "scene_mismatch", "missing_gate", "unmarried_entry"} {
		t.Run(variant, func(t *testing.T) {
			s, session, repos := newWeddingRoomExitTest(t)
			c := weddingExitLoad(t, repos)
			c.Stats["town_id"], c.Stats["area_id"] = 101, 0
			body := weddingNativeExitPacket(t)
			wantTown, wantArea, wantX, blocked := int64(101), int64(0), int64(450), true
			switch variant {
			case "normal_underfoot":
				c.Stats["town_id"] = 42
				body[7] = 42
				wantTown, wantArea, wantX, blocked = 2, 5, 300, false
			case "teleport_from_room":
				body = buildTownTeleportArrayRequest(42, 0, 497, 289, 5)
				wantTown, wantArea, wantX, blocked = 42, 0, 497, false
			case "source_mismatch":
				body[7] = 38
			case "scene_mismatch":
				// 场景快照对不上时**放行客户端原请求**，不再中止移动。
				// 教训：早先这里硬拒，一旦装载期推导不出落点，玩家会被永久困在
				// 婚房里出不去（2026-10-04 真机）。困住玩家远比落点不精确糟糕。
				session.townPositionSnapshot = currentTownPositionSnapshot{CharacterID: 29, TownID: 42, AreaID: 0}
				wantTown, wantArea, wantX, blocked = 2, 5, 300, false
			case "missing_gate":
				// 目录里根本没有 38/1（推导不出落点）→ 同样放行客户端原请求。
				s.townCatalog, _ = dnftown.Load(context.Background(), townMoveTestSource{"town/town.lst": "2 `old.twn`", "town/old.twn": "[area]\n5 `old.map` `[normal]`\n[/area]"}, dnftown.Options{})
				wantTown, wantArea, wantX, blocked = 2, 5, 300, false
			case "unmarried_entry":
				c.Stats["town_id"], c.Stats["area_id"], c.Stats["wedding_state"] = 38, 1, 0
				body = buildTownCrossTownPortalRequest(101, 0, 900, 250, 5, 38)
				wantTown, wantArea = 38, 1
			}
			weddingExitSave(t, repos, c)
			if err := s.handleTownSetUserArea(session, body); err != nil {
				t.Fatal(err)
			}
			got := weddingExitLoad(t, repos)
			if got.Stats["town_id"] != wantTown || got.Stats["area_id"] != wantArea || got.Stats["pos_x"] != wantX {
				t.Fatalf("routing: %+v", got.Stats)
			}
			if blocked && session.conn.(*bufferConn).write.Len() != 0 {
				t.Fatal("blocked route emitted packets")
			}
		})
	}
}

// 落点由装载期从 PVF 推导（town/wedding_room_return.go），其等式本身在
// town/wedding_room_return_test.go 里断言。这里证明 bridge 取到的就是目录里那个
// 点，且确实被送到了 38/1 的婚房门旁（而不是城镇居民区）。
func TestWeddingRoomExitDoorCoordinatesFollowPVF(t *testing.T) {
	s, session, repos := newWeddingRoomExitTest(t)
	seria, found := s.townArea(38, 1)
	if !found || seria.WeddingRoomReturn == nil {
		t.Fatalf("fixture couple-room doorway missing: found=%v area=%+v", found, seria)
	}
	if got := *seria.WeddingRoomReturn; got.X != 100 || got.Y != 250 {
		t.Fatalf("couple-room doorway = %+v, want (100,250)", got)
	}
	c := weddingExitLoad(t, repos)
	c.Stats["town_id"], c.Stats["area_id"] = 101, 0
	weddingExitSave(t, repos, c)
	session.townMu.Lock()
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID: session.selectedCharacterID, TownID: 101, AreaID: 0, PositionValid: true,
	}
	session.townMu.Unlock()

	normalized, exit, err := s.normalizeCurrentWeddingRoomExit(session, c, currentTownSetUserAreaRequest{
		TownID: 1, AreaID: 1, PositionX: 300, PositionY: 200, Direction: 5,
		OpaqueU16A: 101, OpaqueU16B: 0, OpaqueU32: 0, OpaqueTailU8: 0,
	})
	if err != nil || !exit {
		t.Fatalf("normalize: exit=%v err=%v", exit, err)
	}
	if normalized.TownID != 38 || normalized.AreaID != 1 ||
		normalized.PositionX != 100 || normalized.PositionY != 250 ||
		normalized.Direction != newCharacterInitialDirection {
		t.Fatalf("normalized request = %+v", normalized)
	}
}

// 地图里没有这道门时**必须放行**，而不是拒绝 —— 绝不把玩家困在婚房里。
// 落点优先级：WeddingRoomReturn → SeriaRoomReturn → 地图自己的 [gate]。
func TestWeddingRoomExitWithoutDoorFallsBackInsteadOfBlocking(t *testing.T) {
	s, session, repos := newWeddingRoomExitTest(t)
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":  "38 `seria.twn`",
		"town/seria.twn": "[area]\n1 `Cataclysm/Town/Elvengard/new_seria_room.map` `[gate]` 450 234\n[/area]",
		"map/Cataclysm/Town/Elvengard/new_seria_room.map": "[town movable area]\n858 183 30 100 38 0\n[/town movable area]",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.townCatalog = table
	c := weddingExitLoad(t, repos)
	c.Stats["town_id"], c.Stats["area_id"] = 101, 0
	weddingExitSave(t, repos, c)
	normalized, exit, err := s.normalizeCurrentWeddingRoomExit(session, c, currentTownSetUserAreaRequest{
		TownID: 1, AreaID: 1, PositionX: 300, PositionY: 200, Direction: 5,
		OpaqueU16A: 101, OpaqueU16B: 0, OpaqueU32: 0, OpaqueTailU8: 0,
	})
	if err != nil {
		t.Fatalf("缺少门时不应报错（会把玩家困住）: %v", err)
	}
	if !exit {
		t.Fatalf("应当按回退链归一化到地图 [gate]: %+v", normalized)
	}
	// 回退链第三级：地图自己的 [gate] 450 234（该图没有 101 传送门，
	// 所以 WeddingRoomReturn/SeriaRoomReturn 都是 nil）。
	if normalized.TownID != 38 || normalized.AreaID != 1 || normalized.PositionX != 450 || normalized.PositionY != 234 {
		t.Fatalf("回退落点不正确: %+v", normalized)
	}
}

func TestWeddingRoomExitRealPVF(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	table, err := dnftown.Load(context.Background(), archive, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, session, repos := newWeddingRoomExitTest(t)
	s.townCatalog = table
	seria, found := table.FindArea(38, 1)
	if !found || seria.WeddingRoomReturn == nil {
		t.Fatalf("real PVF couple-room doorway missing: found=%v area=%+v", found, seria)
	}
	arrival := *seria.WeddingRoomReturn
	// 落点必须在这张图的每一处传送门触发矩形之外，否则客户端一落地就被送回婚房。
	for _, portal := range seria.MovableAreas {
		if portal.Width <= 0 || portal.Height <= 0 {
			continue
		}
		if arrival.X >= portal.X && arrival.X <= portal.X+portal.Width &&
			arrival.Y >= portal.Y && arrival.Y <= portal.Y+portal.Height {
			t.Fatalf("arrival would retrigger portal: %+v", portal)
		}
	}
	c := weddingExitLoad(t, repos)
	c.Stats["town_id"], c.Stats["area_id"] = 101, 0
	weddingExitSave(t, repos, c)
	if err := s.handleTownSetUserArea(session, weddingNativeExitPacket(t)); err != nil {
		t.Fatal(err)
	}
	splitTownAreaTransitionSequence(t, session.conn.(*bufferConn).write.Bytes(),
		29, 38, 1, uint16(arrival.X), uint16(arrival.Y), 5, 3)
	if err := s.handleCurrentPrevVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	// op1425 的恢复锚点必须落在城镇居民区(38/0)，不是赛丽亚房间——否则"回到上个
	// 城镇"会把玩家塞回房间里。
	origin, ok := s.currentWeddingPublicReturnOrigin(29)
	if !ok || origin.TownID != 38 || origin.AreaID != 0 || !origin.PositionValid {
		t.Fatalf("real PVF public arrival: %+v found=%v", origin, ok)
	}
	// 目录不能对外暴露可变的落点别名。
	area, _ := table.FindArea(38, 0)
	area.SeriaRoomReturn.X = 999
	if again, ok := s.currentWeddingPublicReturnOrigin(29); !ok || again.PositionX == 999 {
		t.Fatalf("mutable catalog alias: %+v", again)
	}
}
