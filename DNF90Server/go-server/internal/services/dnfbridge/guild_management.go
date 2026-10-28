package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentGuildMemberOfflineChannel is the channel byte sub_1D21FD0 turns into
// member+0x34 == -1 (01D223DE cmp cl,0xff / 01D223E3 mov [eax+0x34],-1). An
// earlier note here claimed the window renders no row in that state; the probe
// on 2026-09-08 disproved it -- both places the window compares that field feed
// a `setne` boolean into the row-render call. What actually distinguishes an
// offline member is the elapsed-offline u32; see
// currentGuildMemberOfflineSeconds. An online member must still never be
// encoded with this value, because it would report as offline.
const currentGuildMemberOfflineChannel = byte(0xff)

// currentGuildMemberUnrepresentedChannel marks "online, channel number does
// not fit the row's one-byte field". No advertised channel needs it any more:
// `[deathtower]` moved 508 -> 171, and `[pvp]` (501..507 -> 191..197) and
// `[fiendwar]` (261..263 -> 184..186) followed once the same one-byte limit
// turned out to break town co-presence too, not just this row.
// TestCurrentChannelCatalogFitsGuildMemberChannelByte pins that the exemption
// set is now empty. The guard stays as a protocol rail so a future
// out-of-range channel degrades to "online, channel unknown" instead of
// silently becoming offline. The value is deliberately outside the advertised
// directory so it can never be mistaken for a real channel the way a
// truncated 508 -> 252 was.
const currentGuildMemberUnrepresentedChannel = byte(0xfe)

// currentGuildMemberChannelByte encodes an online member's channel for a
// member-list row. The field is one byte and 0xff is the offline sentinel, so
// only 1..254 are expressible. Keeping the member visible matters more than
// the channel number, so an unrepresentable channel still reports online.
func currentGuildMemberChannelByte(channelID int) byte {
	if channelID <= 0 || channelID > 254 {
		return currentGuildMemberUnrepresentedChannel
	}
	if encoded := byte(channelID); encoded != currentGuildMemberOfflineChannel {
		return encoded
	}
	return currentGuildMemberUnrepresentedChannel
}

func (s *Service) currentGuildOnlineChannels(guild *dnfguild.Guild, requester *gameSession) map[string]byte {
	channels := make(map[string]byte)
	if guild == nil {
		return channels
	}
	for id := range guild.Members {
		// The requester's own row previously bypassed the range check while
		// every other row was dropped from the map, so a member above channel
		// 254 was announced online to itself and offline to everyone else.
		// One encoder now serves both.
		identity, found := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(id)))
		if found && identity.session.conn != nil && identity.session.channel.ID > 0 {
			channels[id] = currentGuildMemberChannelByte(identity.session.channel.ID)
			if channels[id] == currentGuildMemberUnrepresentedChannel {
				s.logPacketEvent("game-guild-member-channel-unrepresented",
					"char_id", id, "channel_id", identity.session.channel.ID)
			}
		}
		if requester != nil && id == strconv.Itoa(int(requester.selectedCharacterID)) {
			channels[id] = currentGuildMemberChannelByte(requester.channel.ID)
		}
	}
	return channels
}

func (s *Service) sendCurrentGuildManagementState(session *gameSession, guild *dnfguild.Guild) error {
	// Login requests op140 before actor creation; op127 dereferences that actor.
	// The initial-town projection sends the latest state after the transition.
	// The initial-town route cursor alone is not enough to express "the actor
	// exists": a cross-town move (op1425 / town_move) commits its own scene
	// transition and then resets the cursor to idle, so every later op70/op127
	// for that session was dropped in silence. A committed town scene for the
	// selected character is the same guarantee the cursor gives.
	if ready, source := s.currentSceneTransitionReadyForState(session); !ready {
		if !currentGuildTownActorCommitted(session) {
			s.logGameEvent(session, "game-guild-management-state-deferred", "reason", source)
			return nil
		}
	}
	if err := s.writeCurrentGuildManagementState(session, guild); err != nil {
		return err
	}
	// The local actor's insignia may have just changed. Peers that already built
	// this actor keep the old decoration otherwise, because the aura rides the
	// actor-appearance block rather than op70.
	s.publishCurrentGuildAuraToTownPeers(session, guild)
	return nil
}

// currentGuildTownActorCommitted reports whether the session already committed
// a town scene for its selected character, which is what op127 dereferences.
func currentGuildTownActorCommitted(session *gameSession) bool {
	if session == nil || session.selectedCharacterID == 0 {
		return false
	}
	session.townMu.Lock()
	committed := session.townSceneReadyCharacterID == session.selectedCharacterID
	session.townMu.Unlock()
	return committed
}

// currentGuildTownActorCommittedNonBlocking is the peer-safe form of
// currentGuildTownActorCommitted; see
// currentSceneTransitionReadyForStateNonBlocking for why a peer read must not
// wait for that peer's town lock.
func currentGuildTownActorCommittedNonBlocking(session *gameSession) (committed bool, busy bool) {
	if session == nil || session.selectedCharacterID == 0 {
		return false, false
	}
	if !session.townMu.TryLock() {
		return false, true
	}
	committed = session.townSceneReadyCharacterID == session.selectedCharacterID
	session.townMu.Unlock()
	return committed, false
}

// sendCurrentGuildManagementStateToPeer refreshes another online member's guild
// window without ever waiting for that member's town lock.
//
// The peer refresh runs on the event loop of the member whose online state just
// changed, and that member's own townMu is already held there:
// sendCurrentInitialTownRoute publishes presence from inside its lock
// (initial_town_route_progress.go). Reading the peer's town state with a blocking
// lock at that point inverts the lock order between two members logging in at the
// same time. Live incident 2026-09-22 20:06 -- the sessions of 1111 and 2222 (one
// guild, one second apart) each held their own townMu and waited for the other's:
//
//	goroutine 934: handleDungeonTutorialFlag -> handleCurrentInitialTownProgress
//	-> sendCurrentInitialTownRoute -> publishCurrentTownPresence ->
//	publishCurrentSocialPresence -> publishCurrentGuildPresence ->
//	sendCurrentGuildManagementState -> currentSceneTransitionReadyForState
//	(initial_town_scene_ready.go:58, sync.Mutex.Lock, 6 minutes)
//
// Neither session processed another packet afterwards, so both clients stayed
// frozen in the scene they had just logged into, the door out of the Seria room
// included, and every later login that published into a wedged session wedged as
// well. The refresh is only a projection of the peer's own guild window, which
// the peer's own login route sends anyway, so a peer that is mid-transition is
// skipped with an event instead of being waited for.
func (s *Service) sendCurrentGuildManagementStateToPeer(session *gameSession, guild *dnfguild.Guild) error {
	if session == nil {
		return nil
	}
	ready, source, busy := s.currentSceneTransitionReadyForStateNonBlocking(session)
	if busy {
		s.logGameEvent(session, "game-guild-presence-refresh-deferred", "reason", source)
		return nil
	}
	if !ready {
		committed, busy := currentGuildTownActorCommittedNonBlocking(session)
		if busy {
			s.logGameEvent(session, "game-guild-presence-refresh-deferred", "reason", source)
			return nil
		}
		if !committed {
			s.logGameEvent(session, "game-guild-management-state-deferred", "reason", source)
			return nil
		}
	}
	return s.writeCurrentGuildManagementState(session, guild)
}

// The caller must have committed the selected actor and scene transition.
func (s *Service) writeCurrentGuildManagementState(session *gameSession, guild *dnfguild.Guild) error {
	grade := byte(100) // sub_1D85270's native nonmember sentinel
	if guild != nil {
		if member, found := guild.Members[strconv.Itoa(int(session.selectedCharacterID))]; found {
			grade = member.Grade
		} else {
			// A nonmember of the guild being projected must not receive op70 at
			// all: its body carries the notice, the insignia and the grade
			// table, none of which a nonmember may see. Upstream nils the guild
			// here so that op70 and every projection below are skipped while
			// op127 still delivers the 100 sentinel.
			guild = nil
		}
	}
	// A nonmember gets only the grade sentinel. op70's handler displays the
	// notice once per client run gated on nothing but a global flag --
	// 01D8B8A2 tests [0x519297C], sets it at 01D8B8BD, then formats the notice
	// through string 0x1A46 and shows it (01D8B8D3..01D8B92D). It never checks
	// whether the notice is empty or whether the viewer is in a guild, so the
	// empty projection the login path sends for a guildless character popped a
	// blank 公会公告 line on every login. Skipping op70 costs the "clear the
	// previous character's guild fields on switch" side effect that the empty
	// projection also had; op127's 100 sentinel plus the membership
	// notification still tell the client it is not a member, and the notice is
	// one-shot per client run anyway.
	if guild != nil {
		if err := s.sendGameUpperRawClass(session, 70, buildCurrentGuildManagementBody(guild, len(s.currentGuildOnlineChannels(guild, session)), s.gameplayNow(), grade), 0); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClass(session, 127, []byte{grade}, 0); err != nil {
		return err
	}
	// Projections that must follow op127. op70 carries the durable guild fields
	// but not the actor decorations or the HUD rows, so without these a member
	// who changes guild state keeps the previous insignia, buffs and rank until
	// a relog. Upstream also writes the 公会地下城 and 守护神 states here
	// (writeCurrentGuildDungeonState / writeCurrentGuildGuardianState); those
	// arrive with their own steps.
	if err := s.writeCurrentGuildAuraState(session, session.selectedCharacterID, guild, currentTownActorOwnerContext(session)); err != nil {
		return err
	}
	if err := s.writeCurrentGuildBuffProjection(session, guild, false, "guild_management"); err != nil {
		return err
	}
	return s.writeCurrentGuildRanking(session, guild)
}

func (s *Service) publishCurrentGuildManagement(session *gameSession, guild *dnfguild.Guild) error {
	for id := range guild.Members {
		if id == strconv.Itoa(int(session.selectedCharacterID)) {
			continue
		}
		if target, found := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(id))); found {
			if err := s.sendCurrentGuildManagementStateToPeer(target.session, guild); err != nil {
				s.logGameEvent(target.session, "game-guild-management-refresh-failed", "error", err)
			}
		}
	}
	return s.sendCurrentGuildManagementState(session, guild)
}

func (s *Service) handleCurrentGuildMark(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketChangeGuildMark)
	if len(body) != 1 || body[0] > 4 {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil || character.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		return registry.SetMark(character.CharacterID, body[0], s.gameplayNow())
	})
	return s.finishCurrentGuildSettings(session, opcode, err, nil)
}

func (s *Service) handleCurrentGuildRecommendedChannel(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketSetGuildRecommandChannel)
	catalog := s.currentCatalog()
	if len(body) != 0 || catalog == nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildChannelSettingError)
	}
	// The request carries no channel: the client means "the channel I am
	// standing in now". The guard used to require ResidentFor(channel).ID ==
	// channel.ID, which is false for every ordinary town channel because
	// isResidentChannel treats type 11 as a death-tower channel (see
	// channelcatalog.SettableGuildChannel), so every 保存 was answered with
	// error 19 and the client showed 只有公会长才能设置.
	channel, found := catalog.Channel(session.channel.ID)
	advertise := s.channelAdvertiseID()
	// The bounds mirror Registry.SetRecommendedChannel (region <= 249, channel
	// 1..255) so a rejected channel keeps op830's own error instead of falling
	// through to the generic one.
	if !found || !catalog.SettableGuildChannel(channel) ||
		advertise < 0 || advertise > 249 || channel.ID <= 0 || channel.ID > 255 {
		return s.sendGameUpperFailure(session, opcode, currentGuildChannelSettingError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetRecommendedChannel(strconv.Itoa(int(session.selectedCharacterID)), uint32(advertise), uint32(channel.ID), s.gameplayNow())
	})
	if err != nil {
		return s.finishCurrentGuildSettings(session, opcode, err, nil)
	}
	// The channel is stored as the region the client advertises for its channel
	// lookups plus the catalog channel id, which is the number the guild window
	// renders as ch<id>.<name>. Re-project the saved state to the actor as well,
	// so the dialog shows the new guild channel without reopening it.
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	guild, guildErr := s.currentGuildForSelectedCharacter(session)
	if guildErr != nil || guild == nil {
		return guildErr
	}
	if err := s.sendCurrentGuildManagementState(session, guild); err != nil {
		return err
	}
	return s.publishCurrentGuildManagement(session, guild)
}

func (s *Service) finishCurrentGuildSettings(session *gameSession, opcode uint16, mutationErr error, response []byte) error {
	if mutationErr != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(session, opcode, response); err != nil {
		return err
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return err
	}
	return s.publishCurrentGuildManagement(session, guild)
}

func parseCurrentGuildRecruitment(body []byte) (uint32, string, bool, uint32, byte, bool) {
	if len(body) < 14 {
		return 0, "", false, 0, 0, false
	}
	mask := binary.LittleEndian.Uint32(body)
	promotion, next, ok := parseCurrentGuildDSTR(body, 4)
	if !ok || next != len(body)-6 || body[next] > 1 {
		return 0, "", false, 0, 0, false
	}
	return mask, promotion, body[next] == 1, binary.LittleEndian.Uint32(body[next+1:]), body[next+5], true
}

// grantCurrentGuildAttendance applies the daily guild attendance that grants one
// 公会积分 to the member and appends the MileageHistory row 公会积分 lists.
//
// The current client has no 签到 button: the game signs in automatically on the
// first login of the day, so the grant hangs off the online publish, where
// Registry.Attend's once-per-local-day idempotence makes repeated calls harmless.
//
// It must never wait for guildMu. The online publish can run inside a guild
// critical section, guildMu is a plain (non-reentrant) mutex, and the first
// version of this hook called mutateCurrentGuild directly: a login then
// deadlocked its own session goroutine, the client hung on the loading screen and
// no further packet was answered (live 2026-09-12 01:33 -- both sessions stopped
// sending keepalives while the accept loop stayed healthy). If the lock is busy
// the grant is simply skipped and the next online publish applies it.
//
// Nothing is pushed to the client here either: 公会积分 and 今日签到成员 are both
// request-driven (op747 / op746), so the new row shows up the next time the window
// is opened, without risking a stray write from a non-session goroutine.
func (s *Service) grantCurrentGuildAttendance(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	// Bounded retry in a background goroutine, never on the caller's: the session
	// path must return immediately even if another login holds guildMu.
	go s.applyCurrentGuildAttendance(characterID, source)
}

// currentGuildAttendanceGrantTimeout bounds the background grant's wait for
// guildMu; a busy lock is retried and then dropped, and the next online publish
// (any area change) applies the attendance instead.
const currentGuildAttendanceGrantTimeout = 10 * time.Second

func (s *Service) applyCurrentGuildAttendance(characterID, source string) {
	if strings.TrimSpace(characterID) == "" {
		return
	}
	deadline := time.Now().Add(currentGuildAttendanceGrantTimeout)
	for {
		if s.guildMu.TryLock() {
			s.guildMu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			s.logPacketEvent("game-guild-attendance-skipped",
				"reason", "guild_lock_busy", "source", source, "char_id", characterID)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	granted := false
	mileage := uint32(0)
	if err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		guild, _, member := registry.GuildForCharacter(characterID)
		if !member || guild == nil {
			return nil // not a member: nothing to sign in for
		}
		applied, err := registry.Attend(characterID, s.gameplayNow())
		if err != nil {
			return err
		}
		granted, mileage = applied, guild.Mileage
		return nil
	}); err != nil {
		s.logPacketEvent("game-guild-attendance-grant-failed", "source", source, "char_id", characterID, "error", err)
		return
	}
	if !granted {
		return
	}
	s.logPacketEvent("game-guild-attendance-granted",
		"source", source, "char_id", characterID, "mileage", mileage)
}

// publishCurrentGuildPresence refreshes the guild window of every other online
// member after characterID's online state changed. The current client has no
// per-member presence packet — the roster map at guild+0x2D8 is an internal
// index, not a wire target — so the refresh reuses the packets a member-list
// request already produces: op70/op127 carry the online count and the viewer's
// own grade, and the member list is replayed only for sessions that already
// asked for it this session, so an unsolicited op67 can never raise the guild
// panel on a client that never opened it.
func (s *Service) publishCurrentGuildPresence(characterID uint16, source string) {
	if s == nil || characterID == 0 {
		return
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var guild *dnfguild.Guild
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		if found, _, ok := registry.GuildForCharacter(strconv.Itoa(int(characterID))); ok {
			guild = found
		}
		return nil
	}); err != nil {
		s.logPacketEvent("game-guild-presence-load-failed", "char_id", characterID, "source", source, "error", err)
		return
	}
	if guild == nil {
		return
	}
	for id := range guild.Members {
		memberID := currentGuildNumericID(id)
		if memberID == 0 || uint16(memberID) == characterID {
			continue
		}
		target, online := s.onlineGameSessionCharacterSnapshot(uint16(memberID))
		if !online || target.session == nil {
			continue
		}
		if err := s.sendCurrentGuildManagementStateToPeer(target.session, guild); err != nil {
			s.logGameEvent(target.session, "game-guild-presence-refresh-failed",
				"char_id", characterID, "source", source, "error", err)
			continue
		}
		if !currentGuildMemberListRequested(target.session) {
			continue
		}
		if err := s.sendGameUpperSuccess(
			target.session,
			uint16(dnfenum.CmdPacketGuildMemerList),
			buildCurrentGuildMemberListBody(guild, true, false,
				s.currentGuildOnlineChannels(guild, target.session), s.gameplayNow()),
		); err != nil {
			s.logGameEvent(target.session, "game-guild-presence-member-list-failed",
				"char_id", characterID, "source", source, "error", err)
		}
	}
}

// currentGuildNoticeMessageOpcode is GUILD_MESSAGE_TO_GUILD_MEMBER: opcode 141
// carried as class 0. It is a different packet from the class-1 op141
// member-list continuation (dnfenum.CmdPacketGuildAllMemberListNext) -- the
// client keeps one handler table per classification, registering sub_1D21FD0
// for op141 in the class-1 table (01D36A7D) and sub_1D85370 in the class-0 one
// (01D92AD2). sub_1D85370 reads a single 0x100-unit DSTR (01D853D0) and shows
// it, and it is the only packet that displays a changed notice without a
// relog: op70's own display is gated on the one-shot global flag at 0x519297C
// (01D8B8A2 / 01D8B8BD), so a later op70 updates guild+0x18 -- the field the
// edit box refills from -- without redisplaying anything. 86JP sends the same
// packet from GuildPacketBuilder.BuildGuildNoticeMessage.
//
// (An earlier version of this comment called 0x1A41 a display template. It is
// not: sub_367BE00(0x1A41) returns the format string sub_3457D20 uses to read
// the DSTR, the same role 0x334/0x335 play in the member-row parse. The string
// op70 formats the visible line with is 0x1A46.)
const currentGuildNoticeMessageOpcode = uint16(141)

// currentGuildMemberRegion is the region/server byte a member row carries at
// member+0x182 and an op1454 settings row carries at row+4. The two must be
// equal, because sub_FBE4D0 joins a settings row to a member record on
// (name, region).
//
// It cannot be 0. sub_325F590 stores this byte on the row control at +0x4C
// (0325F661 loads the row+0x182 argument, 0325F669 writes it) and the member
// context menu hands that field to sub_26A3590 as the op126 request's first
// byte. sub_26A3590 opens with `lea eax,[esi-1]; cmp eax,0xF8; ja bail`, so 0
// underflows to 0xFFFFFFFF and the request is abandoned inside the client --
// which is why clicking 任命副会长 produced no packet at all, confirmed against
// the game-guild-request log.
//
// 1 is what channelServerID() reports here (its default, no config override)
// and the value the client itself tests for at 00FC1813 / 00F9E3BD. What
// leaving 0 bought, and this gives up: sub_2699B10 treats a 0 request as
// "match any region" (02699BAB test ecx,ecx / je), so with a real region the
// live-actor lookup must now match actor+0x38C (sub_26908C0). If it does not,
// member+0x3C stops resolving and rows render from the fields this server
// sends instead of from the live actor -- they stay listed either way, since
// rebuild pass 2 keeps an online row whose +0x3C is 0. Reverting is this one
// constant.
const currentGuildMemberRegion = byte(1)

func buildCurrentGuildNoticeMessageBody(notice string) []byte {
	var writer packetWriter
	writeClientNameDstr(&writer, notice)
	return writer.bytes()
}

// publishCurrentGuildNotice pushes the notice to every online member. The
// town-actor gate is the same one sendCurrentGuildManagementState uses: a
// class-0 packet that lands mid-scene-construction is what crashed character
// selection on 2026-09-08, and a member still loading rereads the notice from
// op70 anyway.
func (s *Service) publishCurrentGuildNotice(guild *dnfguild.Guild) {
	if s == nil || guild == nil {
		return
	}
	body := buildCurrentGuildNoticeMessageBody(guild.Notice)
	for id := range guild.Members {
		memberID := currentGuildNumericID(id)
		if memberID == 0 {
			continue
		}
		target, online := s.onlineGameSessionCharacterSnapshot(uint16(memberID))
		if !online || target.session == nil || !currentGuildTownActorCommitted(target.session) {
			continue
		}
		if err := s.sendGameUpperRawClass(target.session, currentGuildNoticeMessageOpcode, body, 0); err != nil {
			s.logGameEvent(target.session, "game-guild-notice-publish-failed", "error", err)
		}
	}
}

// republishCurrentGuildMemberLists replays op67 for every online member that
// already opened the guild panel this session. A member row carries the grade
// at member+0x144 (sub_1D21FD0 read 12, cross-checked against sort column 5 at
// 00FA72AB), so a grade change stays invisible until the list is sent again --
// op70/op127 only carry the header counters and the viewer's own grade. The
// same memberListRequested gate publishCurrentGuildPresence uses keeps an
// unsolicited op67 from raising the panel on a client that never opened it.
func (s *Service) republishCurrentGuildMemberLists(guild *dnfguild.Guild) {
	if s == nil || guild == nil {
		return
	}
	now := s.gameplayNow()
	for id := range guild.Members {
		memberID := currentGuildNumericID(id)
		if memberID == 0 {
			continue
		}
		target, online := s.onlineGameSessionCharacterSnapshot(uint16(memberID))
		if !online || target.session == nil || !currentGuildMemberListRequested(target.session) {
			continue
		}
		if err := s.sendGameUpperSuccess(
			target.session,
			uint16(dnfenum.CmdPacketGuildMemerList),
			buildCurrentGuildMemberListBody(guild, true, false,
				s.currentGuildOnlineChannels(guild, target.session), now),
		); err != nil {
			s.logGameEvent(target.session, "game-guild-member-list-republish-failed", "error", err)
		}
	}
}

// finishCurrentGuildGradeChange is finishCurrentGuildSettings plus the member
// list replay a grade change needs.
func (s *Service) finishCurrentGuildGradeChange(session *gameSession, opcode uint16, mutationErr error, response []byte) error {
	if err := s.finishCurrentGuildSettings(session, opcode, mutationErr, response); err != nil {
		return err
	}
	if mutationErr != nil {
		return nil
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return err
	}
	s.republishCurrentGuildMemberLists(guild)
	return nil
}

func markCurrentGuildMemberListRequested(session *gameSession) {
	if session == nil {
		return
	}
	session.guild.mu.Lock()
	session.guild.memberListRequested = true
	session.guild.mu.Unlock()
}

func currentGuildMemberListRequested(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.guild.mu.Lock()
	defer session.guild.mu.Unlock()
	return session.guild.memberListRequested
}
