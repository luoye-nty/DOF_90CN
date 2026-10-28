package dnfbridge

import (
	"encoding/hex"
	"errors"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 公会基地 (guild agit) -- class1/op227/228/229/231.
//
// The current client drives the whole feature from four packets and turns every
// refusal into a toast whose text comes from a per-opcode table:
//
//	227 check-create   empty request
//	228 create         empty request (push 0xE4 at 026AE560)
//	229 delete         empty request (push 0xE5 at 026AE580)
//	231 upgrade        u8 target stage (push 0xE7 + sub_3454FC0 at 026AE5A3)
//
// Every reply is {u8 success, u8 code}: zero means failure and the code then
// selects the toast; anything else is success. The handlers (01D0C040 /
// 01D0C150 / 01D0C2B0 / 01D0C400) read nothing else on success -- their success
// branches end in `pop ebp; ret` -- so {1} can never walk the client off the
// packet, which is exactly what made the op747 stub crash. The failure codes
// below are decoded from those handlers' jump tables (D:\re_tools\agit_codes.txt):
//
//	36  -> 829    您不是公会会长              (228, 231)
//	100 -> 19140  您还未加入公会              (all four)
//	181 -> 21492  已经生成公会基地。          (227, 228)
//	182 -> 21496  公会基地不存在。            (229, 231)
//	183 -> 6427   公会Lv不足                  (227, 228)
//	187 -> 21495  金币不足，无法创建公会基地。 (228) / 1660 金币不足 (231)
//	188 -> 21508  公会基地升级Lv设置错误。    (231)
const (
	currentGuildAgitErrorNotMember = byte(100)
	currentGuildAgitErrorNotMaster = byte(36)
	currentGuildAgitErrorExists    = byte(181)
	currentGuildAgitErrorMissing   = byte(182)
	currentGuildAgitErrorLevel     = byte(183)
	currentGuildAgitErrorGold      = byte(187)
	currentGuildAgitErrorStage     = byte(188)
)

func currentGuildAgitErrorCode(err error) byte {
	switch {
	case errors.Is(err, dnfguild.ErrPermissionDenied):
		return currentGuildAgitErrorNotMaster
	case errors.Is(err, dnfguild.ErrNotMember), errors.Is(err, dnfguild.ErrCharacterNotFound):
		return currentGuildAgitErrorNotMember
	case errors.Is(err, dnfguild.ErrBaseExists):
		return currentGuildAgitErrorExists
	case errors.Is(err, dnfguild.ErrBaseMissing):
		return currentGuildAgitErrorMissing
	case errors.Is(err, dnfguild.ErrBaseStage):
		return currentGuildAgitErrorStage
	default:
		return currentGuildAgitErrorNotMember
	}
}

// memberGuild resolves the request's guild, returning ErrNotMember when the
// selected character belongs to none.
func memberGuild(registry *dnfguild.Registry, characterID string) (*dnfguild.Guild, error) {
	guild, _, found := registry.GuildForCharacter(characterID)
	if !found || guild == nil {
		return nil, dnfguild.ErrNotMember
	}
	return guild, nil
}

// logCurrentGuildAgitRequest records every 公会基地 request with its body and the
// stage the server held when it arrived, from inside the guild mutation lock. The
// client drives the whole 公会基地 window from these four opcodes, so this line is
// what tells us which button was pressed and what the server believed the stage
// was. See the 2026-09-12 stage-display investigation in the project notes.
func (s *Service) logCurrentGuildAgitRequest(session *gameSession, opcode uint16, body []byte, guild *dnfguild.Guild) {
	stage := byte(0)
	if guild != nil {
		stage = guild.BaseStage
	}
	s.logGameEvent(session, "game-guild-base-request",
		"opcode", opcode, "body", hex.EncodeToString(body), "stage", stage)
}

// handleCurrentGuildAgitCheck answers 228's precondition probe. The client uses
// only its failure branch, so a member whose guild may place a base gets success.
func (s *Service) handleCurrentGuildAgitCheck(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketCheckCreateGuildAgit)
	err := s.mutateCurrentGuildAgit(session, func(registry *dnfguild.Registry, characterID string) error {
		guild, err := memberGuild(registry, characterID)
		if err != nil {
			return err
		}
		s.logCurrentGuildAgitRequest(session, opcode, body, guild)
		if guild.BaseStage != 0 {
			return dnfguild.ErrBaseExists
		}
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildAgitErrorCode(err))
	}
	return s.sendGameUpperSuccess(session, opcode, nil)
}

func (s *Service) handleCurrentGuildAgitCreate(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketCreateGuildAgit)
	err := s.mutateCurrentGuildAgit(session, func(registry *dnfguild.Registry, characterID string) error {
		guild, err := memberGuild(registry, characterID)
		if err != nil {
			return err
		}
		s.logCurrentGuildAgitRequest(session, opcode, body, guild)
		return registry.CreateBase(characterID, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildAgitErrorCode(err))
	}
	s.logGameEvent(session, "game-guild-base-created", "char_id", session.selectedCharacterID)
	return s.sendGameUpperSuccess(session, opcode, nil)
}

func (s *Service) handleCurrentGuildAgitDelete(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketDeleteGuildAgit)
	err := s.mutateCurrentGuildAgit(session, func(registry *dnfguild.Registry, characterID string) error {
		guild, err := memberGuild(registry, characterID)
		if err != nil {
			return err
		}
		s.logCurrentGuildAgitRequest(session, opcode, body, guild)
		return registry.DeleteBase(characterID, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildAgitErrorCode(err))
	}
	s.logGameEvent(session, "game-guild-base-deleted", "char_id", session.selectedCharacterID)
	return s.sendGameUpperSuccess(session, opcode, nil)
}

// handleCurrentGuildAgitUpgrade consumes the single stage byte op231 carries.
func (s *Service) handleCurrentGuildAgitUpgrade(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketUpgradeGuildAgit)
	if len(body) != 1 {
		return s.sendGameUpperFailure(session, opcode, currentGuildAgitErrorStage)
	}
	stage := body[0]
	err := s.mutateCurrentGuildAgit(session, func(registry *dnfguild.Registry, characterID string) error {
		guild, err := memberGuild(registry, characterID)
		if err != nil {
			return err
		}
		s.logCurrentGuildAgitRequest(session, opcode, body, guild)
		return registry.UpgradeBase(characterID, stage, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildAgitErrorCode(err))
	}
	s.logGameEvent(session, "game-guild-base-upgraded",
		"char_id", session.selectedCharacterID, "stage", stage)
	return s.sendGameUpperSuccess(session, opcode, nil)
}

// mutateCurrentGuildAgit runs one guild mutation for the request's selected
// character. These handlers are request-driven, so taking guildMu here is the
// same pattern the other guild mutators use -- and, unlike the online publish
// path, this goroutine cannot already hold it.
func (s *Service) mutateCurrentGuildAgit(
	session *gameSession,
	apply func(registry *dnfguild.Registry, characterID string) error,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return dnfguild.ErrCharacterNotFound
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	return s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return apply(registry, characterID)
	})
}
