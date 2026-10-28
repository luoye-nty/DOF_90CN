package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentWeeklyAttendanceEventID uint16 = 2208
const currentWeeklyAttendanceClaimID uint16 = 1113
const currentWeeklyAttendanceStateID uint16 = 1093

type currentWeeklyAttendanceProjection struct {
	characterID         uint16
	characterGeneration uint64
	body                [459]byte
}

func (s *Service) currentWeeklyCatalog(ctx context.Context) (*dnfonlineevent.WeeklyCatalog, error) {
	s.weeklyAttendanceMu.Lock()
	defer s.weeklyAttendanceMu.Unlock()
	if s.weeklyAttendanceCatalog != nil {
		return s.weeklyAttendanceCatalog, nil
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return nil, nil
	}
	c, err := dnfonlineevent.LoadWeeklyCatalog(ctx, archive)
	if err != nil {
		return nil, err
	}
	s.weeklyAttendanceCatalog = c
	return c, nil
}

// sub_1524370 reads this packed structure; sub_1523B20 consumes the offsets below.
func buildCurrentWeeklyAttendanceBody(state dnfonlineevent.WeeklySnapshot) [459]byte {
	var b [459]byte
	binary.LittleEndian.PutUint32(b[4:], state.Total)
	binary.LittleEndian.PutUint32(b[8:], state.BonusMask)
	copy(b[0x19c:], state.SignedWeeks[:])
	binary.LittleEndian.PutUint32(b[0x1b4:], state.Clears)
	binary.LittleEndian.PutUint32(b[0x1bc:], state.Catchups)
	if state.SignedThisWeek {
		binary.LittleEndian.PutUint32(b[0x1c0:], 1)
	}
	binary.LittleEndian.PutUint32(b[0x1c4:], state.Week)
	if state.CanSign {
		b[0x1c8] = 1
	}
	if state.CanCatchup {
		b[0x1c9] = 1
	}
	if state.CanBonus {
		b[0x1ca] = 1
	}
	return b
}

func (s *Service) sendCurrentWeeklyAttendance(session *gameSession, claim, bonus bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	// 运营屏蔽了 2208（牧童的每周惊喜 / 周签到）时连状态包也不再推送：
	// 否则达成条件后客户端会凭这一包把图标和面板重新画出来。
	if eventSuppressed(currentWeeklyAttendanceEventID) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	c, err := s.currentWeeklyCatalog(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		if claim || bonus {
			return dnfonlineevent.ErrOwnerUnavailable
		}
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	now := s.gameplayNow()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	state, err := dnfonlineevent.SettleWeeklyAttendance(ctx, repos, c, dnfonlineevent.WeeklyCommand{
		AccountID: s.accountIDForSession(session), CharacterID: characterID, Now: now, Claim: claim, ClaimBonus: bonus,
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
				return "", fmt.Errorf("weekly reward %d expired or unsupported", reward.ItemID)
			}
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID, Title: "周周签到奖励", Body: "签到奖励已发放，请查收附件。", Source: "weekly_attendance_2208", CompletionKey: key, CreatedAt: now,
				Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count, ExpireAt: definition.ExpirationDate}},
			})
		},
	})
	if err != nil {
		return err
	}
	if claim {
		ack := []byte{1, 0, 0, 0, 0}
		if len(state.MailIDs) == 0 {
			ack[1] = 1
		}
		if err := s.sendGameUpperRawClass(session, currentWeeklyAttendanceClaimID, ack, 1); err != nil {
			return err
		}
	}
	if bonus {
		var ack packetWriter
		ack.writeUint32(uint32(currentWeeklyAttendanceEventID))
		if err := s.sendGameUpperSuccess(session, 689, ack.bytes()); err != nil {
			return err
		}
	}
	projection := currentWeeklyAttendanceProjection{session.selectedCharacterID, session.characterGeneration, buildCurrentWeeklyAttendanceBody(state)}
	if session.weeklyAttendance != projection || claim || bonus {
		if err := s.sendGameUpperRawClass(session, currentWeeklyAttendanceStateID, projection.body[:], 0); err != nil {
			return err
		}
		session.weeklyAttendance = projection
	}
	if len(state.MailIDs) > 0 {
		s.logGameEvent(session, "game-weekly-attendance-mail", "char_id", session.selectedCharacterID, "total", state.Total, "mail_count", len(state.MailIDs))
		return s.sendMailboxAlarmForSession(session, session.selectedCharacterID, "weekly_attendance")
	}
	return nil
}

func (s *Service) handleCurrentWeeklyAttendanceClaim(session *gameSession, body []byte) error {
	// Native code copies thirteen opaque stack bytes; rewards and counts are server-owned.
	if len(body) != 13 || session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperRawClass(session, currentWeeklyAttendanceClaimID, []byte{0, 1}, 1)
	}
	return s.sendCurrentWeeklyAttendance(session, true, false)
}

func (s *Service) handleCurrentWeeklyAttendanceBonus(session *gameSession, body []byte) error {
	if len(body) != 16 || binary.LittleEndian.Uint32(body) != uint32(currentWeeklyAttendanceEventID) || binary.LittleEndian.Uint32(body[4:]) != 0 || binary.LittleEndian.Uint64(body[8:]) != 0 || session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperRawClass(session, 689, []byte{0, 1}, 1)
	}
	return s.sendCurrentWeeklyAttendance(session, false, true)
}
