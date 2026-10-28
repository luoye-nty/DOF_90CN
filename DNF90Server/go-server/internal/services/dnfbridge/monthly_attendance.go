package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentMonthlyAttendanceEventID uint16 = 2370
const currentMonthlyAttendanceClaimID uint16 = 1294

// Accessed by the session event owner, including its queued observation tick.
type currentMonthlyAttendanceProjection struct {
	characterID         uint16
	characterGeneration uint64
	date                string
	body                [37]byte
}

func (s *Service) currentMonthlyCatalog(ctx context.Context) (*dnfonlineevent.MonthlyCatalog, error) {
	s.monthlyAttendanceMu.Lock()
	defer s.monthlyAttendanceMu.Unlock()
	if s.monthlyAttendanceCatalog != nil {
		return s.monthlyAttendanceCatalog, nil
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return nil, nil
	}
	catalog, err := dnfonlineevent.LoadMonthlyCatalog(ctx, archive)
	if err != nil {
		return nil, err
	}
	s.monthlyAttendanceCatalog = catalog
	return catalog, nil
}

// sub_E713E0 -> sub_F1C4D0 consumes five u32 fields in narrow op1206.
func buildCurrentMonthlyAttendanceBody(state dnfonlineevent.MonthlySnapshot) []byte {
	var writer packetWriter
	writer.writeUint32(uint32(currentMonthlyAttendanceEventID))
	writer.writeByte(0)
	writer.writeUint32(state.Days)
	for _, value := range state.Bonus {
		writer.writeUint32(value)
	}
	writer.writeUint32(state.Clears)
	writer.writeBytes(make([]byte, 12))
	return writer.bytes()
}

func (s *Service) sendCurrentMonthlyAttendance(session *gameSession, claim bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	catalog, err := s.currentMonthlyCatalog(ctx)
	if err != nil || catalog == nil {
		return err
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return err
	}
	now := s.gameplayNow()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	state, err := dnfonlineevent.SettleMonthlyAttendance(ctx, repos, catalog, dnfonlineevent.MonthlyCommand{
		AccountID: s.accountIDForSession(session), CharacterID: characterID, Now: now, ClaimBonus: claim,
		Deliver: func(mailboxes dnfrepo.MailboxRepository, reward dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
			definition, err := items.ResolveItem(uint32(reward.ItemID))
			if err != nil {
				return "", err
			}
			if definition.Kind != dungeonDropItemStackable || (!definition.ExpirationDate.IsZero() && !definition.ExpirationDate.After(now)) {
				return "", fmt.Errorf("monthly attendance reward item=%d is expired or unsupported", reward.ItemID)
			}
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID, Title: "火热签到奖励", Body: "签到奖励已发放，请查收附件。",
				Source: "monthly_attendance_2370", CompletionKey: key, CreatedAt: now,
				Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count, ExpireAt: definition.ExpirationDate}},
			})
		},
	})
	if err != nil {
		return err
	}
	if claim {
		// Class-1 1294 is registered to DoNothing; op1206 owns UI updates.
		if err := s.sendGameUpperRawClass(session, currentMonthlyAttendanceClaimID, []byte{1}, 1); err != nil {
			return err
		}
	}
	projection := currentMonthlyAttendanceProjection{
		characterID: session.selectedCharacterID, characterGeneration: session.characterGeneration,
		date: now.In(dnfonlineevent.ChinaCalendar).Add(-6 * time.Hour).Format("2006-01-02"),
		body: [37]byte(buildCurrentMonthlyAttendanceBody(state)),
	}
	// sub_F1C4D0 rearms the completion balloon on every completed snapshot.
	// Keep settlement/reward retries running, but publish only changed state.
	if session.monthlyAttendance != projection {
		if err := s.sendGameUpperRawClass(session, currentSpendTimeProgressMsgID, projection.body[:], 0); err != nil {
			return err
		}
		session.monthlyAttendance = projection
	}
	if len(state.MailIDs) > 0 {
		s.logGameEvent(session, "game-monthly-attendance-mail", "char_id", session.selectedCharacterID, "days", state.Days, "mail_count", len(state.MailIDs))
		return s.sendMailboxAlarmForSession(session, session.selectedCharacterID, "monthly_attendance")
	}
	return nil
}

func (s *Service) handleCurrentMonthlyAttendanceClaim(session *gameSession, body []byte) error {
	// F1C3B0 copies 13 opaque stack bytes. None supplies reward IDs or counts.
	if len(body) != 13 || session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperRawClass(session, currentMonthlyAttendanceClaimID, []byte{0, 1}, 1)
	}
	return s.sendCurrentMonthlyAttendance(session, true)
}
