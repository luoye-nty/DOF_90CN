// party_teleport.go 实现 legacy op434 组队传送：目的地解析、全队校验、S2C 377 队员同意
// asks; every packet layout comes from static reverse engineering of the current client
package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// handleCurrentPartyTeleportOrNetworkReport splits the two request shapes that
// arrive on legacy cmd=1/type=434.
//
// opcode 434 is overloaded in this client build:
//
//   - currentPartyNetworkReportSize (18) bytes is the native reliable-UDP path
//     report, which publishes the connection's MTU bucket and protocol version.
//   - the party teleport request is 8 bytes. The live 2026-09-12
//     samples are `29 01 17 02 76 01 05 01` (17:40:04Z) and
//     `2b 00 d7 05 33 03 05 01` (17:53:09Z)
//     (runtime/logs/packet_log.txt, conn game-000125, pkt_seq 190).
//
// Body length is the only field that separates them, so the discriminator has
// to run before either parser. Without it the shorter request reached the
// 18-byte report parser, which refused it - and because inbound game packets
// are submitted as fatal events, that refusal closed the whole game connection,
// which the player saw as "网络连接失败" plus "无法前往".
func (s *Service) handleCurrentPartyTeleportOrNetworkReport(session *gameSession, body []byte) error {
	if len(body) == currentPartyNetworkReportSize {
		return s.handleCurrentPartyNetworkReport(session, body)
	}
	return s.handleCurrentPartyTeleportRequest(session, body)
}

// currentPartyTeleportRequestWireSize is the legacy op434 party teleport body.
const currentPartyTeleportRequestWireSize = 8

// currentPartyTeleportRequest is the party leader's teleport request.
//
// The layout is read off the current client's own writer (DNF.exe VA
// 0x00E9FB00, reached from the teleport controller state machine at
// VA 0x021CC9E0). That writer emits, in order:
//
//	write_u16(0x1B2)                        -- opcode 434
//	write_u8  from accessor 0x01B56A20      -- DestinationTown
//	write_u8  from accessor 0x01C91D50      -- DestinationArea
//	write_u16 from accessor 0x03056590      -- DestinationX
//	write_u16 from accessor 0x03442FE0      -- DestinationY
//	write_u8  constant 5                    -- Direction
//	write_u8  *(byte*)argument              -- Flag
//
// The same four accessors feed the personal (aura) teleport writer op470, whose
// observed body - `ff ff ff ff ff ff ff ff 12 03 3a 02 42 01 05` for town 18,
// area 3, position (570,322) - proves the semantics: the server resolved exactly
// that destination and the character landed on
// Town/kor_Dragon_shonan_holyland/38133_shonan_holy03.map at (570,322).
type currentPartyTeleportRequest struct {
	DestinationTown byte
	DestinationArea byte
	DestinationX    uint16
	DestinationY    uint16
	Direction       byte
	Flag            byte
}

func parseCurrentPartyTeleportRequest(body []byte) (currentPartyTeleportRequest, error) {
	if len(body) != currentPartyTeleportRequestWireSize {
		return currentPartyTeleportRequest{}, fmt.Errorf(
			"current party teleport op434 body length %d, want %d",
			len(body),
			currentPartyTeleportRequestWireSize,
		)
	}
	return currentPartyTeleportRequest{
		DestinationTown: body[0],
		DestinationArea: body[1],
		DestinationX:    binary.LittleEndian.Uint16(body[2:4]),
		DestinationY:    binary.LittleEndian.Uint16(body[4:6]),
		Direction:       body[6],
		Flag:            body[7],
	}, nil
}

// The S2C half of the feature is message id 377, recovered from the client's own
// handler registration: at DNF.exe VA 0x01D92EDB the client executes
// `PUSH 0x1D864F0; PUSH 0x179; MOV ECX, ESI; CALL 0x2262980`, i.e. it registers
// the reader at VA 0x01D864F0 for id 0x179. That reader pulls, through the stream
// readers 0x034E3970 (one byte) and 0x034E39E0 (u16):
//
//	u8         destination index  -> resolved to the displayed place name
//	u8         phase             -> 1 active request, 2 cancel, 3 terminal
//	u8         sub-state         -> 1 also drives the requester's local start
//	u16        requester PARTY SLOT                            (see below)
//	8 x (u16 character id, u8 state)   0xFFFF marks an empty party slot
//	u8         trailing value handed to the dialog
//
// The requester field is a party slot, not a character id: the reader resolves it
// through `0x269FF00(this, index)`, which is literally
// `if (index > 7) return NULL; return this->slots[index];` over the eight-entry
// roster at [0x51B2738+0x48], and the dialog stores it at +0x7C and compares it
// with `0x269FAA0()` - the local player's own slot index. The live run sent the
// leader's character id (1) and every client therefore announced the slot-1
// member instead.
//
// The phase drives the reader's own state machine: phase 1 with sub-state 1
// starts the requester's local request, phase 2 runs the cancel path (it looks
// every occupied slot up and then closes dialog 0x1F4 with the "组队传送被取消"
// notice), and the dialog is recreated for the terminal states 2 and 3
// (`0x21CD470`).
const currentPartyTeleportNotifyOpcode uint16 = 377

// currentPartyTeleportNotifyBodySize is the reader's exact body length.
const currentPartyTeleportNotifyBodySize = 30

// currentPartyTeleportNotifyEmptySlot is the reader's empty-slot sentinel.
const currentPartyTeleportNotifyEmptySlot uint16 = 0xFFFF

// Phase bytes of the S2C 377 reader (body[1]).
//
// The reader keeps the third byte it decodes in `[dialogData+0x24]` and maps it
// to the dialog's own text (DNF.exe VA 0x021CD9C0):
//
//	CMP EAX, 0x2 / PUSH 0x7949   -> string 31049, "the request has ended"
//	CMP EAX, 0x3 / PUSH 0x794E   -> string 31054, the timeout text with
//	                                重新请求/取消 (the box the live run showed
//	                                AFTER the move had already happened)
//
// so 2 - not 3 - is the terminal "close the prompt" phase. 3 is the client's
// timeout rendering and is never used for a successful move any more.
const (
	currentPartyTeleportPhaseRequest byte = 1
	currentPartyTeleportPhaseEnded   byte = 2
	currentPartyTeleportPhaseTimeout byte = 3
)

// The published request phase is 0, not the decoded "request" constant.
//
// Phase 1 makes the client create the consent window and discard it again - the
// "flicker and gone" of every early live run - while phase 0 with everything else identical
// keeps it on screen: the 2026-09-12 21:0x session pressed the retry button until that shape
// was published, and then the prompt stayed, the member accept teleported the whole
// party and both clients could close the window. The 18:18 run that was also stable
// used the same phase.
const currentPartyTeleportWirePhaseRequest byte = 0

// currentPartyTeleportSubStateActive is the reader's body[2]: a flag that, and
// only when set, makes the REQUESTER's own client start its local party-teleport
// countdown (DNF.exe VA 0x021D86769: `MOVZX EAX,[body+2]; DEC EAX; JNE skip`).
// The countdown is `[user+0x1198]` seconds long and the town menu refuses to
// start another teleport while it runs, reporting string 31064 - which is the
// "10 minute teleport cooldown" players see. It is supplied through
// DNF90_PARTY_TELEPORT_COOLDOWN.
const currentPartyTeleportSubStateActive byte = 1

// Per-slot state bytes. They share the answer space of op435, whose two button
// paths were read out of the dialog callback at DNF.exe VA 0x021CD520:
// case 302 sends `SendConfirm(1)` and case 303 sends `SendConfirm(0)`.
const (
	currentPartyTeleportSlotRefused  byte = 0
	currentPartyTeleportSlotAccepted byte = 1
	// currentPartyTeleportSlotPending is the row state for a member that has not
	// answered. The exact pending code is not proved: 2 is the first value that is
	// neither of the two button codes, and it is only a row decoration - the
	// consent decision never depends on it.
	currentPartyTeleportSlotPending byte = 2
)

// Answers carried by C2S op435 (one byte).
const (
	currentPartyTeleportAnswerRefuse byte = 0
	currentPartyTeleportAnswerAccept byte = 1
)

// currentPartyTeleportConsentTimeout bounds how long the party waits for answers.
//
// It must stay INSIDE the client's own dialog timer: that timer is registered
// with 10000 ms (`PUSH 0x2710` at DNF.exe VA 0x021CD515) and on expiry the client
// declares the client timeout text (string 31054) with its retry/cancel buttons -
// that is exactly what the live 2026-09-12 19:09 run showed on screen while the
// server was still holding its own 25 s window. Resolving before the client timer
// fires keeps the two state machines from contradicting each other.
const currentPartyTeleportConsentTimeout = 8 * time.Second

// currentPartyTeleportRetrySentinelTown/... identify the "request this party
// teleport again" form of op434.
//
// The 重新请求 button does not resend the destination: the live 2026-09-12
// 19:09:50 run shows the leader's client sending
// `ff ff ff ff ff ff 05 01` - town 255, area 255, x/y 0xFFFF - three times in a
// row, and the server refused all three as an unknown town, which is why the
// button did nothing. The destination is the server's to remember while a
// request is being consented to, so this shape is a retry of the last request for
// that party.
const (
	currentPartyTeleportRetryTown byte   = 0xFF
	currentPartyTeleportRetryArea byte   = 0xFF
	currentPartyTeleportRetryXY   uint16 = 0xFFFF
)

// currentPartyTeleportRequestIsRetry reports whether one op434 body asks for the
// previously requested party teleport instead of naming a destination.
func currentPartyTeleportRequestIsRetry(request currentPartyTeleportRequest) bool {
	return request.DestinationTown == currentPartyTeleportRetryTown &&
		request.DestinationArea == currentPartyTeleportRetryArea &&
		request.DestinationX == currentPartyTeleportRetryXY &&
		request.DestinationY == currentPartyTeleportRetryXY
}

// currentPartyTeleportCooldownOpcode is S2C 378, the packet that owns the four
// per-character countdowns the party-teleport menu checks.
//
// Contract (reader VA 0x021D55AA0, registered at 0x021D7F108 with
// `PUSH 0x1D55AA0; PUSH 0x17A`): `u8 count`, then per entry `u8 index` +
// `u32 seconds`. The reader calls StartTimer(index, seconds) on
// [0x52B5BB0]+0x679c, a 4-entry/28-byte timer table indexed by the character's
// own user slot; index 1 is ignored and 0 is the party teleport. StartTimer
// stores `now + seconds*1000` and the query (VA 0x03508360) returns the
// remaining milliseconds, so an entry of 0 seconds reads back as "not running".
// Until now the server never sent this packet, which left the countdown the
// client arms for itself (see currentPartyTeleportSubStateActive) running to its
// full length with no way to shorten it.
const currentPartyTeleportCooldownOpcode uint16 = 378

// currentPartyTeleportCooldownSeconds is the countdown length published after a
// successful party teleport: ten minutes, the retail lock the town menu enforces
// before it will start another one.
const currentPartyTeleportCooldownSeconds uint32 = 600

// currentPartyTeleportMemberListOpcode is S2C 379, the packet that builds the
// consent window's per-member rows.
//
// Contract (reader VA 0x021D62B20, registered at 0x021D7F119 with
// `PUSH 0x1D62B20; PUSH 0x17B`): `u8 mode`, `u8 count`, then `count` u16 member
// character ids. Mode 3 (0x021D62B87) walks that list, resolves each member
// through the client's own user table and ALLOCATES one 0x360-byte row object per
// member (`operator new`, DNF.exe VA 0x021D62D0A) filled from that member's data.
//
// The packet is not optional. Dialog 500's window refuses to open without it:
// `Window::Open` (vtable[0x18], VA 0x021CEAF0) ends in 0x021CE5F0 -> 0x021CE3D0,
// which walks the window's UI context for ids 501, 502, ... - one row per party
// member that is not the requester - and returns false as soon as one lookup
// comes back empty. A false Open is why the consent window was created, filled in
// and then discarded unseen (the flash every live run showed) and why old
// screenshots of the dialog carried a single avatar row.
const currentPartyTeleportMemberListOpcode uint16 = 379

// currentPartyTeleportMemberListModeCreate is the reader's mode that allocates the
// per-member row objects.
const currentPartyTeleportMemberListModeCreate byte = 3

// buildCurrentPartyTeleportMemberListBody lays out the S2C 379 body.
func buildCurrentPartyTeleportMemberListBody(members []currentPartyTeleportPlan) []byte {
	body := make([]byte, 0, 2+len(members)*2)
	body = append(body, currentPartyTeleportMemberListModeCreate, byte(len(members)))
	for _, plan := range members {
		var id [2]byte
		binary.LittleEndian.PutUint16(id[:], plan.characterID)
		body = append(body, id[:]...)
	}
	return body
}

// publishCurrentPartyTeleportMemberList sends the row-building packet every time
// before the consent prompt is published.
func (s *Service) publishCurrentPartyTeleportMemberList(
	session *gameSession,
	pending *currentPartyTeleportSession,
	source string,
) {
	if pending == nil {
		return
	}
	// The window's open gate asks for one row per member that is not the
	// requester, in party-slot order - exactly the set the prompt asks for.
	members := s.currentPartyTeleportConsentMembers(pending)
	body := buildCurrentPartyTeleportMemberListBody(members)
	sent := 0
	for _, plan := range pending.plans {
		if plan.session == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(
			plan.session, currentPartyTeleportMemberListOpcode, body, 0, true); err != nil {
			s.logGameEvent(session, "game-party-teleport-member-list-send-failed",
				"char_id", plan.characterID,
				"error", err)
			continue
		}
		sent++
	}
	s.logGameEvent(session, "game-party-teleport-member-list-sent",
		"party_id", pending.partyID,
		"member_count", len(members),
		"sent", sent,
		"msg_id", currentPartyTeleportMemberListOpcode,
		"body_len", len(body),
		"body_hex", hex.EncodeToString(body),
		"source", source)
}

// buildCurrentPartyTeleportCooldownBody lays out the S2C 378 body: one entry, the
// party-teleport index and the published countdown length.
func buildCurrentPartyTeleportCooldownBody(seconds uint32) []byte {
	body := make([]byte, 0, 6)
	body = append(body, 1) // one entry
	body = append(body, 0) // index 0 = party teleport
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], seconds)
	return append(body, encoded[:]...)
}

// publishCurrentPartyTeleportCooldown arms the client's own party-teleport
// countdown. The official server drives that countdown through this packet rather
// than relying on the client's local default, which is why the menu kept working
// after a teleport until the packet was sent.
func (s *Service) publishCurrentPartyTeleportCooldown(
	session *gameSession,
	plans []currentPartyTeleportPlan,
	seconds uint32,
	source string,
) {
	body := buildCurrentPartyTeleportCooldownBody(seconds)
	sent := 0
	for _, plan := range plans {
		if plan.session == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(
			plan.session, currentPartyTeleportCooldownOpcode, body, 0, true); err != nil {
			s.logGameEvent(session, "game-party-teleport-cooldown-send-failed",
				"char_id", plan.characterID,
				"error", err)
			continue
		}
		sent++
	}
	s.logGameEvent(session, "game-party-teleport-cooldown-published",
		"sent", sent,
		"member_count", len(plans),
		"msg_id", currentPartyTeleportCooldownOpcode,
		"body_hex", hex.EncodeToString(body),
		"seconds", seconds,
		"source", source)
}

// currentPartyTeleportPlan is one member that moves with the request.
type currentPartyTeleportPlan struct {
	session     *gameSession
	characterID uint16
	slot        byte
	sourceTown  uint16
	eligible    bool
	reason      string
}

// currentPartyTeleportSession is one request waiting for member answers.
type currentPartyTeleportSession struct {
	partyID    int
	leaderID   uint16
	leaderSlot byte
	request    currentPartyTeleportRequest
	plans      []currentPartyTeleportPlan
	answers    map[uint16]byte
	token      uint64
	// finalAccepted marks the last request-phase publish before the move: every
	// row then carries the accepted state, which is what the client needs to see
	// before it stops asking (row state 2 only hides the row).
	finalAccepted bool
	// guildID is the guild whose 公会传送次数 this request spends, captured when the
	// request passed the uses-available gate. Zero means the request was accepted
	// without a guild charge, which cannot happen while the gate below is in place
	// but keeps the zero value meaningful.
	guildID uint32
}

// handleCurrentPartyTeleportRequest validates the leader's destination for the
// whole party and then asks every member to agree.
//
// What is proved and implemented here:
//
//   - The destination is (town, area, x, y) plus the client's constant direction 5
//     (exactly the transport the personal teleport already uses). Every member is
//     moved through the same op36 owner (op22 user position, op23 user area,
//     op356 completed-quest state, op24 area users) with that member's own
//     persisted town as the route source, so a member standing in another town is
//     admitted by the same PVF route check a personal teleport uses.
//   - Only the leader may issue it. The client gates the command the same way, and the server repeats it so a modified
//     own gate; the server repeats the rule so a modified client cannot pull the
//     party around.
//   - The destination is validated for every member BEFORE anyone moves. A member
//     that cannot be routed - a PVF quest requirement it has not completed, a
//     minimum level it does not meet, an area the runtime PVF does not know -
//     aborts the whole teleport instead of splitting the party, which is what the
//     client's own retry text asks for ("玩家[%s]无法传送到[%s]，要选择其他地点吗？").
//   - The party then sees the consent prompt (S2C 377): the requester's own client
//     shows the waiting text, every other member gets
//     the confirm text with accept/refuse. The move happens
//     when every member has accepted (op435 value 1), is cancelled on a refusal
//     (op435 value 0), and is applied anyway once the consent window expires so a
//     member whose client cannot answer never leaves the leader stuck.
func (s *Service) handleCurrentPartyTeleportRequest(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	request, err := parseCurrentPartyTeleportRequest(body)
	if err != nil {
		// A body of another length is an unproved shape, not a session failure:
		// this opcode is shared with the native path report, and a refused input
		// must never close the player's connection.
		return currentPartyClientInputRejection(uint16(dnfenum.CmdPacketPartyTeleport), body, err)
	}
	charID := selectedCharacterID(session)
	state := runtimePartyStateSnapshot(session)
	members := runtimePartyMembers(state)
	s.logGameEvent(session, "game-party-teleport-request-received",
		"char_id", charID,
		"character_generation", session.characterGeneration,
		"party_id", state.PartyID,
		"leader_id", state.UserID,
		"member_count", len(members),
		"is_leader", state.PartyID > 0 && state.UserID == charID,
		"dest_town", request.DestinationTown,
		"dest_area", request.DestinationArea,
		"dest_x", request.DestinationX,
		"dest_y", request.DestinationY,
		"direction", request.Direction,
		"flag", request.Flag,
		"body_hex", hex.EncodeToString(body))
	if charID == 0 {
		return nil
	}
	if state.PartyID > 0 && len(members) > 1 && state.UserID != charID {
		s.logGameEvent(session, "game-party-teleport-blocked",
			"char_id", charID,
			"party_id", state.PartyID,
			"leader_id", state.UserID,
			"reason", "only_the_party_leader_may_teleport_the_party")
		return nil
	}
	if currentPartyTeleportRequestIsRetry(request) {
		return s.retryCurrentPartyTeleportRequest(session, state, charID)
	}
	targets := s.currentPartyTeleportTargets(session, state, members)
	plans, allEligible := s.planCurrentPartyTeleport(session, targets, request)
	if len(plans) == 0 {
		return nil
	}
	if !allEligible {
		s.logGameEvent(session, "game-party-teleport-blocked",
			"char_id", charID,
			"party_id", state.PartyID,
			"dest_town", request.DestinationTown,
			"dest_area", request.DestinationArea,
			"reason", "member_destination_not_reachable_party_not_moved",
			"member_plans", currentPartyTeleportPlanLog(plans))
		return nil
	}
	leaderID := state.UserID
	if leaderID == 0 {
		leaderID = charID
	}
	pending := &currentPartyTeleportSession{
		partyID:    state.PartyID,
		leaderID:   leaderID,
		leaderSlot: runtimePartyMemberSlots(state)[leaderID],
		request:    request,
		plans:      plans,
		answers:    make(map[uint16]byte, len(plans)),
	}
	// 公会传送次数 gate (decision D4: upstream's rule, taken exactly). A party is only
	// asked to consent when its leader actually holds 公会传送 uses, so nobody is
	// prompted for a teleport the server would then refuse. Upstream checks this
	// same condition immediately before publishing its pending state.
	//
	// This is the one place where this tree now REQUIRES a guild for op434/435:
	// before the port, party teleport was guild-independent and worked for
	// guildless players. That behaviour change was reviewed and chosen explicitly.
	guild, guildErr := s.currentGuildForSelectedCharacter(session)
	if guildErr != nil || guild == nil ||
		!guild.Benefits[7].Active(s.gameplayNow()) || guild.Benefits[7].Value == 0 {
		guildID := uint32(0)
		if guild != nil {
			guildID = guild.ID
		}
		s.logGameEvent(session, "game-party-teleport-blocked",
			"char_id", charID,
			"party_id", state.PartyID,
			"dest_town", request.DestinationTown,
			"dest_area", request.DestinationArea,
			"guild_id", guildID,
			"guild_error", guildErr,
			"reason", "guild_teleport_uses_unavailable")
		return nil
	}
	pending.guildID = guild.ID
	s.rememberCurrentPartyTeleportRequest(state.PartyID, request)
	return s.beginCurrentPartyTeleport(session, pending)
}

// retryCurrentPartyTeleportRequest serves the 重新请求 button: the client resends
// op434 with the destination fields blanked, so the destination comes from the
// request the party last made. The client's own timeout dialog offers exactly this
// button (the client timeout text plus retry/cancel), and without this path the
// button answers with an unknown town and does nothing.
func (s *Service) retryCurrentPartyTeleportRequest(
	session *gameSession,
	state alignedcmd.PartyState,
	charID uint16,
) error {
	remembered, found := s.rememberedCurrentPartyTeleportRequest(state.PartyID)
	if !found {
		s.logGameEvent(session, "game-party-teleport-retry-blocked",
			"char_id", charID,
			"party_id", state.PartyID,
			"reason", "no_previous_request_to_retry")
		return nil
	}
	// A request that is still waiting for answers is simply re-published with its
	// remaining window, so 重新请求 also works before the timeout fires.
	if pending := s.peekCurrentPartyTeleportPending(state.PartyID); pending != nil {
		s.publishCurrentPartyTeleportMemberList(session, pending, "member_retry_published")
		s.broadcastCurrentPartyTeleportNotify(
			session, pending, currentPartyTeleportPhaseRequest, "member_retry_published")
		s.logGameEvent(session, "game-party-teleport-retry-republished",
			"char_id", charID,
			"party_id", state.PartyID,
			"answered", len(pending.answers),
			"required", len(s.currentPartyTeleportConsentMembers(pending)))
		return nil
	}
	targets := s.currentPartyTeleportTargets(session, state, runtimePartyMembers(state))
	plans, allEligible := s.planCurrentPartyTeleport(session, targets, remembered)
	if len(plans) == 0 || !allEligible {
		s.logGameEvent(session, "game-party-teleport-retry-blocked",
			"char_id", charID,
			"party_id", state.PartyID,
			"dest_town", remembered.DestinationTown,
			"dest_area", remembered.DestinationArea,
			"reason", "member_destination_not_reachable_party_not_moved",
			"member_plans", currentPartyTeleportPlanLog(plans))
		return nil
	}
	leaderID := state.UserID
	if leaderID == 0 {
		leaderID = charID
	}
	s.logGameEvent(session, "game-party-teleport-retry-accepted",
		"char_id", charID,
		"party_id", state.PartyID,
		"dest_town", remembered.DestinationTown,
		"dest_area", remembered.DestinationArea,
		"dest_x", remembered.DestinationX,
		"dest_y", remembered.DestinationY,
		"source", "current_exe_retry_button_op434_blank_destination")
	return s.beginCurrentPartyTeleport(session, &currentPartyTeleportSession{
		partyID:    state.PartyID,
		leaderID:   leaderID,
		leaderSlot: runtimePartyMemberSlots(state)[leaderID],
		request:    remembered,
		plans:      plans,
		answers:    make(map[uint16]byte, len(plans)),
	})
}

// rememberCurrentPartyTeleportRequest stores the destination of the last request
// for a party so the retry button has something to replay.
func (s *Service) rememberCurrentPartyTeleportRequest(partyID int, request currentPartyTeleportRequest) {
	if s == nil || partyID <= 0 {
		return
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	if s.partyTeleportLastRequest == nil {
		s.partyTeleportLastRequest = make(map[int]currentPartyTeleportRequest, 2)
	}
	s.partyTeleportLastRequest[partyID] = request
}

func (s *Service) rememberedCurrentPartyTeleportRequest(partyID int) (currentPartyTeleportRequest, bool) {
	if s == nil || partyID <= 0 {
		return currentPartyTeleportRequest{}, false
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	request, found := s.partyTeleportLastRequest[partyID]
	return request, found
}

// currentPartyTeleportTargets resolves the sessions that move with the request.
// A one-member party is the sender alone, and a party with offline members skips
// them, because a member without a live session has no scene to transition.
func (s *Service) currentPartyTeleportTargets(
	session *gameSession,
	state alignedcmd.PartyState,
	members []alignedcmd.PartyMemberState,
) []*gameSession {
	if state.PartyID <= 0 || len(members) == 0 {
		return []*gameSession{session}
	}
	targets := make([]*gameSession, 0, len(members))
	for _, member := range members {
		if member.UserID == 0 {
			continue
		}
		target, ok := s.onlineGameSession(member.UserID)
		if !ok || target == nil {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return []*gameSession{session}
	}
	return targets
}

// planCurrentPartyTeleport loads each member's durable location and validates the
// destination against the runtime PVF before anything moves. The second return
// value is false when at least one member cannot be routed.
func (s *Service) planCurrentPartyTeleport(
	session *gameSession,
	targets []*gameSession,
	request currentPartyTeleportRequest,
) ([]currentPartyTeleportPlan, bool) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		s.logGameEvent(session, "game-party-teleport-blocked",
			"reason", "character_repository_unavailable")
		return nil, false
	}
	memberSlots := runtimePartyMemberSlots(runtimePartyStateSnapshot(session))
	plans := make([]currentPartyTeleportPlan, 0, len(targets))
	allEligible := true
	for _, target := range targets {
		charID := selectedCharacterID(target)
		if charID == 0 {
			continue
		}
		plan := currentPartyTeleportPlan{session: target, characterID: charID, slot: memberSlots[charID]}
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		character, found, err := repositories.Character.Load(ctx, strconv.FormatUint(uint64(charID), 10))
		if err == nil && found && character.Stats != nil {
			town, hasTown := character.Stats["town_id"]
			switch {
			case !hasTown || town <= 0 || town > int64(^uint16(0)):
				plan.reason = "source_town_id_missing_or_invalid"
			default:
				plan.sourceTown = uint16(town)
				plan.eligible, plan.reason = s.currentPartyTeleportDestinationEligibility(
					ctx, repositories, character, charID, request)
			}
		} else if err != nil {
			plan.reason = "character_load_failed"
		} else {
			plan.reason = "character_not_found"
		}
		cancel()
		if !plan.eligible {
			allEligible = false
		}
		plans = append(plans, plan)
	}
	return plans, allEligible
}

// currentPartyTeleportDestinationEligibility repeats the runtime-PVF gates the
// op36 owner applies, so a member that would be refused is known before the party
// is touched.
func (s *Service) currentPartyTeleportDestinationEligibility(
	ctx context.Context,
	repositories dnfrepo.Group,
	character dnfrepo.CharacterRecord,
	characterID uint16,
	request currentPartyTeleportRequest,
) (bool, string) {
	area, found := s.townArea(int64(request.DestinationTown), int64(request.DestinationArea))
	if !found {
		return false, "target_town_area_missing_from_runtime_pvf"
	}
	if area.MinLevel > int64(character.Level) {
		return false, "target_area_min_level_not_met"
	}
	if len(area.NeedQuests) == 0 {
		return true, ""
	}
	completed, err := s.townAreaNeedQuestsCompleted(
		ctx, repositories, strconv.FormatUint(uint64(characterID), 10), area.NeedQuests)
	if err != nil {
		return false, "target_area_need_quest_check_failed"
	}
	if !completed {
		return false, "target_area_has_pvf_quest_requirement_without_completed_quest_owner"
	}
	return true, ""
}

// runtimePartyMemberSlots indexes the party's native slots by character id so the
// S2C prompt can address the same slots the client's own party table uses.
func runtimePartyMemberSlots(state alignedcmd.PartyState) map[uint16]byte {
	if state.PartyID <= 0 {
		return nil
	}
	slots := make(map[uint16]byte, len(state.Members))
	for _, member := range state.Members {
		if member.UserID == 0 {
			continue
		}
		slots[member.UserID] = member.Slot
	}
	return slots
}

// beginCurrentPartyTeleport publishes the consent request, or applies the move
// straight away when there is nobody else to ask.
func (s *Service) beginCurrentPartyTeleport(session *gameSession, pending *currentPartyTeleportSession) error {
	if len(s.currentPartyTeleportConsentMembers(pending)) == 0 {
		s.logGameEvent(session, "game-party-teleport-consent-skipped",
			"party_id", pending.partyID,
			"member_count", len(pending.plans),
			"reason", "no_member_answer_required")
		s.applyCurrentPartyTeleport(session, pending, "no_member_answer_required")
		return nil
	}
	s.storeCurrentPartyTeleportPending(pending)
	// The row-building packet has to arrive before the prompt: without it the
	// client creates the consent window and then refuses to open it.
	s.publishCurrentPartyTeleportMemberList(session, pending, "consent_request_published")
	s.broadcastCurrentPartyTeleportNotify(
		session, pending, currentPartyTeleportPhaseRequest, "consent_request_published")
	s.scheduleCurrentPartyTeleportTimeout(session, pending)
	s.logGameEvent(session, "game-party-teleport-consent-window-open",
		"party_id", pending.partyID,
		"leader_id", pending.leaderID,
		"required", len(s.currentPartyTeleportConsentMembers(pending)),
		"window_ms", currentPartyTeleportConsentTimeout.Milliseconds(),
		"resolution_on_timeout", "refused")
	return nil
}

// currentPartyTeleportConsentMembers returns the members whose answer is required:
// everyone but the leader, which is also the set the client shows in the prompt.
func (s *Service) currentPartyTeleportConsentMembers(pending *currentPartyTeleportSession) []currentPartyTeleportPlan {
	members := make([]currentPartyTeleportPlan, 0, len(pending.plans))
	for _, plan := range pending.plans {
		if plan.characterID == pending.leaderID {
			continue
		}
		members = append(members, plan)
	}
	return members
}

func (s *Service) storeCurrentPartyTeleportPending(pending *currentPartyTeleportSession) {
	if s == nil || pending == nil {
		return
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	if s.partyTeleportPending == nil {
		s.partyTeleportPending = make(map[int]*currentPartyTeleportSession, 2)
	}
	s.partyTeleportToken++
	pending.token = s.partyTeleportToken
	s.partyTeleportPending[pending.partyID] = pending
}

// takeCurrentPartyTeleportPending removes and returns the pending request, but only
// for the expected token: a token mismatch means the request was already resolved
// (applied, cancelled or expired) and the caller is looking at a stale timer.
func (s *Service) takeCurrentPartyTeleportPending(partyID int, token uint64) *currentPartyTeleportSession {
	if s == nil {
		return nil
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	pending, found := s.partyTeleportPending[partyID]
	if !found {
		return nil
	}
	if token != 0 && pending.token != token {
		return nil
	}
	delete(s.partyTeleportPending, partyID)
	return pending
}

func (s *Service) peekCurrentPartyTeleportPending(partyID int) *currentPartyTeleportSession {
	if s == nil {
		return nil
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	return s.partyTeleportPending[partyID]
}

// scheduleCurrentPartyTeleportTimeout refuses a request that nobody answered
// inside the consent window.
//
// A timeout is a refusal: the party does not move. The prompt is published exactly
// ONCE - a repeated request packet re-runs the client's window Open path, which
// resets the dialog state and re-arms its own 10 s timer, and the live 2026-09-12
// 20:12 trace shows exactly that flicker at a perfect 1.5 s cadence while a
// re-publish loop was running. Progress updates still re-publish, because a member
// answering has to reach the other clients' rows.
func (s *Service) scheduleCurrentPartyTeleportTimeout(session *gameSession, pending *currentPartyTeleportSession) {
	if pending == nil || session == nil {
		return
	}
	partyID := pending.partyID
	token := pending.token
	time.AfterFunc(currentPartyTeleportConsentTimeout, func() {
		expired := s.takeCurrentPartyTeleportPending(partyID, token)
		if expired == nil {
			// Answered, refused, cancelled or already resolved.
			return
		}
		s.logGameEvent(session, "game-party-teleport-consent-timeout",
			"party_id", partyID,
			"answered", len(expired.answers),
			"required", len(s.currentPartyTeleportConsentMembers(expired)),
			"resolution", "refused_without_answer")
		s.cancelCurrentPartyTeleport(session, expired, "consent_timeout_refused")
	})
}

// applyCurrentPartyTeleport moves every member and then ends the request.
//
// Phase 2 IS published on success. Live 2026-09-12 21:3x: with the terminal phase
// skipped, the dialog's countdown bar never finished and the window stayed in its
// request state, so the client needs phase 2 to end the request - it is the only
// state that stops the timer and the only thing that makes the client consider the
// prompt finished. Its text (组队传送被取消, string 0x7949) is the client's own
// wording for "the request is over" and is accepted.
func (s *Service) applyCurrentPartyTeleport(
	session *gameSession,
	pending *currentPartyTeleportSession,
	source string,
) {
	if pending == nil {
		return
	}
	// Publish "every member agreed" BEFORE the move. The client's per-row update
	// (SetSlot, VA 0x021CDFC0) only shows or hides a row from the state byte - row
	// state 2 hides it - so the accepted state is how each client shows that its
	// own row is done.
	pending.finalAccepted = true
	// Spend one 公会传送 use before anyone moves, so a teleport can never be
	// delivered without being paid for. Upstream reserves the use at exactly this
	// point and aborts the whole teleport when the reservation fails.
	//
	// The abort is deliberately NOT copied: by the time this runs the "every member
	// accepted" notify has already gone out, and refusing here would leave the
	// party holding an accepted-but-never-executed dialog with no way back --
	// structurally worse than a rare free teleport. The request-time gate above
	// makes the residual race (the last use spent between request and consent)
	// the only way to reach this branch, and it is logged when it happens.
	if pending.guildID != 0 {
		ctx, cancel := s.currentGuildContext()
		spendErr := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
			return registry.UseTeleport(strconv.Itoa(int(pending.leaderID)), pending.guildID, s.gameplayNow())
		})
		cancel()
		if spendErr != nil {
			s.logGameEvent(session, "game-party-teleport-uses-not-spent",
				"guild_id", pending.guildID,
				"leader_id", pending.leaderID,
				"party_id", pending.partyID,
				"source", source,
				"reason", spendErr)
		}
	}
	s.broadcastCurrentPartyTeleportNotify(
		session, pending, currentPartyTeleportPhaseRequest, source+"_all_members_accepted")
	transport := currentSoloTeleportRequest{
		TownID:    pending.request.DestinationTown,
		AreaID:    pending.request.DestinationArea,
		PositionX: int16(pending.request.DestinationX),
		PositionY: int16(pending.request.DestinationY),
		Direction: pending.request.Direction,
	}
	for _, plan := range pending.plans {
		s.moveCurrentPartyTeleportMember(session, plan, transport)
	}
	// End the request, then arm the retail countdown. Both are required: without
	// phase 2 the dialog's countdown bar never finishes (live 2026-09-12 21:3x),
	// and the countdown itself is what the town menu checks before it lets the
	// party teleport again.
	s.broadcastCurrentPartyTeleportNotify(
		session, pending, currentPartyTeleportPhaseEnded, source+"_request_ended")
	s.publishCurrentPartyTeleportCooldown(
		session, pending.plans, currentPartyTeleportCooldownSeconds, source+"_cooldown_armed")
}

// cancelCurrentPartyTeleport publishes the client's timeout phase and tells nobody
// to move.
//
// A refused or expired request deliberately uses phase 3: the client renders its
// own 超过时间限制 text with the 重新请求/取消 buttons and - unlike phase 2 - never
// closes the window, so the requester still sees that nothing happened and can ask
// again. Phase 2 is reserved for a request every member accepted, which the client
// patch turns into a close.
func (s *Service) cancelCurrentPartyTeleport(
	session *gameSession,
	pending *currentPartyTeleportSession,
	reason string,
) {
	if pending == nil {
		return
	}
	s.broadcastCurrentPartyTeleportNotify(session, pending, currentPartyTeleportPhaseTimeout, reason)
}

// broadcastCurrentPartyTeleportNotify publishes one phase of the request to every
// member session.
func (s *Service) broadcastCurrentPartyTeleportNotify(
	session *gameSession,
	pending *currentPartyTeleportSession,
	phase byte,
	source string,
) {
	if pending == nil {
		return
	}
	body := buildCurrentPartyTeleportNotifyBody(pending, phase)
	s.sendCurrentPartyTeleportNotify(session, pending, body, phase, len(pending.answers), source)
}

// sendCurrentPartyTeleportNotify delivers one already-built notify body. The
// answer count is passed in because the re-publish timer snapshots it under the
// registry lock instead of reading the answer map concurrently.
func (s *Service) sendCurrentPartyTeleportNotify(
	session *gameSession,
	pending *currentPartyTeleportSession,
	body []byte,
	phase byte,
	answered int,
	source string,
) {
	if pending == nil {
		return
	}
	sent := 0
	for _, plan := range pending.plans {
		if plan.session == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(
			plan.session, currentPartyTeleportNotifyOpcode, body, 0, true); err != nil {
			s.logGameEvent(session, "game-party-teleport-notify-deferred",
				"char_id", plan.characterID,
				"phase", phase,
				"error", err)
			continue
		}
		sent++
	}
	s.logGameEvent(session, "game-party-teleport-notify-sent",
		"party_id", pending.partyID,
		"leader_id", pending.leaderID,
		"leader_slot", pending.leaderSlot,
		"phase", phase,
		"body_phase", body[1],
		"member_count", len(pending.plans),
		"sent", sent,
		"answered", answered,
		"msg_id", currentPartyTeleportNotifyOpcode,
		"body_len", len(body),
		"body_hex", hex.EncodeToString(body),
		"destination_index", pending.request.DestinationTown,
		"source", source)
}

// buildCurrentPartyTeleportNotifyBody lays out the reader's 30 bytes: destination
// index, phase, sub-state, requester party slot, the eight roster rows and the
// dialog's trailing value. Rows carry the member's character id and its state -
// accepted, refused, or still pending.
func buildCurrentPartyTeleportNotifyBody(pending *currentPartyTeleportSession, phase byte) []byte {
	body := make([]byte, currentPartyTeleportNotifyBodySize)
	if pending == nil {
		return body
	}
	// A request publish is sent as the wire phase that keeps the consent window on
	// screen; the terminal phases (2 = ended, 3 = timeout) keep their decoded
	// meaning.
	requested := phase == currentPartyTeleportPhaseRequest
	if requested {
		phase = currentPartyTeleportWirePhaseRequest
	}
	body[0] = pending.request.DestinationTown
	body[1] = phase
	if requested {
		// body[2] is the requester's own countdown flag; the retail client arms its
		// party-teleport lock from it.
		body[2] = currentPartyTeleportSubStateActive
	}
	// The requester field is a party slot: the reader resolves it against its own
	// roster (VA 0x021D86604) and the window's open gate skips that slot.
	binary.LittleEndian.PutUint16(body[3:5], uint16(pending.leaderSlot))
	for slot := 0; slot < 8; slot++ {
		binary.LittleEndian.PutUint16(body[5+slot*3:7+slot*3], currentPartyTeleportNotifyEmptySlot)
	}
	for _, plan := range pending.plans {
		if plan.slot > 7 {
			continue
		}
		binary.LittleEndian.PutUint16(body[5+int(plan.slot)*3:7+int(plan.slot)*3], plan.characterID)
		body[7+int(plan.slot)*3] = currentPartyTeleportSlotState(pending, plan)
	}
	return body
}

// currentPartyTeleportSlotState is the per-row byte. The client's SetSlot
// (VA 0x021CDFC0) uses it to show or hide the row: state 2 hides it, 0/1 show it
// as refused/accepted.
func currentPartyTeleportSlotState(pending *currentPartyTeleportSession, plan currentPartyTeleportPlan) byte {
	if pending.finalAccepted {
		// The whole party agreed: every row - including the requester's, which
		// never answers - reports the accepted state so the client has nothing
		// left to ask.
		return currentPartyTeleportSlotAccepted
	}
	answer, answered := pending.answers[plan.characterID]
	switch {
	case !answered:
		return currentPartyTeleportSlotPending
	case answer == currentPartyTeleportAnswerAccept:
		return currentPartyTeleportSlotAccepted
	default:
		return currentPartyTeleportSlotRefused
	}
}

// handleCurrentPartyTeleportConfirm consumes one member's answer.
//
// op435 carries a single byte, and its two button paths were read off the dialog
// callback at DNF.exe VA 0x021CD520: switch case 302 calls `SendConfirm(1)` for
// 接受 and case 303 calls `SendConfirm(0)` for 拒绝 (the writer itself is at
// VA 0x021CD420 and emits `write_u16(0x1B3); write_u8(value)`). The live
// 2026-09-12 18:18 run produced exactly three answers - 0x01 from the leader and
// 0x03/0x00 from the member - which is also where the third value comes from: it
// is not one of the two button codes and is treated as "not accepted", leaving the
// decision to the consent window.
func (s *Service) handleCurrentPartyTeleportConfirm(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	charID := selectedCharacterID(session)
	state := runtimePartyStateSnapshot(session)
	answer := byte(0)
	if len(body) > 0 {
		answer = body[0]
	}
	s.logGameEvent(session, "game-party-teleport-confirm-received",
		"char_id", charID,
		"party_id", state.PartyID,
		"leader_id", state.UserID,
		"answer", answer,
		"answer_meaning", currentPartyTeleportAnswerLabel(answer),
		"body_len", len(body),
		"body_hex", hex.EncodeToString(body))
	if charID == 0 {
		return nil
	}
	pending := s.resolveCurrentPartyTeleportAnswer(session, charID, answer)
	if pending == nil {
		return nil
	}
	// Resolution is one-shot: taking the record by its own token means a duplicate
	// answer, or the consent timer firing at the same moment, cannot apply or
	// cancel the same request twice.
	switch {
	case answer == currentPartyTeleportAnswerRefuse:
		if s.takeCurrentPartyTeleportPending(pending.partyID, pending.token) == nil {
			return nil
		}
		s.cancelCurrentPartyTeleport(session, pending, "member_refused")
	case s.currentPartyTeleportConsentComplete(pending):
		if s.takeCurrentPartyTeleportPending(pending.partyID, pending.token) == nil {
			return nil
		}
		s.applyCurrentPartyTeleport(session, pending, "all_members_accepted")
	default:
		s.broadcastCurrentPartyTeleportNotify(
			session, pending, currentPartyTeleportPhaseRequest, "consent_progress")
	}
	return nil
}

// resolveCurrentPartyTeleportAnswer records one answer and returns the pending
// request when it is still live.
func (s *Service) resolveCurrentPartyTeleportAnswer(
	session *gameSession,
	charID uint16,
	answer byte,
) *currentPartyTeleportSession {
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	if len(s.partyTeleportPending) == 0 {
		return nil
	}
	for partyID, pending := range s.partyTeleportPending {
		if !currentPartyTeleportSessionContains(pending, charID) {
			continue
		}
		if pending.answers == nil {
			pending.answers = make(map[uint16]byte, len(pending.plans))
		}
		pending.answers[charID] = answer
		s.logGameEvent(session, "game-party-teleport-answer-recorded",
			"party_id", partyID,
			"char_id", charID,
			"answer", answer,
			"answered", len(pending.answers),
			"required", len(s.currentPartyTeleportConsentMembers(pending)))
		return pending
	}
	return nil
}

func currentPartyTeleportSessionContains(pending *currentPartyTeleportSession, charID uint16) bool {
	if pending == nil || charID == 0 {
		return false
	}
	for _, plan := range pending.plans {
		if plan.characterID == charID {
			return true
		}
	}
	return false
}

func (s *Service) currentPartyTeleportConsentComplete(pending *currentPartyTeleportSession) bool {
	for _, plan := range s.currentPartyTeleportConsentMembers(pending) {
		if pending.answers[plan.characterID] != currentPartyTeleportAnswerAccept {
			return false
		}
	}
	return true
}

func currentPartyTeleportAnswerLabel(answer byte) string {
	switch answer {
	case currentPartyTeleportAnswerAccept:
		return "accept"
	case currentPartyTeleportAnswerRefuse:
		return "refuse"
	default:
		return "unproved_value_treated_as_not_accepted"
	}
}

// moveCurrentPartyTeleportMember performs one member's scene transition. The
// member's own persisted town is the route source, so the shared op36 owner
// validates the destination against the PVF exactly as it does for a personal
// teleport.
func (s *Service) moveCurrentPartyTeleportMember(
	session *gameSession,
	plan currentPartyTeleportPlan,
	transport currentSoloTeleportRequest,
) {
	if plan.session == nil || plan.sourceTown == 0 {
		return
	}
	s.logGameEvent(session, "game-party-teleport-member-moving",
		"char_id", plan.characterID,
		"slot", plan.slot,
		"source_town_id", plan.sourceTown,
		"dest_town", transport.TownID,
		"dest_area", transport.AreaID,
		"dest_x", transport.PositionX,
		"dest_y", transport.PositionY)
	if err := s.handleTownSetUserArea(
		plan.session, buildCurrentSoloTeleportMoveBody(transport, plan.sourceTown)); err != nil {
		s.logGameEvent(session, "game-party-teleport-member-blocked",
			"char_id", plan.characterID,
			"error", err)
	}
}

// endCurrentPartyTeleportForSession drops any pending request that a leaving
// session was part of, so a stale prompt cannot be answered after the party is
// gone.
func (s *Service) endCurrentPartyTeleportForSession(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	charID := selectedCharacterID(session)
	if charID == 0 {
		return
	}
	s.partyTeleportMu.Lock()
	defer s.partyTeleportMu.Unlock()
	for partyID, pending := range s.partyTeleportPending {
		if currentPartyTeleportSessionContains(pending, charID) {
			delete(s.partyTeleportPending, partyID)
		}
	}
}

// currentPartyTeleportPlanLog renders per-member eligibility for one log line.
func currentPartyTeleportPlanLog(plans []currentPartyTeleportPlan) string {
	out := make([]string, 0, len(plans))
	for _, plan := range plans {
		verdict := "ineligible"
		if plan.eligible {
			verdict = "eligible"
		}
		out = append(out, fmt.Sprintf("%d:%s:slot%d:sourceTown%d:%s",
			plan.characterID, verdict, plan.slot, plan.sourceTown, plan.reason))
	}
	return "[" + strings.Join(out, " ") + "]"
}
