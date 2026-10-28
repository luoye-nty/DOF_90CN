package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/actionobject"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The three 能源储藏室 scripts, verbatim in shape (see actionobject tests for
// the transcription). The runtime is exercised through them because their
// trigger-switch choreography is exactly what a per-visit model gets wrong.
const (
	testEnergySaveGateScript = "" +
		"[TRIGGER]\n[LIMIT]\n1\n[ENABLE]\n[ON]\n" +
		"[DUNGEON SYMBOL COMPARE]\n`storage gate open`\n[<]\n2\n[DO BEHAVIOR]\n[ME]\n0\n[/TRIGGER]\n" +
		"[BEHAVIOR]\n[NOTICE]\n`gate closed` 255 255 0 `None` 4000 1000\n[/NOTICE]\n" +
		"[GATE ENABLE]\n-1 -1 0\n[DOWN]\n[/GATE ENABLE]\n[TRIGGER]\n1\n[ON]\n[TRIGGER]\n0\n[OFF]\n[/BEHAVIOR]\n" +
		"[TRIGGER]\n[LIMIT]\n1\n[ENABLE]\n[OFF]\n" +
		"[DUNGEON SYMBOL COMPARE]\n`storage gate open`\n[>=]\n2\n[DO BEHAVIOR]\n[ME]\n1\n[/TRIGGER]\n" +
		"[BEHAVIOR]\n[NOTICE]\n`gate open` 255 255 0 `None` 4000 1000\n[/NOTICE]\n" +
		"[GATE ENABLE]\n-1 -1 1\n[DOWN]\n[/GATE ENABLE]\n[/BEHAVIOR]\n"
	testEnergySaveCylinderScript = "" +
		"[TRIGGER]\n[LIMIT]\n1\n[WHICH]\n[MONSTER]\n[CHECKUP]\n[IS INDEX]\n%d\n[/IS INDEX]\n" +
		"[CHECKED NO]\n[<]\n1\n[/CHECKUP]\n[DO BEHAVIOR]\n[ME]\n0\n[/TRIGGER]\n" +
		"[BEHAVIOR]\n[DUNGEON SYMBOL CALC]\n`storage gate open`\n[+]\n1\n[/BEHAVIOR]\n"
)

func testActionObjectScript(t *testing.T, name, text string) actionobject.Script {
	t.Helper()
	document, err := dnfpvf.Parse(name, text)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	script, err := actionobject.Parse(document)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return script
}

func testActionObjectRoom(x, y int64, mapID int64, monsterID int64) *runtimeDungeonRoom {
	room := &runtimeDungeonRoom{
		coordinate:  worldmap.RoomCoordinate{X: x, Y: y},
		mapID:       mapID,
		byObjectKey: make(map[uint32]int),
	}
	if monsterID != 0 {
		room.monsters = append(room.monsters, runtimeDungeonMonster{
			ObjectKey: 1,
			Spawn:     worldmap.MonsterSpawn{MonsterID: monsterID},
			State:     runtimeDungeonMonsterAnnounced,
		})
		room.byObjectKey[1] = 0
	}
	return room
}

func testActionObjectInstance(objectID int64, key runtimeDungeonRoomKey, script actionobject.Script) *runtimeDungeonActionObjectInstance {
	instance := &runtimeDungeonActionObjectInstance{
		ObjectID: objectID,
		Room:     key,
		Script:   script,
		Triggers: make([]runtimeDungeonActionObjectTrigger, len(script.Triggers)),
	}
	for index, trigger := range script.Triggers {
		instance.Triggers[index].Enabled = trigger.Enabled
	}
	return instance
}

// TestActionObjectsOpenEnergySaveBossDoorAfterBothCylinders walks the room
// order a squad actually takes: start room (door closes), left room kill,
// back through start, right room kill, back to start (door opens).
func TestActionObjectsOpenEnergySaveBossDoorAfterBothCylinders(t *testing.T) {
	s := &Service{}
	start := runtimeDungeonRoomKey{X: 1, Y: 0, MapID: 74249}
	left := runtimeDungeonRoomKey{X: 0, Y: 0, MapID: 74248}
	right := runtimeDungeonRoomKey{X: 2, Y: 0, MapID: 74250}

	startRoom := testActionObjectRoom(1, 0, 74249, 0)
	leftRoom := testActionObjectRoom(0, 0, 74248, 57156)
	rightRoom := testActionObjectRoom(2, 0, 74250, 56068)
	runtime := &runtimeDungeonState{
		Room: startRoom,
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			start: {Room: startRoom},
			left:  {Room: leftRoom},
			right: {Room: rightRoom},
		},
	}
	state := runtime.actionObjectState()
	state.Instances = []*runtimeDungeonActionObjectInstance{
		testActionObjectInstance(64308, start, testActionObjectScript(t, "gate", testEnergySaveGateScript)),
		testActionObjectInstance(64307, left, testActionObjectScript(t, "left", sprintfScript(testEnergySaveCylinderScript, 57156))),
		testActionObjectInstance(64306, right, testActionObjectScript(t, "right", sprintfScript(testEnergySaveCylinderScript, 56068))),
	}

	// Entering the start room: symbol 0 < 2, the door closes and the reopen
	// trigger is armed.
	effects := s.evaluateActionObjectsLocked(runtime, start)
	if effects.GateOpen == nil || *effects.GateOpen {
		t.Fatalf("start room gate after first entry = %v want closed", effects.GateOpen)
	}
	if len(effects.Notices) != 1 || effects.Notices[0].Text != "gate closed" {
		t.Fatalf("first-entry notices = %+v", effects.Notices)
	}
	if !state.Instances[0].Triggers[1].Enabled || state.Instances[0].Triggers[0].Enabled {
		t.Fatalf("gate triggers after close = %+v", state.Instances[0].Triggers)
	}

	// Left room: kill 57156. The cylinder counts it and the symbol reaches 1.
	runtime.Room = leftRoom
	leftRoom.monsters[0].State = runtimeDungeonMonsterDefeated
	effects = s.evaluateActionObjectsLocked(runtime, left)
	if state.Symbols["storage gate open"] != 1 {
		t.Fatalf("symbol after left kill = %d want 1", state.Symbols["storage gate open"])
	}
	if effects.GateOpen != nil {
		t.Fatalf("left room reported a gate verdict %v; it has no gate script", *effects.GateOpen)
	}

	// Back through the start room: still short, nothing new fires, door stays shut.
	runtime.Room = startRoom
	effects = s.evaluateActionObjectsLocked(runtime, start)
	if effects.Fired != 0 || effects.GateOpen == nil || *effects.GateOpen {
		t.Fatalf("start room mid-way: fired=%d gate=%v", effects.Fired, effects.GateOpen)
	}

	// Right room: kill 56068 -> symbol 2. The gate object is not in this room,
	// but it is evaluated anyway and flips its own room's gate open now.
	runtime.Room = rightRoom
	rightRoom.monsters[0].State = runtimeDungeonMonsterDefeated
	effects = s.evaluateActionObjectsLocked(runtime, right)
	if state.Symbols["storage gate open"] != 2 {
		t.Fatalf("symbol after right kill = %d want 2", state.Symbols["storage gate open"])
	}
	if len(effects.Notices) != 0 {
		t.Fatalf("the opening notice leaked into the right room: %+v", effects.Notices)
	}
	if !state.gateOpen[start] {
		t.Fatal("start room gate did not open once the symbol reached 2")
	}

	// Walking back into the start room publishes the open verdict.
	runtime.Room = startRoom
	effects = s.evaluateActionObjectsLocked(runtime, start)
	if effects.GateOpen == nil || !*effects.GateOpen {
		t.Fatalf("start room final gate = %v want open", effects.GateOpen)
	}
	// [LIMIT] 1 on both triggers: nothing fires again.
	if again := s.evaluateActionObjectsLocked(runtime, start); again.Fired != 0 {
		t.Fatalf("triggers fired past their limit: %+v", again)
	}
}

// TestActionObjectsReopenSurvivesLeavingBeforeSymbolIsMet is the ordering a
// per-visit object model breaks: the squad leaves the start room with the
// reopen trigger armed, the symbol is completed elsewhere, and the door must
// still open on return even though the closing trigger no longer matches.
func TestActionObjectsReopenSurvivesLeavingBeforeSymbolIsMet(t *testing.T) {
	s := &Service{}
	start := runtimeDungeonRoomKey{X: 1, Y: 0, MapID: 74249}
	startRoom := testActionObjectRoom(1, 0, 74249, 0)
	runtime := &runtimeDungeonState{
		Room:  startRoom,
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{start: {Room: startRoom}},
	}
	state := runtime.actionObjectState()
	state.Instances = []*runtimeDungeonActionObjectInstance{
		testActionObjectInstance(64308, start, testActionObjectScript(t, "gate", testEnergySaveGateScript)),
	}
	s.evaluateActionObjectsLocked(runtime, start)
	// Both cylinders count in while the squad is elsewhere.
	state.Symbols["storage gate open"] = 2
	effects := s.evaluateActionObjectsLocked(runtime, start)
	if effects.GateOpen == nil || !*effects.GateOpen {
		t.Fatalf("gate after symbol met = %v want open", effects.GateOpen)
	}
	if len(effects.Notices) != 1 || effects.Notices[0].Text != "gate open" {
		t.Fatalf("reopen notices = %+v", effects.Notices)
	}
}

func sprintfScript(format string, monsterID int64) string {
	out := make([]byte, 0, len(format)+8)
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) && format[i+1] == 'd' {
			out = append(out, []byte(itoaTest(monsterID))...)
			i++
			continue
		}
		out = append(out, format[i])
	}
	return string(out)
}

func itoaTest(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
