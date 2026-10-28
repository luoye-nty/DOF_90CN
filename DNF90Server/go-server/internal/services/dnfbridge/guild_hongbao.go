package dnfbridge

import (
	"encoding/binary"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

func (s *Service) handleCurrentGuildHongbaoList(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketGetGuildHongbaoList)
	if len(body) != 0 {
		return s.sendCurrentGuildHongbaoFailure(session, opcode)
	}
	if updates, err := s.applyCurrentGuildContribution(session, ""); err != nil {
		s.logGameEvent(session, "game-guild-contribution-delivery-deferred", "error", err)
	} else if err = s.sendCurrentGuildContributionUpdates(session, updates); err != nil {
		return err
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return s.sendCurrentGuildHongbaoFailure(session, opcode)
	}
	// sub_D643A0：公会人数、今日消费、今日领取数、红包记录数。
	var writer packetWriter
	writer.writeUint16(uint16(len(guild.Members)))
	writer.writeUint32(0)
	writer.writeUint32(0)
	writer.writeUint32(0)
	return s.sendGameUpperSuccess(session, opcode, writer.bytes())
}

func (s *Service) handleCurrentGuildHongbaoPoints(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketGetGuildHongbaoPointList)
	if len(body) != 0 {
		return s.sendCurrentGuildHongbaoFailure(session, opcode)
	}
	if err := s.flushCurrentGuildContribution(session); err != nil {
		return err
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return s.sendCurrentGuildHongbaoFailure(session, opcode)
	}
	return s.sendGameUpperSuccess(session, opcode, buildCurrentGuildHongbaoPointsBody(guild))
}

func (s *Service) sendCurrentGuildHongbaoFailure(session *gameSession, opcode uint16) error {
	// 红包失败回调还会读取一个 u32 错误码，普通两字节失败包不足。
	return s.sendGameUpperRaw(session, opcode, []byte{0, currentGuildGenericError, currentGuildGenericError, 0, 0, 0})
}

func buildCurrentGuildHongbaoPointsBody(guild *dnfguild.Guild) []byte {
	members := dnfguild.SortedMembers(guild)
	// sub_D63EF0 的原生缓冲区最多容纳 1000 条 40 字节记录。
	if len(members) > 1000 {
		members = members[:1000]
	}
	var writer packetWriter
	writer.writeUint32(uint32(len(members)))
	for _, member := range members {
		var raw [40]byte
		binary.LittleEndian.PutUint32(raw[:], currentGuildNumericID(member.CharacterID))
		// sub_FAE7D0 使用 +4 的 u8 等级填写 myleveltext（lv.%d）。
		raw[4] = byte(member.Level)
		copy(raw[5:34], rosterNameBytes(member.Name))
		// sub_D63EF0 reads the independent contribution points at byte +35.
		binary.LittleEndian.PutUint32(raw[35:39], member.ContributionPoints)
		writer.writeBytes(raw[:])
	}
	return writer.bytes()
}
