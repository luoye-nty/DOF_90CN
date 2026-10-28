package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The selector page's 挑战深渊派对 button is greyed by op27 list0: the client's
// class0 reader (sub_1D868B0) reads a u8 count plus that many u16 slots and sets
// its hell-button flag to 0 as soon as the count is non-zero, showing 5431
// ("因存在未完成深渊派对任务的队员，无法激活深渊派对按钮。") through the notice
// manager. The list values themselves are never used, so an entry can only
// disable the button. These tests pin the projection that fills that list.
const (
	hellPartyButtonTownID           = 31
	hellPartyButtonGateAreaID       = 2
	hellPartyButtonPlainAreaID      = 1
	hellPartyButtonOrdinaryTownID   = 32
	hellPartyButtonOrdinaryGateArea = 2
	hellPartyButtonSoloCharacterID  = 19
	hellPartyButtonPartyMemberID    = 23
	hellPartyButtonHellAreaID       = 290
	hellPartyButtonOrdinaryAreaID   = 100
	hellPartyButtonHellQuestID      = 2900
	hellPartyButtonHellDungeonID    = 700
	hellPartyButtonOrdinaryDungeon  = 701
)

type hellPartyButtonFixture struct {
	service *Service
	session *gameSession
	repos   dnfrepo.Group
	table   *worldmap.Table
	conn    *bufferConn
}

// newHellPartyButtonFixture builds a world map with one hell area (290, owning
// [hell dungeon] and [hell quest] 2900) and one ordinary area (100), plus the
// two town dungeon-gate areas that lead to them.
func newHellPartyButtonFixture(t *testing.T) *hellPartyButtonFixture {
	t.Helper()
	ctx := context.Background()
	table, err := worldmap.LoadSource(ctx, hellPartyWorldMapTestSource{
		worldmap.DefaultWorldMapList: "100 `worldmap/ordinary.wdm`\n290 `worldmap/metrocenter.wdm`\n",
		"worldmap/ordinary.wdm":      "[name]\n`Ordinary`\n[dungeon]\n701 -1\n[/dungeon]\n",
		"worldmap/metrocenter.wdm": "[name]\n`Metro Center`\n[dungeon]\n700 -1\n[/dungeon]\n" +
			"[hell dungeon]\n1\n[hell quest]\n2900\n[/hell quest]\n",
	}, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	towns, err := dnftown.Load(ctx, townMoveTestSource{
		"town/town.lst": "31 `hellgate.twn` 32 `ordinarygate.twn`",
		"town/hellgate.twn": "[area]\n1 `HellGate_Plain.map` `[normal]`\n[/area]\n" +
			"[area]\n2 `HellGate_Dungeon.map` `[dungeon gate]` 290\n[/area]\n[name]\n`HellGate`",
		"town/ordinarygate.twn": "[area]\n2 `OrdinaryGate.map` `[dungeon gate]` 100\n[/area]\n[name]\n`OrdinaryGate`",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:      table,
		worldMapResolver:   resolver,
		townCatalog:        towns,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: hellPartyButtonSoloCharacterID,
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, hellPartyButtonTownID, hellPartyButtonGateAreaID, 500, 300)
	return &hellPartyButtonFixture{
		service: service,
		session: session,
		repos:   repositories,
		table:   table,
		conn:    connection,
	}
}

func (fixture *hellPartyButtonFixture) rebindAt(t *testing.T, townID, areaID byte) {
	t.Helper()
	bindDungeonSelectorOriginForTestAt(t, fixture.service, fixture.session, townID, areaID, 500, 300)
}

func (fixture *hellPartyButtonFixture) saveQuestState(t *testing.T, characterID string, questID int64, status string) {
	t.Helper()
	if err := fixture.repos.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: characterID,
		States:      map[int64]dnfrepo.QuestState{questID: {Status: status}},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *hellPartyButtonFixture) upsertPermission(t *testing.T, characterID string, areaID, questID int64) {
	t.Helper()
	if _, _, err := fixture.repos.HellPartyPermission.Upsert(context.Background(), characterID, areaID, questID); err != nil {
		t.Fatal(err)
	}
}

func (fixture *hellPartyButtonFixture) setParty(t *testing.T, leaderID uint16, members ...alignedcmd.PartyMemberState) {
	t.Helper()
	storeRuntimePartyState(fixture.session, alignedcmd.PartyState{
		PartyID:    7,
		IsLeader:   leaderID == fixture.session.selectedCharacterID,
		UserID:     leaderID,
		MaxMembers: 4,
		Members:    members,
	})
}

// TestCurrentHellPartyButtonBlockedSlotsFollowsQuestAndPermission walks the
// states a single character can be in on a hell dungeon gate.
func TestCurrentHellPartyButtonBlockedSlotsFollowsQuestAndPermission(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T, fixture *hellPartyButtonFixture)
		rebind    func(t *testing.T, fixture *hellPartyButtonFixture)
		wantSlots []uint16
	}{
		{
			name:      "unfinished hell quest blocks the solo slot",
			prepare:   func(t *testing.T, fixture *hellPartyButtonFixture) {},
			wantSlots: []uint16{0},
		},
		{
			name: "completed hell quest clears the list",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "completed")
			},
		},
		{
			name: "active hell quest still blocks",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "active")
			},
			wantSlots: []uint16{0},
		},
		{
			name: "persisted area permission clears the list",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.upsertPermission(t, "19", hellPartyButtonHellAreaID, hellPartyButtonHellQuestID)
			},
		},
		{
			name: "permission for another area does not clear this one",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.upsertPermission(t, "19", hellPartyButtonOrdinaryAreaID, hellPartyButtonHellQuestID)
			},
			wantSlots: []uint16{0},
		},
		{
			name: "completed unrelated quest does not clear the area",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.saveQuestState(t, "19", 3155, "completed")
			},
			wantSlots: []uint16{0},
		},
		{
			name: "ordinary gate area never blocks",
			rebind: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.rebindAt(t, hellPartyButtonOrdinaryTownID, hellPartyButtonOrdinaryGateArea)
			},
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {},
		},
		{
			name: "town area without a dungeon gate never blocks",
			rebind: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.rebindAt(t, hellPartyButtonTownID, hellPartyButtonPlainAreaID)
			},
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHellPartyButtonFixture(t)
			if test.rebind != nil {
				test.rebind(t, fixture)
			}
			test.prepare(t, fixture)
			got := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test")
			if !equalHellPartySlots(got, test.wantSlots) {
				t.Fatalf("blocked slots=%v want=%v", got, test.wantSlots)
			}
		})
	}
}

// TestCurrentHellPartyButtonBlockedSlotsListsOnlyUnqualifiedPartyMembers pins
// the party fan-out: the list carries the native party slots of the members who
// cannot challenge, and the leader's own slot is one of them.
func TestCurrentHellPartyButtonBlockedSlotsListsOnlyUnqualifiedPartyMembers(t *testing.T) {
	tests := []struct {
		name       string
		leaderDone bool
		memberDone bool
		wantSlots  []uint16
	}{
		{name: "member unfinished", leaderDone: true, wantSlots: []uint16{1}},
		{name: "leader unfinished", memberDone: true, wantSlots: []uint16{0}},
		{name: "both unfinished", wantSlots: []uint16{0, 1}},
		{name: "both finished", leaderDone: true, memberDone: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHellPartyButtonFixture(t)
			fixture.setParty(t, hellPartyButtonSoloCharacterID,
				alignedcmd.PartyMemberState{UserID: hellPartyButtonSoloCharacterID, Slot: 0},
				alignedcmd.PartyMemberState{UserID: hellPartyButtonPartyMemberID, Slot: 1},
			)
			if test.leaderDone {
				fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "completed")
			}
			if test.memberDone {
				fixture.saveQuestState(t, "23", hellPartyButtonHellQuestID, "completed")
			}
			got := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test")
			if !equalHellPartySlots(got, test.wantSlots) {
				t.Fatalf("blocked slots=%v want=%v", got, test.wantSlots)
			}
		})
	}
}

// TestCurrentHellPartyButtonBlockedSlotsUsesLeaderSelectorArea keeps a follower
// on the leader's page: the follower's own town snapshot may point at another
// area, and the page that is open is the leader's.
func TestCurrentHellPartyButtonBlockedSlotsUsesLeaderSelectorArea(t *testing.T) {
	fixture := newHellPartyButtonFixture(t)
	// The follower stands somewhere else; only the leader's gate is bound.
	fixture.setParty(t, hellPartyButtonPartyMemberID,
		alignedcmd.PartyMemberState{UserID: hellPartyButtonPartyMemberID, Slot: 0},
		alignedcmd.PartyMemberState{UserID: hellPartyButtonSoloCharacterID, Slot: 1},
	)
	fixture.rebindAt(t, hellPartyButtonOrdinaryTownID, hellPartyButtonOrdinaryGateArea)
	if got := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test"); got != nil {
		t.Fatalf("follower without an online leader session resolved %v, want fallback to its own area", got)
	}

	leaderConnection := &bufferConn{}
	leader := &gameSession{conn: leaderConnection, selectedCharacterID: hellPartyButtonPartyMemberID}
	bindDungeonSelectorOriginForTestAt(t, fixture.service, leader, hellPartyButtonTownID, hellPartyButtonGateAreaID, 500, 300)
	fixture.service.gameSessions = map[uint16]*gameSession{hellPartyButtonPartyMemberID: leader}

	got := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test")
	if !equalHellPartySlots(got, []uint16{0, 1}) {
		t.Fatalf("follower on the leader's hell page blocked slots=%v want=[0 1]", got)
	}
}

// TestCurrentHellPartyButtonBlockedSlotsFailsOpen guards the packet contract:
// every unavailable dependency leaves the op27 body exactly as it was before
// this projection existed (no entries), and never returns an error.
func TestCurrentHellPartyButtonBlockedSlotsFailsOpen(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(fixture *hellPartyButtonFixture)
	}{
		{
			name: "no repository provider",
			mutate: func(fixture *hellPartyButtonFixture) {
				fixture.service.repositoryProvider = nil
			},
		},
		{
			name: "no world map",
			mutate: func(fixture *hellPartyButtonFixture) {
				fixture.service.worldMapTable = nil
				fixture.service.worldMapResolver = nil
			},
		},
		{
			name: "no town catalog",
			mutate: func(fixture *hellPartyButtonFixture) {
				fixture.service.townCatalog = nil
			},
		},
		{
			name: "no selector origin",
			mutate: func(fixture *hellPartyButtonFixture) {
				fixture.session.townMu.Lock()
				fixture.session.townPositionSnapshot = currentTownPositionSnapshot{}
				fixture.session.townMu.Unlock()
			},
		},
		{
			name: "no selected character",
			mutate: func(fixture *hellPartyButtonFixture) {
				fixture.session.selectedCharacterID = 0
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHellPartyButtonFixture(t)
			test.mutate(fixture)
			if got := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test"); got != nil {
				t.Fatalf("blocked slots=%v want nil", got)
			}
		})
	}
}

// TestSendCurrentDungeonContextOp27CarriesHellBlockedSlots pins the wire body:
// 37 bytes when nobody is blocked (the historical body, byte for byte), and the
// count byte at +8 followed by one little-endian slot per blocked member.
func TestSendCurrentDungeonContextOp27CarriesHellBlockedSlots(t *testing.T) {
	t.Run("ordinary area keeps the historical body", func(t *testing.T) {
		fixture := newHellPartyButtonFixture(t)
		fixture.rebindAt(t, hellPartyButtonOrdinaryTownID, hellPartyButtonOrdinaryGateArea)
		if err := fixture.service.sendCurrentDungeonContextOp27(fixture.session, "test"); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, fixture.conn.write.Bytes())
		if packet.Header.MsgID != currentDungeonContextMsgID {
			t.Fatalf("op27 header=%+v", packet.Header)
		}
		if len(packet.Body) != 37 {
			t.Fatalf("op27 body len=%d want=37 body=%x", len(packet.Body), packet.Body)
		}
		if got := hex.EncodeToString(packet.Body); got != "00000000000000000000000000000000000000000000000000000000000000000000000000" {
			t.Fatalf("op27 body=%s", got)
		}
	})

	t.Run("blocked solo character adds one slot", func(t *testing.T) {
		fixture := newHellPartyButtonFixture(t)
		if err := fixture.service.sendCurrentDungeonContextOp27(fixture.session, "test"); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, fixture.conn.write.Bytes())
		if len(packet.Body) != 39 {
			t.Fatalf("op27 body len=%d want=39 body=%x", len(packet.Body), packet.Body)
		}
		if packet.Body[8] != 1 {
			t.Fatalf("op27 list0 count=%d want=1 body=%x", packet.Body[8], packet.Body)
		}
		if slot := binary.LittleEndian.Uint16(packet.Body[9:11]); slot != 0 {
			t.Fatalf("op27 list0 slot=%d want=0 body=%x", slot, packet.Body)
		}
		if !bytes.Equal(packet.Body[11:], make([]byte, len(packet.Body)-11)) {
			t.Fatalf("op27 tail changed after list0: %x", packet.Body[11:])
		}
	})

	t.Run("blocked member adds its party slot", func(t *testing.T) {
		fixture := newHellPartyButtonFixture(t)
		fixture.setParty(t, hellPartyButtonSoloCharacterID,
			alignedcmd.PartyMemberState{UserID: hellPartyButtonSoloCharacterID, Slot: 0},
			alignedcmd.PartyMemberState{UserID: hellPartyButtonPartyMemberID, Slot: 1},
		)
		fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "completed")
		if err := fixture.service.sendCurrentDungeonContextOp27(fixture.session, "test"); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, fixture.conn.write.Bytes())
		if len(packet.Body) != 39 || packet.Body[8] != 1 {
			t.Fatalf("op27 body=%x", packet.Body)
		}
		if slot := binary.LittleEndian.Uint16(packet.Body[9:11]); slot != 1 {
			t.Fatalf("op27 list0 slot=%d want=1 body=%x", slot, packet.Body)
		}
	})
}

// TestHellPartyButtonProjectionAgreesWithTheEntryGate is the anti-divergence
// guard: for one character and one area, the access proof used by the dungeon
// entry gate and the proof used to grey the selector button must be the same, in
// both directions. A greyed button with an accepted entry (or the reverse) is
// the regression this pins.
func TestHellPartyButtonProjectionAgreesWithTheEntryGate(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, fixture *hellPartyButtonFixture)
	}{
		{name: "nothing recorded", prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {}},
		{
			name: "quest completed",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "completed")
			},
		},
		{
			name: "quest active",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "active")
			},
		},
		{
			name: "permission row for the area",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.upsertPermission(t, "19", hellPartyButtonHellAreaID, hellPartyButtonHellQuestID)
			},
		},
		{
			name: "permission row for another area",
			prepare: func(t *testing.T, fixture *hellPartyButtonFixture) {
				fixture.upsertPermission(t, "19", hellPartyButtonOrdinaryAreaID, hellPartyButtonHellQuestID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHellPartyButtonFixture(t)
			test.prepare(t, fixture)
			entryErr := fixture.service.authorizeCurrentDungeonHellParty(
				context.Background(), fixture.session, fixture.table, hellPartyButtonHellDungeonID)
			blocked := fixture.service.currentHellPartyButtonBlockedSlots(fixture.session, "test")
			entryAllowed := entryErr == nil
			buttonEnabled := len(blocked) == 0
			if entryAllowed != buttonEnabled {
				t.Fatalf("entry allowed=%t (err=%v) but button enabled=%t (blocked=%v)",
					entryAllowed, entryErr, buttonEnabled, blocked)
			}
		})
	}
}

// TestAuthorizeCurrentDungeonHellPartyKeepsLockedSemantics re-pins the entry
// gate's own contract after the shared-predicate extraction: an area without
// [hell dungeon] and a locked area still refuse, and the error carries the area
// and quest ids for the log.
func TestAuthorizeCurrentDungeonHellPartyKeepsLockedSemantics(t *testing.T) {
	fixture := newHellPartyButtonFixture(t)
	ctx := context.Background()
	if err := fixture.service.authorizeCurrentDungeonHellParty(ctx, fixture.session, fixture.table, hellPartyButtonOrdinaryDungeon); err == nil {
		t.Fatal("ordinary area accepted a hell-party entry")
	}
	err := fixture.service.authorizeCurrentDungeonHellParty(ctx, fixture.session, fixture.table, hellPartyButtonHellDungeonID)
	if err == nil {
		t.Fatal("locked hell area accepted a hell-party entry")
	}
	message := err.Error()
	for _, want := range []string{"area=290", "character=19", "2900"} {
		if !strings.Contains(message, want) {
			t.Fatalf("locked error %q does not carry %q", message, want)
		}
	}
	fixture.saveQuestState(t, "19", hellPartyButtonHellQuestID, "completed")
	if err := fixture.service.authorizeCurrentDungeonHellParty(ctx, fixture.session, fixture.table, hellPartyButtonHellDungeonID); err != nil {
		t.Fatalf("completed hell quest refused: %v", err)
	}
}

func equalHellPartySlots(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
