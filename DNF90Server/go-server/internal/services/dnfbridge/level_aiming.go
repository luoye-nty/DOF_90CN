package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// levelAimingWindowActive 是"全职Lv预约"（活动 222）此刻是否落在它自己的 PVF
// 窗口（event/levelaimingevent.evt 的 [control panel] 起止时间）内的唯一判据。
//
// 注册/结算（SettleLevelAiming 的 active）、角色选择页的下发和 op108 事件表必须
// 用同一个判据：只要有一处漏判，客户端就会在活动早已结束时继续显示入口图标。
func levelAimingWindowActive(catalog *dnfonlineevent.LevelAimingCatalog, now time.Time) bool {
	if catalog == nil || catalog.Starts.IsZero() || catalog.Ends.IsZero() || now.IsZero() {
		return false
	}
	return !now.Before(catalog.Starts) && now.Before(catalog.Ends)
}

func (s *Service) currentLevelAimingCatalog(ctx context.Context) (*dnfonlineevent.LevelAimingCatalog, error) {
	s.levelAimingMu.Lock()
	defer s.levelAimingMu.Unlock()
	if s.levelAimingCatalog != nil {
		return s.levelAimingCatalog, nil
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return nil, nil
	}
	catalog, err := dnfonlineevent.LoadLevelAimingCatalog(ctx, archive)
	if err == nil {
		s.levelAimingCatalog = catalog
	}
	return catalog, err
}

// sub_1D56CD0 consumes exactly u32 + eight bytes. The first field gates
// creating another event character; +8 (the penultimate byte) drives the HUD.
func buildCurrentLevelAimingBody(state dnfonlineevent.LevelAimingState, character dnfrepo.CharacterRecord, selector bool) []byte {
	var w packetWriter
	id, _ := strconv.ParseUint(state.CharacterID, 10, 32)
	w.writeUint32(uint32(id))
	start, target, status, job := byte(1), byte(255), byte(255), byte(0)
	if state.CharacterID != "" && state.CharacterID == character.CharacterID {
		start = byte(state.Start)
		if state.Target != 0 {
			target = byte(state.Target)
		}
		job = packRosterJobGrow(byte(numericCharacterStat(character.Job)), byte(numericCharacterStatValue(character, "grow_type")))
		status = 3
		// Initial selector synchronization must not trigger sub_33E0CA0's
		// automatic enter-game branch. Reservation responses use status 3.
		if selector {
			status = 1
		}
	}
	w.writeByte(start)
	w.writeByte(target)
	w.writeByte(0)
	w.writeByte(0)
	w.writeByte(job)
	w.writeByte(0)
	w.writeByte(status)
	w.writeByte(byte(state.Reservations))
	return w.bytes()
}

func (s *Service) settleCurrentLevelAiming(ctx context.Context, session *gameSession, catalog *dnfonlineevent.LevelAimingCatalog, id uint16, register bool, target uint32) (dnfonlineevent.LevelAimingResult, error) {
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfonlineevent.LevelAimingResult{}, dnfonlineevent.ErrOwnerUnavailable
	}
	characterID := strconv.Itoa(int(id))
	now := s.gameplayNow()
	return dnfonlineevent.SettleLevelAiming(ctx, repos, catalog, dnfonlineevent.LevelAimingCommand{
		AccountID: s.accountIDForSession(session), CharacterID: characterID, Register: register, Target: target, Now: now,
		Deliver: func(mailboxes dnfrepo.MailboxRepository, reward dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
			items, err := s.currentPVFItemCatalog()
			if err != nil {
				return "", err
			}
			definition, err := items.ResolveItem(uint32(reward.ItemID))
			if err != nil {
				return "", err
			}
			if definition.Kind != dungeonDropItemStackable || (!definition.ExpirationDate.IsZero() && !definition.ExpirationDate.After(now)) {
				return "", fmt.Errorf("level aiming reward %d expired or unsupported", reward.ItemID)
			}
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID, Title: "Lv预约升级奖励", Body: "预约升级奖励已发放，请查收附件。", Source: "level_aiming_222", CompletionKey: key, CreatedAt: now,
				Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count, ExpireAt: definition.ExpirationDate}},
			})
		},
	})
}

func (s *Service) sendCurrentLevelAiming(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	catalog, err := s.currentLevelAimingCatalog(ctx)
	if err != nil || catalog == nil {
		return err
	}
	result, err := s.settleCurrentLevelAiming(ctx, session, catalog, session.selectedCharacterID, false, 0)
	if err != nil {
		return err
	}
	if err = s.sendGameUpperRawClass(session, 623, buildCurrentLevelAimingBody(result.LevelAimingState, result.Character, false), 0); err != nil {
		return err
	}
	if len(result.MailIDs) > 0 {
		return s.sendMailboxAlarmForSession(session, session.selectedCharacterID, "level_aiming")
	}
	return nil
}

func (s *Service) sendCurrentLevelAimingSelector(session *gameSession) error {
	if session == nil || session.selectedCharacterID != 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	catalog, err := s.currentLevelAimingCatalog(ctx)
	if err != nil || catalog == nil {
		return err
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Account == nil || repos.Character == nil {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	account, found, err := repos.Account.Load(ctx, s.accountIDForSession(session))
	if err != nil {
		return err
	}
	if !found {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	state, err := dnfonlineevent.ReadLevelAimingState(account)
	if err != nil {
		return err
	}
	var ch dnfrepo.CharacterRecord
	if state.CharacterID != "" {
		ch, found, err = repos.Character.Load(ctx, state.CharacterID)
		if err != nil {
			return err
		}
		if !found || ch.AccountID != account.AccountID {
			ch = dnfrepo.CharacterRecord{}
		}
	}
	spend, available, err := s.currentSpendTimeRuntimeCatalogIfAvailable(ctx)
	if err != nil {
		return err
	}
	if !available {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	now := s.gameplayNow()
	body, err := buildCurrentOnlineEventInfoBodyAt(spend.rewardItemIDs, spend.sumRewardItemIDs, spend.totalStageSeconds, spend.nativePCRoom != nil, levelAimingWindowActive(catalog, now), spend.laborDay, now)
	if err != nil {
		return err
	}
	compressed, err := zlibCompress(body)
	if err != nil {
		return err
	}
	if err = s.sendCurrentProtectedClass0Packet(session, currentSpendTimeEventInfoMsgID, compressed, currentSpendTimeEventInfoCodec, "level_aiming_selector_catalog"); err != nil {
		return err
	}
	if !levelAimingWindowActive(catalog, now) {
		// A nonzero gate prevents new event creation outside the configured dates.
		state.CharacterID = "65535"
		ch = dnfrepo.CharacterRecord{}
	}
	return s.sendGameUpperRawClass(session, 623, buildCurrentLevelAimingBody(state, ch, true), 0)
}

func (s *Service) handleCurrentLevelAiming(session *gameSession, body []byte) error {
	fail := func(err error) error {
		s.logGameEvent(session, "game-level-aiming-rejected", "error", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, 689, []byte{0, 222, 0, 0, 0, 1}, 1)
	}
	if session == nil || len(body) != 16 || binary.LittleEndian.Uint32(body) != 222 || binary.LittleEndian.Uint32(body[12:]) != 0 {
		return fail(fmt.Errorf("invalid level aiming request"))
	}
	reference, target := binary.LittleEndian.Uint32(body[4:]), binary.LittleEndian.Uint32(body[8:])
	register := target == 255
	if target == 0 || (register && (reference == 0 || reference > 65535 || session.selectedCharacterID != 0 || session.levelAimingCreatedID != uint16(reference))) || (!register && reference >= maxCharacterSlots) {
		return fail(fmt.Errorf("level aiming session/character mismatch"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	id := uint16(reference)
	if !register {
		// sub_1D037E0 registers an ID; Main2/sub_13F27D0 reserves by roster slot,
		// including when opened in town. Never fall back from a slot to an ID.
		repos, ok := s.repositoryGroup()
		if !ok || repos.Character == nil {
			return fail(dnfonlineevent.ErrOwnerUnavailable)
		}
		accountID := s.accountIDForSession(session)
		characters, err := repos.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
		if err != nil {
			return fail(err)
		}
		character, _, found := selectCharacterRecord(characters, selectCharacterRequest{slot: int(reference)})
		parsedID, err := strconv.ParseUint(character.CharacterID, 10, 16)
		if !found || character.AccountID != accountID || err != nil || parsedID == 0 {
			return fail(fmt.Errorf("level aiming slot %d has no owned character", reference))
		}
		id = uint16(parsedID)
		if session.selectedCharacterID != 0 && session.selectedCharacterID != id {
			return fail(fmt.Errorf("level aiming slot %d does not match selected character", reference))
		}
	}
	catalog, err := s.currentLevelAimingCatalog(ctx)
	if err != nil {
		return fail(err)
	}
	if catalog == nil {
		return fail(dnfonlineevent.ErrOwnerUnavailable)
	}
	if register {
		target = 0
	}
	result, err := s.settleCurrentLevelAiming(ctx, session, catalog, id, register, target)
	if err != nil {
		return fail(err)
	}
	if err = s.sendGameUpperRawClass(session, 689, []byte{1, 222, 0, 0, 0}, 1); err != nil {
		return err
	}
	if register && session.selectedCharacterID == 0 {
		repos, _ := s.repositoryGroup()
		characters, err := s.listCharacters(ctx, repos, s.accountIDForSession(session))
		if err != nil {
			return err
		}
		if err = s.sendUpperRoster(session, characters); err != nil {
			return err
		}
	}
	if err = s.sendGameUpperRawClass(session, 623, buildCurrentLevelAimingBody(result.LevelAimingState, result.Character, false), 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-level-aiming-committed", "character_id", id, "request_reference", reference, "register", register, "target", result.Target, "reservations", result.Reservations, "mails", len(result.MailIDs))
	if len(result.MailIDs) > 0 && session.selectedCharacterID == id {
		return s.sendMailboxAlarmForSession(session, id, "level_aiming")
	}
	return nil
}
