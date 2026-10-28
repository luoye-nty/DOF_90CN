package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/party"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// runtimePartyDungeonOwner is the process-local owner for one admitted party
// dungeon. Admitted members retain their native fixed slots for the lifetime
// of the instance. A member that confirms a mid-run town return is removed
// from the roster without renumbering survivors; otherwise room loading,
// shared deaths and object-key allocation keep waiting on a runtime that no
// longer exists.
//
// Connection writers deliberately do not live here. Every delivery resolves
// the current game-session generation before entering that session's mailbox.
type runtimePartyDungeonOwner struct {
	mu      sync.Mutex
	worldMu sync.Mutex

	partyID    uint16
	leaderID   uint16
	leaderSlot byte
	members    []runtimePartyDungeonMember
	// entryMemberCount 是进入副本那一刻的队伍人数（2..4）。它冻结"本次进图
	// 是不是双人队"，因为婚姻通关判定只认正好两人的普通副本；用 owner.members
	// 的当前长度会在中途退队/补人后得出错误结论。
	entryMemberCount int
	// weddingClearKnown / weddingClearCouple 在第一次权威通关时冻结双人婚姻
	// 判定，避免先回城的那一方把自己的状态改掉、让另一方的结果失效。随 owner
	// 生命周期回收（dungeon_runtime_state.go 里把 partyDungeonOwner 置 nil）。
	weddingClearKnown, weddingClearCouple bool

	loadingCycle uint64
	readySlots   uint8
	deadSlots    uint8
	// fiendWarReviveCount 是超时空一次区域尝试里整队已用掉的复活币数。额度是
	// 小队共用的 —— 一起打同一块区域的成员各自的副本实例读的是同一个计数 ——
	// 所以它挂在 party owner 上，而不是单个会话上。与复活币事务同受 worldMu 保护。
	fiendWarReviveCount int64
	released            bool
	retired             bool

	// loadingReleasedMask / loadingReleasedCycle / loadingReleasedLeader 记录
	// "本次读条周期里，谁已经把自己的放行链写完了"。
	//
	// 为什么要它：旧实现是"最后一个交 finish-loading 的人**同步**替全队放行"，
	// 替别人做就要等别人的 event loop，而别人的 loop 可能正停在它自己的等待里 ——
	// 实测 20 人攻坚时发起者的 loop 被停满 10 秒（partyFanoutBudget），那 10 秒里
	// 它一个包都处理不了，玩家看到的就是卡顿/读条不动。改成"投递 + 全员完成后才
	// 投递队长"之后没有任何 loop 等待另一个 loop，而这些位图让"谁已完成"成为可
	// 观测的数据（与参照实现的 LoadingReadyUserIds 同源）。
	loadingReleasedMask   uint8
	loadingReleasedCycle  uint64
	loadingReleasedLeader bool

	actorProjectionOnce sync.Once
	actorProjection     runtimePartyDungeonActorProjection
	actorProjectionErr  error

	// op35's free rewards are indexed by the same fixed member slots as
	// native names/portraits. Freeze one public table before any result UI.
	cardRewards             [3][currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair
	cardRewardReadySlots    uint8
	cardRewardDepartedSlots uint8
	cardRewardsFrozen       bool
	cardScreenOwners        [dungeonCardSideCount][dungeonCardSlotsPerSide]uint16

	// entryPreparedSlots is the initial-room prepare barrier. Every member must
	// receive the complete op23/op2/op11/op3/op26/op28 prelude before any
	// connection receives op800/op29. NoPack rebuilds the reliable-UDP actor
	// routes during this prelude; letting the leader enter the room first leaves
	// the follower route only partially initialized and drops durable actor
	// mutations such as riding/transform state.
	entryPreparedSlots uint8
	entryRoomReleased  bool
	entryContinuations map[uint16]runtimePartyDungeonEntryContinuation

	// worldRooms is the authoritative party-wide room/actor ledger. Every
	// member still owns a connection-local runtime for inventory, EXP and drop
	// projection, but room topology and actor lifecycle are committed here once.
	worldRevision uint64
	worldRooms    map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom
	worldDeaths   map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact
}

type runtimePartyDungeonSharedActor struct {
	kind      string
	reference worldmap.HostileReference
	hasRef    bool
	code      uint32
	state     runtimeDungeonMonsterState
}

type runtimePartyDungeonSharedRoom struct {
	seed     uint32
	actors   map[uint32]runtimePartyDungeonSharedActor
	cleared  bool
	revision uint64
}

type runtimePartyDungeonDeathKey struct {
	room      runtimeDungeonRoomKey
	objectKey uint32
}

// runtimePartyDungeonDeathFact is connection-neutral. The original op39 is
// retained only as current-EXE evidence; its owner object key is rewritten for
// each recipient before that member materializes private EXP/drop effects.
type runtimePartyDungeonDeathFact struct {
	revision  uint64
	key       runtimePartyDungeonDeathKey
	request   []byte
	consumers uint8
}

type runtimePartyDungeonEntryContinuation struct {
	session                 *gameSession
	characterID             uint16
	generation              uint64
	runtime                 *runtimeDungeonState
	scene                   worldmap.DungeonRoomScene
	actorSnapshot           []byte
	randomizedCreatureQueue []byte
	randomizedCreatureCount int
	startMap                []byte
	passiveDropPlan         currentDungeonPassiveObjectDropPlan
	requestBody             []byte
}

// runtimePartyDungeonNextObjectKey returns the party-wide object-key
// high-water mark. It is called while owner.worldMu is held, before a leader
// plans a room transition. Private drops can consume a different number of
// keys per member; the next shared room must start after every local stream or
// its actor keys will diverge and peer hit packets will target the wrong actor.
func (s *Service) runtimePartyDungeonNextObjectKey(owner *runtimePartyDungeonOwner) (uint32, error) {
	if s == nil || owner == nil {
		return 0, errors.New("party dungeon world owner is unavailable")
	}
	var next uint32
	for _, member := range owner.roster() {
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			return 0, fmt.Errorf("party member %d has no current game session", member.userID)
		}
		identity.session.dungeon.mu.Lock()
		runtime := identity.session.dungeon.runtime
		if runtime == nil || runtime.partyDungeonOwner != owner {
			identity.session.dungeon.mu.Unlock()
			return 0, fmt.Errorf("party member %d dungeon runtime owner changed", member.userID)
		}
		candidate := runtime.NextObjectKey
		identity.session.dungeon.mu.Unlock()
		if candidate > next {
			next = candidate
		}
	}
	if next == 0 {
		return 0, errors.New("party dungeon object-key stream is unavailable")
	}
	return next, nil
}

func (owner *runtimePartyDungeonOwner) setMemberDead(
	userID uint16,
	generation uint64,
	dead bool,
) (changed bool, allDead bool) {
	if owner == nil {
		return false, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return false, false
	}
	localSlot := byte(0xff)
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
		if member.userID == userID && member.generation == generation {
			localSlot = member.slot
		}
	}
	if localSlot == 0xff {
		return false, false
	}
	bit := uint8(1) << localSlot
	wasDead := owner.deadSlots&bit != 0
	if dead {
		owner.deadSlots |= bit
	} else {
		owner.deadSlots &^= bit
	}
	return wasDead != dead, expected != 0 && owner.deadSlots == expected
}

func (owner *runtimePartyDungeonOwner) allMembersDead() bool {
	if owner == nil {
		return false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
	}
	return !owner.retired && expected != 0 && owner.deadSlots == expected
}

// deadSlotsSnapshot reports the owner's dead-slot bitmap for diagnosis. The
// death watchdog logs it next to the roster size so "the server pulled a living
// player out of the dungeon" and "the whole party was down" can be told apart
// from the log alone; the bitmap is one byte and takes no other lock.
func (owner *runtimePartyDungeonOwner) deadSlotsSnapshot() uint8 {
	if owner == nil {
		return 0
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.deadSlots
}

// deadMembersExcept returns the roster members the shared world currently holds
// as dead, in slot order, excluding one identity (the member that is about to
// receive a replay of those facts).
func (owner *runtimePartyDungeonOwner) deadMembersExcept(
	userID uint16,
	generation uint64,
) []runtimePartyDungeonMember {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired || owner.deadSlots == 0 {
		return nil
	}
	dead := make([]runtimePartyDungeonMember, 0, len(owner.members))
	for _, member := range owner.members {
		if member.userID == userID && member.generation == generation {
			continue
		}
		if owner.deadSlots&(uint8(1)<<member.slot) == 0 {
			continue
		}
		dead = append(dead, member)
	}
	sort.Slice(dead, func(i, j int) bool { return dead[i].slot < dead[j].slot })
	return dead
}

// publishRuntimePartyDungeonCharacterState delivers one peer-safe op32 to
// every admitted member except the source connection, which has already
// received the frame from its local command handler. Delivery is posted to
// each member's mailbox: a dungeon command must never synchronously wait for
// another member's mailbox while it owns the shared party-dungeon world lock.
func (s *Service) publishRuntimePartyDungeonCharacterState(
	owner *runtimePartyDungeonOwner,
	sourceID uint16,
	sourceGeneration uint64,
	body []byte,
	eventSource string,
) error {
	if s == nil || owner == nil || len(body) != currentDungeonDeathStateBodySize {
		return nil
	}
	for _, member := range owner.roster() {
		if member.userID == sourceID && member.generation == sourceGeneration {
			continue
		}
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			s.logGameEvent(nil, "party-dungeon-character-state-projection-skipped",
				"member_char_id", member.userID,
				"event_source", eventSource,
				"reason", "session_generation_changed")
			continue
		}
		target := identity.session
		projectionBody := append([]byte(nil), body...)
		err := s.postGameSessionCharacterEvent(
			target,
			eventSource,
			member.userID,
			member.generation,
			func() error {
				target.dungeon.mu.Lock()
				peerRuntime := target.dungeon.runtime
				target.dungeon.mu.Unlock()
				if peerRuntime == nil || peerRuntime.partyDungeonOwner != owner {
					s.logGameEvent(target, "party-dungeon-character-state-projection-skipped",
						"member_char_id", member.userID,
						"event_source", eventSource,
						"reason", "runtime_owner_changed")
					return nil
				}
				return s.sendGameUpperRawClass(
					target,
					currentDungeonDeathStateMsgID,
					projectionBody,
					0,
				)
			},
		)
		if err != nil {
			s.logGameEvent(target, "party-dungeon-character-state-projection-skipped",
				"member_char_id", member.userID,
				"event_source", eventSource,
				"reason", "member_mailbox_unavailable",
				"error", err)
		}
	}
	return nil
}

// replayRuntimePartyDungeonDeathsAfterRoomLoad re-installs the shared death
// facts into one member whose client just finished loading a room. Every
// client respawns the party actors from its own room load, so without this
// replay a peer that died in an earlier room is drawn as a live actor on the
// freshly loaded client while the server (and the dead member's own client)
// still hold the ghost.
func (s *Service) replayRuntimePartyDungeonDeathsAfterRoomLoad(
	session *gameSession,
	owner *runtimePartyDungeonOwner,
	eventSource string,
) error {
	if s == nil || session == nil || owner == nil {
		return nil
	}
	dead := owner.deadMembersExcept(session.selectedCharacterID, session.characterGeneration)
	if len(dead) == 0 {
		return nil
	}
	for _, member := range dead {
		body := buildCurrentDungeonDeathStateBody(
			currentSceneActorObjectKey(member.userID),
			0,
			0,
		)
		if err := s.sendGameUpperRawClass(
			session,
			currentDungeonDeathStateMsgID,
			body,
			0,
		); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "party-dungeon-death-state-replay",
		"party_id", owner.partyID,
		"char_id", session.selectedCharacterID,
		"replayed_member_count", len(dead),
		"replayed_char_ids", formatRuntimeRaidMemberIDs(deadMemberIDs(dead)),
		"event_source", eventSource)
	return nil
}

func deadMemberIDs(members []runtimePartyDungeonMember) []uint16 {
	ids := make([]uint16, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.userID)
	}
	return ids
}

// armRuntimePartyDungeonAllDeadReturns aligns every dead member to the same
// ten-second all-dead edge. Peer timers are installed through their own
// mailboxes after the shared all-dead state has committed.
func (s *Service) armRuntimePartyDungeonAllDeadReturns(
	owner *runtimePartyDungeonOwner,
	source *gameSession,
) error {
	if s == nil || owner == nil || !owner.allMembersDead() {
		return nil
	}
	for _, member := range owner.roster() {
		arm := func(target *gameSession) error {
			target.dungeon.mu.Lock()
			defer target.dungeon.mu.Unlock()
			runtime := target.dungeon.runtime
			if runtime == nil || runtime.partyDungeonOwner != owner || !runtime.deathReturnWaiting {
				return errors.New("party dungeon all-dead member runtime is not waiting")
			}
			transition := cloneCurrentDungeonTownTransition(runtime.deathReturnTransition)
			return s.armCurrentDungeonDeathReturnAfterLocked(
				target,
				runtime,
				transition,
				currentPartyDungeonAllDeadReturnDelay,
			)
		}
		if source != nil && member.userID == source.selectedCharacterID &&
			member.generation == source.characterGeneration {
			if err := arm(source); err != nil {
				return fmt.Errorf("arm party dungeon all-dead source %d: %w", member.userID, err)
			}
			continue
		}
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			s.logGameEvent(source, "party-dungeon-all-dead-arm-skipped",
				"member_char_id", member.userID,
				"reason", "session_generation_changed")
			continue
		}
		target := identity.session
		err := s.postGameSessionCharacterEvent(
			target,
			"party-dungeon-all-dead-arm",
			member.userID,
			member.generation,
			func() error {
				if !owner.allMembersDead() {
					return nil
				}
				return arm(target)
			},
		)
		if err != nil {
			s.logGameEvent(target, "party-dungeon-all-dead-arm-skipped",
				"member_char_id", member.userID,
				"reason", "member_mailbox_unavailable",
				"error", err)
		}
	}
	return nil
}

func (s *Service) executeRuntimePartyDungeonCommand(
	source *gameSession,
	eventSource string,
	local func(*gameSession) error,
) error {
	if source == nil || local == nil {
		return nil
	}
	source.dungeon.mu.Lock()
	runtime := source.dungeon.runtime
	var owner *runtimePartyDungeonOwner
	if runtime != nil {
		owner = runtime.partyDungeonOwner
	}
	source.dungeon.mu.Unlock()
	if owner == nil {
		return local(source)
	}
	if _, current := owner.member(source.selectedCharacterID, source.characterGeneration); !current {
		return errors.New("party dungeon command source is not in the immutable roster")
	}
	sourceID := source.selectedCharacterID
	sourceGeneration := source.characterGeneration
	roster := owner.roster()

	// Same-version reference behavior has two distinct phases: mutate/capture
	// the shared world command under its world lock, then publish the committed
	// projection through member mailboxes. Waiting on a peer mailbox here forms
	// an AB/BA deadlock when two clients report kills at the same time: each
	// mailbox owns one inbound command while one of them owns worldMu.
	owner.worldMu.Lock()
	localErr := local(source)
	owner.worldMu.Unlock()
	if localErr != nil {
		return localErr
	}

	for _, member := range roster {
		if member.userID == sourceID && member.generation == sourceGeneration {
			continue
		}
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			s.logGameEvent(source, "party-dungeon-command-projection-skipped",
				"member_char_id", member.userID,
				"event_source", eventSource,
				"reason", "session_generation_changed")
			continue
		}
		target := identity.session
		err := s.postGameSessionCharacterEvent(
			target,
			eventSource+"-party-projection",
			member.userID,
			member.generation,
			func() error {
				target.dungeon.mu.Lock()
				peerRuntime := target.dungeon.runtime
				target.dungeon.mu.Unlock()
				if peerRuntime == nil || peerRuntime.partyDungeonOwner != owner {
					s.logGameEvent(target, "party-dungeon-command-projection-skipped",
						"member_char_id", member.userID,
						"event_source", eventSource,
						"reason", "runtime_owner_changed")
					return nil
				}
				if _, current := owner.member(member.userID, member.generation); !current {
					return nil
				}
				owner.worldMu.Lock()
				defer owner.worldMu.Unlock()
				return local(target)
			},
		)
		if err != nil {
			s.logGameEvent(target, "party-dungeon-command-projection-skipped",
				"member_char_id", member.userID,
				"event_source", eventSource,
				"reason", "member_mailbox_unavailable",
				"error", err)
		}
	}
	return nil
}

func runtimePartyDungeonActorStateRank(state runtimeDungeonMonsterState) int {
	switch state {
	case runtimeDungeonMonsterPlanned:
		return 1
	case runtimeDungeonMonsterAnnounced:
		return 2
	case runtimeDungeonMonsterDefeated:
		return 3
	default:
		return 0
	}
}

func runtimePartyDungeonRoomWorld(runtime *runtimeDungeonState) (
	runtimeDungeonRoomKey,
	*runtimePartyDungeonSharedRoom,
	error,
) {
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		return runtimeDungeonRoomKey{}, nil, errDungeonWorldMapUnavailable
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return runtimeDungeonRoomKey{}, nil, worldmap.ErrDungeonRunRequired
	}
	snapshot := runtime.Room.Snapshot()
	key := runtimeDungeonRoomKeyFromScene(scene)
	if snapshot.Coordinate != scene.Coordinate || snapshot.MapID != scene.Map.Map.ID {
		return runtimeDungeonRoomKey{}, nil, fmt.Errorf(
			"%w: party world scene=%s/%d room=%s/%d",
			errDungeonMoveRuntimeOwnerMismatch,
			scene.Coordinate,
			scene.Map.Map.ID,
			snapshot.Coordinate,
			snapshot.MapID,
		)
	}
	room := &runtimePartyDungeonSharedRoom{
		seed: runtime.Seed, cleared: scene.Cleared,
		actors: make(map[uint32]runtimePartyDungeonSharedActor, len(snapshot.Monsters)+len(snapshot.ExtendedActors)),
	}
	for _, monster := range snapshot.Monsters {
		room.actors[monster.ObjectKey] = runtimePartyDungeonSharedActor{
			kind: "monster", reference: monster.Reference, hasRef: true,
			code: uint32(monster.Spawn.MonsterID), state: monster.State,
		}
	}
	for _, actor := range snapshot.ExtendedActors {
		shared := runtimePartyDungeonSharedActor{
			kind: string(actor.Kind), code: actor.Packet.Code, state: actor.State,
		}
		if actor.HostileReference != nil {
			shared.reference = *actor.HostileReference
			shared.hasRef = true
		}
		room.actors[actor.ObjectKey] = shared
	}
	return key, room, nil
}

// observeRuntimePartyDungeonWorldLocked admits or advances one local mirror
// into the shared room ledger. It must be called while owner.worldMu is held
// (or during admission before the owner is published to any session).
func (owner *runtimePartyDungeonOwner) observeRuntimePartyDungeonWorldLocked(
	runtime *runtimeDungeonState,
) (runtimeDungeonRoomKey, error) {
	if owner == nil {
		return runtimeDungeonRoomKey{}, errors.New("party dungeon world owner is unavailable")
	}
	key, candidate, err := runtimePartyDungeonRoomWorld(runtime)
	if err != nil {
		return runtimeDungeonRoomKey{}, err
	}
	if owner.worldRooms == nil {
		owner.worldRooms = make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom)
	}
	shared := owner.worldRooms[key]
	if shared == nil {
		owner.worldRevision++
		candidate.revision = owner.worldRevision
		owner.worldRooms[key] = candidate
		return key, nil
	}
	if shared.seed != candidate.seed || len(shared.actors) != len(candidate.actors) {
		return runtimeDungeonRoomKey{}, fmt.Errorf(
			"party dungeon shared room topology mismatch: room=%s/%d seed=%d/%d actors=%d/%d",
			worldmap.RoomCoordinate{X: key.X, Y: key.Y}, key.MapID,
			shared.seed, candidate.seed, len(shared.actors), len(candidate.actors),
		)
	}
	changed := false
	for objectKey, actor := range candidate.actors {
		canonical, ok := shared.actors[objectKey]
		if !ok || canonical.kind != actor.kind || canonical.hasRef != actor.hasRef ||
			canonical.reference != actor.reference || canonical.code != actor.code {
			return runtimeDungeonRoomKey{}, fmt.Errorf(
				"party dungeon shared actor topology mismatch: room=%s/%d object_key=%d",
				worldmap.RoomCoordinate{X: key.X, Y: key.Y}, key.MapID, objectKey,
			)
		}
		if runtimePartyDungeonActorStateRank(actor.state) > runtimePartyDungeonActorStateRank(canonical.state) {
			canonical.state = actor.state
			shared.actors[objectKey] = canonical
			changed = true
		}
	}
	if candidate.cleared && !shared.cleared {
		shared.cleared = true
		changed = true
	}
	if changed {
		owner.worldRevision++
		shared.revision = owner.worldRevision
	}
	return key, nil
}

func runtimePartyDungeonActorDefeated(runtime *runtimeDungeonState, objectKey uint32) bool {
	if runtime == nil || runtime.Room == nil || objectKey == 0 {
		return false
	}
	snapshot := runtime.Room.Snapshot()
	for _, monster := range snapshot.Monsters {
		if monster.ObjectKey == objectKey {
			return monster.State == runtimeDungeonMonsterDefeated
		}
	}
	for _, actor := range snapshot.ExtendedActors {
		if actor.ObjectKey == objectKey {
			return actor.State == runtimeDungeonMonsterDefeated
		}
	}
	return false
}

func rewriteRuntimePartyDungeonDeathOwner(body []byte, ownerObjectKey uint16) []byte {
	rewritten := append([]byte(nil), body...)
	if len(rewritten) < 6 || binary.LittleEndian.Uint16(rewritten[4:6]) == ^uint16(0) {
		return rewritten
	}
	binary.LittleEndian.PutUint16(rewritten[4:6], ownerObjectKey)
	return rewritten
}

// executeRuntimePartyDungeonMonsterDeath commits one room-world death fact and
// lets every immutable member consume it once. This is deliberately separate
// from executeRuntimePartyDungeonCommand: replaying a raw op39 independently in
// two room runtimes permits one validation path to accept while the other
// rejects, leaving a monster alive on only one client.
func (s *Service) executeRuntimePartyDungeonMonsterDeath(source *gameSession, body []byte) error {
	if source == nil {
		return nil
	}
	request, decodeErr := dungeoncmd.DecodeDieMonsterRequest(body)
	if decodeErr != nil || len(request.OpaqueTail) != 0 {
		return s.handleDungeonMonsterDeathLocal(source, body)
	}
	source.dungeon.mu.Lock()
	runtime := source.dungeon.runtime
	var owner *runtimePartyDungeonOwner
	if runtime != nil {
		owner = runtime.partyDungeonOwner
	}
	source.dungeon.mu.Unlock()
	if owner == nil {
		return s.handleDungeonMonsterDeathLocal(source, body)
	}
	sourceMember, current := owner.member(source.selectedCharacterID, source.characterGeneration)
	if !current || sourceMember.slot >= 8 {
		return errors.New("party dungeon monster-death source is not in the immutable roster")
	}

	owner.worldMu.Lock()
	roomKey, roomErr := owner.observeRuntimePartyDungeonWorldLocked(runtime)
	if roomErr != nil {
		owner.worldMu.Unlock()
		return roomErr
	}
	deathKey := runtimePartyDungeonDeathKey{room: roomKey, objectKey: request.RuntimeObjectKey}
	if owner.worldDeaths == nil {
		owner.worldDeaths = make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact)
	}
	fact := owner.worldDeaths[deathKey]
	if fact == nil {
		localErr := s.handleDungeonMonsterDeathLocal(source, append([]byte(nil), body...))
		if localErr != nil {
			owner.worldMu.Unlock()
			return localErr
		}
		if !runtimePartyDungeonActorDefeated(runtime, request.RuntimeObjectKey) {
			owner.worldMu.Unlock()
			return nil
		}
		if _, observeErr := owner.observeRuntimePartyDungeonWorldLocked(runtime); observeErr != nil {
			owner.worldMu.Unlock()
			return observeErr
		}
		owner.worldRevision++
		fact = &runtimePartyDungeonDeathFact{
			revision: owner.worldRevision,
			key:      deathKey,
			request:  append([]byte(nil), body...),
		}
		owner.worldDeaths[deathKey] = fact
	} else if fact.consumers&(uint8(1)<<sourceMember.slot) == 0 {
		if !runtimePartyDungeonActorDefeated(runtime, request.RuntimeObjectKey) {
			requestBody := rewriteRuntimePartyDungeonDeathOwner(
				fact.request,
				currentSceneActorObjectKey(sourceMember.userID),
			)
			if localErr := s.handleDungeonMonsterDeathLocal(source, requestBody); localErr != nil {
				owner.worldMu.Unlock()
				return localErr
			}
		}
		if !runtimePartyDungeonActorDefeated(runtime, request.RuntimeObjectKey) {
			owner.worldMu.Unlock()
			return fmt.Errorf(
				"party dungeon shared death was not consumed by source member %d: room=%+v object_key=%d revision=%d",
				sourceMember.userID, deathKey.room, deathKey.objectKey, fact.revision,
			)
		}
		if _, observeErr := owner.observeRuntimePartyDungeonWorldLocked(runtime); observeErr != nil {
			owner.worldMu.Unlock()
			return observeErr
		}
	}
	fact.consumers |= uint8(1) << sourceMember.slot
	recipients := make([]runtimePartyDungeonMember, 0, len(owner.members)-1)
	for _, member := range owner.roster() {
		if fact.consumers&(uint8(1)<<member.slot) == 0 {
			recipients = append(recipients, member)
		}
	}
	revision := fact.revision
	owner.worldMu.Unlock()

	for _, member := range recipients {
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			s.logGameEvent(source, "party-dungeon-monster-death-projection-skipped",
				"member_char_id", member.userID,
				"world_revision", revision,
				"reason", "session_generation_changed")
			continue
		}
		target := identity.session
		memberCopy := member
		if err := s.postGameSessionCharacterEvent(
			target,
			"party-dungeon-monster-death-fact",
			member.userID,
			member.generation,
			func() error {
				return s.applyRuntimePartyDungeonMonsterDeathFact(target, owner, deathKey, memberCopy)
			},
		); err != nil {
			s.logGameEvent(target, "party-dungeon-monster-death-projection-skipped",
				"member_char_id", member.userID,
				"world_revision", revision,
				"reason", "member_mailbox_unavailable",
				"error", err)
		}
	}
	return nil
}

func (s *Service) applyRuntimePartyDungeonMonsterDeathFact(
	target *gameSession,
	owner *runtimePartyDungeonOwner,
	deathKey runtimePartyDungeonDeathKey,
	member runtimePartyDungeonMember,
) error {
	if target == nil || owner == nil || member.slot >= 8 {
		return nil
	}
	owner.worldMu.Lock()
	defer owner.worldMu.Unlock()
	fact := owner.worldDeaths[deathKey]
	if fact == nil || fact.consumers&(uint8(1)<<member.slot) != 0 {
		return nil
	}
	target.dungeon.mu.Lock()
	runtime := target.dungeon.runtime
	target.dungeon.mu.Unlock()
	if runtime == nil || runtime.partyDungeonOwner != owner {
		return nil
	}
	roomKey, err := owner.observeRuntimePartyDungeonWorldLocked(runtime)
	if err != nil {
		return err
	}
	if roomKey != fact.key.room {
		return fmt.Errorf(
			"party dungeon death projection room mismatch: member=%d current=%+v fact=%+v",
			member.userID, roomKey, fact.key.room,
		)
	}
	if !runtimePartyDungeonActorDefeated(runtime, fact.key.objectKey) {
		requestBody := rewriteRuntimePartyDungeonDeathOwner(fact.request, currentSceneActorObjectKey(member.userID))
		if err := s.handleDungeonMonsterDeathLocal(target, requestBody); err != nil {
			return err
		}
	}
	if !runtimePartyDungeonActorDefeated(runtime, fact.key.objectKey) {
		return fmt.Errorf(
			"party dungeon death fact was not consumed: member=%d room=%+v object_key=%d revision=%d",
			member.userID, fact.key.room, fact.key.objectKey, fact.revision,
		)
	}
	if _, err := owner.observeRuntimePartyDungeonWorldLocked(runtime); err != nil {
		return err
	}
	fact.consumers |= uint8(1) << member.slot
	s.logGameEvent(target, "party-dungeon-monster-death-fact-consumed",
		"member_char_id", member.userID,
		"room", worldmap.RoomCoordinate{X: fact.key.room.X, Y: fact.key.room.Y}.String(),
		"map_id", fact.key.room.MapID,
		"object_key", fact.key.objectKey,
		"world_revision", fact.revision,
		"projection", "shared_actor_death_with_private_member_rewards")
	return nil
}

type runtimePartyDungeonMember struct {
	slot       byte
	userID     uint16
	generation uint64
}

type runtimePartyDungeonReadyRegistration struct {
	cycle   uint64
	slot    byte
	first   bool
	release bool
}

type runtimePartyDungeonMemberRemoval struct {
	member    runtimePartyDungeonMember
	survivors []runtimePartyDungeonMember
	removed   bool
}

func newRuntimePartyDungeonOwner(snapshot party.RuntimePartySnapshot) (*runtimePartyDungeonOwner, error) {
	if snapshot.ID == 0 || snapshot.Leader == 0 || len(snapshot.Members) < 2 || len(snapshot.Members) > 4 {
		return nil, errors.New("party dungeon owner requires an active 2..4 member party")
	}
	owner := &runtimePartyDungeonOwner{
		entryMemberCount: len(snapshot.Members),
		partyID:          snapshot.ID, leaderID: snapshot.Leader,
		leaderSlot: 0xff, loadingCycle: 1,
		members:     make([]runtimePartyDungeonMember, 0, len(snapshot.Members)),
		worldRooms:  make(map[runtimeDungeonRoomKey]*runtimePartyDungeonSharedRoom),
		worldDeaths: make(map[runtimePartyDungeonDeathKey]*runtimePartyDungeonDeathFact),
	}
	mask := uint8(0)
	for _, member := range snapshot.Members {
		if member.UserID == 0 || member.SessionGeneration == 0 || member.Slot >= 8 {
			return nil, errors.New("party dungeon owner received an incomplete member identity")
		}
		bit := uint8(1) << member.Slot
		if mask&bit != 0 {
			return nil, fmt.Errorf("party dungeon owner received duplicate slot %d", member.Slot)
		}
		mask |= bit
		if member.UserID == snapshot.Leader {
			owner.leaderSlot = member.Slot
		}
		owner.members = append(owner.members, runtimePartyDungeonMember{
			slot: member.Slot, userID: member.UserID, generation: member.SessionGeneration,
		})
	}
	if owner.leaderSlot == 0xff {
		return nil, errors.New("party dungeon owner has no leader slot")
	}
	return owner, nil
}

func (owner *runtimePartyDungeonOwner) member(userID uint16, generation uint64) (runtimePartyDungeonMember, bool) {
	if owner == nil || userID == 0 || generation == 0 {
		return runtimePartyDungeonMember{}, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return runtimePartyDungeonMember{}, false
	}
	for _, member := range owner.members {
		if member.userID == userID && member.generation == generation {
			return member, true
		}
	}
	return runtimePartyDungeonMember{}, false
}

func (owner *runtimePartyDungeonOwner) roster() []runtimePartyDungeonMember {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return nil
	}
	return append([]runtimePartyDungeonMember(nil), owner.members...)
}

// contributionClearRoster is the party that actually cleared, as character-id
// strings, for the 公会贡献度 "party dungeon clear" rule.
//
// Upstream lazily caches this list on the owner the first time it is asked
// (owner.contributionMembers) and then keeps returning that snapshot. This tree
// computes it fresh instead: the owner's roster is only two small slices, the
// call happens once per settlement, and a cached snapshot would keep reporting a
// member who left the run mid-dungeon. Registry.ContributionEligible performs the
// authoritative "was this character in the guild for the whole run" check
// anyway, against the entry snapshot, so a fresher list can only be more
// accurate -- it cannot credit anyone the snapshot check rejects.
func (owner *runtimePartyDungeonOwner) contributionClearRoster() []string {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return nil
	}
	out := make([]string, 0, len(owner.members))
	for _, member := range owner.members {
		out = append(out, strconv.Itoa(int(member.userID)))
	}
	return out
}

// loadingReleaseRoster keeps the native UDP controller dormant until every// non-controller member has consumed its complete post-loading actor state.
// The leader slot is the op26 host on every client; releasing that connection
// first lets it start authoritative combat while a follower is still inside
// op120/op3/op37/op19 initialization. The resulting half-ready peer stream is
// rendered by the current client as minimum (1 HP) damage. Preserve stable
// member order, but always publish the current leader last.
func (owner *runtimePartyDungeonOwner) loadingReleaseRoster() []runtimePartyDungeonMember {
	if owner == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return nil
	}
	release := make([]runtimePartyDungeonMember, 0, len(owner.members))
	var leader runtimePartyDungeonMember
	leaderFound := false
	for _, member := range owner.members {
		if member.userID == owner.leaderID && member.slot == owner.leaderSlot {
			leader = member
			leaderFound = true
			continue
		}
		release = append(release, member)
	}
	if leaderFound {
		release = append(release, leader)
	}
	return release
}

// removeMember retires one confirmed town-returning member from the shared
// dungeon owner. The world lock is taken before the roster lock to preserve
// the same ordering as authoritative death and room-transition paths.
func (owner *runtimePartyDungeonOwner) removeMember(
	userID uint16,
	generation uint64,
	newLeaderID uint16,
) runtimePartyDungeonMemberRemoval {
	if owner == nil || userID == 0 || generation == 0 {
		return runtimePartyDungeonMemberRemoval{}
	}
	owner.worldMu.Lock()
	defer owner.worldMu.Unlock()
	return owner.removeMemberWhileWorldLocked(userID, generation, newLeaderID)
}

// removeMemberWhileWorldLocked is the mutation half used by the confirmed
// town-return path while it serializes the central-party leave and every
// native reduction projection under the same party-dungeon world edge. This
// prevents two members returning concurrently from publishing an older
// survivor roster after the later leave has already disbanded it.
func (owner *runtimePartyDungeonOwner) removeMemberWhileWorldLocked(
	userID uint16,
	generation uint64,
	newLeaderID uint16,
) runtimePartyDungeonMemberRemoval {
	if owner == nil || userID == 0 || generation == 0 {
		return runtimePartyDungeonMemberRemoval{}
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()

	if owner.retired {
		return runtimePartyDungeonMemberRemoval{}
	}
	removedIndex := -1
	for i, member := range owner.members {
		if member.userID == userID && member.generation == generation {
			removedIndex = i
			break
		}
	}
	if removedIndex < 0 {
		return runtimePartyDungeonMemberRemoval{}
	}

	removed := owner.members[removedIndex]
	owner.members = append(owner.members[:removedIndex], owner.members[removedIndex+1:]...)
	bit := uint8(1) << removed.slot
	owner.readySlots &^= bit
	owner.deadSlots &^= bit
	owner.entryPreparedSlots &^= bit
	delete(owner.entryContinuations, userID)

	owner.leaderID = 0
	owner.leaderSlot = 0xff
	for _, member := range owner.members {
		if member.userID == newLeaderID {
			owner.leaderID = member.userID
			owner.leaderSlot = member.slot
			break
		}
	}
	if owner.leaderID == 0 && len(owner.members) != 0 {
		leader := owner.members[0]
		for _, member := range owner.members[1:] {
			if member.slot < leader.slot {
				leader = member
			}
		}
		owner.leaderID = leader.userID
		owner.leaderSlot = leader.slot
	}
	if len(owner.members) == 0 {
		owner.retired = true
		owner.released = true
	}
	return runtimePartyDungeonMemberRemoval{
		member:    removed,
		survivors: append([]runtimePartyDungeonMember(nil), owner.members...),
		removed:   true,
	}
}

func (owner *runtimePartyDungeonOwner) registerEntryPrepared(
	continuation runtimePartyDungeonEntryContinuation,
) ([]runtimePartyDungeonEntryContinuation, error) {
	if owner == nil || continuation.session == nil || continuation.characterID == 0 ||
		continuation.generation == 0 || continuation.runtime == nil ||
		continuation.runtime.partyDungeonOwner != owner || len(continuation.actorSnapshot) == 0 ||
		len(continuation.startMap) == 0 {
		return nil, errors.New("party dungeon entry continuation is incomplete")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return nil, errors.New("party dungeon entry owner is retired")
	}
	localSlot := byte(0xff)
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
		if member.userID == continuation.characterID && member.generation == continuation.generation {
			localSlot = member.slot
		}
	}
	if localSlot == 0xff {
		return nil, errors.New("party dungeon entry session is not in the immutable roster")
	}
	if owner.entryContinuations == nil {
		owner.entryContinuations = make(map[uint16]runtimePartyDungeonEntryContinuation, len(owner.members))
	}
	if _, exists := owner.entryContinuations[continuation.characterID]; exists {
		return nil, nil
	}
	continuation.actorSnapshot = append([]byte(nil), continuation.actorSnapshot...)
	continuation.startMap = append([]byte(nil), continuation.startMap...)
	continuation.requestBody = append([]byte(nil), continuation.requestBody...)
	continuation.passiveDropPlan.ExtraEntries = append(
		[]currentDungeonStartMapExtraEntry(nil), continuation.passiveDropPlan.ExtraEntries...)
	continuation.passiveDropPlan.Drops = append(
		[]*runtimeDungeonDrop(nil), continuation.passiveDropPlan.Drops...)
	owner.entryContinuations[continuation.characterID] = continuation
	owner.entryPreparedSlots |= uint8(1) << localSlot
	if owner.entryPreparedSlots != expected || owner.entryRoomReleased {
		return nil, nil
	}
	owner.entryRoomReleased = true
	release := make([]runtimePartyDungeonEntryContinuation, 0, len(owner.members))
	for _, member := range owner.members {
		prepared, exists := owner.entryContinuations[member.userID]
		if !exists {
			return nil, fmt.Errorf("party dungeon entry continuation is missing member %d", member.userID)
		}
		release = append(release, prepared)
	}
	// The returned slice owns deep-copied packet bodies. Once the immutable
	// entry barrier opens, retaining this map would keep every old session and
	// dungeon runtime reachable for the lifetime of the surviving party owner.
	owner.entryContinuations = nil
	return release, nil
}

func (owner *runtimePartyDungeonOwner) beginRoomLoading() uint64 {
	if owner == nil {
		return 0
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return 0
	}
	owner.loadingCycle++
	if owner.loadingCycle == 0 {
		owner.loadingCycle++
	}
	owner.readySlots = 0
	owner.released = false
	return owner.loadingCycle
}

// readySnapshot 返回本次读条屏障的就绪位图/期望位图/是否已放行/当前周期。
//
// 只给团本日志用：排查"全队卡在读条"时必须能一眼看到"就绪 2/4、谁还没到"，
// 而这三个字段原本只活在 owner 内部。取 owner.mu（**不是** dungeon.mu），
// 因此在持有 dungeon.mu 的调用栈里调用也是安全的。
func (owner *runtimePartyDungeonOwner) readySnapshot() (ready uint8, expected uint8, released bool, cycle uint64) {
	if owner == nil {
		return 0, 0, false, 0
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	return owner.readySlots, owner.expectedReadySlots(), owner.released, owner.loadingCycle
}

// expectedReadySlots 计算"全员到齐"应有的位图。调用方需持有 owner.mu。
func (owner *runtimePartyDungeonOwner) expectedReadySlots() uint8 {
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
	}
	return expected
}

// resetLoadingReleaseLocked 在新的一轮读条周期开始时清空"已放行"集合。
// 调用方需持有 owner.mu。
func (owner *runtimePartyDungeonOwner) resetLoadingReleaseLocked(cycle uint64) {
	if owner.loadingReleasedCycle == cycle {
		return
	}
	owner.loadingReleasedCycle = cycle
	owner.loadingReleasedMask = 0
	owner.loadingReleasedLeader = false
}

// markLoadingReleased 记录"这个成员已经把自己的放行链写完"。
func (owner *runtimePartyDungeonOwner) markLoadingReleased(userID uint16, cycle uint64) {
	if owner == nil || userID == 0 {
		return
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return
	}
	owner.resetLoadingReleaseLocked(cycle)
	for _, member := range owner.members {
		if member.userID != userID {
			continue
		}
		owner.loadingReleasedMask |= uint8(1) << member.slot
		if userID == owner.leaderID {
			owner.loadingReleasedLeader = true
		}
		return
	}
}

// loadingReleaseProgress 返回"已放行/应放行"人数、还没放行的槽位、队长是否已放行。
// 团本日志用它回答"卡在读条时到底谁没完成"。
func (owner *runtimePartyDungeonOwner) loadingReleaseProgress(cycle uint64) (released int, total int, missingSlots []int, leaderDone bool) {
	if owner == nil {
		return 0, 0, nil, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return 0, 0, nil, false
	}
	sameCycle := owner.loadingReleasedCycle == cycle
	for _, member := range owner.members {
		total++
		bit := uint8(1) << member.slot
		if sameCycle && owner.loadingReleasedMask&bit != 0 {
			released++
		} else {
			missingSlots = append(missingSlots, int(member.slot))
		}
	}
	return released, total, missingSlots, sameCycle && owner.loadingReleasedLeader
}

func (owner *runtimePartyDungeonOwner) registerReady(
	userID uint16,
	generation uint64,
) (runtimePartyDungeonReadyRegistration, error) {
	if owner == nil || userID == 0 || generation == 0 {
		return runtimePartyDungeonReadyRegistration{}, errors.New("party dungeon loading owner is unavailable")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retired {
		return runtimePartyDungeonReadyRegistration{}, errors.New("party dungeon loading owner is retired")
	}
	localSlot := byte(0xff)
	expected := uint8(0)
	for _, member := range owner.members {
		expected |= uint8(1) << member.slot
		if member.userID == userID && member.generation == generation {
			localSlot = member.slot
		}
	}
	if localSlot == 0xff {
		return runtimePartyDungeonReadyRegistration{}, errors.New("party dungeon loading session is not in the roster")
	}
	registration := runtimePartyDungeonReadyRegistration{cycle: owner.loadingCycle, slot: localSlot}
	bit := uint8(1) << localSlot
	if owner.readySlots&bit != 0 {
		return registration, nil
	}
	owner.readySlots |= bit
	registration.first = true
	if owner.readySlots == expected && !owner.released {
		owner.released = true
		registration.release = true
	}
	return registration, nil
}

func runtimePartyDungeonContainsActor(runtime *runtimeDungeonState, objectKey uint16) bool {
	if runtime == nil || runtime.partyDungeonOwner == nil || objectKey == 0 {
		return false
	}
	for _, member := range runtime.partyDungeonOwner.roster() {
		if member.userID == objectKey {
			return true
		}
	}
	return false
}

func (s *Service) attachRuntimePartyDungeonOwner(
	leader *gameSession,
	leaderRuntime *runtimeDungeonState,
	prepared []*runtimePartyPreparedDungeonEntry,
	state alignedcmd.PartyState,
) error {
	if s == nil || leader == nil || leaderRuntime == nil || len(prepared) == 0 ||
		state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) {
		return nil
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByID(uint16(state.PartyID))
	if !found {
		snapshot = party.RuntimePartySnapshot{
			ID: uint16(state.PartyID), Leader: state.UserID, Settings: state,
			Members: make([]party.RuntimePartyMember, 0, len(state.Members)),
		}
		for slot, cached := range runtimePartyMembers(state) {
			identity, current := s.onlineGameSessionCharacterSnapshot(cached.UserID)
			if !current || identity.session == nil || slot > int(^uint8(0)) {
				return fmt.Errorf("party dungeon admission member %d has no current session", cached.UserID)
			}
			snapshot.Members = append(snapshot.Members, party.RuntimePartyMember{
				UserID: cached.UserID, SessionGeneration: identity.generation,
				State: cached, Slot: byte(slot),
			})
		}
	}
	if snapshot.Leader != leader.selectedCharacterID || len(snapshot.Members) != len(prepared)+1 {
		return errors.New("party dungeon admission roster changed before commit")
	}
	owner, err := newRuntimePartyDungeonOwner(snapshot)
	if err != nil {
		return err
	}
	// The same-version admission owner proves the complete town actor table and
	// every native peer endpoint before it commits any member.  Keeping these
	// checks in emitRuntimePartyDungeonStart is still useful as a race guard, but
	// discovering a missing row there is too late: the leader has already
	// received the successful op16 ACK and both session runtimes are committed,
	// so returning the error tears down the TCP session instead of returning the
	// client to the selector.
	projection, err := s.buildRuntimePartyDungeonActorProjection(context.Background(), leader, owner)
	if err != nil {
		return fmt.Errorf("prepare party dungeon actor projection: %w", err)
	}
	for _, member := range owner.roster() {
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			return fmt.Errorf("party dungeon member %d has no current endpoint session", member.userID)
		}
		memberState := runtimePartyStateSnapshot(identity.session)
		if memberState.PartyID != int(owner.partyID) || memberState.UserID != owner.leaderID {
			return fmt.Errorf("party dungeon member %d endpoint roster is stale", member.userID)
		}
		if !s.runtimePartyPublishablePeerEndpoints(
			identity.session,
			runtimePartyStateWithoutLocal(memberState, member.userID),
		) {
			return fmt.Errorf("party dungeon member %d native peer endpoint table is incomplete", member.userID)
		}
	}
	owner.actorProjectionOnce.Do(func() {
		owner.actorProjection = cloneRuntimePartyDungeonActorProjection(projection)
	})
	runtimes := make(map[uint16]*runtimeDungeonState, len(prepared)+1)
	runtimes[leader.selectedCharacterID] = leaderRuntime
	for _, entry := range prepared {
		if entry == nil || entry.session == nil || entry.runtime == nil {
			return errors.New("party dungeon admission has an incomplete prepared member")
		}
		runtimes[entry.session.selectedCharacterID] = entry.runtime
	}
	for _, member := range snapshot.Members {
		runtime := runtimes[member.UserID]
		if runtime == nil {
			return fmt.Errorf("party dungeon admission is missing member %d runtime", member.UserID)
		}
		runtime.partyDungeonOwner = owner
		runtime.partyMemberIndex = member.Slot
		runtime.partyMemberIndexed = true
		// Room topology and actor object keys are shared by the party, but
		// ground-drop rolls are private to each admitted member.  Re-key every
		// already prepared visit after the immutable owner assigns the stable
		// party slot and before either member plans passive-object drops.
		for _, visit := range runtime.Rooms {
			if visit != nil {
				visit.DropRNG = currentPartyDungeonPrivateDropRNG(runtime, visit.Seed)
			}
		}
		if _, err := owner.observeRuntimePartyDungeonWorldLocked(runtime); err != nil {
			return fmt.Errorf("admit party member %d to shared dungeon world: %w", member.UserID, err)
		}
	}
	return nil
}

type runtimePartyDungeonActorProjection struct {
	mode0     []byte
	mode1     []byte
	userState []byte
	snapshot  []byte
}

func cloneRuntimePartyDungeonActorProjection(projection runtimePartyDungeonActorProjection) runtimePartyDungeonActorProjection {
	projection.mode0 = append([]byte(nil), projection.mode0...)
	projection.mode1 = append([]byte(nil), projection.mode1...)
	projection.userState = append([]byte(nil), projection.userState...)
	projection.snapshot = append([]byte(nil), projection.snapshot...)
	return projection
}

// runtimePartyDungeonActorProjectionForOwner freezes the complete actor table
// once for the admitted party. The same-version server captures one immutable
// owner-produced projection before mailbox fan-out; rebuilding the remote row
// independently for each receiver can attach different late session state to
// the same stable wire actor and leaves only one client with the full roster.
func (s *Service) runtimePartyDungeonActorProjectionForOwner(
	ctx context.Context,
	receiver *gameSession,
	owner *runtimePartyDungeonOwner,
) (runtimePartyDungeonActorProjection, error) {
	if owner == nil {
		return runtimePartyDungeonActorProjection{}, errors.New("party dungeon actor projection owner is unavailable")
	}
	owner.actorProjectionOnce.Do(func() {
		projection, err := s.buildRuntimePartyDungeonActorProjection(ctx, receiver, owner)
		if err != nil {
			owner.actorProjectionErr = err
			return
		}
		owner.actorProjection = cloneRuntimePartyDungeonActorProjection(projection)
	})
	if owner.actorProjectionErr != nil {
		return runtimePartyDungeonActorProjection{}, owner.actorProjectionErr
	}
	return cloneRuntimePartyDungeonActorProjection(owner.actorProjection), nil
}

func (s *Service) buildRuntimePartyDungeonActorProjection(
	ctx context.Context,
	receiver *gameSession,
	owner *runtimePartyDungeonOwner,
) (runtimePartyDungeonActorProjection, error) {
	if s == nil || receiver == nil || owner == nil {
		return runtimePartyDungeonActorProjection{}, errors.New("party dungeon actor projection owner is unavailable")
	}
	roster := owner.roster()
	if len(roster) < 2 || len(roster) > 4 {
		return runtimePartyDungeonActorProjection{}, errors.New("party dungeon actor projection roster is invalid")
	}
	var mode0 packetWriter
	mode0.writeByte(0)
	mode0.writeUint16(uint16(len(roster)))
	var mode1 packetWriter
	mode1.writeByte(1)
	mode1.writeUint16(uint16(len(roster)))
	var userState packetWriter
	userState.writeByte(byte(len(roster)))
	var snapshot packetWriter
	snapshot.writeByte(byte(len(roster)))
	var projectionOwnerServer byte
	var projectionOwnerChannel byte
	projectionOwnerSet := false
	for _, member := range roster {
		identity, current := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !current || identity.session == nil || identity.generation != member.generation {
			return runtimePartyDungeonActorProjection{}, fmt.Errorf("party dungeon member %d session generation changed", member.userID)
		}
		memberProjection, found := currentPartyActorProjectionSnapshot(
			identity.session,
			member.userID,
			member.generation,
		)
		if !found {
			return runtimePartyDungeonActorProjection{}, fmt.Errorf(
				"party dungeon member %d owner-produced actor projection is unavailable",
				member.userID,
			)
		}
		// A town skill edit refreshes only the owner's upper/area state and never
		// re-captures the party actor projection, so the cached mode1 skill page
		// would freeze at the previous scene snapshot and every teammate admitted
		// after the edit would see the last session's buff levels. Refresh the
		// member's mode1 row from the current durable skill state before freezing
		// the shared projection; a failed refresh keeps the cached row and the
		// admission proceeds as before.
		if s.refreshCurrentPartyActorProjectionMode1FromDurableState(ctx, identity.session) {
			refreshed, ok := currentPartyActorProjectionSnapshot(
				identity.session,
				member.userID,
				member.generation,
			)
			if ok {
				memberProjection = refreshed
			}
		}
		if !projectionOwnerSet {
			projectionOwnerServer = memberProjection.ownerServer
			projectionOwnerChannel = memberProjection.ownerChannel
			projectionOwnerSet = true
		} else if memberProjection.ownerServer != projectionOwnerServer ||
			memberProjection.ownerChannel != projectionOwnerChannel {
			return runtimePartyDungeonActorProjection{}, fmt.Errorf(
				"party dungeon member %d actor projection belongs to a different scene",
				member.userID,
			)
		}
		if currentPartySceneOwnerContext(identity.session) != memberProjection.ownerChannel {
			return runtimePartyDungeonActorProjection{}, fmt.Errorf(
				"party dungeon member %d actor projection owner is stale",
				member.userID,
			)
		}
		for _, value := range memberProjection.mode0[3:] {
			mode0.writeByte(value)
		}
		for _, value := range memberProjection.mode1[3:] {
			mode1.writeByte(value)
		}
		userState.writeUint16(member.userID)
		userState.writeByte(currentDungeonPlayerUserState)
		snapshot.writeUint16(member.userID)
		snapshot.writeByte(0)
	}
	return runtimePartyDungeonActorProjection{
		mode0: mode0.bytes(), mode1: mode1.bytes(),
		userState: userState.bytes(), snapshot: snapshot.bytes(),
	}, nil
}

func runtimePartyStateWithoutLocal(state alignedcmd.PartyState, localID uint16) alignedcmd.PartyState {
	filtered := state
	filtered.Members = make([]alignedcmd.PartyMemberState, 0, len(state.Members))
	for _, member := range state.Members {
		if member.UserID != 0 && member.UserID != localID {
			filtered.Members = append(filtered.Members, member)
		}
	}
	return filtered
}

func buildRuntimePartyDungeonPlacementBody(slot byte) []byte {
	return []byte{slot, 0}
}

func (s *Service) emitRuntimePartyDungeonStart(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	requestBody []byte,
	passiveDropPlan currentDungeonPassiveObjectDropPlan,
	itemUpdates []currentItemListEntry,
	requestOwner bool,
	retryFromSettlement bool,
) error {
	if session == nil || runtime == nil || runtime.partyDungeonOwner == nil {
		return errors.New("party dungeon start owner is unavailable")
	}
	member, current := runtime.partyDungeonOwner.member(session.selectedCharacterID, session.characterGeneration)
	if !current {
		return errors.New("party dungeon start session generation changed")
	}
	resetDungeonEntrySceneGates(session)
	if requestOwner {
		if err := s.sendGameUpperSuccess(
			session, uint16(dnfenum.CmdPacketSelectDungeon),
			buildSelectDungeonBodyForCharacter(session.selectedCharacterID),
		); err != nil {
			return err
		}
	}
	if len(itemUpdates) != 0 {
		if err := s.sendGameUpperRawClass(session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, itemUpdates), 0); err != nil {
			return err
		}
	}
	if err := s.sendCurrentDungeonResourceState(session, runtime, "party_dungeon_start_after_leader_op16_ack"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	projection, err := s.runtimePartyDungeonActorProjectionForOwner(ctx, session, runtime.partyDungeonOwner)
	if err != nil {
		return err
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	session.townMu.Unlock()
	stateBits := currentActorDefaultUserStateBits
	if runtime.Character.Stats != nil {
		if value, exists := runtime.Character.Stats["user_state_bits"]; exists && value >= 0 && value <= 0xff {
			stateBits = byte(value)
		}
	}
	areaBody := buildCurrentTownUserAreaNotificationBody(
		currentSceneActorObjectKey(session.selectedCharacterID),
		position.TownID, 0xff, position.PositionX, position.PositionY,
		position.MovementCode, stateBits,
	)
	if err := s.sendGameUpperRawClass(session, currentTownUserAreaNotificationMsgID, areaBody, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), projection.mode0, 0); err != nil {
		return err
	}
	if _, _, err := s.buildAndSendDungeonActorPassivePrebind(
		session,
		ctx,
		runtime.Character,
		"party_dungeon_after_mode0_before_full_mode1",
	); err != nil {
		return fmt.Errorf("preinstall party dungeon actor passive skill: %w", err)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), projection.mode1, 0); err != nil {
		return err
	}
	// Replay the 公会徽章 decoration for every party actor built above. The aura
	// rides the actor appearance block rather than the guild window, so without
	// this a party that enters a dungeon together sees each other's insignia
	// missing until something else rebuilds the actor. One batched registry read
	// for the whole roster: a read per member made this projection N^2 in
	// repository traffic and tripped the raid stress cap.
	if runtime.partyDungeonOwner != nil {
		roster := runtime.partyDungeonOwner.roster()
		characterIDs := make([]uint16, 0, len(roster))
		for _, member := range roster {
			characterIDs = append(characterIDs, member.userID)
		}
		if err := s.sendCurrentGuildAurasForCharacters(ctx, session, characterIDs, currentPartySceneOwnerContext(session)); err != nil {
			return fmt.Errorf("restore party actor guild aura: %w", err)
		}
	}
	// 和公会徽章同一类：上面这组 mode0/mode1 重建了本地 actor，而创建行不带勋章守护珠的
	// raw socket 字与称号的 raw 随机附魔 —— 组队进图/团本里勋章宝珠会整排消失
	//（2026-10-08 现场："组队状态下（即使单人进图）/任意团本"，出图即恢复）。
	s.restoreCurrentSceneRawEquipmentRows(session, "party_dungeon_after_full_mode1")
	// 婚房装饰奖励（HP/MP +200、所有属性强化 +1）同样是"按婚房数据在场景构建时应用"的，
	// 组队进图这条 mode0/mode1 重建同样会把它清掉（2026-10-08 现场：单人进图已修好，
	// 组队进图后婚房属性又失效）。
	s.restoreCurrentSceneWeddingRoomState(session, session.selectedCharacterID, "party_dungeon_after_full_mode1")
	partyState := runtimePartyStateSnapshot(session)
	remotePartyState := runtimePartyStateWithoutLocal(partyState, session.selectedCharacterID)
	if !s.runtimePartyPublishablePeerEndpoints(session, remotePartyState) {
		return errors.New("party dungeon native peer endpoint table is incomplete")
	}
	peerBody := s.buildRuntimePartyPeerEndpointInfoForReceiver(session, remotePartyState)
	if err := s.sendGameUpperRawClass(session, 11, peerBody, 0); err != nil {
		return err
	}
	// op3 sits here, before op28, exactly where the captured party prelude puts it
	// (dungeon_entry_packets_test.go asserts this position). What was WRONG was its
	// BODY, not its position: it used to carry the whole roster as a counted
	// multi-row projection, and the current client's op3 reader (0x01D88A10) takes a
	// single record -- one u8 state, one object key, one u8 -- with no row count and
	// no loop. It read the count byte as the state. This member's own single record
	// is built the same way the solo path builds it; at this point in the entry the
	// client's stage is not armed yet, so this one is the harmless pre-commit and
	// the record sent after FINISH_LOADING is the one that commits the room.
	userStateBody, err := buildCurrentDungeonUserStateBody(currentSceneActorObjectKey(session.selectedCharacterID))
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyUserState), userStateBody, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, 26, []byte{runtime.partyDungeonOwner.leaderSlot}, 0); err != nil {
		return err
	}
	// op27 (current dungeon context) immediately before op28, exactly where the
	// single-session path puts it (dungeon_navigation_runtime.go "after_op27" ->
	// op28).
	//
	// Why it is needed here: handleDungeonSelectUpperPlanned returns early for a
	// shared party dungeon (runtime.partyDungeonOwner != nil), so neither the
	// leader nor any follower ever reaches the single-session prelude that owns
	// this packet. A party that retried straight from the settlement dialog
	// (再次挑战) never went through the op15 selector page either, so nothing sent
	// op27 at all: the dungeon entry itself succeeds (op5/op28/op29 are all
	// emitted here) but the client's context is never switched and the settlement
	// options stay on screen after the re-entry. Solo retries work because they
	// take the single-session prelude.
	//
	// Guarded ONLY by retryFromSettlement, deliberately NOT by
	// session.enterSelectDungeonContextSent.
	//
	// That latch means "this town visit already sent the selector-page context",
	// not "this entry sent it". A party that retries has necessarily just entered
	// through the op15 selector page, so EVERY member's latch is already true from
	// that page and would suppress the packet the client needs for the re-entry --
	// measured live 2026-09-20: the latch-gated version fired for the leader only
	// (whose latch had been cleared by the retirement into the selector) and the
	// member, whose latch was still set from 18:16:35, received nothing and kept
	// its settlement options on screen.
	//
	// A repeated context is harmless: the leader took one at 18:16:35 (from op15)
	// and another at 18:17:03 (from here) and re-entered correctly.
	//
	// 2026-10-01 23:07 补上另一半：**latch 为假时也必须发**。
	//
	// 上面那条"只看 retryFromSettlement、故意不看 latch"的推理只覆盖了
	// "latch 已经是真、但仍然需要再发一次"的情况；它漏掉了反过来的情况 ——
	// 这一次进图**从来没有经过 op15 选图页**，所以整条链路上没有任何人发过 op27，
	// latch 一直是假。超时空就是这种：进图由"走进有怪的区域"（op36）触发，
	// 服务端自己驱动整场，玩家根本没打开过选图页。真机证据（分片 033）：
	//
	//	15:07:17.699 op27 -> char 22   （队友，source=leader_op16_party_follower_preflight_after_fatigue）
	//	    ← 队友那一侧由 prepareRuntimePartyFollowerEnterSelect →
	//	      sendCurrentTownEnterSelectContext 补发了 op27 并把 latch 置真
	//	（char 1 驱动者：整个会话一条 op27 都没有）
	//	15:07:17.890 char 1 start-committed（op5/op28/op29 都发了，没有任何报错）
	//	15:07:19.906 char 22 RECV op37 FINISH_LOADING   ← 队友完成加载
	//	（char 1 再没有任何 C2S）                        ← 驱动者卡在进图
	//
	// 客户端拿不到 op27 就不会切进副本上下文，于是永远不发 op37，人就卡死在进图。
	// 安东/卢克没暴露是因为他们的队长是从 op15 选图页进来的 —— op15 早就发过 op27，
	// latch 已经是真；所以这一条只在"服务端驱动的进图"（超时空遭遇战）上才成立。
	//
	// 合并后的条件对已有路径逐字等价：
	//   * 安东/卢克选图页进图：latch 真、retryFromSettlement 假 → 仍然不发（不变）；
	//   * 再次挑战：retryFromSettlement 真 → 照旧每次都发（不变）；
	//   * 超时空遭遇战：latch 假 → 补发（本次修复）。
	if retryFromSettlement || !session.enterSelectDungeonContextSent {
		if err := s.sendCurrentDungeonContextOp27(session, "party_dungeon_retry_after_op26_before_op28"); err != nil {
			return err
		}
		session.enterSelectDungeonContextSent = true
	}
	entryPackets, err := buildCurrentDungeonEntryPacketsWithPassiveDrops(runtime, scene, passiveDropPlan.ExtraEntries)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonInfoNotification, entryPackets.DungeonInfo, 0); err != nil {
		return err
	}
	if err := s.sendCurrentTowerOfDespairOpponent(session, runtime); err != nil {
		return fmt.Errorf("initialize despair tower opponent before op29: %w", err)
	}
	continuations, err := runtime.partyDungeonOwner.registerEntryPrepared(
		runtimePartyDungeonEntryContinuation{
			session: session, characterID: session.selectedCharacterID,
			generation: session.characterGeneration, runtime: runtime, scene: scene,
			actorSnapshot:           projection.snapshot,
			randomizedCreatureQueue: entryPackets.RandomizedCreatureQueue,
			randomizedCreatureCount: entryPackets.RandomizedCreatureCount,
			startMap:                entryPackets.StartMap,
			passiveDropPlan:         passiveDropPlan, requestBody: requestBody,
		},
	)
	if err != nil {
		return err
	}
	s.logGameEvent(session, "game-party-dungeon-entry-prepared",
		"party_id", runtime.partyDungeonOwner.partyID,
		"leader_char_id", runtime.partyDungeonOwner.leaderID,
		"leader_slot", runtime.partyDungeonOwner.leaderSlot,
		"local_slot", member.slot,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"map_id", scene.Map.Map.ID,
		"request_body_len", len(requestBody),
		"release", len(continuations) != 0,
		"packet_order", "op16-owner-ack-op5-op23-ff-op2-mode0-all-op2-mode1-all-op11-remotes-op3-all-op26-leader-op28-then-all-member-barrier")
	if len(continuations) == 0 {
		return nil
	}
	var releaseErr error
	for _, prepared := range continuations {
		prepared := prepared
		err := s.postGameSessionCharacterEvent(
			prepared.session,
			"runtime-party-dungeon-initial-room-release",
			prepared.characterID,
			prepared.generation,
			func() error {
				if err := s.completeRuntimePartyDungeonStartRoom(prepared); err != nil {
					s.logGameEvent(prepared.session, "game-party-dungeon-initial-room-release-failed",
						"party_id", prepared.runtime.partyDungeonOwner.partyID,
						"char_id", prepared.characterID,
						"error", err)
					return err
				}
				return nil
			},
		)
		if err != nil {
			releaseErr = errors.Join(releaseErr, fmt.Errorf(
				"queue party dungeon initial room for member %d: %w", prepared.characterID, err))
		}
	}
	s.logGameEvent(session, "game-party-dungeon-entry-barrier-released",
		"party_id", runtime.partyDungeonOwner.partyID,
		"member_count", len(continuations),
		"packet_order", "all-members-through-op28-before-any-op800-op29")
	return releaseErr
}

func (s *Service) completeRuntimePartyDungeonStartRoom(
	prepared runtimePartyDungeonEntryContinuation,
) error {
	session, runtime := prepared.session, prepared.runtime
	if s == nil || session == nil || runtime == nil || runtime.partyDungeonOwner == nil {
		return errors.New("party dungeon initial room continuation is unavailable")
	}
	identity, current := s.onlineGameSessionCharacterSnapshot(prepared.characterID)
	if !current || identity.session != session || identity.generation != prepared.generation {
		return errors.New("party dungeon initial room session generation changed")
	}
	member, current := runtime.partyDungeonOwner.member(prepared.characterID, prepared.generation)
	if !current {
		return errors.New("party dungeon initial room member is no longer admitted")
	}
	session.dungeon.mu.Lock()
	runtimeCurrent := session.dungeon.runtime == runtime
	session.dungeon.mu.Unlock()
	if !runtimeCurrent {
		return errors.New("party dungeon initial room runtime changed")
	}
	// op800 (0x0320 currentTownActorSceneSnapshotMsgID) stays BETWEEN op28 and op29
	// on purpose. 86JP emits nothing here (DungeonEntryHandler.cs:2006 -> :2030) and
	// the current client's op800 handler (0x01D6C6F0) makes zero calls into the scene
	// accessor (0x23591E0) that op28 calls 21 times and op29 5 times, so this packet
	// is NOT what commits the room and NOT the cause of the leftover geometry. Its
	// necessity is a separate question this change does not answer: removing it
	// broke the deliberate assertion in dungeon_entry_packets_test.go:396
	// (op800_before_op29), and "86 does not send it" is not evidence that this client
	// does not need it. Left exactly as it was.
	if err := s.sendGameUpperRawClass(session, currentTownActorSceneSnapshotMsgID, prepared.actorSnapshot, 0); err != nil {
		return err
	}
	if prepared.randomizedCreatureCount != 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketVerifyCreatureQuest),
			prepared.randomizedCreatureQueue,
			0,
		); err != nil {
			return err
		}
		s.logGameEvent(session, "game-party-dungeon-randomized-neutral-creature-queued",
			"party_id", runtime.partyDungeonOwner.partyID,
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", prepared.scene.Coordinate.String(),
			"creature_count", prepared.randomizedCreatureCount,
			"packet_order", "op28-barrier-op800-op479-explicit-creature-op29-room")
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonStartMapNotificationForRuntime(runtime), prepared.startMap, 0); err != nil {
		return err
	}
	if err := s.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session,
		runtime,
		prepared.scene,
		runtime.Room,
		"party_start_room_after_op29",
	); err != nil {
		return fmt.Errorf("publish party trap event-monster condition after op29: %w", err)
	}
	// 物件脚本（上游 b402882）：进房后跑一次，房间里的被动对象脚本该开门的开门。
	s.runCurrentDungeonActionObjectsLocked(session, runtime, prepared.scene, runtimeDungeonActionObjectRoomEntered, "party_start_room_after_op29")
	if err := s.consumeCurrentDungeonRoomFatigueAfterCommit(session, runtime, "party_start_room_after_op29_commit"); err != nil {
		return fmt.Errorf("consume party start-room fatigue: %w", err)
	}
	announcedActors, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		return fmt.Errorf("commit party start-map actors: %w", err)
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache party start-map room: %w", err)
	}
	runtime.partyDungeonOwner.worldMu.Lock()
	_, worldErr := runtime.partyDungeonOwner.observeRuntimePartyDungeonWorldLocked(runtime)
	runtime.partyDungeonOwner.worldMu.Unlock()
	if worldErr != nil {
		return fmt.Errorf("publish party start-map shared world: %w", worldErr)
	}
	if err := commitCurrentDungeonPassiveObjectDrops(runtime, prepared.scene, prepared.passiveDropPlan); err != nil {
		return fmt.Errorf("commit party start-map passive drops: %w", err)
	}
	// Every party actor was created in this member's room by the shared
	// mode0/mode1 prelude, so the room may now name all of them in one op9 slot
	// table. This is what arms the client's host arbitration; it is sent once
	// per dungeon, here, and never on room transitions (the same lifecycle rule
	// as op19, whose replay destroys live P2P skill state).
	if err := s.sendRuntimePartyDungeonRoomSlotTable(
		session,
		runtime.partyDungeonOwner,
		"party_start_room_after_op29_actors",
	); err != nil {
		s.logGameEvent(session, "game-party-dungeon-room-slot-table-deferred",
			"party_id", runtime.partyDungeonOwner.partyID,
			"char_id", prepared.characterID,
			"dungeon_id", runtime.Dungeon.ID,
			"error", err)
	}
	session.preDungeonContextPlayerStateSent = true
	session.postStartMapPlayerStateSent = true
	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = true
	s.logGameEvent(session, "game-party-dungeon-start-committed",
		"party_id", runtime.partyDungeonOwner.partyID,
		"leader_char_id", runtime.partyDungeonOwner.leaderID,
		"leader_slot", runtime.partyDungeonOwner.leaderSlot,
		"local_slot", member.slot,
		"char_id", prepared.characterID,
		"dungeon_id", runtime.Dungeon.ID,
		"map_id", prepared.scene.Map.Map.ID,
		"actors", announcedActors,
		"request_body_len", len(prepared.requestBody),
		"packet_order", "all-members-through-op28-then-op800-all-op29-optional-trap-event-op312-ff-op36-op9-room-slot-table")
	if err := s.armCurrentCrystalContractTimer(session, runtime); err != nil {
		s.logGameEvent(session, "game-crystal-contract-party-entry-timer-deferred",
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"error", err)
	}
	return nil
}

// sendRuntimePartyDungeonRoomSlotTable publishes the initial room's complete
// party slot table to one member as a class0/op9 kind-0 record keyed by that
// member's own actor in the dungeon's owner context 0.
//
// The rows name every admitted member at its real fixed slot; all of those
// actors already exist in the receiver's room because the shared prelude sent
// one mode0 and one mode1 containing the whole roster before op28. The tail
// carries the leader slot, identical for every receiver, so exactly one client
// finds its own actor at the host slot and runs the authoritative simulation.
// The build is all-or-nothing: a row that cannot be resolved would leave one
// member unnamed, so the whole table is skipped and logged instead.
func (s *Service) sendRuntimePartyDungeonRoomSlotTable(
	session *gameSession,
	owner *runtimePartyDungeonOwner,
	source string,
) error {
	if s == nil || session == nil || owner == nil {
		return nil
	}
	if s.options.dungeonRoomPartySlotOff {
		s.logGameEvent(session, "game-party-dungeon-room-slot-table-disabled",
			"party_id", owner.partyID,
			"char_id", session.selectedCharacterID,
			"source", source,
			"reason", "DNFBRIDGE_DUNGEON_ROOM_PARTY_SLOT_OFF")
		return nil
	}
	leaderSlot := owner.leaderSlot
	if leaderSlot > 7 {
		return fmt.Errorf("party dungeon room slot table leader slot %d is unavailable", leaderSlot)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	partyState := runtimePartyStateSnapshot(session)
	roster := owner.roster()
	rows := make([]currentSceneOp9PartySlotRow, 0, len(roster))
	var receiverCharacter dnfrepo.CharacterRecord
	receiverName := ""
	receiverFound := false
	for _, rosterMember := range roster {
		identity, current := s.onlineGameSessionCharacterSnapshot(rosterMember.userID)
		if !current || identity.session == nil || identity.generation != rosterMember.generation {
			return fmt.Errorf("party dungeon room slot table member %d session generation changed", rosterMember.userID)
		}
		charID, charName, character, hasCharacter := s.characterForEnter(ctx, identity.session, rosterMember.userID)
		if charID != rosterMember.userID || !hasCharacter {
			return fmt.Errorf("party dungeon room slot table member %d record is unavailable", rosterMember.userID)
		}
		if rosterMember.userID == session.selectedCharacterID {
			receiverCharacter, receiverName, receiverFound = character, charName, true
		}
		level := byte(1)
		if character.Level > 0 && character.Level < 256 {
			level = byte(character.Level)
		}
		rows = append(rows, currentSceneOp9PartySlotRow{
			Slot: rosterMember.slot,
			Member: currentSceneOp9PartyMemberProjection{
				State: runtimePartyMemberStateForSlot(partyState, rosterMember.userID, rosterMember.slot),
				Name:  charName,
				Job:   byte(numericCharacterStat(character.Job)),
				Level: level,
				Grow:  byte(numericCharacterStatValue(character, "grow_type")),
			},
		})
	}
	if !receiverFound {
		return errors.New("party dungeon room slot table receiver is not in the roster")
	}
	if len(rows) < 2 {
		return errors.New("party dungeon room slot table requires at least two members")
	}
	// The client keys each row by its explicit slot; slot order keeps the
	// packet deterministic.
	sort.Slice(rows, func(left, right int) bool { return rows[left].Slot < rows[right].Slot })
	objectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	body := buildCurrentSceneOp9DungeonRoomSlotTableBodyInContext(
		objectKey,
		receiverCharacter,
		true,
		receiverName,
		currentSceneObjectContext,
		rows,
		leaderSlot,
		currentSceneOp9PartyMemberCap(partyState),
		currentSceneOp9PartyTypeAux(partyState),
	)
	s.logPacketEvent("game-party-dungeon-room-slot-table-send",
		"conn_id", session.connID,
		"source", source,
		"party_id", owner.partyID,
		"char_id", session.selectedCharacterID,
		"object_key", objectKey,
		"owner_channel", currentSceneObjectContext,
		"row_count", len(rows),
		"host_slot", leaderSlot,
		"body_len", len(body),
		"msg_id", uint16(dnfenum.CmdPacketRecoverStamina),
		"classification", 0,
		"body_source", "current_exe_sub_1D64CA0_kind0_room_slot_table_after_all_party_actors_extended_rows")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), body, 0)
}

// runtimePartyMemberStateForSlot returns the live party member state for one
// admitted member, falling back to a neutral state at the admitted slot when
// the session's party snapshot no longer lists it.
func runtimePartyMemberStateForSlot(state alignedcmd.PartyState, userID uint16, slot byte) alignedcmd.PartyMemberState {
	for _, member := range state.Members {
		if member.UserID == userID {
			member.Slot = slot
			return member
		}
	}
	return alignedcmd.PartyMemberState{UserID: userID, Slot: slot}
}

func (s *Service) completeRuntimePartyDungeonLoading(
	session *gameSession,
	owner *runtimePartyDungeonOwner,
	loadingCycle uint64,
) error {
	if session == nil || owner == nil {
		return nil
	}
	stepStarted := time.Now()
	defer s.probeSlowStep(session, "party-dungeon-loading-release", "complete_loading_total", stepStarted)
	// 第二支探针：无缝分段。把整个函数的墙钟按语句顺序切成连续段落，
	// Σ段 + 未归因 ≡ 总时长（构造上恒等），所以不存在「哪段都不慢但总共很慢」
	// 这种空白结论 —— 那正是下面 5 个分步探针在实测里给出的结果。
	seg := newSlowProbeSegments()
	defer seg.report(s, session, "party-dungeon-loading-release", "loading_cycle", loadingCycle)
	runtime := session.dungeon.runtime
	member, current := owner.member(session.selectedCharacterID, session.characterGeneration)
	if runtime == nil || runtime.partyDungeonOwner != owner || !current {
		return nil
	}
	seg.mark("owner_member_lookup")
	for _, rosterMember := range owner.roster() {
		if err := s.sendGameUpperRawClass(
			session, uint16(dnfenum.CmdPacketRequestBlacklist),
			buildRuntimePartyDungeonPlacementBody(rosterMember.slot), 0,
		); err != nil {
			return err
		}
	}
	seg.mark("roster_op235_per_member")
	// op3 (USER_STATE) is a SINGLE record on the wire. The current client's reader
	// (0x01D88A10) reads one u8 state, one object key and one u8 -- no row count and
	// no loop: it takes the FIRST byte as the state, bails out immediately when that
	// byte is zero, and otherwise resolves the key through 0x2699A30 (the same
	// actor-by-object-key entry the client plugin uses). The roster projection this
	// used to send prefixed a count byte (0x02) and then wrote (u16 key,u8 state)
	// per member, so the client consumed the count as the state and everything after
	// it shifted: the entry commit never ran. That is why a party entering a
	// hatchery kept rendering the PREVIOUS room's client-authored geometry -- the
	// doors and the 「前进」 marker of 能量阻截战 218's room stayed, because op29
	// re-authors the room but op3 is what commits it.
	//
	// Measured on the live client, 271 op29 events, no counter-example: every
	// connection that received the 4-byte record raised `lua enter_dungeon
	// confirmed` ~0.2 s after op29 (17186 @23:36:14Z, 17187 @00:50:37Z), and every
	// connection that received the 7-byte counted form never raised it at all
	// (party hatchery entries 17190 @2026-09-20T02:13:33Z and 17183 @03:07:23Z).
	//
	// 86JP writes this same single record for BOTH modes
	// (EnterSelectDungeonStateBuilder.cs:9-16), and this server's own solo path
	// already builds it with buildCurrentDungeonUserStateBody
	// (scene_post_start_map.go:17-26) -- the party path was the only one that
	// disagreed with the client.
	userStateBody, err := buildCurrentDungeonUserStateBody(currentSceneActorObjectKey(session.selectedCharacterID))
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyUserState), userStateBody, 0); err != nil {
		return err
	}
	seg.mark("op3_user_state")
	if err := s.sendCurrentFinishLoadingCharacterStateSnapshot(session, "party_dungeon_shared_loading_barrier"); err != nil {
		return err
	}
	session.currentFinishLoadingStateSent = true
	seg.mark("finish_loading_snapshot")
	if err := s.sendCurrentIncreaseStatusResult(session, "party_dungeon_shared_loading_barrier"); err != nil {
		return err
	}
	session.currentFinishLoadingCompletionSent = true
	seg.mark("increase_status")
	// The solo op37 path publishes the raid participation "loaded" event here;
	// the shared party barrier returns before it and used to skip that step
	// entirely. A party raid gate therefore stayed at progress=selected forever,
	// and the opener's own completion was rejected by the raid state machine
	// ("runtime raid dungeon event is out of order"), which aborted the inbound
	// packet and closed the member's socket mid-raid. Publish it for every party
	// loading cycle: a joined squadmate's binding belongs to the opener and only
	// the owner's attempt is recorded.
	if err := s.notifyRuntimeRaidDungeonLoaded(session); err != nil {
		// Raid bookkeeping never owns the party loading barrier: a missing or
		// already-retired raid attempt must not stop every member from finishing
		// its room projection.
		s.logGameEvent(session, "game-raid-dungeon-loaded-publish-deferred",
			"char_id", session.selectedCharacterID,
			"party_id", owner.partyID,
			"loading_cycle", loadingCycle,
			"raid_key", runtime.raidDungeon.RaidKey,
			"reason", "party_loading_release_continues",
			"error", err)
	}
	// 这一段在持 session.dungeon.mu 的情况下跑整个 M×(M-1) 扇出（见
	// raid_dungeon_lifecycle.go），所以它单独占一段。
	seg.mark("raid_loaded_fanout")
	if loadingCycle > 1 {
		// op19 rebuilds the selected actor's complete skill manager. That is
		// required once, after the party's initial actor binding, but repeating it
		// on every room transition destroys live P2P skill state such as a
		// teammate's Courage Blessing and its derived HUD counter. Ordinary solo
		// room moves already preserve this manager. Keep the same lifecycle for
		// party room moves and only refresh room placement and character status.
		session.dungeonSceneSkillInfoSent = true
		session.postFinishLoadingPlayerStateSent = true
		// The room payload this client just consumed respawns every party actor,
		// including members that are still ghosts. Re-assert the shared death
		// facts after the actor projection and before the consumable-use gate.
		if err := s.replayRuntimePartyDungeonDeathsAfterRoomLoad(
			session,
			owner,
			"party_dungeon_shared_loading_barrier_room_reload",
		); err != nil {
			return err
		}
		seg.mark("replay_deaths")
		if err := s.sendCurrentHellPartyMonsterInfoAfterFinishLoading(
			session,
			"party_dungeon_shared_loading_barrier_preserve_skill_manager",
		); err != nil {
			return err
		}
		seg.mark("hell_party_info")
		// 装载完成后补发卢克阶段复活币已用数（非卢克时内部直接返回）。
		s.syncRuntimeRaidPhaseCoinUsageAfterFinishLoading(
			session,
			"party_dungeon_shared_loading_barrier_preserve_skill_manager",
		)
		// 物件脚本门（上游 b402882）。
		s.publishCurrentDungeonActionObjectGateAfterFinishLoading(
			session,
			"party_dungeon_shared_loading_barrier_preserve_skill_manager",
		)
		if err := s.sendCurrentDungeonEntryConsumableUseGateReset(
			session,
			"party_dungeon_shared_loading_barrier_after_preserved_actor_projection",
		); err != nil {
			return err
		}
		seg.mark("op42_gate_reset")
		s.logGameEvent(session, "game-party-dungeon-loading-released",
			"party_id", owner.partyID, "char_id", session.selectedCharacterID,
			"local_slot", member.slot,
			"loading_cycle", loadingCycle,
			"packet_order", "op120-each-stable-slot-op3-all-op37-op30-preserve-live-skill-and-buff-manager-dead-state-replay-hell-party-data-only-in-encounter-room")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, found := s.selectedCharacterForEnter(ctx, session)
	if !found || charID != session.selectedCharacterID {
		return errors.New("party dungeon loading character record is unavailable")
	}
	seg.mark("selected_character_for_enter")
	if err := s.sendCurrentSceneSkillInfo(session, ctx, character, "party_dungeon_shared_loading_barrier"); err != nil {
		return err
	}
	session.dungeonSceneSkillInfoSent = true
	seg.mark("op19_scene_skill")
	// The shared party path bypasses sendCurrentPostStartMapPlayerPlacement.
	// Rehydrate registered Chronicle rows and op559 here, after the final op19
	// skill manager exists; otherwise party dungeons never bind the durable BUFF
	// enhancement set at all.
	if err := s.sendSkillSwitchInventoryAllDataRefresh(
		ctx,
		session,
		"party_dungeon_shared_loading_barrier_after_op19",
	); err != nil {
		return fmt.Errorf("rehydrate party dungeon BUFF registration: %w", err)
	}
	session.postFinishLoadingPlayerStateSent = true
	seg.mark("op559_chronicle_rehydrate")
	if err := s.sendCurrentHellPartyMonsterInfoAfterFinishLoading(
		session,
		"party_dungeon_shared_loading_barrier_after_initial_skill_state",
	); err != nil {
		return err
	}
	seg.mark("hell_party_info")
	// 装载完成后补发卢克阶段复活币已用数（非卢克时内部直接返回）。
	s.syncRuntimeRaidPhaseCoinUsageAfterFinishLoading(
		session,
		"party_dungeon_shared_loading_barrier_after_initial_skill_state",
	)
	// 物件脚本门（上游 b402882）。
	s.publishCurrentDungeonActionObjectGateAfterFinishLoading(
		session,
		"party_dungeon_shared_loading_barrier_after_initial_skill_state",
	)
	// The initial party projection installs op19 after op37/op30.  As with the
	// solo op120 placement, that native actor initialization can re-arm the
	// shared item-use wait byte. Keep bodyless class0/op42 last.
	if err := s.sendCurrentDungeonEntryConsumableUseGateReset(
		session,
		"party_dungeon_shared_loading_barrier_after_initial_actor_projection",
	); err != nil {
		return err
	}
	seg.mark("op42_gate_reset")
	s.logGameEvent(session, "game-party-dungeon-loading-released",
		"party_id", owner.partyID, "char_id", session.selectedCharacterID,
		"local_slot", member.slot,
		"loading_cycle", loadingCycle,
		"packet_order", "op120-each-stable-slot-op3-all-op37-op30-op19-op14-registered-chronicle-op559-hell-party-data-only-in-encounter-room")
	return nil
}

func buildRuntimePartyDungeonUserState(roster []runtimePartyDungeonMember) []byte {
	var writer packetWriter
	writer.writeByte(byte(len(roster)))
	for _, member := range roster {
		writer.writeUint16(member.userID)
		writer.writeByte(currentDungeonPlayerUserState)
	}
	return writer.bytes()
}
