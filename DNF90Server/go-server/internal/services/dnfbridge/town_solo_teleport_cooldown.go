package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// Personal teleport (个人传送, C2S op470) shares the teleport countdown table with
// the party teleport, but it does NOT share the party teleport's timer entry.
//
// The client keeps every teleport countdown in one table: four 0x1C-byte entries
// at [0x52B5BB0]+0x679C, reached through Cooldown::Start (VA 0x024A08E0):
//
//	sub eax, 4 / jge return            ; index >= 4 is dropped
//	lea edx, [eax*8]; sub edx, eax     ; stride 7 dwords = 0x1C
//	lea esi, [ecx + edx*4 + 0x679C]    ; &table[index]
//	imul eax, [ebp+0xC], 0x3E8         ; seconds -> milliseconds
//
// Exactly four call sites arm that table, and only one of them can arm an entry
// the client did not choose for itself:
//
//	0x01D55B08  the S2C 378 reader   -> StartTimer(packet index, packet seconds)
//	0x01D867B1  the S2C 377 reader   -> StartTimer(0,                       user[+0x1198])
//	0x011D9B71  the op470 side       -> StartTimer(user[+0x1194],           user[+0x1198])
//	0x01D0DD94  (index 0 reset)      -> StartTimer(0,                       stack value)
//
// The two self-arm sites take the countdown LENGTH from the local user object
// (user[+0x1198], populated from character data +0x104C). That value is zero in
// live play: the 2026-09-12 party-teleport capture (tools/partydlg_trace, which
// hooks Cooldown::Start itself) recorded 40 calls of `index=0 seconds=0` and no
// arm with a real length. That is exactly why the party chain had to publish its
// 600 s through S2C 378, and it is why the personal teleport currently has no
// cooldown at all: nothing ever arms its entry with a non-zero length.
const (
	// currentSoloTeleportCooldownOpcode is S2C 378, the same packet the party
	// chain publishes. Contract: `u8 count`, then per entry `u8 index` +
	// `u32 seconds`. The reader (VA 0x01D55AA0, registered at 0x01D7F108 with
	// `push 0x1D55AA0; push 0x17A`) applies StartTimer for index 0 and for
	// index 2/3 only - index 1 fails its `lea ecx,[eax-2]; cmp cl,1; ja skip`
	// guard and is silently dropped.
	currentSoloTeleportCooldownOpcode = currentPartyTeleportCooldownOpcode

	// currentSoloTeleportCooldownSeconds is the retail personal-teleport lock:
	// five minutes, the value the town-menu entry counts down and the op470
	// request gate refuses on.
	currentSoloTeleportCooldownSeconds uint32 = 300

	// currentSoloTeleportFailureOpcode is S2C 470 - the opcode's own failure
	// callback, not an invented error code. The reader registered for op470 in
	// the game dispatcher (VA 0x01D39A20, registered at 0x01D515F3 with
	// `push 0x1D39A20; push 0x1D6`) reads no body at all and unconditionally
	// shows its generic "请稍候再试" dialog (string 0x570A = 22282). The
	// successful path deliberately never sends op470 - it advances the scene
	// through the shared op24 owner - so this packet is free to serve as the
	// server-side refusal.
	currentSoloTeleportFailureOpcode uint16 = uint16(dnfenum.CmdPacketSoloTelepoart)
)

// currentSoloTeleportCooldownDefaultIndices are the S2C 378 entries the personal
// teleport owns.
//
// The client itself decides which index the personal teleport uses: the op470
// request gate (VA 0x011D98C4) and the town-menu entry (VA 0x01F84BCF,
// 0x01F84C59) all call the same accessor (VA 0x024A4EF0), which reads the index
// out of the local user object:
//
//	mov eax, [esi]; mov edx, [eax+0xB28]; push 9; call edx   ; local user
//	mov eax, [eax+0x1194]                                    ; timer index
//	cmp eax, 4 / jl ok / or eax, -1                          ; >= 4 -> "always cooling"
//	lea ecx, [eax*8]; sub ecx, eax; lea ecx, [esi+ecx*4+0x679C]
//	jmp 0x03508360                                           ; remaining milliseconds
//
// That field is copied from character data (+0x1048) at VA 0x0230D640. Its two
// possible personal values are what the town-menu entry turns into a label
// (VA 0x01F84C06):
//
//	test esi, esi / je -> push 0x33BF (=13247 "组队传送")     ; index 0
//	add esi, -2 / cmp esi, 1 / ja skip
//	                     push 0x33C2 (=13250 "个人传送")      ; index 2 or 3
//
// so index 0 belongs to 组队传送 and 2/3 to 个人传送, and the same {0,2,3} set is
// what the S2C 378 reader accepts. The client treats 2 and 3 as one feature -
// one label, one tooltip (string 0x33BE), one shared cooldown query - so the
// server arms both and never touches index 0, which stays reserved for the party
// chain's 600 s.
var currentSoloTeleportCooldownDefaultIndices = []byte{2, 3}

// currentSoloTeleportCooldownIndicesEnv optionally overrides which S2C 378
// entries are armed, so an operator can pin the exact entry a client uses
// (Cooldown::Start logs `index=` for every arm, see tools/partydlg_trace) without
// a rebuild. `off` (or an empty list) disables the publish and the gate.
const currentSoloTeleportCooldownIndicesEnv = "DNF90_SOLO_TELEPORT_COOLDOWN_INDICES"

// currentSoloTeleportCooldownIndices resolves the entries to arm. Only the three
// indices the client's own reader accepts (0, 2, 3) are honoured; index 1 is
// dropped because the client drops it.
func currentSoloTeleportCooldownIndices() []byte {
	raw := strings.TrimSpace(os.Getenv(currentSoloTeleportCooldownIndicesEnv))
	if raw == "" {
		return currentSoloTeleportCooldownDefaultIndices
	}
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "none") {
		return nil
	}
	indices := make([]byte, 0, 4)
	seen := make(map[byte]struct{}, 4)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, err := strconv.ParseUint(field, 0, 16)
		if err != nil || !currentSoloTeleportCooldownIndexAccepted(byte(value)) {
			continue
		}
		index := byte(value)
		if _, duplicate := seen[index]; duplicate {
			continue
		}
		seen[index] = struct{}{}
		indices = append(indices, index)
	}
	return indices
}

// currentSoloTeleportCooldownIndexAccepted reports whether the client's S2C 378
// reader would actually apply an entry for this index: 0 and 2/3 pass its
// `test al,al / je apply` and `lea ecx,[eax-2]; cmp cl,1; ja skip` guards, while
// index 1 and anything above 3 is dropped.
func currentSoloTeleportCooldownIndexAccepted(index byte) bool {
	return index == 0 || index == 2 || index == 3
}

// buildCurrentSoloTeleportCooldownBody lays out one S2C 378 body: `u8 count`
// followed by `count` entries of `u8 index` + `u32 seconds`.
func buildCurrentSoloTeleportCooldownBody(indices []byte, seconds uint32) []byte {
	body := make([]byte, 0, 1+len(indices)*5)
	body = append(body, byte(len(indices)))
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], seconds)
	for _, index := range indices {
		body = append(body, index)
		body = append(body, encoded[:]...)
	}
	return body
}

// currentSoloTeleportClock returns the clock the cooldown is measured against.
func (s *Service) currentSoloTeleportClock() time.Time {
	if s != nil && s.soloTeleportClock != nil {
		return s.soloTeleportClock()
	}
	return time.Now()
}

// currentSoloTeleportCooldownRemaining reports how much of the character's
// personal-teleport cooldown is left. The lock is per CHARACTER, never per
// account: the client's countdown lives on the logged-in user object, so two
// characters on one account each keep their own.
func (s *Service) currentSoloTeleportCooldownRemaining(characterID uint16) (time.Duration, bool) {
	if s == nil || characterID == 0 {
		return 0, false
	}
	now := s.currentSoloTeleportClock()
	s.soloTeleportCooldownMu.Lock()
	defer s.soloTeleportCooldownMu.Unlock()
	until, tracked := s.soloTeleportCooldownUntil[characterID]
	if !tracked {
		return 0, false
	}
	if !until.After(now) {
		// Expired entries are dropped so the map cannot grow without bound.
		delete(s.soloTeleportCooldownUntil, characterID)
		return 0, false
	}
	return until.Sub(now), true
}

// armCurrentSoloTeleportCooldown starts (or restarts) the character's
// personal-teleport cooldown.
func (s *Service) armCurrentSoloTeleportCooldown(characterID uint16) time.Time {
	if s == nil || characterID == 0 {
		return time.Time{}
	}
	until := s.currentSoloTeleportClock().Add(
		time.Duration(currentSoloTeleportCooldownSeconds) * time.Second)
	s.soloTeleportCooldownMu.Lock()
	defer s.soloTeleportCooldownMu.Unlock()
	if s.soloTeleportCooldownUntil == nil {
		s.soloTeleportCooldownUntil = make(map[uint16]time.Time, 2)
	}
	s.soloTeleportCooldownUntil[characterID] = until
	return until
}

// clearCurrentSoloTeleportCooldown releases the character's cooldown. It exists
// for regression tests and for the explicit "the character left" lifecycle; the
// cooldown itself survives scene transitions.
func (s *Service) clearCurrentSoloTeleportCooldown(characterID uint16) {
	if s == nil || characterID == 0 {
		return
	}
	s.soloTeleportCooldownMu.Lock()
	defer s.soloTeleportCooldownMu.Unlock()
	delete(s.soloTeleportCooldownUntil, characterID)
}

// publishCurrentSoloTeleportCooldown arms the client's own personal-teleport
// countdown. Without it the town-menu entry never counts down and the op470
// request gate never refuses, because both read the same timer entry and nothing
// else in the client can arm it (see the call-site list above).
func (s *Service) publishCurrentSoloTeleportCooldown(
	session *gameSession,
	characterID uint16,
	source string,
) {
	indices := currentSoloTeleportCooldownIndices()
	if len(indices) == 0 {
		s.logGameEvent(session, "game-town-solo-teleport-cooldown-disabled",
			"char_id", characterID,
			"source", source)
		return
	}
	body := buildCurrentSoloTeleportCooldownBody(indices, currentSoloTeleportCooldownSeconds)
	if err := s.sendGameUpperRawClassCodec(
		session, currentSoloTeleportCooldownOpcode, body, 0, true); err != nil {
		s.logGameEvent(session, "game-town-solo-teleport-cooldown-send-failed",
			"char_id", characterID,
			"error", err,
			"source", source)
		return
	}
	s.logGameEvent(session, "game-town-solo-teleport-cooldown-published",
		"char_id", characterID,
		"msg_id", currentSoloTeleportCooldownOpcode,
		"body_hex", hex.EncodeToString(body),
		"indices", currentSoloTeleportCooldownIndexLog(indices),
		"seconds", currentSoloTeleportCooldownSeconds,
		"source", source)
}

// refuseCurrentSoloTeleportDuringCooldown answers a request that arrived inside
// the cooldown window.
//
// The client normally never sends this request: its own gate (VA 0x011D98C4,
// `call 0x024A4EF0` then `push 0x8D3E`) refuses locally and shows string
// 0x8D3E = 36158 "个人传送功能正在冷却中" before op470 is written. A request can
// still arrive from a client that ignored that gate, so the server refuses it
// through the opcode's own failure callback instead of moving the character.
func (s *Service) refuseCurrentSoloTeleportDuringCooldown(
	session *gameSession,
	request currentSoloTeleportRequest,
	remaining time.Duration,
) {
	body := []byte{0}
	if err := s.sendGameUpperRawClassCodec(
		session, currentSoloTeleportFailureOpcode, body, 0, true); err != nil {
		s.logGameEvent(session, "game-town-solo-teleport-cooldown-refusal-send-failed",
			"char_id", session.selectedCharacterID,
			"error", err)
		return
	}
	s.logGameEvent(session, "game-town-solo-teleport-cooldown-blocked",
		"char_id", session.selectedCharacterID,
		"town_id", request.TownID,
		"area_id", request.AreaID,
		"remaining_ms", remaining.Milliseconds(),
		"msg_id", currentSoloTeleportFailureOpcode,
		"body_hex", hex.EncodeToString(body),
		"source", "current_exe_op470_request_inside_server_cooldown")
}

// armCurrentSoloTeleportCooldownAfterMove arms the cooldown only when the shared
// op36 owner actually landed the destination.
//
// The shared owner reports every refusal as a nil error (it logs a
// `game-town-set-user-area-blocked` reason instead), so success is read back from
// the durable location: a refused or redirected destination must not punish the
// player with a five-minute lock.
func (s *Service) armCurrentSoloTeleportCooldownAfterMove(
	session *gameSession,
	request currentSoloTeleportRequest,
) {
	if session == nil || session.selectedCharacterID == 0 {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(
		ctx, strconv.FormatUint(uint64(session.selectedCharacterID), 10))
	if err != nil || !found || character.Stats == nil {
		s.logGameEvent(session, "game-town-solo-teleport-cooldown-not-armed",
			"char_id", session.selectedCharacterID,
			"reason", "selected_character_location_unavailable",
			"error", err)
		return
	}
	if character.Stats["town_id"] != int64(request.TownID) ||
		character.Stats["area_id"] != int64(request.AreaID) {
		s.logGameEvent(session, "game-town-solo-teleport-cooldown-not-armed",
			"char_id", session.selectedCharacterID,
			"reason", "destination_not_committed",
			"requested_town_id", request.TownID,
			"requested_area_id", request.AreaID,
			"stored_town_id", character.Stats["town_id"],
			"stored_area_id", character.Stats["area_id"])
		return
	}
	until := s.armCurrentSoloTeleportCooldown(session.selectedCharacterID)
	s.publishCurrentSoloTeleportCooldown(
		session, session.selectedCharacterID, "current_exe_op470_move_committed")
	s.logGameEvent(session, "game-town-solo-teleport-cooldown-armed",
		"char_id", session.selectedCharacterID,
		"town_id", request.TownID,
		"area_id", request.AreaID,
		"until_unix_ms", until.UnixMilli(),
		"seconds", currentSoloTeleportCooldownSeconds)
}

// currentSoloTeleportCooldownIndexLog renders the armed entries for one log line.
func currentSoloTeleportCooldownIndexLog(indices []byte) string {
	if len(indices) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(indices))
	for _, index := range indices {
		parts = append(parts, strconv.Itoa(int(index)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
