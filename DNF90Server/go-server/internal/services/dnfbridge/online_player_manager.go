package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// onlinePlayerInfo is one player's town presence state.
type onlinePlayerInfo struct {
	CharacterID uint16
	AccountID   string
	Name        string
	Job         byte
	GrowType    byte
	Level       byte
	// ChannelID separates the copies of one town. Without it two characters
	// standing on the same map in different channels key into the same area
	// and become peers: they see each other, receive each other's movement,
	// and can interact across a channel boundary.
	ChannelID uint16
	TownID    byte
	AreaID    byte
	// InstanceKey partitions a visually identical town area into private
	// scene instances. Ordinary towns and the public raid lobby use zero;
	// the Anton waiting area uses the server-owned RaidKey.
	InstanceKey uint32
	PositionX   uint16
	PositionY   uint16
	Direction   byte
	AreaState   byte
	Session     *gameSession
}

// areaKey identifies a unique town area inside one channel. A channel is a
// separate copy of the same town, so the channel id belongs in the key.
type areaKey struct {
	ChannelID   uint16
	TownID      byte
	AreaID      byte
	InstanceKey uint32
}

// townPresenceChannelID is the channel component of every areaKey. It is the
// catalog id, not the u8 town-actor-owner byte: the byte only has to fit the
// client's scene table, while the key has to keep two channels apart even when
// both of their ids would truncate to the same byte.
func townPresenceChannelID(session *gameSession) uint16 {
	if session == nil || session.channel.ID <= 0 {
		return 0
	}
	return uint16(session.channel.ID)
}

// OnlinePlayerManager tracks online players by channel, town, area, and private
// scene instance and provides broadcast capabilities for town multiplayer
// visibility.
type OnlinePlayerManager struct {
	mu     sync.RWMutex
	byArea map[areaKey]map[uint16]*onlinePlayerInfo // areaKey -> characterID -> info
	byChar map[uint16]areaKey                       // characterID -> current area
}

func newOnlinePlayerManager() *OnlinePlayerManager {
	return &OnlinePlayerManager{
		byArea: make(map[areaKey]map[uint16]*onlinePlayerInfo),
		byChar: make(map[uint16]areaKey),
	}
}

// EnterArea registers a player in an area and returns the list of other
// players already in that area (for sending the full area list to the newcomer).
func (m *OnlinePlayerManager) EnterArea(info *onlinePlayerInfo) []onlinePlayerInfo {
	if m == nil || info == nil || info.CharacterID == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key := areaKey{
		ChannelID:   info.ChannelID,
		TownID:      info.TownID,
		AreaID:      info.AreaID,
		InstanceKey: info.InstanceKey,
	}

	// Remove from previous area if any.
	m.removeFromAreaLocked(info.CharacterID)

	// Add to new area.
	if m.byArea[key] == nil {
		m.byArea[key] = make(map[uint16]*onlinePlayerInfo)
	}
	m.byArea[key][info.CharacterID] = info
	m.byChar[info.CharacterID] = key

	// Collect other players in the area.
	others := make([]onlinePlayerInfo, 0, len(m.byArea[key])-1)
	for id, other := range m.byArea[key] {
		if id != info.CharacterID {
			others = append(others, *other)
		}
	}
	return others
}

// LeaveArea removes a player from their current area and returns the list of
// remaining players (for broadcasting the leave notification).
func (m *OnlinePlayerManager) LeaveArea(characterID uint16) (areaKey, []onlinePlayerInfo) {
	if m == nil || characterID == 0 {
		return areaKey{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key, ok := m.byChar[characterID]
	if !ok {
		return areaKey{}, nil
	}
	m.removeFromAreaLocked(characterID)

	// Collect remaining players.
	others := make([]onlinePlayerInfo, 0, len(m.byArea[key]))
	for _, other := range m.byArea[key] {
		others = append(others, *other)
	}
	return key, others
}

// LeaveAreaSession removes presence only when the disconnecting transport is
// still the transport that registered the town actor.  The current client
// opens short-lived same-character auxiliary game connections (for example
// the cross-channel/raid directory probe); those connections must not evict
// the resident town connection when they close.
func (m *OnlinePlayerManager) LeaveAreaSession(characterID uint16, session *gameSession) (areaKey, []onlinePlayerInfo, bool) {
	if m == nil || characterID == 0 || session == nil {
		return areaKey{}, nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key, ok := m.byChar[characterID]
	if !ok {
		return areaKey{}, nil, false
	}
	area := m.byArea[key]
	current := area[characterID]
	if current == nil || current.Session != session {
		return areaKey{}, nil, false
	}
	m.removeFromAreaLocked(characterID)
	others := make([]onlinePlayerInfo, 0, len(m.byArea[key]))
	for _, other := range m.byArea[key] {
		others = append(others, *other)
	}
	return key, others, true
}

func (m *OnlinePlayerManager) SessionForCharacter(characterID uint16) *gameSession {
	if m == nil || characterID == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.byChar[characterID]
	if !ok {
		return nil
	}
	info := m.byArea[key][characterID]
	if info == nil {
		return nil
	}
	return info.Session
}

func (m *OnlinePlayerManager) PlayerForCharacter(characterID uint16) (onlinePlayerInfo, bool) {
	if m == nil || characterID == 0 {
		return onlinePlayerInfo{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.byChar[characterID]
	if !ok {
		return onlinePlayerInfo{}, false
	}
	info := m.byArea[key][characterID]
	if info == nil {
		return onlinePlayerInfo{}, false
	}
	return *info, true
}

// UpdatePosition updates a player's position and returns the list of other
// players in the same area (for broadcasting the move notification).
func (m *OnlinePlayerManager) UpdatePosition(characterID uint16, posX, posY uint16) []onlinePlayerInfo {
	if m == nil || characterID == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key, ok := m.byChar[characterID]
	if !ok {
		return nil
	}
	info := m.byArea[key][characterID]
	if info == nil {
		return nil
	}
	info.PositionX = posX
	info.PositionY = posY

	others := make([]onlinePlayerInfo, 0, len(m.byArea[key])-1)
	for id, other := range m.byArea[key] {
		if id != characterID {
			others = append(others, *other)
		}
	}
	return others
}

// GetAreaPlayers returns players in the public (instance zero) form of an area
// of one channel. The channel id is required: the same town area exists once
// per channel, and a caller that omitted it would reach every copy of the town.
func (m *OnlinePlayerManager) GetAreaPlayers(channelID uint16, townID, areaID byte) []onlinePlayerInfo {
	return m.GetAreaInstancePlayers(channelID, townID, areaID, 0)
}

// GetAreaInstancePlayers returns only the actors in one concrete scene
// instance. This is required by Anton's private waiting map: different raid
// groups use the same PVF town/area ids but must never see each other.
func (m *OnlinePlayerManager) GetAreaInstancePlayers(
	channelID uint16,
	townID, areaID byte,
	instanceKey uint32,
) []onlinePlayerInfo {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := areaKey{
		ChannelID:   channelID,
		TownID:      townID,
		AreaID:      areaID,
		InstanceKey: instanceKey,
	}
	players := make([]onlinePlayerInfo, 0, len(m.byArea[key]))
	for _, info := range m.byArea[key] {
		players = append(players, *info)
	}
	return players
}

// GetChannelTownInstancePlayers returns every player registered in the public
// (instance zero) town of one channel, regardless of which area of that town
// they stand in. The channel-wide friend/guild context-menu experiment needs
// these peers: the current client only resolves a member row's peer object from
// its own name-keyed user table, and that table is fed by actor creation, so a
// same-channel member standing in another area has to be sent as an actor even
// though the two players never share a scene.
func (m *OnlinePlayerManager) GetChannelTownInstancePlayers(channelID uint16, excludeID uint16) []onlinePlayerInfo {
	if m == nil || channelID == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	players := make([]onlinePlayerInfo, 0, 8)
	for key, area := range m.byArea {
		if key.ChannelID != channelID || key.InstanceKey != 0 {
			continue
		}
		for id, info := range area {
			if info == nil || id == 0 || id == excludeID {
				continue
			}
			players = append(players, *info)
		}
	}
	sort.Slice(players, func(i, j int) bool {
		return players[i].CharacterID < players[j].CharacterID
	})
	return players
}

// GetChannelRaidInstancePlayers returns every player registered in one channel's
// raid instance area, i.e. the players whose areaKey carries the raid key as its
// instance component.
//
// GetChannelTownInstancePlayers deliberately keeps only instance zero, because it
// answers "who is in this channel's public town". A raid channel's waiting area
// (town 19 area 2, Town/BattleShip/BattleShip_Anton.map) is registered with
// InstanceKey == raidKey instead, so a member standing there is invisible to that
// pass. Measured: with the leader inside the raid instance and a member on the
// public deck, the member's client never received the leader's actor record, so
// the attack-party roster lost the leader's portrait and level; walking the leader
// onto the deck fixed it and walking back out broke it again.
func (m *OnlinePlayerManager) GetChannelRaidInstancePlayers(channelID uint16, instanceKey uint32, excludeID uint16) []onlinePlayerInfo {
	if m == nil || channelID == 0 || instanceKey == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	players := make([]onlinePlayerInfo, 0, 8)
	for key, area := range m.byArea {
		if key.ChannelID != channelID || key.InstanceKey != instanceKey {
			continue
		}
		for id, info := range area {
			if info == nil || id == 0 || id == excludeID {
				continue
			}
			players = append(players, *info)
		}
	}
	sort.Slice(players, func(i, j int) bool {
		return players[i].CharacterID < players[j].CharacterID
	})
	return players
}

// peerActorInfoForCharacter resolves the record sendTownRemoteActorState needs for
// one character.
//
// PlayerForCharacter answers from the public presence map, and a character inside
// a private scene -- the Seria room every ordinary login lands in, a raid
// channel's Seria room, the PVP lobby -- is deliberately never registered there,
// so the lookup misses. The actor record is still required: the client resolves an
// op7 prompt's source through its own user table and drops the whole notification
// when the key is absent (sub_1D82CF0 -> sub_2699A30 -> jz). Falling back to the
// bound session gives a record with the only two fields the send path reads,
// CharacterID and Session.
func (s *Service) peerActorInfoForCharacter(characterID uint16) (onlinePlayerInfo, bool) {
	if s == nil || characterID == 0 {
		return onlinePlayerInfo{}, false
	}
	if info, ok := s.onlinePlayers.PlayerForCharacter(characterID); ok {
		return info, true
	}
	session, ok := s.onlineGameSession(characterID)
	if !ok || session == nil || session.conn == nil {
		return onlinePlayerInfo{}, false
	}
	return onlinePlayerInfo{
		CharacterID: characterID,
		ChannelID:   townPresenceChannelID(session),
		Session:     session,
	}, true
}

// runtimeRaidMemberInsideOpenDungeonRun reports whether this raid already holds an
// open gate run for the member, i.e. whether that client is rendering a dungeon
// instance rather than a scene the channel owns.
//
// A run is open from the moment its gate is selected until it completes or is
// abandoned, which is exactly the window in which the member's client is inside the
// instance. It reads only the raid snapshot its caller owns: taking the target
// session's dungeon lock here would deadlock every caller that already holds it
// (notifyRuntimeRaidDungeonLifecycleLocked publishes these installs while the
// session's dungeon lock is held).
func runtimeRaidMemberInsideOpenDungeonRun(state runtimeRaidState, characterID uint16) bool {
	if characterID == 0 {
		return false
	}
	for _, progress := range state.Lifecycle.Dungeons {
		if progress.CharacterID != characterID {
			continue
		}
		if progress.Status == runtimeRaidDungeonProgressCompleted {
			continue
		}
		return true
	}
	return false
}

// runtimeRaidTargetSceneChannelOwned reports whether the receiving client is
// currently rendering a scene the channel owns -- a town, the raid waiting area,
// a channel-owned room -- as opposed to a dungeon instance, its settlement
// screen, or a selector/transition scene.
//
// The basis is session.townActorOwnerChannel (character_protocol_constants.go:50
// currentTownActorOwnerContext), the byte the client's global scene table keys a
// remote actor by. Every scene the channel owns commits it through
// currentConnectionTownActorOwnerContext (game_bootstrap.go:54-55,
// initial_town_route_state.go:117 and :176, initial_town_reconnect.go:25/:107/:193,
// initial_town_route_progress.go:181, return_select_character.go:119,
// dungeon_return_town.go:975 and :1741) and every scene the channel does not own
// resets it to currentSceneObjectContext (0) in resetDungeonEntrySceneGates
// (dungeon_return_town.go:1952), which the dungeon selector, dungeon navigation,
// practice room and party-dungeon entries all call. A byte of 0 is therefore "this
// client holds no channel-owned scene actor" -- a real channel never has ID 0
// (currentPartySceneOwnerContext, character_protocol_constants.go:63-73).
//
// It is read without session.townMu on purpose: the callers below publish these
// installs from notifyRuntimeRaidDungeonLifecycleLocked, which already holds
// session.dungeon.mu for the acting session, and the acting session is one of the
// targets here -- taking its town or dungeon lock again would self-deadlock. The
// field is a single byte, it is only ever compared against a constant, and the
// install already reads it lock-free to fill the target_local_owner log column.
func runtimeRaidTargetSceneChannelOwned(target *gameSession) bool {
	if target == nil || target.conn == nil {
		return false
	}
	return currentTownActorOwnerContext(target) != currentSceneObjectContext
}

// runtimeRaidPeerActorInstallMode is the answer the peer-actor install uses. It
// has two values rather than a yes/no because the only thing that must never
// reach a client rendering an instance is the mode1 binding.
type runtimeRaidPeerActorInstallMode byte

const (
	// runtimeRaidPeerActorInstallRecord sends mode0 only: the actor record the
	// client's own user table (roster portrait, level, name) is resolved from.
	runtimeRaidPeerActorInstallRecord runtimeRaidPeerActorInstallMode = iota
	// runtimeRaidPeerActorInstallBound additionally sends mode1, which binds the
	// peer's equipment and skill pages to the created object.
	runtimeRaidPeerActorInstallBound
)

// runtimeRaidPeerActorInstallModeFor decides how much of a peer's actor record a
// target client may receive.
//
// It is deliberately no longer a deny gate. Blocking the install inside an
// instance traded one bug for another: the member rending the Anton dungeon then
// saw every squad mate's roster portrait as the default one, because nothing ever
// created those records in its user table.
//
// The owner context does not need to change for a dungeon scene either. The
// client's own trace shows `mode0-owner-compare scene_channel=23` with
// `dungeon=218 / 219 / 220 / 222 / 223` in scope and `branch=current-scene-create`
// -- the same branch a town actor takes -- so a channel-owner body is created in
// the client's global scene table inside a raid instance exactly as it is in
// town. What differs there is what the *mode1* half does: it carries the peer's
// skill pages, and the client applies those to whichever actor it controls, which
// is its own dungeon actor ("帝血弑天 moved from 9999 onto 1111" at the 210 boss
// clear). Mode0 alone creates the record without that binding.
//
// 2026-09-29: the mode1 half is no longer sent ANYWHERE, the waiting area
// included, and the evidence is the operator's own rule that "过多的 mode1 下发会
// 导致严重卡顿". Live raid 1507329, 8 members, own build -- same fan-out, same
// members, same code path, the ONLY difference being whether the installs carried
// mode1:
//
//	phase-1 activation 00:41:08 (waiting area -> mode0+mode1):
//	    all EIGHT sessions  981ms 999ms 1.0129s 1.0129s 1.0219s 1.0226s 1.0244s 1.0254s
//	phase-2 activation 00:46:07 (already inside -> mode0 only):
//	    the SAME eight loops 351ms 351ms 373ms 373ms 374ms 376ms 377ms 377ms
//
// 1.0s versus 0.37s: the mode1 half is ~63% of a full-second freeze for every
// client in the raid. Its volume that session: 302 of 861 installs carried it and
// mode1 was 39.9% of all op2 bytes. The town co-presence path stopped sending it
// on 2026-09-26 for the same reason ("mode1 现在没有任何消费者" -- the client's
// combat-power feature is off) and peers render fine there without it, so nothing
// a waiting-area client needs lives in that half.
func runtimeRaidPeerActorInstallModeFor(state runtimeRaidState, target *gameSession) runtimeRaidPeerActorInstallMode {
	return runtimeRaidPeerActorInstallRecord
}

// sendRuntimeRaidPeerActor installs one character's actor record in another
// character's client, so a window or prompt that resolves the peer through the
// client's own user table has something to find. Failures are logged, never
// propagated: the caller is a gameplay step whose own result must not depend on a
// cosmetic actor record.
func (s *Service) sendRuntimeRaidPeerActor(state runtimeRaidState, target *gameSession, characterID uint16, source string) {
	s.sendRuntimeRaidPeerActorWithContext(context.Background(), state, target, characterID, source)
}

// sendRuntimeRaidPeerActorWithContext is sendRuntimeRaidPeerActor with the
// caller's context, so a fan-out can hand the projection path its batch-scoped
// read cache. Without one it behaves exactly like sendRuntimeRaidPeerActor.
func (s *Service) sendRuntimeRaidPeerActorWithContext(ctx context.Context, state runtimeRaidState, target *gameSession, characterID uint16, source string) {
	s.sendRuntimeRaidPeerActorWithMode(ctx, state, target, characterID, source, runtimeRaidPeerActorInstallModeFor(state, target))
}

// sendRuntimeRaidPeerActorRecord installs only the mode0 record (+ the op9 party frame)
// of one raid peer, without the mode1 binding half and whatever the receiving client's
// scene would otherwise select.
//
// It exists for prompts: the client resolves an op7 source through its own user table,
// which mode0 fills on its own (sendTownRemoteActorRecord documents the split), and it
// has no use for the peer's equipment/skill-page binding. The operator's report
// (2026-09-26) is why the cheaper form matters: the raid application built the mode1
// half of BOTH members in front of the leader's prompt, ~87 ms of equipment and PVF
// work for a record nobody asked to see.
func (s *Service) sendRuntimeRaidPeerActorRecord(state runtimeRaidState, target *gameSession, characterID uint16, source string) {
	s.sendRuntimeRaidPeerActorWithMode(
		context.Background(), state, target, characterID, source, runtimeRaidPeerActorInstallRecord)
}

// sendRuntimeRaidPeerActorWithMode is the shared body: it installs one character's actor
// record in another character's client with an explicit install mode.
func (s *Service) sendRuntimeRaidPeerActorWithMode(ctx context.Context, state runtimeRaidState, target *gameSession, characterID uint16, source string, mode runtimeRaidPeerActorInstallMode) {
	if s == nil || target == nil || target.conn == nil || characterID == 0 {
		return
	}
	if target.selectedCharacterID == characterID {
		return
	}
	// The body installed below carries the receiving channel's scene context
	// (currentPartySceneOwnerContext), which is also the context a raid instance
	// resolves, but the mode1 binding half is withheld from a client that is
	// rendering an instance or that has not committed a channel-owned scene: its
	// skill pages land on the locally controlled actor there. See
	// runtimeRaidPeerActorInstallModeFor.
	//
	// The completed-run window is the reason the mode is derived from both
	// halves. A run status alone misses it: live 2026-09-17 07:14:02.843Z
	// installed character 22 into character 1 with `raid_dungeon_completed_raid_mate_actors
	// target_local_owner=0 remote_owner=23` 44ms after the boss-death commit
	// (game-dungeon-ordinary-final-clear-committed, 07:14:02.798Z) and 85ms before
	// the completed lifecycle that owns the install was even logged
	// (game-raid-dungeon-lifecycle event=completed, 07:14:02.928Z, immediately
	// followed by game-dungeon-settlement-entry-op31-send). At that instant the
	// run was already Completed, so the run half alone would call it "town". The
	// scene half alone misses the opposite boundary, the moment between selecting
	// a gate and the client tearing its town scene down: the run is already
	// Selected while townActorOwnerChannel is still the channel (live 2026-09-17
	// 08:47:31.338Z, raid_dungeon_selected_raid_mate_actors target_local_owner=23).
	// Either way the actor record itself is still installed.
	actor, ok := s.peerActorInfoForCharacter(characterID)
	if !ok {
		s.logGameEvent(target, "game-raid-peer-actor-unavailable",
			"peer_char_id", characterID, "source", source)
		return
	}
	if err := s.sendTownRemoteActorProjectionWithContext(ctx, target, actor, source, mode == runtimeRaidPeerActorInstallBound); err != nil {
		s.logGameEvent(target, "game-raid-peer-actor-failed",
			"peer_char_id", characterID, "source", source, "error", err)
		return
	}
	s.logGameEvent(target, "game-raid-peer-actor-sent",
		"peer_char_id", characterID, "source", source,
		"mode", describeRuntimeRaidPeerActorInstallMode(mode))
}

func describeRuntimeRaidPeerActorInstallMode(mode runtimeRaidPeerActorInstallMode) string {
	if mode == runtimeRaidPeerActorInstallBound {
		return "mode0_record_and_mode1_binding"
	}
	return "mode0_record_only"
}

// syncRuntimeRaidMateActors gives this character and every online raid-mate each
// other's actor record.
//
// A raid roster renders a member row's portrait and level from the client's own
// user table, and that table is only fed by actor creation. Peers standing in the
// same area get theirs from ordinary co-presence; every other combination -- a
// mate inside the raid instance area, a mate inside a private room, a mate in
// another area of the same channel -- had nothing, so the roster silently degraded
// until the two happened to walk into the same area.
func (s *Service) syncRuntimeRaidMateActors(session *gameSession, source string) {
	s.syncRuntimeRaidMateActorsWith(session, source, s.sendRuntimeRaidPeerActor)
}

// syncRuntimeRaidMateActorRecords is syncRuntimeRaidMateActors with the record-only
// install form: the mode0 record and the op9 party frame, never the mode1 binding.
//
// It is the form the admission path uses, by the operator's rule (2026-09-27): a
// member that just got accepted needs the roster row to resolve its portrait and
// level from the client's own user table, and that is mode0's job alone. The mode1
// half installs the peer's equipment and skill pages -- measured on the wire at
// 679 B (leader) and 1373 B (member) plus a 14-slot equipment projection each --
// and nothing on the accepted-member path reads them. Same split the application
// prompt already uses (sendRuntimeRaidPeerActorRecord).
func (s *Service) syncRuntimeRaidMateActorRecords(session *gameSession, source string) {
	s.syncRuntimeRaidMateActorsWith(session, source, s.sendRuntimeRaidPeerActorRecord)
}

// syncRuntimeRaidMateActorsWith installs this character and every online raid-mate
// into each other's client, once per direction, through the caller's install form.
//
// It is deliberately ONE call per direction: the loop below already covers both
// halves of every pair, so a caller that also ran it for the other side of the same
// pair sent the identical records twice (live 2026-09-26 17:18:06Z, raid 1507329:
// 472/1373 bytes to the applicant at .404 and again at .529, 459/679 bytes to the
// leader at .434 and again at .463 -- 2983 B of a 6674 B acceptance).
func (s *Service) syncRuntimeRaidMateActorsWith(
	session *gameSession,
	source string,
	install func(runtimeRaidState, *gameSession, uint16, string),
) {
	if s == nil || session == nil || session.conn == nil {
		return
	}
	self := session.selectedCharacterID
	if self == 0 {
		return
	}
	state, ok := s.runtimeRaidForMember(self)
	if !ok {
		return
	}
	for _, member := range state.Members {
		if member.CharID == self {
			continue
		}
		mateSession, online := s.onlineGameSession(member.CharID)
		if !online || mateSession == nil || mateSession.conn == nil {
			continue
		}
		// Co-presence already installed both directions for the same area; the
		// duplicate would only repaint the same actor.
		if s.onlinePlayers.PeerInSameArea(self, member.CharID) {
			continue
		}
		install(state, session, member.CharID, source+"_raid_mate_into_self")
		install(state, mateSession, self, source+"_self_into_raid_mate")
	}
}

// syncRuntimeRaidMateActorsAll gives every pair of frozen raid members each
// other's actor record, without the same-area shortcut.
//
// Everything the raid draws about another member resolves that member's key in
// the client's own user table, which only actor creation fills: the 战况表 grid
// cell looks the key up (sub_F14C40 -> sub_2699A30) and hides the whole cell on a
// miss, and the standby room renderer next to it does the same (sub_15C8560 ->
// sub_15C87FC). Co-presence installs those records only while both characters
// stand in the same scene, and a raid deliberately scatters them: one squad is
// inside the instance while another waits outside, and a member that just changed
// scene lost the records the old scene carried (see
// currentChannelWidePeerActorsEnabled). That is why this variant is called from
// the scene boundaries themselves and never asks PeerInSameArea -- at those
// boundaries "same area" is precisely the state that just stopped being true.
func (s *Service) syncRuntimeRaidMateActorsAll(state runtimeRaidState, source string) {
	s.syncRuntimeRaidMateActorsAllAtSceneBoundary(state, source, false)
}

// syncRuntimeRaidMateActorsAllAtSceneBoundary is syncRuntimeRaidMateActorsAll
// with an optional "the receiver may be between scenes" guard.
//
// The `selected` boundary runs while the moving client is still tearing its old
// scene down: its channel-owned scene is already gone (the owner byte the client
// reports is 0) and the next one does not exist yet. A record installed into such
// a client lands in a scene that is about to be destroyed, and the client then
// walks that half-built entry from its own raid UI. Measured 2026-09-17 -- the
// only two hatchery entries after the 2026-09-17 18:20 (local) build:
//
//	11:05:52.017 selected(224) target_local_owner=0 -> client died 11:05:52.377
//	                                                (eip=0, ret0=0x0263A2ED, after
//	                                                 eight 0x025BD2FF skips)
//	10:31:39.375 selected(221) target_local_owner=0 -> client died 10:31:39.721
//	                                                (eip=0, ret0=0x025BD2FF)
//
// while the same session's 218 entry ran the same hook with target_local_owner=23
// and was fine, and every pre-18:20 hatchery entry reached `loaded` and
// `completed` (03:48/08:51/09:49). The `loaded` boundary fires once the scene
// exists and installs exactly the same record, so deferring here costs nothing
// the roster needs.
func (s *Service) syncRuntimeRaidMateActorsAllAtSceneBoundary(
	state runtimeRaidState, source string, deferWhileBetweenScenes bool) {
	if s == nil || len(state.FrozenMembers) < 2 {
		return
	}
	// 每个接收者都要一份同一个演员的完整投影，而投影只由演员自己的角色 ID 决定。
	// 这次扇出期间挂一个只读缓存，同一个演员在这批里只解析一次；函数返回即失效。
	ctx, _ := withProjectionReadCache(context.Background())
	// 同一次扇出内再挂一层「按演员复用投影 body」的缓存。
	// 56 个有序对的两份 body 只由演员与 remoteOwner 决定，而 remoteOwner 对
	// 同一个接收者是常量（currentTownRemoteActorOwnerContext 忽略 charID），
	// 所以 56 次构建会退化成约 M 次。见 actor_projection_bodies_cache.go。
	ctx = withActorProjectionBodiesCache(ctx)
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	runtimeRaidSortMembers(members)
	for _, self := range members {
		selfSession, online := s.onlineGameSession(self.CharID)
		if !online || selfSession == nil || selfSession.conn == nil {
			continue
		}
		if deferWhileBetweenScenes && !runtimeRaidTargetSceneChannelOwned(selfSession) {
			s.logGameEvent(selfSession, "game-raid-peer-actor-deferred",
				"char_id", self.CharID,
				"source", source,
				"reason", "scene_between_scenes",
				"local_owner", byte(currentTownActorOwnerContext(selfSession)))
			continue
		}
		// 把「对这个接收者的整批安装」投进**它自己的**信箱，而不是在发起者的
		// event loop 上同步写完 M 个接收者。
		//
		// 实测（2026-09-26 测试服，8 人攻坚）：body 构建已经靠缓存从 56 次降到
		// 8 次（cache hit 48 / miss 8 / distinct_keys 8），但 raid_loaded_fanout
		// 仍是 4271ms。4271ms ÷ 8 接收者 ≈ 534ms/人 ≈ 2 × gameWriterSyncBudget
		// (250ms) —— 剩下那一整段就是「同步写进每个接收者的 writer」，与构建无关。
		//
		// 这与战斗路径的既有约定一致：publishRuntimePartyDungeonCharacterState
		// 的注释原文是 "a dungeon command must never synchronously wait for another
		// member's mailbox"。进出图路径此前没有照做。
		//
		// 顺序不变：整批仍按 members 顺序执行，信箱本身是 FIFO；而且每个接收者现在
		// 是在自己的 loop 上写自己的 wireMu（此前是跨 loop 抢锁）。
		//
		// 2026-09-29 00:41:08 实测（8 人攻坚，自家构建）：整批作为**一个**任务投递时，
		// 8 个成员各自的 event loop 全部被占满约 1.0 秒
		// （raid_phase_activation_raid_mate_actors duration=981ms..1.025s，同一瞬间
		// 8 个会话全部命中）—— 全团每个客户端整整卡 1 秒，就是"严重卡顿"。5 分钟后
		// 二阶段同一次扇出只花 0.35 秒，唯一差别是那批安装不带 mode1 半边。也就是说
		// 单次任务耗时 ≈ 安装数 × 每次写入。分批不减少总工作量，只是让客户端自己的包
		// 在批次之间得到处理，而不是排在整批之后。
		receiver, selfID := selfSession, self.CharID
		mates := make([]uint16, 0, len(members))
		for index := range members {
			if members[index].CharID != selfID {
				mates = append(mates, members[index].CharID)
			}
		}
		for start := 0; start < len(mates); start += currentRaidMateActorInstallChunk {
			stop := start + currentRaidMateActorInstallChunk
			if stop > len(mates) {
				stop = len(mates)
			}
			chunk := append([]uint16(nil), mates[start:stop]...)
			if err := s.postGameSessionCharacterEvent(
				receiver,
				source,
				selfID,
				receiver.characterGeneration,
				func() error {
					started := time.Now()
					for _, mateID := range chunk {
						s.sendRuntimeRaidPeerActorWithContext(ctx, state, receiver, mateID, source)
					}
					s.logRuntimeRaidMateActorChunk(receiver, source, len(mates), len(chunk), time.Since(started))
					return nil
				},
			); err != nil {
				s.logGameEvent(receiver, "game-raid-peer-actor-fanout-deferred",
					"char_id", selfID, "source", source, "error", err)
				break
			}
		}
	}
	// 诊断：把这次扇出的缓存命中/未命中计数打出来。
	// 目的是判定「同一个演员为什么没有在 7 个接收者之间复用」——
	// 如果 miss_bind / miss_nobind 两边都很大，说明 withBinding 随接收者变化、
	// 把同一个演员拆成了两个键；如果 distinct_keys 接近 M×2，键就是在按接收者分裂。
	//
	// 注意：这些计数取自 `postGameSessionCharacterEvent` **之后**，而投递是异步的
	// （session.events.loop.SubmitBlocking，async=true），所以这里读到的 hit/miss/
	// build_ms/send_ms 通常仍是 0 —— 见 actor_projection_bodies_cache.go 的 2026-09-27
	// 实测：96 次 raid 边界扇出的 summary 全是 0。要拿真数必须把 summary 挪进投递的
	// 那个闭包里（本轮未改，避免动诊断语义）。
	if bodiesCache := actorProjectionBodiesCacheFromContext(ctx); bodiesCache != nil {
		bodiesCache.logSummary(s, source)
	}
}

// syncRuntimeRaidMateActorsMovedAtSceneBoundary installs only the pairs that one
// member's scene change actually invalidated.
//
// The four raid-dungeon lifecycle boundaries (selected / loaded / completed /
// abandoned) used to sweep every ordered pair of frozen members -- N*(N-1)
// installs per boundary -- but the event describes ONE member changing scene, and
// only two directions can be stale because of it:
//
//	"the member that just moved lost the records for everyone who stayed behind,
//	 and the members that stayed never had a record for the one inside"
//	   -- the rationale notifyRuntimeRaidDungeonLifecycleLocked carries at its
//	      fan-out call site in raid_dungeon_lifecycle.go
//
// So the moved member needs every mate's record and every mate needs the moved
// member's record: 2*(N-1) installs instead of N*(N-1). The remaining pairs have
// neither side changing scene, and every path that does invalidate them still
// runs: ordinary co-presence (broadcastTownPlayerEnter), the channel-wide pass
// for another area of the same channel (sendChannelWidePeerActors), a move
// between areas (raid_mate_actor_after_area_enter), admission of a new member
// (raid_admitted_member_actor_*), phase activation, and every non-boundary caller
// (syncRuntimeRaidMateActorsAll, which is left on the full sweep).
//
// Measured on 203.0.113.11, 600 MB of packet_log covering ~1.7 h (2026-09-27):
// the four boundaries produced 11,298 of the 11,663 peer-actor installs (96.9%),
// every one of them carrying a mode1 half, 58.9% a full one (19..31 equipment
// create rows, each row set costing a PVF attribute recompute). mode1 was 15.7 MB
// of the 21.1 MB those installs carried. The raid snapshot in that window froze
// 12 members, so one boundary cost 132 installs where 22 are stale.
//
// The sweep was also re-sending verbatim: in the same window 88.2% of the
// `loaded` boundary's installs and 57.5% of the `selected` boundary's were an
// identical (target, actor, mode1 length) triple already sent to the same target
// for the same actor within 10 s -- which is what a per-member event driving an
// every-pair sweep produces.
//
// The moved member is still deferred at `selected` while it is between scenes,
// exactly as in the full sweep; covering that defer is what `loaded` is for.
func (s *Service) syncRuntimeRaidMateActorsMovedAtSceneBoundary(
	state runtimeRaidState, movedCharID uint16, source string, deferWhileBetweenScenes bool) {
	if s == nil || len(state.FrozenMembers) < 2 {
		return
	}
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	runtimeRaidSortMembers(members)
	movedFrozen := false
	if movedCharID != 0 {
		for index := range members {
			if members[index].CharID == movedCharID {
				movedFrozen = true
				break
			}
		}
	}
	if !movedFrozen {
		// The event names a member this snapshot does not freeze, so the pair scope
		// is unknown. Keep the full sweep rather than drop records for a member
		// nobody would install.
		s.syncRuntimeRaidMateActorsAllAtSceneBoundary(state, source, deferWhileBetweenScenes)
		return
	}
	// Same two caches as the full sweep: one read cache for the actor records and
	// one per-actor body cache for the two projections this boundary rebuilds.
	ctx, _ := withProjectionReadCache(context.Background())
	ctx = withActorProjectionBodiesCache(ctx)
	// The scope the stayed-behind clients can be missing is the whole 战况表 ROW the
	// moved member appears in, not the moved member alone. See
	// runtimeRaidBoardRowInstallScope.
	rowScope := runtimeRaidBoardRowInstallScope(state, movedCharID)
	for _, self := range members {
		selfSession, online := s.onlineGameSession(self.CharID)
		if !online || selfSession == nil || selfSession.conn == nil {
			continue
		}
		if deferWhileBetweenScenes && !runtimeRaidTargetSceneChannelOwned(selfSession) {
			s.logGameEvent(selfSession, "game-raid-peer-actor-deferred",
				"char_id", self.CharID,
				"source", source,
				"reason", "scene_between_scenes",
				"local_owner", byte(currentTownActorOwnerContext(selfSession)))
			continue
		}
		receiver, selfID := selfSession, self.CharID
		if selfID != movedCharID {
			// A mate that stayed kept its own scene, but what it can be missing is
			// the record of ANY member of the row the moved member's squad draws --
			// the board resolves that cell through the record of the row's own
			// member, which is not necessarily the member that moved (see
			// runtimeRaidBoardRowInstallScope). So install the row, minus whoever
			// is receiving it. Install form stays record-only (no mode1 half), so
			// this is a few small packets, not a second equipment projection.
			installIDs := make([]uint16, 0, len(rowScope))
			for _, scopeID := range rowScope {
				if scopeID == 0 || scopeID == selfID {
					continue
				}
				if _, online := s.onlineGameSession(scopeID); !online {
					// The projection needs a live actor; an offline row member is
					// skipped exactly as the moved-client batch skips offline mates.
					continue
				}
				installIDs = append(installIDs, scopeID)
			}
			if len(installIDs) == 0 {
				// No row member is online but the mover (already covered above), so
				// keep the moved-only install rather than send nothing.
				installIDs = append(installIDs, movedCharID)
			}
			// Chunked on the same bound as the moved client's own batch: a receiver
			// loop is never handed more than currentRaidMateActorInstallChunk
			// installs in one task, which is the budget the measured ~50ms-per-
			// record-only-install stall was sized against.
			for start := 0; start < len(installIDs); start += currentRaidMateActorInstallChunk {
				stop := start + currentRaidMateActorInstallChunk
				if stop > len(installIDs) {
					stop = len(installIDs)
				}
				chunk := append([]uint16(nil), installIDs[start:stop]...)
				if err := s.postGameSessionCharacterEvent(
					receiver,
					source,
					selfID,
					receiver.characterGeneration,
					func() error {
						started := time.Now()
						for _, installID := range chunk {
							s.sendRuntimeRaidPeerActorWithContext(ctx, state, receiver, installID, source)
						}
						s.logRuntimeRaidMateActorChunk(receiver, source, len(installIDs), len(chunk), time.Since(started))
						return nil
					},
				); err != nil {
					s.logGameEvent(receiver, "game-raid-peer-actor-fanout-deferred",
						"char_id", selfID, "source", source, "error", err)
					break
				}
			}
			continue
		}
		// The moved client rebuilt its scene, so it lost every mate's record.
		//
		// This batch is the one place the moved-only scope still posts many
		// installs to a single loop, and it is split into chunks for the reason
		// measured live 2026-09-29 00:41:08 (8-member raid, phase activation,
		// the full sweep as ONE task per receiver): every member's own event loop
		// was blocked for ~1.0 s (duration=981ms..1.025s, all eight sessions at the
		// same instant), while the same fan-out carrying no mode1 half cost 0.35 s
		// on those same loops five minutes later. Installs x per-install write is
		// the multiplier. Chunking does not reduce the total work; it lets the
		// client's own packets run between the chunks instead of after all of them.
		mates := make([]uint16, 0, len(members))
		for index := range members {
			if members[index].CharID != selfID {
				mates = append(mates, members[index].CharID)
			}
		}
		for start := 0; start < len(mates); start += currentRaidMateActorInstallChunk {
			stop := start + currentRaidMateActorInstallChunk
			if stop > len(mates) {
				stop = len(mates)
			}
			chunk := append([]uint16(nil), mates[start:stop]...)
			if err := s.postGameSessionCharacterEvent(
				receiver,
				source,
				selfID,
				receiver.characterGeneration,
				func() error {
					started := time.Now()
					for _, mateID := range chunk {
						s.sendRuntimeRaidPeerActorWithContext(ctx, state, receiver, mateID, source)
					}
					s.logRuntimeRaidMateActorChunk(receiver, source, len(mates), len(chunk), time.Since(started))
					return nil
				},
			); err != nil {
				s.logGameEvent(receiver, "game-raid-peer-actor-fanout-deferred",
					"char_id", selfID, "source", source, "error", err)
				break
			}
		}
	}
	if bodiesCache := actorProjectionBodiesCacheFromContext(ctx); bodiesCache != nil {
		bodiesCache.logSummary(s, source)
	}
}

// runtimeRaidBoardRowInstallScope lists the members whose actor records a client
// needs before its 战况表 can draw the row the moved member belongs to.
//
// The board does not draw one cell per member: it draws one cell per (dungeon,
// squad) row, and the client resolves that cell through the record of ONE member
// of the row -- the first member key the 584 row carries (sub_F14C40 ->
// sub_2699A30 looks that key up in the client's own user table and hides the whole
// cell on a miss; the same lookup _probe_634_report.md tracks for the row's
// dungeon). Installing only the member that MOVED therefore leaves the cell
// unresolvable whenever the mover is not the row's first member, which is the
// ordinary case: a squad leader opens the gate last, or a squadmate enters alone.
//
// Live 2026-09-29 12:25:33 (raid 1507329, our build, 4 squads x 4 members):
//
//	12:25:33 selected map=218 char=35     <- squad 1's slot-1 member enters
//	12:25:36 loaded   map=218 char=35
//	12:31:01 the board rows published     P1[1 35 36 37] P2[2 39 40 41] ...
//	12:31:22 the operator's 2队 client: its own row fine, squad 1's row drawn as
//	         单人 with a black avatar -- that client had char 35's record (the
//	         member that moved) and never char 1's (the row's first member)
//	12:31:32 phase activation's full sweep installs every mate; the row draws
//
// So the install scope at a boundary is the ROW: the members of the squad the
// moved member's own roster byte names, plus the moved member when its live label
// no longer names a squad. The scope is capped by the squad size, which is what
// keeps this well below the every-pair sweep the move-only scope replaced.
func runtimeRaidBoardRowInstallScope(state runtimeRaidState, movedCharID uint16) []uint16 {
	if movedCharID == 0 {
		return nil
	}
	group := byte(0)
	if index := runtimeRaidMemberIndex(state, movedCharID); index >= 0 {
		group = runtimeRaidMemberSquad(state.Members[index].GroupIndex)
	} else if member, ok := runtimeRaidFrozenMember(state, movedCharID); ok {
		group = runtimeRaidMemberSquad(member.GroupIndex)
	}
	if group == 0 {
		// An unassigned member is its own 单人 row, so its own record is the scope.
		return []uint16{movedCharID}
	}
	members := runtimeRaidMembersInSquadLiveRows(state, group)
	for _, member := range members {
		if member == movedCharID {
			return members
		}
	}
	// The live roster no longer lists the moved member under this squad (its label
	// was cleared, or that row is gone): keep it in the scope rather than install a
	// row that omits the member this boundary is about.
	members = append(members, movedCharID)
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	return members
}

// currentRaidMateActorInstallChunk bounds how many peer actor installs one posted
// task may perform on a receiver's event loop. Two keeps a chunk near the write
// budget instead of the ~1 s a seven-install batch measured; see the call site.
const currentRaidMateActorInstallChunk = 2

// logRuntimeRaidMateActorChunk reports one chunk of the raid mate-actor fan-out so
// the next live raid can show whether the per-loop stall went down.
func (s *Service) logRuntimeRaidMateActorChunk(
	receiver *gameSession,
	source string,
	installs int,
	chunk int,
	elapsed time.Duration,
) {
	if s == nil || receiver == nil {
		return
	}
	s.logPacketEvent("game-raid-mate-actor-install-chunk",
		"conn_id", receiver.connID,
		"char_id", receiver.selectedCharacterID,
		"source", source,
		"installs_in_batch", installs,
		"installs_in_chunk", chunk,
		"chunk_wall_ms", raidMillis(elapsed),
		"chunk_size", currentRaidMateActorInstallChunk,
		"reason", "bounded_so_one_client_loop_is_never_blocked_by_the_whole_sweep")
	// Feed the per-raid cost summary: this is the number the operator reads back
	// after a raid of more than eight players (see raid_cost_summary.go).
	if raidKey := s.runtimeRaidKeyForMember(receiver.selectedCharacterID); raidKey != 0 {
		s.raidCostMember(raidKey, receiver.selectedCharacterID)
		s.noteRaidCostInstallChunk(raidKey, chunk, elapsed)
	}
}

// currentChannelWidePeerActorsEnabled is unconditionally true.
//
// It used to be an opt-in environment switch. It is not one any more because the
// behaviour it guards is required, not experimental: the client resolves a remote
// character only through the record that actor creation puts in its own table
// (sub_1A089E0 -> sub_1C5B070 looks the key up in [this+0x278] and silently does
// nothing when the lookup misses). Without those records a player in another area
// of the same channel simply has no entry, and every window that shows remote
// characters degrades -- the attack-party roster loses its squad column, level and
// portrait, and the friend/guild context menu falls back to its offline variant.
// Measured: stepping the outsider into the member own area fixed the roster, and
// switching channels and back broke it again, because the records arrive with
// co-presence and are lost with the scene.
func currentChannelWidePeerActorsEnabled() bool {
	return true
}

// sendChannelWidePeerActors installs an actor for every same-channel player who
// is standing in another area of the same town. The current client resolves a
// friend/guild member row's peer object only through its own name-keyed user
// table, and that table gains entries from actor creation: a member in another
// area of the same channel therefore has no record, the row's peer stays null,
// and the member context menu degrades to its offline variant even though the
// member is online in the player's own channel. Sending the peer actor gives the
// client that record, which is what the online menu items need.
//
// Peers already co-present in the same area are skipped: the ordinary
// co-presence path has already installed them.
func (s *Service) sendChannelWidePeerActors(session *gameSession, self *onlinePlayerInfo, source string) {
	if s == nil || session == nil || self == nil || !currentChannelWidePeerActorsEnabled() {
		return
	}
	channelID := townPresenceChannelID(session)
	if channelID == 0 {
		return
	}
	peers := s.onlinePlayers.GetChannelTownInstancePlayers(channelID, self.CharacterID)
	// A raid channel's waiting area registers with InstanceKey == raidKey, which
	// the public-town pass above filters out. Without this second pass a raid mate
	// standing there had no actor record at all on this client.
	if raidKey := s.runtimeRaidKeyForMember(self.CharacterID); raidKey != 0 {
		peers = append(peers, s.onlinePlayers.GetChannelRaidInstancePlayers(channelID, raidKey, self.CharacterID)...)
	}
	// 这次扇出挂上两层缓存，与另两条兄弟路径（broadcastTownPlayerEnter:1332、
	// syncRuntimeRaidMateActorsAllAtSceneBoundary:606）保持一致。
	//
	// 改前这里调的是 sendTownRemoteActorState（:995），它一路走到
	// sendTownRemoteActorProjection（:1029）并自己造一个 context.Background()，
	// 于是**两层缓存都没有**：仓储读不去重，投影 body 也不复用。
	//
	// 实测（2026-09-25/26 测试服）：game-town-copresence-actor-state-send 在同一个
	// 窗口里 1920 次，**比攻坚扇出自己的 1477 次还多** —— 城镇共现才是更大的
	// 生产者，而它此前完全没走缓存。本函数由 area enter / town move /
	// dungeon return 三条路径调用，每次都对全频道不同区域的每个 peer 重做一遍。
	ctx, _ := withProjectionReadCache(context.Background())
	ctx = withActorProjectionBodiesCache(ctx)
	for _, peer := range peers {
		if peer.TownID != self.TownID || peer.AreaID == self.AreaID {
			continue
		}
		if err := s.sendTownRemoteActorRecordWithContext(ctx, session, peer, source); err != nil {
			s.logGameEvent(session, "game-channel-wide-peer-actor-failed",
				"peer_char_id", peer.CharacterID,
				"peer_area_id", peer.AreaID,
				"error", err)
			continue
		}
		s.logGameEvent(session, "game-channel-wide-peer-actor-sent",
			"peer_char_id", peer.CharacterID,
			"peer_area_id", peer.AreaID,
			"peer_name", peer.Name,
			"source", source)
	}
}

// PeerInSameArea verifies that both characters are registered in the same
// concrete town area. A visible remote actor is only interactive inside this
// scope; an online session in another area is not a valid peer target.
func (m *OnlinePlayerManager) PeerInSameArea(sourceID, targetID uint16) bool {
	if m == nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	sourceKey, sourceOK := m.byChar[sourceID]
	targetKey, targetOK := m.byChar[targetID]
	if !sourceOK || !targetOK || sourceKey != targetKey {
		return false
	}
	area := m.byArea[sourceKey]
	return area != nil && area[sourceID] != nil && area[targetID] != nil
}

// PeerInSameChannel verifies that both characters are registered under the same
// channel component regardless of town, area, or private instance. Attack-party
// admission needs this wider scope: the applicant resolves the raid leader from
// the public directory while standing on the lobby deck, and the leader is
// already inside the raid's private waiting instance, so the two never share an
// areaKey even though the request is legitimate.
//
// Only the channel component is compared, exactly as PeerInSameArea compares the
// whole key: production sessions always carry a catalog channel id, so the
// comparison is meaningful there, and a registration that carries none must not
// be treated as a different channel or the admission path would fail closed.
func (m *OnlinePlayerManager) PeerInSameChannel(sourceID, targetID uint16) bool {
	if m == nil || sourceID == 0 || targetID == 0 || sourceID == targetID {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	sourceKey, sourceOK := m.byChar[sourceID]
	targetKey, targetOK := m.byChar[targetID]
	return sourceOK && targetOK && sourceKey.ChannelID == targetKey.ChannelID
}

// PeersInSameArea returns a stable snapshot of every other player registered
// beside characterID. Callers use it for appearance-only projections that do
// not change movement state or area ownership.
func (m *OnlinePlayerManager) PeersInSameArea(characterID uint16) []onlinePlayerInfo {
	if m == nil || characterID == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	key, ok := m.byChar[characterID]
	if !ok {
		return nil
	}
	area := m.byArea[key]
	peers := make([]onlinePlayerInfo, 0, len(area))
	for id, info := range area {
		if id == characterID || info == nil {
			continue
		}
		peers = append(peers, *info)
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].CharacterID < peers[j].CharacterID
	})
	return peers
}

func (m *OnlinePlayerManager) removeFromAreaLocked(characterID uint16) {
	key, ok := m.byChar[characterID]
	if !ok {
		return
	}
	delete(m.byChar, characterID)
	if area := m.byArea[key]; area != nil {
		delete(area, characterID)
		if len(area) == 0 {
			delete(m.byArea, key)
		}
	}
}

// publishCurrentTownPresence registers a completed selected-town actor in the
// shared scene directory and performs the native peer-creation exchange. Cold
// character selection and channel reconnect both finish a complete mode0,
// mode1 and op24 route without a client op36 SET_USER_AREA request, so they
// must publish explicitly at that server-owned completion boundary.
func (s *Service) publishCurrentTownPresence(
	session *gameSession,
	character dnfrepo.CharacterRecord,
	townID, areaID byte,
	row currentSceneTransitionRow,
	instanceKey uint32,
	mapPath string,
	source string,
) {
	if s == nil || session == nil ||
		session.selectedCharacterID == 0 || session.conn == nil {
		return
	}
	characterID := session.selectedCharacterID
	// Whatever this route decides, it owns the character's town registration
	// from here on. A dungeon-selector retirement memo left behind would only
	// let restoreTownPresenceAfterDungeonSelector resurrect a stale actor later.
	clearTownSelectorRetiredPresence(session)
	// 婚姻个人状态随"角色场景完成"一起下发：原生登录流程（冷选角 / 换频道重连）
	// 不会走 op36，所以这个边界是唯一可靠的恢复点。weddingStateSent 让重复经过
	// 同一路径时不再重发 1000（重发会清掉客户端的伴侣预览）。
	// 三个下发都是 best-effort：失败只记账，不能让城镇发布整体失败。
	if _, hasWedding := character.Stats["wedding_state"]; hasWedding || session.weddingStateSent.Load() {
		if err := s.sendWeddingCharacterState(session, character); err != nil {
			s.logGameEvent(session, "game-wedding-login-state-failed", "error", err)
		}
		if err := s.sendWeddingCapsuleState(session, character); err != nil {
			s.logGameEvent(session, "game-wedding-capsule-restore-failed", "error", err)
		}
		// 仪式恢复要在本次发布返回后再做：它自己会走传送/时间队列，
		// 在城镇注册尚未落定之前触发会读到过期的场景快照。
		defer func() {
			if err := s.restoreWeddingCeremony(session, character); err != nil {
				s.logGameEvent(session, "game-wedding-ceremony-restore-failed", "error", err)
			}
		}()
	}
	// 婚姻场景按夫妻对分实例（见 wedding_presence.go）。这一步必须早于下面的
	// privateRoomScene 分类：教堂/婚房是"按对分实例的公共场景"，不是私密场景。
	weddingKey, weddingScoped, weddingAdmitted := s.currentWeddingTownInstanceKey(
		character, townID, areaID, uint16(townPresenceChannelID(session)))
	if !weddingAdmitted {
		s.logGameEvent(session, "game-town-presence-wedding-rejected",
			"source", source, "char_id", characterID, "town_id", townID, "area_id", areaID,
			"reason", "wedding_reservation_or_marriage_required")
		return
	}
	if weddingScoped {
		instanceKey = weddingKey
	}
	// 婚房状态（家具/情侣树）既是"进婚房别看到空房间"的数据，也是**结婚戒指上婚房属性的
	// 唯一数据源**。原先只在 townID==婚房 时发一次，于是登录后不去婚房就永远没有这份数据
	//（戒指不显示属性），进图再回城更是彻底丢失。已婚角色在任何区域都要有它。
	if townID == currentWeddingRoomTownID {
		if err := s.sendWeddingRoomState(session, character); err != nil {
			s.logGameEvent(session, "game-wedding-room-restore-failed", "error", err)
			return
		}
	} else {
		s.restoreCurrentSceneWeddingRoomState(session, characterID, source)
	}
	// 决斗场认证面板(op48)+徽章钱包(op14)在每次城镇出现时下发（不限 PVP 频道）；
	// 每日任务清单(op323)只在 PVP 频道下发。总战绩频道另走认证面板。
	// 两个都是 best-effort：失败只记账，不能让城镇发布整体失败。
	if err := s.sendCurrentPVPDailyState(session); err != nil {
		s.logGameEvent(session, "game-pvp-daily-initial-failed", "error", err)
	}
	if err := s.sendCurrentPVPTotalTeamSelection(session); err != nil {
		s.logGameEvent(session, "game-pvp-total-team-initial-failed", "error", err)
	}
	// 上面的个人状态（婚姻 + 决斗场）属于"已完成演员"，即使没有在线玩家管理器也要下发；
	// 公开共处目录才开始需要 onlinePlayers。
	if s.onlinePlayers == nil {
		return
	}
	// Seria rooms, a raid channel's Seria room, and the PVP channel lobby (town 10
	// / area 0) are all character-private scenes even though every visitor uses
	// the same PVF town/area identifiers. Retire any public-town presence before
	// entering and never place this actor in byArea, so visitors never see each
	// other. Other PVP channel areas (battle-ready waiting areas etc.) stay public.
	seriaMap := townMapPathLooksLikeSeria(mapPath)
	raidGateRoom := currentRaidGateAreaIsSeriaRoom(session, townID, areaID)
	pvpLobbyRoom := isPVPChannel(session.channel) &&
		townID == byte(currentPVPTownID) && areaID == byte(currentPVPAreaID)
	privateRoomScene := seriaMap || raidGateRoom || pvpLobbyRoom
	privateRoomKind := "private_seria_room"
	if privateRoomScene && !seriaMap && !raidGateRoom {
		privateRoomKind = "private_pvp_lobby"
	}
	if privateRoomScene {
		privateDestination := &onlinePlayerInfo{
			CharacterID: characterID,
			ChannelID:   townPresenceChannelID(session),
			TownID:      townID,
			AreaID:      areaID,
			InstanceKey: instanceKey,
			PositionX:   row.Value1,
			PositionY:   row.Value2,
			Direction:   row.Value3,
			AreaState:   row.Value4,
			Session:     session,
		}
		s.closeCurrentExpertJobStoreSession(session, true)
		s.closeCurrentPrivateStoreSession(session, true)
		_, oldOthers := s.onlinePlayers.LeaveArea(characterID)
		if len(oldOthers) > 0 {
			s.broadcastTownPlayerAreaChange(
				privateDestination,
				oldOthers,
				runtimePartyStateSnapshot(session),
			)
		}
		s.promoteResidentGameSession(session, characterID)
		// A private Seria room is invisible to town co-presence, not to social
		// state: login lands here, so skipping the social announcements left
		// friends and guild members without an online notification until the
		// character walked out into the public town.
		s.publishCurrentSocialPresence(session, characterID, character.Name, source+"_"+privateRoomKind)
		s.logGameEvent(session, "game-town-presence-suppressed",
			"source", source,
			"char_id", characterID,
			"town_id", townID,
			"area_id", areaID,
			"map_path", mapPath,
			"retired_peer_count", len(oldOthers),
			"private_room_kind", privateRoomKind,
			"reason", privateRoomKind)
		// The PVP directory must survive an invisible lobby actor: it is the
		// only way clients browse/create/join rooms. sendCurrentPVPDirectory
		// is guarded internally by isPVPChannel + selectedCharacterID, so the
		// ordinary private Seria room simply no-ops here.
		if err := s.sendCurrentPVPDirectory(session); err != nil {
			s.logGameEvent(session, "game-pvp-directory-private-scene-failed", "error", err)
		}
		return
	}
	channelID := townPresenceChannelID(session)
	if current, ok := s.onlinePlayers.PlayerForCharacter(characterID); ok &&
		current.Session == session && current.ChannelID == channelID &&
		current.TownID == townID &&
		current.AreaID == areaID && current.InstanceKey == instanceKey {
		s.promoteResidentGameSession(session, characterID)
		s.logGameEvent(session, "game-town-presence-publish-skipped",
			"source", source,
			"char_id", characterID,
			"channel_id", channelID,
			"town_id", townID,
			"area_id", areaID,
			"instance_key", instanceKey,
			"reason", "same_session_scene_already_published")
		return
	}
	name := character.Name
	if name == "" {
		name = csharpSelectCharacterName(characterID)
	}
	player := &onlinePlayerInfo{
		CharacterID: characterID,
		AccountID:   character.AccountID,
		Name:        name,
		Job:         byte(numericCharacterStat(character.Job)),
		GrowType:    byte(numericCharacterStatValue(character, "grow_type")),
		Level:       byte(character.Level),
		ChannelID:   channelID,
		TownID:      townID,
		AreaID:      areaID,
		InstanceKey: instanceKey,
		PositionX:   row.Value1,
		PositionY:   row.Value2,
		Direction:   row.Value3,
		AreaState:   row.Value4,
		Session:     session,
	}
	others := s.onlinePlayers.EnterArea(player)
	s.promoteResidentGameSession(session, characterID)
	s.broadcastTownPlayerEnter(player, others)
	s.sendChannelWidePeerActors(session, player, "channel_wide_peer_actor_after_area_enter")
	// Walking out of a private scene, or between two areas of the raid channel,
	// makes the client drop the actors that are no longer co-present. Raid mates
	// are not co-present by definition in those cases, so re-install them.
	s.syncRuntimeRaidMateActors(session, "raid_mate_actor_after_area_enter")
	s.publishCurrentSocialPresence(session, characterID, player.Name, source)
	s.replayCurrentExpertJobStores(session, townID, areaID)
	s.replayCurrentPrivateStores(session, townID, areaID)
	s.logGameEvent(session, "game-town-presence-published",
		"source", source,
		"char_id", characterID,
		"town_id", townID,
		"area_id", areaID,
		"instance_key", instanceKey,
		"map_path", mapPath,
		"position_x", row.Value1,
		"position_y", row.Value2,
		"direction", row.Value3,
		"area_state", row.Value4,
		"peer_count", len(others),
		"sequence", "remote_mode0_then_mode1_then_op9_then_op23_both_directions")
	if err := s.sendCurrentPVPDirectory(session); err != nil {
		s.logGameEvent(session, "game-pvp-directory-initial-failed", "error", err)
	}
}

// --- Broadcast packet builders ---
//
// IDA-verified wire formats (current 23.4.15.0 NoPack EXE):
//
//	NOTI 0x0016 (sub_1D83990): u16 actorKey, u16 x, u16 y, u8 direction, u16 movementRate
//	NOTI 0x0017 (sub_1D89590): u16 actorKey, u8 town, u8 area, u16 x, u16 y, u8 areaState, u8 direction
//	NOTI 0x0006 (C# ref):      u16 userId

const (
	currentTownAreaUserListMsgID uint16 = 0x0018
	currentTownUserLeaveMsgID    uint16 = 0x0006
)

// buildTownAreaUserListBody builds the current EXE sub_1D901D0 layout:
// u8 town, u8 area, u16 count, repeat(u16 actorKey, u16 x, u16 y, u8 dir, u8 state).
//
// This is more than a co-presence roster. The final row for the selected
// character rebinds the local scene actor used by town camera tracking.
func buildTownAreaUserListBody(townID, areaID byte, players []onlinePlayerInfo) []byte {
	count := len(players)
	if count > 0xFFFF {
		count = 0xFFFF
	}
	body := make([]byte, 4+count*8)
	body[0] = townID
	body[1] = areaID
	binary.LittleEndian.PutUint16(body[2:4], uint16(count))
	for i := 0; i < count; i++ {
		offset := 4 + i*8
		binary.LittleEndian.PutUint16(body[offset:offset+2], currentSceneActorObjectKey(players[i].CharacterID))
		binary.LittleEndian.PutUint16(body[offset+2:offset+4], players[i].PositionX)
		binary.LittleEndian.PutUint16(body[offset+4:offset+6], players[i].PositionY)
		body[offset+6] = players[i].Direction
		body[offset+7] = players[i].AreaState
	}
	return body
}

// buildTownUserLeaveBody builds NOTI 0x0006 body: u16 userId.
func buildTownUserLeaveBody(characterID uint16) []byte {
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, characterID)
	return body
}

// currentTownRemoteActorOwnerContext returns the receiving session's committed
// town-channel owner. Current NoPack sub_2009160 treats an owner equal to the
// resident town channel as a global scene actor and creates it by its distinct
// uint16 object key through sub_20036C0. A character-ID-derived owner instead
// selects the auxiliary context-object branch; that object can be drawn, but it
// is absent from the native global table used by player interaction menus.
func currentTownRemoteActorOwnerContext(target *gameSession, _ uint16) byte {
	// The selected local actor intentionally uses context 0 in mode0/mode1,
	// but remote co-presence actors must remain in the channel-owned global
	// scene table used by the party and interaction handlers.
	return currentPartySceneOwnerContext(target)
}

// sendTownRemoteActorState installs one repository-backed peer actor in the
// target client's town scene. 86JP establishes co-presence with both appearance
// and full user-info records before the area-state notification. The current
// 90CN client uses its typed mode0/mode1 equivalents instead of 86JP's legacy
// USERINFO subtype0/subtype1 bodies.
func (s *Service) sendTownRemoteActorState(
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
) error {
	return s.sendTownRemoteActorProjection(target, actor, source, true)
}

// sendTownRemoteActorStateWithContext is sendTownRemoteActorState with the
// caller's context, so a fan-out can hand the projection path its batch-scoped
// read cache. Without one it behaves exactly like sendTownRemoteActorState.
func (s *Service) sendTownRemoteActorStateWithContext(
	ctx context.Context,
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
) error {
	return s.sendTownRemoteActorProjectionWithContext(ctx, target, actor, source, true)
}

// sendTownRemoteActorRecord installs only the mode0 actor record plus the op9
// party frame, leaving out the mode1 binding. It is the form a target client
// that is rendering a dungeon instance receives: mode0 is what its own user
// table -- and therefore every roster portrait, level and name -- resolves the
// peer from, while mode1 is the half that binds the peer's equipment and skill
// pages and therefore the half that must not reach an instance.
func (s *Service) sendTownRemoteActorRecord(
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
) error {
	return s.sendTownRemoteActorProjection(target, actor, source, false)
}

// sendTownRemoteActorRecordWithContext is sendTownRemoteActorRecord with the
// caller's context, so a fan-out can hand the projection path its batch-scoped
// read and body caches.
//
// 只用于**同频道跨区域**的那条共现扇出（sendChannelWidePeerActors）。
//
// 依据是客户端自己的实现，不是猜的。90CN.cpp 的 ProxyClass0Dispatch 在原生处理器
// 之后留着一段注释，原文是：The native handler has already accepted and applied
// this packet. Cache only the complete local mode1/mode3 stat block for the Lua
// calculator; remote town actors and incomplete bodies are deliberately ignored.
//
// 也就是说 mode1 描述的是**接收者自己**的 HP/MP/六维与装备行，原生处理器会把它应用
// 到本地受控角色并覆盖本地快照；90CN 之所以要加 !currentActorMatch -> return 这道
// 守卫，正是因为服务端会给 peer 也发一份。那道守卫每进程只报 8 次就不报了。
//
// 跨区域 peer 的 mode1 语义上必然用不上：他和接收者不在同一张图，那份属性快照只会
// 污染接收者自己的战斗力基准。参考端同样把这条写死 —— 区域共存广播只允许 USERINFO
// MINIMUM(subtype0/occ0)，完整 subtype1 属于角色自身初始化。
//
// 刻意**不**动的：broadcastTownPlayerEnter 的两个方向。它们少了 mode1 会打乱
// town_move_test.go:616/690/708/783 与 online_player_manager_test.go:497 钉死的
// mode0 -> mode1 -> op9 -> 0x0017 线序契约；那是一套被多处字节级断言锁住的协议
// 契约，要不要跟着改是产品决定，不在这条改动里。
func (s *Service) sendTownRemoteActorRecordWithContext(
	ctx context.Context,
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
) error {
	return s.sendTownRemoteActorProjectionWithContext(ctx, target, actor, source, false)
}

func (s *Service) sendTownRemoteActorProjection(
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
	withBinding bool,
) error {
	return s.sendTownRemoteActorProjectionWithContext(context.Background(), target, actor, source, withBinding)
}

// sendTownRemoteActorProjectionWithContext is sendTownRemoteActorProjection
// derived from the caller's context instead of context.Background. A fan-out
// installs its batch-scoped read cache on that context; every other caller
// passes context.Background and gets the original behaviour byte for byte.
func (s *Service) sendTownRemoteActorProjectionWithContext(
	parent context.Context,
	target *gameSession,
	actor onlinePlayerInfo,
	source string,
	withBinding bool,
) error {
	if target == nil || target.conn == nil || actor.Session == nil || actor.CharacterID == 0 {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithTimeout(parent, createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.characterForEnter(
		ctx,
		actor.Session,
		actor.CharacterID,
	)
	if charID != actor.CharacterID || !hasCharacter {
		return fmt.Errorf(
			"remote town actor repository record unavailable: requested=%d loaded=%d found=%t",
			actor.CharacterID,
			charID,
			hasCharacter,
		)
	}

	remoteOwner := currentTownRemoteActorOwnerContext(target, charID)
	if remoteOwner == currentSceneObjectContext {
		return fmt.Errorf("remote town actor owner channel is not committed")
	}
	// 一次扇出内按**演员维度**复用建好的两份 body。
	//
	// 这两份 body 里与接收者相关的**只有一个字节**：偏移 4 的 ownerChannel
	// （字符集见 actor_projection_bodies_cache.go）。而
	// currentTownRemoteActorOwnerContext(target, _) 显式忽略 charID（:978-983），
	// 只取决于接收者。所以缓存键**不含** owner：按演员存一份「字节 4 归零」的
	// body，取出来再按接收者补那一个字节，最终字节与逐对重建**逐字相同**。
	//
	// ⚠️ 第一版把 ownerChannel 放进了键里，于是每个接收者各建一遍、命中率为零，
	// 一次边界只从 6.0s 降到 4.5s。别再犯。
	bodiesKey := actorProjectionBodiesKey{
		charID:      charID,
		withBinding: withBinding,
	}
	bodiesCache := actorProjectionBodiesCacheFromContext(ctx)
	mode0Body, mode1Body, bodiesCached := bodiesCache.lookup(bodiesKey)
	buildStarted := time.Now()
	if !bodiesCached {
		var buildErr error
		// 对端 actor 的 mode-0 行必须清掉副职业字段：观看者客户端是"从 0 新建"这条记录，
		// 带非零副职业字节会被当成"刚获得"并播放获得特效（2026-10-08 现场：B 回城后
		// 看到已回城的 A 身上冒副职业特效）。mode1 不承载该字段，保持原记录。
		mode0Body, buildErr = s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
			ctx,
			actor.Session,
			charID,
			charName,
			character,
			hasCharacter,
			remoteOwner,
		)
		if buildErr != nil {
			return fmt.Errorf("build remote town actor mode0: %w", buildErr)
		}
		adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(
			ctx,
			actor.Session,
			character,
			hasCharacter,
		)
		// includeEquipment 必须跟随 withBinding。
		//
		// withBinding 只决定"发不发"（下面 `if withBinding` 才发 mode1），
		// 但这里原本硬编码 true，于是每一对都把 ~31 行装备绑定整份建出来再丢掉。
		// 实测一次 loaded 边界的 112 次发送里 withBinding 对**每一个**都是 false，
		// 也就是说一半的构建量是纯废工。
		//
		// 身份校验只需要 mode1 的 raw16+objectKey 前缀，那段写在 includeEquipment
		// 分支**之前**（userinfo_current_modes.go:161-164），关掉装备行不削弱校验。
		mode1Body = s.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
			ctx,
			actor.Session,
			character,
			hasCharacter,
			charID,
			uint32(adventureSummary.ManageLevel),
			remoteOwner,
			withBinding,
		)
		bodiesCache.store(bodiesKey, mode0Body, mode1Body)
	}
	bodiesCache.noteBuild(time.Since(buildStarted))
	// 缓存里按 0 存，这里补上本接收者的 owner 字节（偏移 4）。
	// 未命中时刚构建出来的 body 本来就是该接收者的值，这一步是等值重写。
	applyActorProjectionOwnerChannel(mode0Body, remoteOwner)
	applyActorProjectionOwnerChannel(mode1Body, remoteOwner)
	// The installed character's own key must be the one both bodies carry. mode0
	// writes it after its fixed 0x47-byte actor state and mode1 after its 21-byte
	// prefix; validateCurrentPartyActorProjectionRows is the same contract the
	// selected actor's own cached projection is held to. Without this check a body
	// built from the wrong record is indistinguishable from a correct one on the
	// wire -- the fields this function logs are its arguments, not the encoded
	// bytes -- and the client resolves the record purely by the key inside it.
	if _, _, identityErr := validateCurrentPartyActorProjectionRows(charID, mode0Body, mode1Body); identityErr != nil {
		s.logGameEvent(target, "game-town-copresence-actor-identity-rejected",
			"source", source,
			"target_char_id", target.selectedCharacterID,
			"actor_char_id", charID,
			"remote_owner", remoteOwner,
			"mode0_body_len", len(mode0Body),
			"mode1_body_len", len(mode1Body),
			"error", identityErr)
		return fmt.Errorf("remote town actor identity rejected: %w", identityErr)
	}
	s.logPacketEvent("game-town-copresence-actor-state-send",
		"conn_id", target.connID,
		"channel_id", target.channel.ID,
		"source", source,
		"target_char_id", target.selectedCharacterID,
		"target_local_owner", currentTownActorOwnerContext(target),
		"actor_char_id", charID,
		"object_key", currentSceneActorObjectKey(charID),
		"remote_owner", remoteOwner,
		"actor_job", numericCharacterStat(character.Job),
		"actor_grow_type", numericCharacterStatValue(character, "grow_type"),
		"actor_level", character.Level,
		"mode0_body_len", len(mode0Body),
		"mode1_body_len", len(mode1Body),
		"body_source", "current_exe_mode0_create_then_mode1_bind_then_same_owner_op9_party_state_before_area_state")
	sendStarted := time.Now()
	if err := s.sendGameUpperRawClass(
		target,
		uint16(dnfenum.CmdPacketSetUDPIPPort),
		mode0Body,
		0,
	); err != nil {
		return fmt.Errorf("send remote town actor mode0: %w", err)
	}
	if withBinding {
		if err := s.sendGameUpperRawClass(
			target,
			uint16(dnfenum.CmdPacketSetUDPIPPort),
			mode1Body,
			0,
		); err != nil {
			return fmt.Errorf("send remote town actor mode1: %w", err)
		}
	}

	// Current EXE sub_1D64CA0 kind0 does not create an actor. Resolve and send
	// the actor's actual runtime party state only after mode0 globally created
	// and mode1 bound the remote object. An empty state clears stale join-party
	// menu state; an explicitly created solo party remains joinable.
	actorParty := runtimePartyStateSnapshot(actor.Session)
	if _, err := s.sendCurrentPartyActorFrameProjection(
		target,
		actor.Session,
		actorParty,
		"town_copresence_actor_initial_party_state",
	); err != nil {
		return fmt.Errorf("send remote town actor op9 display: %w", err)
	}
	// When the observer and the actor fight in the same ordinary party, that record IS
	// the observer's own party model, and op153 is the only carrier of the per-member
	// HP/MP rows (party.BuildSingleMemberRealtimeInfo). Ending the observer's frame on
	// it leaves the party HUD health bars empty -- which is what the operator sees
	// whenever a squadmate re-enters the area: entering a raid gate, returning to town,
	// or a phase-end forced return all run this path. Live 2026-09-20T13:16:52Z, conn
	// game-000169 (char 1 entering 224): three consecutive op9 records for chars
	// 20/22/21 with party_model_key=4 and no op153 behind any of them. Send the
	// observer its OWN rows, from the observer's own state -- never the actor's.
	if observerParty := runtimePartyStateSnapshot(target); observerParty.PartyID > 0 &&
		observerParty.PartyID == actorParty.PartyID {
		if err := s.sendRuntimePartyRealtimeInfoLocal(target, observerParty); err != nil {
			return fmt.Errorf("send remote town actor op153 realtime rows: %w", err)
		}
	}
	// 累计「发包」耗时，与 noteBuild 一起回答那 4.3 秒究竟在构建还是在投递。
	bodiesCache.noteSend(time.Since(sendStarted))
	return nil
}

// sendTownRemoteActorAppearanceRefresh repaints one already-created peer actor in
// the target client's town scene. It is sendTownRemoteActorState without the party
// frame: mode0 is rebuilt from the repository in the same remote owner context the
// actor was created in, with the actor-state initialization event suppressed
// because the actor already exists, and mode1 re-binds it with its equipment.
//
// guild is the ACTOR's guild, already resolved by the caller. It is a parameter
// rather than a lookup here because this runs once per (receiver, actor) ordered
// pair: resolving it inside turned the projection quadratic in repository reads,
// which stress_raid_projection_test.go caught. Nil means the actor is in no guild
// and the aura block is skipped (a fresh nonmember already carries the native -1
// insignia indices).
func (s *Service) sendTownRemoteActorAppearanceRefresh(
	target *gameSession,
	actor onlinePlayerInfo,
	guild *dnfguild.Guild,
	source string,
) error {
	if target == nil || target.conn == nil || actor.Session == nil || actor.CharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, charName, character, hasCharacter := s.characterForEnter(
		ctx,
		actor.Session,
		actor.CharacterID,
	)
	if charID != actor.CharacterID || !hasCharacter {
		return fmt.Errorf(
			"remote actor appearance record unavailable: requested=%d loaded=%d found=%t",
			actor.CharacterID,
			charID,
			hasCharacter,
		)
	}
	remoteOwner := currentTownRemoteActorOwnerContext(target, charID)
	if remoteOwner == currentSceneObjectContext {
		return fmt.Errorf("remote town actor owner channel is not committed")
	}
	// 对端 actor 的 mode-0 行必须清掉副职业字段（同 sendTownRemoteActorProjectionWithContext）：
	// 观看者客户端是"从 0 新建"这条记录，带非零副职业字节会被当成"刚获得"并播获得特效。
	// 这条是**穿脱装备时的对端重绘**，因此现场表现为"穿脱勋章时他人视角出现一次特效"
	//（2026-10-08）。
	mode0Body, err := s.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		ctx,
		actor.Session,
		charID,
		charName,
		character,
		hasCharacter,
		remoteOwner,
	)
	if err != nil {
		return fmt.Errorf("build remote actor appearance mode0: %w", err)
	}
	if !suppressCurrentSceneObjectActorStateEvent(mode0Body) {
		return fmt.Errorf("remote actor appearance mode0 could not suppress actor-state event")
	}
	adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(
		ctx,
		actor.Session,
		character,
		hasCharacter,
	)
	mode1Body := s.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx,
		actor.Session,
		character,
		hasCharacter,
		charID,
		uint32(adventureSummary.ManageLevel),
		remoteOwner,
		true,
	)
	s.logPacketEvent("game-town-actor-appearance-refresh-send",
		"conn_id", target.connID,
		"channel_id", target.channel.ID,
		"source", source,
		"target_char_id", target.selectedCharacterID,
		"actor_char_id", charID,
		"remote_owner", remoteOwner,
		"mode0_body_len", len(mode0Body),
		"mode1_body_len", len(mode1Body),
		"body_source", "character_equipment_repository_existing_actor_mode0_event_suppressed_then_equipment_mode1")
	if err := s.sendGameUpperRawClass(
		target,
		uint16(dnfenum.CmdPacketSetUDPIPPort),
		mode0Body,
		0,
	); err != nil {
		return fmt.Errorf("send remote actor appearance mode0: %w", err)
	}
	if err := s.sendGameUpperRawClass(
		target,
		uint16(dnfenum.CmdPacketSetUDPIPPort),
		mode1Body,
		0,
	); err != nil {
		return fmt.Errorf("send remote actor appearance mode1: %w", err)
	}
	// The peer now has this actor, so replay its guild insignia decoration. The
	// aura rides the actor appearance block rather than the guild window, and the
	// peer built this actor from scratch, so without it the newcomer renders with
	// no insignia until something else rebuilds the actor. guild is a parameter
	// because this runs once per (receiver, actor) ordered pair: resolving it here
	// turned the projection quadratic in repository reads.
	if guild != nil {
		if err := s.writeCurrentGuildAuraState(target, charID, guild, remoteOwner); err != nil {
			return fmt.Errorf("send remote town actor guild aura: %w", err)
		}
	}
	return nil
}

// broadcastTownActorAppearanceRefresh repaints this player's actor on every client
// standing in the same town area after the player's own appearance changed.
//
// The wardrobes/equipment refresh only ever repainted the owner's own client
// (PostActionRefreshSelectedActorAppearance -> sendSelectedActorMode0AppearanceRefresh),
// so a peer kept drawing the old look until the owner re-entered the scene. The
// live 2026-09-12 20:04 run is exactly that: 1111 took the avatars off, B still
// saw them on, and only 1111's relogin (a fresh co-presence broadcast) made B
// repaint. Peers are re-sent the same appearance projection the co-presence path
// uses, which is what makes the change visible immediately.
func (s *Service) broadcastTownActorAppearanceRefresh(session *gameSession, source string) {
	if s == nil || session == nil || s.onlinePlayers == nil {
		return
	}
	charID := selectedCharacterID(session)
	if charID == 0 {
		return
	}
	self, found := s.onlinePlayers.PlayerForCharacter(charID)
	if !found {
		s.logGameEvent(session, "game-town-actor-appearance-refresh-skipped",
			"char_id", charID,
			"reason", "self_not_registered_in_town_area",
			"source", source)
		return
	}
	peers := s.onlinePlayers.PeersInSameArea(charID)
	// The 公会徽章 decoration depends only on the ACTOR, so resolve it once for the
	// whole fan-out instead of once per receiver.
	auraCtx, auraCancel := s.currentGuildContext()
	guild, guildErr := s.currentGuildForCharacterID(auraCtx, charID)
	auraCancel()
	if guildErr != nil {
		s.logGameEvent(session, "game-town-actor-appearance-guild-lookup-failed",
			"char_id", charID, "error", guildErr)
		guild = nil
	}
	sent := 0
	for _, peer := range peers {
		if peer.Session == nil || peer.CharacterID == 0 || peer.CharacterID == charID {
			continue
		}
		if err := s.sendTownRemoteActorAppearanceRefresh(peer.Session, self, guild, source); err != nil {
			s.logGameEvent(session, "game-town-actor-appearance-refresh-failed",
				"peer_char_id", peer.CharacterID,
				"error", err)
			continue
		}
		sent++
	}
	s.logGameEvent(session, "game-town-actor-appearance-refresh",
		"char_id", charID,
		"peer_count", len(peers),
		"sent", sent,
		"source", source)
}

// broadcastTownPlayerEnter sends each remote actor's current typed mode0,
// equipment-bearing mode1, same-owner op9 display registration, then
// incremental 0x0017 state. The town transition owner has already sent one
// complete authoritative op24 packet; replaying a second self-containing op24
// here rebinds the local actor and breaks camera ownership.
func (s *Service) broadcastTownPlayerEnter(newPlayer *onlinePlayerInfo, others []onlinePlayerInfo) {
	if s == nil || s.onlinePlayers == nil || newPlayer == nil || newPlayer.Session == nil {
		return
	}
	// 两个循环都是 (接收者, 演员) 扇出：第一个循环把同一个新人的投影发给 N-1 个
	// 接收者，第二个循环把 N-1 个演员的投影发给同一个接收者。投影只由演员自己的
	// 角色 ID 决定，所以这次扇出期间挂一个只读缓存，每个演员只解析一次。
	ctx, _ := withProjectionReadCache(context.Background())
	// ★ 补上 body 缓存。兄弟路径 sendChannelWidePeerActors 早就挂了两层
	// （:726-727），这里此前只挂了读缓存 —— 这就是「B 进区域 0，A 也跟着卡一下」
	// 那一条路径的漏洞。
	//
	// 关键在于下面【第一个循环】：它的演员恒为 *newPlayer，接收者却换 P−1 次，
	// 于是同一个人的整份投影（mode0 外观表 + mode1 的 ~31 行装备 + 一次 PVF 属性
	// 重算）被从头重算 P−1 遍。body 缓存的键是 (演员 charID, withBinding)，
	// 所以这里正好是 1 次构建 + P−2 次命中。
	//
	// 缓存生命周期仍然只有这一次调用，过期窗口是微秒级，不引入任何新的失效责任
	// （服务级长生命周期方案已被否决：写侧没有卡点，且有 4 处墙上时钟会翻转投影）。
	ctx = withActorProjectionBodiesCache(ctx)
	peers := append([]onlinePlayerInfo(nil), others...)
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].CharacterID < peers[j].CharacterID
	})
	actorKey := currentSceneActorObjectKey(newPlayer.CharacterID)

	// To each existing player: newcomer's mode0/mode1/op9, then 0x0017.
	//
	// NOTE 2026-09-26: 试过把这里的 0x0017 换成 0x0016，被三个钉死契约的用例挡回
	// （town_move_test.go:697/715 明确断言 peer 收到 currentTownUserAreaNotificationMsgID
	// 且 body >= 10 字节、Body[3] != 0）。而且 town_move.go:341 / assault.go:208 /
	// party_town_return.go:77 都在给 peer 发 0x0017 —— 它是成体系的既有设计，
	// 不是疏忽。故保留原样。
	enterBody := buildCurrentTownUserAreaNotificationBody(
		actorKey, newPlayer.TownID, newPlayer.AreaID,
		newPlayer.PositionX, newPlayer.PositionY,
		newPlayer.Direction, newPlayer.AreaState,
	)
	for i := range peers {
		if peers[i].Session == nil {
			continue
		}
		if err := s.sendTownRemoteActorRecordWithContext(
			ctx,
			peers[i].Session,
			*newPlayer,
			"town_copresence_actor_state_newcomer_to_existing",
		); err != nil {
			s.logGameEvent(peers[i].Session, "game-town-copresence-actor-state-failed",
				"source", "town_copresence_actor_state_newcomer_to_existing",
				"actor_char_id", newPlayer.CharacterID,
				"error", err)
			continue
		}
		_ = s.sendCurrentSceneFixedClass0Packet(peers[i].Session,
			currentTownUserAreaNotificationMsgID, enterBody,
			"town_copresence_enter_0x0017_to_existing")
	}

	// To newcomer: each existing player's mode0/mode1/op9, then 0x0017.
	for i := range peers {
		if peers[i].Session == nil {
			continue
		}
		if err := s.sendTownRemoteActorRecordWithContext(
			ctx,
			newPlayer.Session,
			peers[i],
			"town_copresence_actor_state_existing_to_newcomer",
		); err != nil {
			s.logGameEvent(newPlayer.Session, "game-town-copresence-actor-state-failed",
				"source", "town_copresence_actor_state_existing_to_newcomer",
				"actor_char_id", peers[i].CharacterID,
				"error", err)
			continue
		}
		otherKey := currentSceneActorObjectKey(peers[i].CharacterID)
		otherBody := buildCurrentTownUserAreaNotificationBody(
			otherKey, peers[i].TownID, peers[i].AreaID,
			peers[i].PositionX, peers[i].PositionY,
			peers[i].Direction, peers[i].AreaState,
		)
		_ = s.sendCurrentSceneFixedClass0Packet(newPlayer.Session,
			currentTownUserAreaNotificationMsgID, otherBody,
			"town_copresence_enter_0x0017_existing_to_newcomer")
	}
	// 探针：把这次城镇共现扇出的缓存命中/未命中与 build/send 耗时打出来。
	// 第一循环理想值 = hit (P−2) / miss 1；若 miss 接近 P−1，说明键又被打散了。
	if bodiesCache := actorProjectionBodiesCacheFromContext(ctx); bodiesCache != nil {
		bodiesCache.logSummary(s, "town_copresence_player_enter")
	}
}

// sendTownRemotePartyActors restores party members that are online in another
// town area after SET_USER_AREA rebuilt the receiving client's actor manager.
// Current sub_1D64CA0's same-owner party rows contain only user ids, so every
// referenced user must first exist through the normal mode0/mode1/op9 actor
// path. The following op23 carries the member's real town/area and position;
// it does not claim co-presence in the receiver's destination area.
func (s *Service) sendTownRemotePartyActors(receiver *gameSession, state alignedcmd.PartyState) error {
	if s == nil || s.onlinePlayers == nil || receiver == nil || receiver.selectedCharacterID == 0 || state.PartyID <= 0 {
		return nil
	}
	for _, member := range runtimePartyMembers(state) {
		if member.UserID == 0 || member.UserID == receiver.selectedCharacterID ||
			s.onlinePlayers.PeerInSameArea(receiver.selectedCharacterID, member.UserID) {
			continue
		}
		actor, ok := s.onlinePlayers.PlayerForCharacter(member.UserID)
		if !ok || actor.Session == nil {
			continue
		}
		if err := s.sendTownRemoteActorState(receiver, actor, "town_remote_party_actor_after_set_user_area"); err != nil {
			return err
		}
		areaBody := buildCurrentTownUserAreaNotificationBody(
			currentSceneActorObjectKey(actor.CharacterID),
			actor.TownID,
			actor.AreaID,
			actor.PositionX,
			actor.PositionY,
			actor.Direction,
			actor.AreaState,
		)
		if err := s.sendCurrentSceneFixedClass0Packet(
			receiver,
			currentTownUserAreaNotificationMsgID,
			areaBody,
			"town_remote_party_actor_real_area_after_state",
		); err != nil {
			return err
		}
	}
	return nil
}

// broadcastTownPlayerLeave sends NOTI 0x0006 (u16 userId) to remaining
// players so they remove the departed player's town actor.
func (s *Service) broadcastTownPlayerLeave(characterID uint16, others []onlinePlayerInfo) {
	if s == nil || s.onlinePlayers == nil || len(others) == 0 {
		return
	}
	leaveBody := buildTownUserLeaveBody(characterID)
	for i := range others {
		if others[i].Session != nil {
			_ = s.sendCurrentSceneFixedClass0Packet(others[i].Session,
				currentTownUserLeaveMsgID, leaveBody,
				"town_copresence_leave_0x0006")
		}
	}
}

// broadcastTownPlayerAreaChange removes an ordinary old-area actor, but keeps
// an active party member bound to the stationary client's party manager. The
// current client treats op6 as both a town-actor destroy and a party-member
// removal. For party peers the native op23 area update is sufficient: it moves
// the existing actor out of the rendered area while retaining the roster and
// minimap identity. Recreating that actor after op6 is unsafe because the
// client answers the synthetic cross-area rebuild with LEAVE_PARTY.
func (s *Service) broadcastTownPlayerAreaChange(
	mover *onlinePlayerInfo,
	oldOthers []onlinePlayerInfo,
	state alignedcmd.PartyState,
) {
	if s == nil || mover == nil || len(oldOthers) == 0 {
		return
	}
	partyMembers := make(map[uint16]struct{}, len(runtimePartyMembers(state)))
	if state.PartyID > 0 {
		for _, member := range runtimePartyMembers(state) {
			if member.UserID != 0 && member.UserID != mover.CharacterID {
				partyMembers[member.UserID] = struct{}{}
			}
		}
	}
	leaveBody := buildTownUserLeaveBody(mover.CharacterID)
	areaBody := buildCurrentTownUserAreaNotificationBody(
		currentSceneActorObjectKey(mover.CharacterID),
		mover.TownID,
		mover.AreaID,
		mover.PositionX,
		mover.PositionY,
		mover.Direction,
		mover.AreaState,
	)
	for i := range oldOthers {
		peer := oldOthers[i].Session
		if peer == nil {
			continue
		}
		_, sameAuthoritativeParty := partyMembers[oldOthers[i].CharacterID]
		peerState := runtimePartyStateSnapshot(peer)
		if sameAuthoritativeParty && peerState.PartyID == state.PartyID {
			_ = s.sendCurrentSceneFixedClass0Packet(
				peer,
				currentTownUserAreaNotificationMsgID,
				areaBody,
				"town_party_member_area_change_0x0017",
			)
			continue
		}
		_ = s.sendCurrentSceneFixedClass0Packet(
			peer,
			currentTownUserLeaveMsgID,
			leaveBody,
			"town_copresence_leave_0x0006",
		)
	}
}

// broadcastTownPlayerMove sends NOTI 0x0016 (sub_1D83990 position update) to
// all other players in the area.
//
// movementRate is the mover's own reported town movement rate (op35's final
// u16, the client's actor+0x748 scaled by 100). It must be forwarded verbatim:
// the receiving client applies it to the remote actor, so dropping it back to
// the 100 initialization value makes a boosted character look normal-speed on
// every other screen while it still moves correctly on its own.
func (s *Service) broadcastTownPlayerMove(mover *onlinePlayerInfo, movementRate uint16, others []onlinePlayerInfo) {
	if s == nil || s.onlinePlayers == nil || mover == nil || len(others) == 0 {
		return
	}
	actorKey := currentSceneActorObjectKey(mover.CharacterID)
	moveBody := buildCurrentTownUserPositionNotificationBody(
		actorKey, mover.PositionX, mover.PositionY, mover.Direction, movementRate,
	)
	// 建包与两次运行时命令查表都只取决于 (msgID, body)，对同场景的每个收件人完全一样，
	// 所以只做一次再逐个发送。改前是逐个收件人重复走一遍完整发送路径：在 P 个人的
	// 场景里，每个 op35 都要白做 P-1 次分配与 2(P-1) 次查表（建包结果完全相同）。
	// 构建失败时与改前等价：输入相同，改前也是每个收件人都失败、且错误同样被忽略。
	reusable, err := buildReusableCurrentSceneFixedClass0Packet(
		currentTownUserPositionNotificationMsgID,
		moveBody,
		"town_copresence_move_0x0016",
	)
	if err != nil {
		return
	}
	for i := range others {
		if others[i].Session != nil {
			_ = s.sendReusableCurrentSceneFixedClass0Packet(others[i].Session, reusable)
		}
	}
}

// cleanupOnlinePlayer removes a player from the online player manager on
// disconnect and broadcasts the leave notification to remaining players.
func (s *Service) cleanupOnlinePlayer(session *gameSession) {
	if s == nil || s.onlinePlayers == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	s.closeCurrentExpertJobStoreSession(session, false)
	s.closeCurrentPrivateStoreSession(session, false)
	departing, hadDeparting := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	_, others, removed := s.onlinePlayers.LeaveAreaSession(session.selectedCharacterID, session)

	// "Left the game" does not depend on town co-presence. A character that
	// published its social presence from the private Seria room route never
	// enters the area registry at all, so both the lookup above and the removal
	// come back empty for it — and gating the announcement on either is why a
	// guild mate saw the login line but never the logout one.
	departingName := ""
	if hadDeparting {
		departingName = departing.Name
	}
	s.announceCurrentSocialOffline(session, departingName, "game_session_disconnect")
	s.publishCurrentGuildPresence(session.selectedCharacterID, "game_session_disconnect")

	// A 1:1 window dies with the session that owned it: the 86 reference registers
	// exactly this on its SessionEnding event. The peer is not told here -- the
	// reference's LEAVE_USER_FROM_GROUP is its own notification type and the 90CN
	// client's equivalent is still unidentified -- so the peer's window stays open
	// and simply stops receiving lines.
	for _, conversation := range s.closeCurrentChatConversationsForSession(session) {
		s.logGameEvent(session, "game-peer-chat-conversation-closed",
			"conversation_id", conversation.id,
			"source_char_id", session.selectedCharacterID,
			"reason", "game_session_disconnect",
			"peer_notice", "unsupported_by_current_client")
	}

	if !removed {
		return
	}
	if len(others) > 0 {
		s.broadcastTownPlayerLeave(session.selectedCharacterID, others)
	}
}

// announceCurrentSocialOffline tells the friend list and the guild the
// character is gone. The name falls back to the repository because the caller's
// area snapshot is empty for exactly the sessions this has to cover.
func (s *Service) announceCurrentSocialOffline(session *gameSession, name string, source string) {
	characterID := session.selectedCharacterID
	if characterID == 0 {
		return
	}
	if strings.TrimSpace(name) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		loadedID, loadedName, _, found := s.characterForEnter(ctx, session, characterID)
		if !found || loadedID != characterID {
			s.logGameEvent(session, "game-social-offline-name-unavailable", "char_id", characterID, "source", source)
			return
		}
		name = loadedName
	}
	notified := s.notifyCurrentUnitedFriendPresence(name, currentFriendPresence{}, source+"_friend_offline")
	s.notifyCurrentGuildMemberPresence(characterID, name, currentFriendPresence{}, source+"_guild_offline", notified)
}
