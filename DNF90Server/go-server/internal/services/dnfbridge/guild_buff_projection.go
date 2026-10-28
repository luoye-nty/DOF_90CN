package dnfbridge

import (
	"math"
	"strconv"
	"sync"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

type currentGuildBuff struct {
	index     uint32
	expiresAt time.Time
}

type currentGuildBuffProjection struct {
	mu          sync.Mutex
	characterID uint16
	buffs       [2]currentGuildBuff
}

// The caller has committed op24 and owns townMu. Recreate the guild buffs on
// dungeon return: sub_18ADDA0 updates parameters but retains the original tick.
func (s *Service) sendCurrentGuildBuffProjection(session *gameSession, source string) error {
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil {
		return err
	}
	return s.writeCurrentGuildBuffProjection(session, guild, true, source)
}

// Called only after the selected actor and scene transition exist.
func (s *Service) writeCurrentGuildBuffProjection(session *gameSession, guild *dnfguild.Guild, force bool, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	now := s.gameplayNow().UTC()
	var desired [2]currentGuildBuff
	if guild != nil {
		if _, member := guild.Members[strconv.Itoa(int(session.selectedCharacterID))]; member {
			// Current Script.pvf: Guild_Status / Guild_Status1, both icon 206.
			// Other content values (EXP percentage, teleport) are not buff IDs.
			for i, kind := range [...]byte{3, 5} {
				benefit, found := guild.Benefits[kind]
				if found && benefit.Value == uint32(167+i*2) && benefit.Active(now) {
					desired[i] = currentGuildBuff{index: benefit.Value, expiresAt: benefit.ExpiresAt}
				}
			}
		}
	}
	state := &session.guildBuffProjection
	state.mu.Lock()
	defer state.mu.Unlock()
	force = force || state.characterID != session.selectedCharacterID
	// 2026-10-08：重放时**先把两份都移除、再按登录顺序把它们加回来**。
	// 原先每个槽位是"移除 A→加 A→移除 B→加 B"交替下发；两个公会属性 buff 共用同一个
	// 图标(206)，交替 churn 之后客户端只剩最后加进去的那一个的加成效力 —— 现场就是
	// "两个公会属性（限时167/永久169，各+60四维）进图回城后只回来 60 点"。
	// 拆成两趟之后，添加顺序与登录时（167 然后 169，实测 +120 正常）完全一致。
	var pending [2]currentGuildBuff
	for i, buff := range desired {
		previous := state.buffs[i]
		if !force && previous.index == buff.index && previous.expiresAt.Equal(buff.expiresAt) {
			pending[i] = previous
			continue
		}
		if previous.index != 0 {
			var body packetWriter
			body.writeByte(1)
			body.writeUint32(previous.index)
			// sub_18AE7D0 removes this exact VariousBuff and its HUD entry.
			if err := s.sendGameUpperRawClass(session, 488, body.bytes(), 0); err != nil {
				return err
			}
			state.buffs[i] = currentGuildBuff{}
		}
		pending[i] = buff
	}
	for i, buff := range pending {
		if buff.index == 0 {
			continue
		}
		seconds := uint32(0)
		if !buff.expiresAt.IsZero() {
			seconds = uint32(math.Min(math.Ceil(buff.expiresAt.Sub(now).Seconds()), math.MaxInt32))
		}
		var body packetWriter
		body.writeByte(1)
		body.writeUint32(buff.index)
		body.writeUint32(0)       // expire_time parameter 0, as defined in PVF
		body.writeUint32(seconds) // parameter 1; zero means permanent
		body.writeUint32(0)       // auxiliary value, unused by Guild_Status groups
		// sub_18AE360 -> sub_18AD260 -> sub_18A9060 -> sub_1F8B060.
		if err := s.sendGameUpperRawClass(session, 487, body.bytes(), 0); err != nil {
			return err
		}
		state.buffs[i] = buff
		s.logPacketEvent("game-guild-various-buff-send",
			"conn_id", session.connID, "char_id", session.selectedCharacterID,
			"source", source, "buff_index", buff.index, "remaining_seconds", seconds,
			"sequence", "class0/op487-various-buff")
	}
	state.characterID = session.selectedCharacterID
	return nil
}
