package dnfbridge

import (
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// op1460's per-row handler (0x009AF190) rejects any key outside 81..125
// (`lea ecx,[eax-0x51]` / `cmp ecx,0x2c` / `ja default`), and those ids are the
// Fiend War state symbols from `etc/raid/fiendwar/fiendwar.etc.symbol`:
// 81 radar / 82 battery / 83..88 outposts and 兵站本营 / 92..96 boss state,
// mechanism digits and boss HP / 110,111 the BOSS display HP.
//
// Before this the board only routed `key >= 121` to 1460, so 81..111 travelled
// on op583 and the client read none of them: the radar/battery effects, the boss
// state and the boss HP bar were all blank.
func TestFiendWarBoardRoutesTheStateSymbolsOnOp1460(t *testing.T) {
	now := time.Unix(1700000000, 0)
	const (
		areaStateKey   = 79  // last [n AREA STATE] symbol -- stays on op583
		radarStateKey  = 81  // 雷达
		batteryKey     = 82  // 炮塔
		commandCenter  = 88  // 南部兵站本营
		patternState   = 94  // BOSS 机制位
		bossViewHPKey  = 110 // TOP BOSS VIEW HP
		movieCenterKey = 125 // MOVIE COMMAND CENTER ON
		outOfRangeKey  = 126 // outside the client's window, for either opcode
	)
	b := &raid.FiendWarBattle{
		Config:        &raid.FiendWarBattleConfig{WaitingArea: 2, Areas: map[int]raid.FiendWarArea{}},
		Areas:         map[int]raid.FiendWarAreaState{},
		GroupAreas:    map[byte]int{1: 14},
		MemberAreas:   map[uint16]int{1: 14},
		RecoveryUntil: map[uint16]time.Time{},
		Symbols: map[uint32]int{
			areaStateKey:  1,
			radarStateKey: 1,
			batteryKey:    1,
			commandCenter: 1,
			patternState:  1111,
			bossViewHPKey: 100,
			outOfRangeKey: 7,
		},
	}
	state := runtimeRaidState{
		RaidKey:       1,
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1}},
		Lifecycle:     runtimeRaidLifecycleState{FiendWar: b},
	}

	nativeRows := func(packets []runtimeRaidWireNotification) map[uint32]uint32 {
		for _, packet := range packets {
			if packet.msgID != 1460 {
				continue
			}
			rows := make(map[uint32]uint32)
			for offset := 1; offset+8 <= len(packet.body); offset += 8 {
				rows[binary.LittleEndian.Uint32(packet.body[offset:])] = binary.LittleEndian.Uint32(packet.body[offset+4:])
			}
			return rows
		}
		return nil
	}
	symbolRows := func(packets []runtimeRaidWireNotification) map[uint32]uint32 {
		for _, packet := range packets {
			if packet.msgID != raid.RaidSymbolMsgID {
				continue
			}
			rows := make(map[uint32]uint32)
			for offset := 1; offset+8 <= len(packet.body); offset += 8 {
				rows[binary.LittleEndian.Uint32(packet.body[offset:])] = binary.LittleEndian.Uint32(packet.body[offset+4:])
			}
			return rows
		}
		return nil
	}

	// Frame 1: the whole board is new, so the state symbols must be present.
	first, err := runtimeFiendWarBoardPackets(state, runtimeRaidState{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	native := nativeRows(first)
	if native == nil {
		t.Fatal("no op1460 packet was built")
	}
	for key, want := range map[uint32]uint32{
		radarStateKey: 1,
		batteryKey:    1,
		commandCenter: 1,
		patternState:  1111,
		bossViewHPKey: 100,
	} {
		got, ok := native[key]
		if !ok {
			t.Errorf("symbol %d was not routed to op1460 (the client drops it on op583)", key)
			continue
		}
		if got != want {
			t.Errorf("symbol %d on op1460 = %d, want %d", key, got, want)
		}
	}
	for _, key := range []uint32{areaStateKey, outOfRangeKey} {
		if _, ok := native[key]; ok {
			t.Errorf("symbol %d must stay off op1460 (outside the client's 81..125 window)", key)
		}
	}
	// The legacy op583 route is untouched, and this change is deliberately
	// ADDITIVE: 81..120 ride both opcodes for now, so nothing that used to work
	// can regress while the new route is being proven on the live client.
	// Trimming op583 to `key < 81` is a later, separate experiment.
	symbols := symbolRows(first)
	if symbols == nil {
		t.Fatal("no op583 symbol table was built")
	}
	if _, ok := symbols[areaStateKey]; !ok {
		t.Errorf("symbol %d must still be carried by op583", areaStateKey)
	}
	if _, ok := symbols[radarStateKey]; !ok {
		t.Errorf("symbol %d stays on op583 while the op1460 route is unproven", radarStateKey)
	}
	// 121..125 were never on op583 and must not become op583 traffic.
	if _, ok := symbols[movieCenterKey]; ok {
		t.Errorf("symbol %d is op1460-only traffic and must not ride op583", movieCenterKey)
	}

	// Frame 2: the 121..125 one-shots keep their "first frame, non-zero only"
	// rule, so they appear when they change and not before.
	previous := cloneRuntimeRaidState(state)
	b.Symbols[movieCenterKey] = 1
	second, err := runtimeFiendWarBoardPackets(state, previous, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := nativeRows(second)[movieCenterKey]; got != 1 {
		t.Errorf("symbol %d after its change = %d, want 1 routed to op1460", movieCenterKey, got)
	}
	if _, ok := nativeRows(first)[movieCenterKey]; ok {
		t.Errorf("symbol %d must not be replayed on the first frame while zero", movieCenterKey)
	}
}

// In single mode the client must receive one selectable BOSS and one terminal
// BOSS marker. Publishing zero for the absent side is not enough after the
// PVF has opened both sides: zero is the pre-gate/default value, while state 10
// is the side's finished state. The lower BOSS dungeon is also hidden.
func TestFiendWarSingleBoardPublishesOnlyTheSelectedBossAsSelectable(t *testing.T) {
	const (
		topBossState   = 92
		botBossState   = 93
		topBossDungeon = 4978
		botBossDungeon = 4953
	)
	b := &raid.FiendWarBattle{
		Config: &raid.FiendWarBattleConfig{
			WaitingArea: 2,
			Areas: map[int]raid.FiendWarArea{
				2:  {Index: 2, Symbol: 50},
				30: {Index: 30, Dungeon: topBossDungeon, Kind: "top boss", Symbol: 78},
				31: {Index: 31, Dungeon: botBossDungeon, Kind: "bot boss", Symbol: 79},
			},
		},
		Symbols: map[uint32]int{
			50:           0,
			78:           1,
			79:           1,
			topBossState: 1,
			botBossState: 10,
		},
		Areas: map[int]raid.FiendWarAreaState{
			2: {}, 30: {}, 31: {},
		},
		GroupAreas:    map[byte]int{1: 2},
		MemberAreas:   map[uint16]int{1: 2},
		RecoveryUntil: map[uint16]time.Time{},
		Single: &raid.FiendWarSingleBattle{
			BossArea:     30,
			AllowedAreas: map[int]bool{2: true, 30: true},
		},
	}
	state := runtimeRaidState{
		RaidKey: 1,
		Lifecycle: runtimeRaidLifecycleState{
			FiendWar: b,
			Objectives: map[int]runtimeRaidDungeonObjective{
				topBossDungeon: {MapID: topBossDungeon, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
				botBossDungeon: {MapID: botBossDungeon, State: raid.AntonDungeonHide, MaxHP: 1, RemainingHP: 1},
			},
		},
	}
	packets, err := runtimeFiendWarBoardPackets(state, runtimeRaidState{}, 1, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}

	var native map[uint32]uint32
	var dungeonStates map[uint32]uint32
	var dungeonBody []byte
	for _, packet := range packets {
		switch packet.msgID {
		case 1460:
			native = make(map[uint32]uint32)
			for offset := 1; offset+8 <= len(packet.body); offset += 8 {
				native[binary.LittleEndian.Uint32(packet.body[offset:])] = binary.LittleEndian.Uint32(packet.body[offset+4:])
			}
		case raid.RaidDungeonStateMsgID:
			dungeonBody = append([]byte(nil), packet.body...)
			dungeonStates = make(map[uint32]uint32)
			count := int(packet.body[1])
			for index := 0; index < count; index++ {
				offset := 2 + index*5
				dungeonStates[binary.LittleEndian.Uint32(packet.body[offset:])] = uint32(packet.body[offset+4])
			}
		}
	}
	if native[topBossState] != 1 || native[botBossState] != 10 {
		t.Fatalf("boss state symbols: top=%d bot=%d, want 1/10", native[topBossState], native[botBossState])
	}
	if dungeonStates[topBossDungeon] != runtimeRaidObjectiveWireState(raid.AntonDungeonOpen) ||
		dungeonStates[botBossDungeon] != runtimeRaidObjectiveWireState(raid.AntonDungeonHide) {
		t.Fatalf("boss dungeon states: top=%d bot=%d, want open/hide; body=%x",
			dungeonStates[topBossDungeon], dungeonStates[botBossDungeon], dungeonBody)
	}
}
