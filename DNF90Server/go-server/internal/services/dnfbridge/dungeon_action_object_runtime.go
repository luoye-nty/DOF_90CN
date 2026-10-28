package dnfbridge

import (
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/actionobject"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The dungeon-symbol runtime. A room's placed passive objects run `.act`
// scripts that count live monsters, read and write run-scoped "dungeon
// symbols" and open or close the room's gates.
//
// The client runs these scripts too, but only its own room-local half: it
// posts the `[NOTICE]` and applies `[GATE ENABLE] … 0` itself (live
// 2026-09-23: entering 能源储藏室 shows 「守门将的力量关闭了能量室的大门。」 in the
// screen centre and shuts the boss door), while the symbol the cylinders add
// to never reaches 2 on its side. So the server keeps the run's symbols, and
// when a script opens a gate it sets the client's pass-gate flag with op312.
// Notices stay the client's; the server posts none.
//
// Instances live for the whole run, not per visit. The shipped scripts depend on
// that: gate_control_gate.act ships its reopen trigger disabled and has the
// closing behavior switch it on, so an object rebuilt on every room entry with
// the symbol already satisfied would never reopen anything.
//
// Every instance is evaluated on every event, whichever room the player stands
// in. Symbol writes apply at once; a gate verdict is remembered per room and
// published whenever that room is (or becomes) the loaded one.
//
// op312 must reach the room it is for. Handler sub_1D8A520 writes
// byte [map+0xBFC] = 1 (sub_2359210) on whatever map is current when it runs;
// the local character's per-frame update (0x1BC0847) calls sub_1B8E200 while
// its latch byte [char+0x17C4] is 0, and that walks the room's objects calling
// vfunc +0x248(1) on every same-team one once the map flag is set, then sets the
// latch. The latch is cleared by event 0x25 of the character's message switch
// (0x2A44160), i.e. once per room load. Sent straight after op29 the flag can
// land on the room being left: live 2026-09-23 conn game-000899 got op312
// 0.27 s before its op37 and the door stayed shut, while game-000893 with the
// identical sequence walked in. So a room-entry gate is published after the
// room's FINISH_LOADING, on every visit, since a fresh map starts with the
// flag clear.

// runtimeDungeonActionObjectMaxPasses bounds the fixpoint loop: a behavior can
// satisfy another trigger's condition, so evaluation repeats until a pass fires
// nothing. `[LIMIT]` and the trigger switches make every shipped script settle
// in two or three passes.
const runtimeDungeonActionObjectMaxPasses = 8

type runtimeDungeonActionObjectTrigger struct {
	Enabled bool
	Fired   int
}

type runtimeDungeonActionObjectInstance struct {
	ObjectID int64
	Room     runtimeDungeonRoomKey
	Script   actionobject.Script
	Triggers []runtimeDungeonActionObjectTrigger
	// Destroyed records `[DESTROY]`: the object stops evaluating.
	Destroyed bool
}

type runtimeDungeonActionObjectState struct {
	Symbols   map[string]int64
	Instances []*runtimeDungeonActionObjectInstance
	// instantiated marks rooms whose objects are already built.
	instantiated map[runtimeDungeonRoomKey]bool
	// gateOpen is the last `[GATE ENABLE]` verdict per room; absent means the
	// scripts never touched that room's gates.
	gateOpen map[runtimeDungeonRoomKey]bool
	// gatePublished is the room whose open verdict already went to the wire
	// for the current visit, so a re-evaluation does not resend it.
	gatePublished map[runtimeDungeonRoomKey]bool
}

func newRuntimeDungeonActionObjectState() *runtimeDungeonActionObjectState {
	return &runtimeDungeonActionObjectState{
		Symbols:       make(map[string]int64),
		instantiated:  make(map[runtimeDungeonRoomKey]bool),
		gateOpen:      make(map[runtimeDungeonRoomKey]bool),
		gatePublished: make(map[runtimeDungeonRoomKey]bool),
	}
}

func (runtime *runtimeDungeonState) actionObjectState() *runtimeDungeonActionObjectState {
	if runtime.actionObjects == nil {
		runtime.actionObjects = newRuntimeDungeonActionObjectState()
	}
	return runtime.actionObjects
}

// runtimeDungeonActionObjectEffects is what one evaluation wants on the wire.
type runtimeDungeonActionObjectEffects struct {
	Notices  []actionobject.Operation
	Fired    int
	Symbols  []string
	GateOpen *bool
}

// instantiateActionObjectsLocked builds the instances for a room the first time
// it is seen. Objects without a driving script are skipped, so most rooms add
// nothing.
func (s *Service) instantiateActionObjectsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) {
	state := runtime.actionObjectState()
	key := runtimeDungeonRoomKeyFromScene(scene)
	if state.instantiated[key] {
		return
	}
	state.instantiated[key] = true
	catalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		s.logGameEvent(session, "game-dungeon-action-object-catalog-unavailable",
			"dungeon_id", runtime.Dungeon.ID, "room", scene.Coordinate.String(), "error", err)
		return
	}
	for _, object := range scene.PassiveObjects {
		script, found, err := catalog.passiveObjectActionScript(object.ObjectID)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-action-object-script-failed",
				"dungeon_id", runtime.Dungeon.ID, "room", scene.Coordinate.String(),
				"object_id", object.ObjectID, "error", err)
			continue
		}
		if !found {
			continue
		}
		instance := &runtimeDungeonActionObjectInstance{
			ObjectID: object.ObjectID,
			Room:     key,
			Script:   script,
			Triggers: make([]runtimeDungeonActionObjectTrigger, len(script.Triggers)),
		}
		unsupported := make([]string, 0)
		for index, trigger := range script.Triggers {
			instance.Triggers[index].Enabled = trigger.Enabled
			unsupported = append(unsupported, trigger.Unsupported...)
		}
		for _, behavior := range script.Behaviors {
			unsupported = append(unsupported, behavior.Unsupported...)
		}
		state.Instances = append(state.Instances, instance)
		s.logGameEvent(session, "game-dungeon-action-object-instantiated",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"object_id", object.ObjectID,
			"script", script.Path,
			"triggers", len(script.Triggers),
			"behaviors", len(script.Behaviors),
			"unsupported", strings.Join(unsupported, ","))
	}
}

// liveMonsterCount answers `[WHICH][MONSTER][CHECKUP][IS INDEX]` for one room:
// how many placed monsters of that index have not been reported dead.
func runtimeDungeonActionObjectLiveMonsterCount(
	runtime *runtimeDungeonState,
	key runtimeDungeonRoomKey,
	monsterID int64,
) int64 {
	var room *runtimeDungeonRoom
	if runtime.Room != nil {
		if snapshot := runtime.Room.Snapshot(); snapshot.MapID == key.MapID &&
			snapshot.Coordinate.X == key.X && snapshot.Coordinate.Y == key.Y {
			room = runtime.Room
		}
	}
	if room == nil {
		if visit, ok := runtime.Rooms[key]; ok && visit != nil {
			room = visit.Room
		}
	}
	if room == nil {
		return 0
	}
	count := int64(0)
	for _, monster := range room.Snapshot().Monsters {
		if monster.Spawn.MonsterID == monsterID && monster.State != runtimeDungeonMonsterDefeated {
			count++
		}
	}
	return count
}

func (state *runtimeDungeonActionObjectState) conditionHolds(
	runtime *runtimeDungeonState,
	instance *runtimeDungeonActionObjectInstance,
	condition actionobject.Condition,
) bool {
	switch condition.Kind {
	case actionobject.ConditionDungeonSymbol:
		return condition.Op.Satisfied(state.Symbols[condition.Symbol], condition.Value)
	case actionobject.ConditionMonsterCount:
		count := runtimeDungeonActionObjectLiveMonsterCount(runtime, instance.Room, condition.MonsterID)
		return condition.Op.Satisfied(count, condition.Value)
	default:
		return false
	}
}

// evaluateActionObjectsLocked runs every instance to a fixpoint and returns the
// effects that concern the current room.
func (s *Service) evaluateActionObjectsLocked(
	runtime *runtimeDungeonState,
	current runtimeDungeonRoomKey,
) runtimeDungeonActionObjectEffects {
	state := runtime.actionObjectState()
	effects := runtimeDungeonActionObjectEffects{}
	touched := make(map[string]struct{})
	for pass := 0; pass < runtimeDungeonActionObjectMaxPasses; pass++ {
		firedThisPass := 0
		for _, instance := range state.Instances {
			if instance.Destroyed {
				continue
			}
			for index, trigger := range instance.Script.Triggers {
				runtimeTrigger := &instance.Triggers[index]
				if !runtimeTrigger.Enabled || (trigger.Limit > 0 && runtimeTrigger.Fired >= trigger.Limit) {
					continue
				}
				if trigger.Behavior < 0 || trigger.Behavior >= len(instance.Script.Behaviors) {
					continue
				}
				satisfied := true
				for _, condition := range trigger.Conditions {
					if !state.conditionHolds(runtime, instance, condition) {
						satisfied = false
						break
					}
				}
				if !satisfied {
					continue
				}
				runtimeTrigger.Fired++
				firedThisPass++
				state.runBehavior(instance, instance.Script.Behaviors[trigger.Behavior], current, &effects, touched)
				if instance.Destroyed {
					break
				}
			}
		}
		effects.Fired += firedThisPass
		if firedThisPass == 0 {
			break
		}
	}
	for symbol := range touched {
		effects.Symbols = append(effects.Symbols, symbol)
	}
	sort.Strings(effects.Symbols)
	if open, known := state.gateOpen[current]; known {
		value := open
		effects.GateOpen = &value
	}
	return effects
}

func (state *runtimeDungeonActionObjectState) runBehavior(
	instance *runtimeDungeonActionObjectInstance,
	behavior actionobject.Behavior,
	current runtimeDungeonRoomKey,
	effects *runtimeDungeonActionObjectEffects,
	touched map[string]struct{},
) {
	for _, operation := range behavior.Operations {
		switch operation.Kind {
		case actionobject.OperationSymbolCalc:
			switch operation.Calc {
			case "-":
				state.Symbols[operation.Symbol] -= operation.Value
			default:
				state.Symbols[operation.Symbol] += operation.Value
			}
			touched[operation.Symbol] = struct{}{}
		case actionobject.OperationSymbolSet:
			state.Symbols[operation.Symbol] = operation.Value
			touched[operation.Symbol] = struct{}{}
		case actionobject.OperationGateEnable:
			state.gateOpen[instance.Room] = operation.GateEnabled
			if !operation.GateEnabled {
				// A re-close lets a later open publish again.
				delete(state.gatePublished, instance.Room)
			}
		case actionobject.OperationNotice, actionobject.OperationCenterMessage:
			if instance.Room == current {
				effects.Notices = append(effects.Notices, operation)
			}
		case actionobject.OperationTriggerSwitch:
			if operation.TriggerIndex >= 0 && operation.TriggerIndex < len(instance.Triggers) {
				instance.Triggers[operation.TriggerIndex].Enabled = operation.TriggerOn
			}
		case actionobject.OperationDestroy:
			instance.Destroyed = true
			return
		}
	}
}

// runtimeDungeonActionObjectEvent says whether the event happened inside an
// already-loaded room (a death) or is a room being entered (op29 just sent,
// the client's op37 still to come).
type runtimeDungeonActionObjectEvent byte

const (
	runtimeDungeonActionObjectRoomEntered runtimeDungeonActionObjectEvent = iota
	runtimeDungeonActionObjectInRoom
)

// runCurrentDungeonActionObjectsLocked is the one evaluation entry point: it
// instantiates the scene's objects if needed and evaluates everything. A gate
// the current room is owed goes out at once only for an in-room event; a room
// entry leaves it to publishCurrentDungeonActionObjectGateAfterFinishLoading.
// Caller holds session.dungeon.mu. Presentation failures are logged, never
// returned: the gameplay event that got us here is done.
func (s *Service) runCurrentDungeonActionObjectsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	event runtimeDungeonActionObjectEvent,
	source string,
) {
	if s == nil || session == nil || runtime == nil || runtime.Room == nil || len(scene.PassiveObjects) == 0 &&
		(runtime.actionObjects == nil || len(runtime.actionObjects.Instances) == 0) {
		return
	}
	s.instantiateActionObjectsLocked(session, runtime, scene)
	state := runtime.actionObjectState()
	if len(state.Instances) == 0 {
		return
	}
	current := runtimeDungeonRoomKeyFromScene(scene)
	if event == runtimeDungeonActionObjectRoomEntered {
		// A new visit is a new client map with its pass-gate flag clear.
		delete(state.gatePublished, current)
	}
	effects := s.evaluateActionObjectsLocked(runtime, current)

	gatePublished := false
	if event == runtimeDungeonActionObjectInRoom {
		gatePublished = s.publishCurrentDungeonActionObjectGateLocked(session, runtime, scene, source)
	}

	if effects.Fired == 0 && !gatePublished {
		return
	}
	symbols := make([]string, 0, len(state.Symbols))
	for name, value := range state.Symbols {
		symbols = append(symbols, name+"="+strconv.FormatInt(value, 10))
	}
	sort.Strings(symbols)
	gate := "untouched"
	if effects.GateOpen != nil {
		gate = "closed"
		if *effects.GateOpen {
			gate = "open"
		}
	}
	s.logGameEvent(session, "game-dungeon-action-object-evaluated",
		"source", source,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"triggers_fired", effects.Fired,
		"symbols_touched", strings.Join(effects.Symbols, ","),
		"symbols", strings.Join(symbols, ","),
		"room_gate", gate,
		"gate_published", gatePublished,
		"client_notices", len(effects.Notices),
		"instances", len(state.Instances))
}

// publishCurrentDungeonActionObjectGateLocked sends op312 when the loaded room's
// scripts say its gate is open and this visit has not had it yet. Caller holds
// session.dungeon.mu.
func (s *Service) publishCurrentDungeonActionObjectGateLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	source string,
) bool {
	if runtime == nil || runtime.actionObjects == nil {
		return false
	}
	state := runtime.actionObjects
	current := runtimeDungeonRoomKeyFromScene(scene)
	if !state.gateOpen[current] || state.gatePublished[current] {
		return false
	}
	// op312 COMPLETE_CONDITION_PASS_GATE is the pass-gate flag the boss-entrance
	// and trap-event conditions already set; see the file comment for how the
	// client turns it into open doors. It carries no direction: an opened gate
	// is the room's boss door, which every shipped `[GATE ENABLE] … 1` is.
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonCompleteConditionPassGateMsgID,
		buildCurrentDungeonCompleteConditionPassGateBody(),
		0,
	); err != nil {
		s.logGameEvent(session, "game-dungeon-action-object-gate-publish-failed",
			"source", source, "dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(), "error", err)
		return false
	}
	state.gatePublished[current] = true
	return true
}

// publishCurrentDungeonActionObjectGateAfterFinishLoading is the room-entry half
// of the gate: it runs once the client has reported the new room loaded, so the
// op312 flag lands on that room's map and not the one just left.
func (s *Service) publishCurrentDungeonActionObjectGateAfterFinishLoading(session *gameSession, source string) {
	if s == nil || session == nil {
		return
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.actionObjects == nil {
		return
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return
	}
	if s.publishCurrentDungeonActionObjectGateLocked(session, runtime, scene, source) {
		s.logGameEvent(session, "game-dungeon-action-object-gate-published",
			"source", source,
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"response_opcode", currentDungeonCompleteConditionPassGateMsgID)
	}
}
