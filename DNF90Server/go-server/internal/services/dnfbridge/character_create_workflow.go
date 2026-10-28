package dnfbridge

import (
	"context"
	"errors"
	"strconv"
	"time"

	dnfpvp "longheng.io/server/internal/modules/dnf/pvp"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) createCharacter(session *gameSession, req createCharacterRequest, upper bool) error {
	s.characterCreateMu.Lock()
	defer s.characterCreateMu.Unlock()

	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		s.logGameEvent(session, "game-create-repository-missing", "upper", upper)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	}
	// 决斗场专用角色：校色请求把"专用转职号"放在已有 options 里，不新增字段。
	// 0xff = 普通角色（两个形态都不给专用转职）。
	privateGrow := byte(0xff)
	if len(req.options) >= 6 {
		privateGrow = req.options[5]
		// sub_3098640 uses eight options: reserved byte 5, private grow 6,
		// then an event flag. sub_33D4990 uses six, with private grow at 5.
		// A nonzero byte 5 also identifies the short form with codec padding.
		if len(req.options) >= 8 && privateGrow == 0 {
			privateGrow = req.options[6]
		}
	}
	private := privateGrow != 0xff
	// 专用角色要查角色列表+删除历史做资格校验，比普通建号慢，给它更宽的预算。
	timeout := createWriteTimeout
	if private {
		timeout = currentRepositorySnapshotTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	accountID := s.accountIDForSession(session)
	name, err := decodeValidatedCharacterName(req.nameRaw)
	if err != nil {
		s.logGameEvent(session, "game-create-name-decode-failed", "account_id", accountID, "name_len", len(req.nameRaw), "error", err)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	}
	existing, slotLimit, err := s.listCharactersWithLimit(ctx, repos, accountID)
	if err != nil {
		s.logGameEvent(session, "game-create-list-failed", "account_id", accountID, "error", err)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	}
	// Character-slot tickets persist an account-specific capacity in the
	// 32..60 range.  The selector already projects that value, so the create
	// gate must use the same authoritative limit instead of the old fixed 32.
	if len(existing) >= slotLimit {
		return s.sendCreateFailure(session, upper, createCodeSlotFull)
	}
	if duplicated, err := s.characterNameExists(ctx, repos, existing, name); err != nil {
		s.logGameEvent(session, "game-create-name-check-failed", "account_id", accountID, "name", name, "error", err)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	} else if duplicated {
		return s.sendCreateFailure(session, upper, createCodeDuplicated)
	}
	characterID, err := s.nextCharacterID(ctx, repos, existing)
	if err != nil {
		s.logGameEvent(session, "game-create-id-failed", "account_id", accountID, "error", err)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	}
	if upper && characterID > upperMaxCharacter {
		s.logGameEvent(session, "game-upper-create-id-too-large", "account_id", accountID, "character_id", characterID)
		return s.sendUpperCreateError(session, createCodeServerError)
	}
	slot := nextCharacterSlot(existing, slotLimit)
	if slot < 0 {
		s.logGameEvent(session, "game-create-slot-full",
			"account_id", accountID,
			"existing", len(existing),
			"slot_limit", slotLimit,
			"reason", "no_free_slot")
		return s.sendCreateFailure(session, upper, createCodeSlotFull)
	}
	now := time.Now().UTC()
	level := newCharacterInitialLevel
	stats := defaultCreatedCharacterStatsFromRequest(req)
	if private {
		plan, err := s.loadPVPPrivateCreation(req.job, privateGrow)
		if err != nil {
			s.logGameEvent(session, "game-pvp-private-create-plan-failed", "job", req.job, "grow", privateGrow, "error", err)
			return s.sendCreateFailure(session, upper, createCodeServerError)
		}
		if _, err := plan.Check(ctx, repos.Character, accountID, now); err != nil {
			s.logGameEvent(session, "game-pvp-private-create-rejected", "error", err)
			return s.sendCreateFailure(session, upper, createCodeServerError)
		}
		level = plan.Level
		experience, err := s.currentPVFLevelEntryExperience(ctx, level)
		if err != nil {
			return s.sendCreateFailure(session, upper, createCodeServerError)
		}
		stats[dnfpvp.PrivateCharacterKey], stats["grow_type"] = 1, int64(plan.Grow)
		stats["exp"], stats["tutorial_completed"] = int64(experience), 1
		stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] = int64(level)
	}
	record := dnfrepo.CharacterRecord{
		CharacterID: strconv.Itoa(characterID),
		AccountID:   accountID,
		Slot:        slot,
		Name:        name,
		Job:         strconv.Itoa(int(req.job)),
		Level:       level,
		Stats:       stats,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.saveNewCharacter(ctx, repos, record); err != nil {
		var rejection dnfpvp.PrivateCreateError
		if errors.As(err, &rejection) {
			s.logGameEvent(session, "game-pvp-private-create-transaction-rejected", "code", byte(rejection))
		}
		s.logGameEvent(session, "game-create-save-failed", "account_id", accountID, "character_id", characterID, "error", err)
		return s.sendCreateFailure(session, upper, createCodeServerError)
	}
	updated, err := s.listCharacters(ctx, repos, accountID)
	if err != nil {
		updated = append(existing, record)
	}
	// 注：参考实现在这里会设置 session.levelAimingCreatedID（专用角色清零）。
	// 本树没有"预约升级"这条链路（gameSession 无该字段），所以不引入该语义。
	if err := s.sendCreateSuccess(session, upper, record, updated); err != nil {
		return err
	}
	s.logGameEvent(session, "game-create-success",
		"upper", upper,
		"account_id", accountID,
		"character_id", characterID,
		"slot", slot,
		"job", req.job,
		"name", name,
		"option_len", len(req.options))
	return nil
}
