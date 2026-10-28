package dnfbridge

import (
	"encoding/binary"
	"errors"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentGuildGuardianStateMsgID = uint16(1297)
	currentGuildGuardianStateSize  = 2174
)

func buildCurrentGuildGuardianStateBody(guild *dnfguild.Guild) []byte {
	body := make([]byte, currentGuildGuardianStateSize)
	body[1] = 0xff
	binary.LittleEndian.PutUint32(body[2166:], 65537)
	if guild == nil {
		return body
	}
	binary.LittleEndian.PutUint16(body[2166:], guild.WarShopLevel(0))
	binary.LittleEndian.PutUint16(body[2168:], guild.WarShopLevel(1))
	// sub_E88380 reads AP/GP at guild raw11804/11808, relative to11781.
	body[2] = guild.AreaLevel
	binary.LittleEndian.PutUint32(body[23:], guild.GuardianAP)
	binary.LittleEndian.PutUint32(body[27:], guild.GuardianGP)
	for i, guardian := range guild.Guardians {
		if i >= 5 {
			break
		}
		off := 2040 + i*18
		binary.LittleEndian.PutUint32(body[off:], guardian.ID)
		body[off+4] = guardian.Level
		copy(body[off+5:off+10], guardian.Skills[:])
		binary.LittleEndian.PutUint32(body[off+10:], guardian.APExp)
		binary.LittleEndian.PutUint32(body[off+14:], guardian.GPExp)
		if guardian.ID == guild.ActiveGuardian {
			// sub_E92E00 uses this ID to replace the base's guardian NPC.
			binary.LittleEndian.PutUint32(body[68:], guardian.ID)
		}
	}
	return body
}

func parseCurrentGuildGuardianRequest(body []byte) (byte, uint32, bool) {
	if len(body) != 16 {
		return 0, 0, false
	}
	action := binary.LittleEndian.Uint32(body)
	id := binary.LittleEndian.Uint32(body[4:])
	valid := action <= 8 && id != 0
	if action >= 7 {
		valid = valid && binary.LittleEndian.Uint64(body[8:]) == 0
	} else {
		valid = valid && binary.LittleEndian.Uint32(body[12:]) != 0
	}
	return byte(action), id, valid
}

func (s *Service) handleCurrentGuildGuardian(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketAgitWarGuardian)
	action, guardianID, ok := parseCurrentGuildGuardianRequest(body)
	ready, _ := s.currentSceneTransitionReadyForState(session)
	if !ok || !ready {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	costs, err := dnfguild.LoadGuardianCosts(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	var growth dnfguild.GuardianGrowth
	if action < 7 {
		growth, err = dnfguild.LoadGuardianGrowth(items.source)
		if err != nil {
			return s.sendGameUpperFailure(session, opcode, 1)
		}
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err = s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		var applyErr error
		if action < 7 {
			applyErr = registry.GrowGuardian(character.CharacterID, action, guardianID, binary.LittleEndian.Uint32(body[8:]), binary.LittleEndian.Uint32(body[12:]), growth, s.gameplayNow())
		} else {
			applyErr = registry.SetGuardian(character.CharacterID, action, guardianID, costs, s.gameplayNow())
		}
		if applyErr != nil {
			return applyErr
		}
		selected, _, _ = registry.GuildForCharacter(character.CharacterID)
		return nil
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-guardian-rejected", "action", action, "guardian_id", guardianID, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildGuardianError(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	s.logGameEvent(session, "game-guild-guardian-saved", "action", action, "guardian_id", guardianID)
	return s.publishCurrentGuildGuardianState(session, selected)
}

func currentGuildGuardianError(err error) byte {
	if errors.Is(err, dnfguild.ErrInsufficientGuardian) {
		return 22
	}
	if errors.Is(err, dnfguild.ErrNotMember) {
		return 100
	}
	if errors.Is(err, dnfguild.ErrRepositoryUnavailable) {
		return 1
	}
	return 3
}

func (s *Service) handleCurrentGuildAreaExtend(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketAgitWarExtend)
	ready, _ := s.currentSceneTransitionReadyForState(session)
	if len(body) != 1 || !ready {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	costs, err := dnfguild.LoadAreaCosts(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err = s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		if err := registry.ExtendArea(character.CharacterID, body[0], costs, s.gameplayNow()); err != nil {
			return err
		}
		selected, _, _ = registry.GuildForCharacter(character.CharacterID)
		return nil
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-area-extend-rejected", "level", body[0], "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildGuardianError(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode, []byte{selected.AreaLevel}); err != nil {
		return err
	}
	s.logGameEvent(session, "game-guild-area-extend-saved", "level", selected.AreaLevel)
	return s.publishCurrentGuildGuardianState(session, selected)
}

func (s *Service) writeCurrentGuildGuardianState(session *gameSession, guild *dnfguild.Guild) error {
	if guild != nil {
		if repos, ok := s.repositoryGroup(); ok && repos.Settings != nil && repos.Character != nil {
			ctx, cancel := s.currentGuildContext()
			defer cancel()
			var body []byte
			err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, r dnfguild.Registry) error {
				body = buildCurrentGuildWarOverviewBody(guild, r)
				return nil
			})
			if err != nil {
				return err
			}
			return s.sendGameUpperRawClass(session, currentGuildGuardianStateMsgID, body, 0)
		}
	}
	return s.sendGameUpperRawClass(session, currentGuildGuardianStateMsgID, buildCurrentGuildGuardianStateBody(guild), 0)
}

func (s *Service) publishCurrentGuildGuardianState(session *gameSession, guild *dnfguild.Guild) error {
	for id := range guild.Members {
		if id == strconv.Itoa(int(session.selectedCharacterID)) {
			continue
		}
		if target, found := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(id))); found {
			if ready, _ := s.currentSceneTransitionReadyForState(target.session); ready {
				if err := s.writeCurrentGuildGuardianState(target.session, guild); err != nil {
					s.logGameEvent(target.session, "game-guild-guardian-refresh-failed", "error", err)
				}
			}
		}
	}
	return s.writeCurrentGuildGuardianState(session, guild)
}
