package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) currentLaborDayCatalog(ctx context.Context) (*dnfonlineevent.LaborDayCatalog, error) {
	s.laborDayMu.Lock()
	defer s.laborDayMu.Unlock()
	if s.laborDayCatalog != nil {
		return s.laborDayCatalog, nil
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	repos, ok := s.repositoryGroup()
	if archive == nil || !ok || repos.Settings == nil {
		return nil, nil
	}
	c, err := dnfonlineevent.LoadLaborDayCatalog(ctx, archive)
	if err != nil {
		return nil, err
	}
	c.Policy, err = dnfonlineevent.LoadOrCreateLaborDayPolicy(ctx, repos.Settings, s.gameplayNow())
	if err != nil {
		return nil, err
	}
	s.laborDayCatalog = c
	s.logPacketEvent("labor-day-2348-window", "starts", c.Policy.Starts, "ends", c.Policy.Ends, "daily_seconds", 600, "daily_limit", 10)
	return c, nil
}

func (s *Service) preloadLaborDayCatalog(ctx context.Context) error {
	_, err := s.currentLaborDayCatalog(ctx)
	return err
}

func (s *Service) settleCurrentLaborDay(ctx context.Context, session *gameSession, c *dnfonlineevent.LaborDayCatalog, from, to time.Time, login bool) error {
	if c == nil {
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfonlineevent.ErrOwnerUnavailable
	}
	characterID := session.spendTime.characterID
	if characterID == 0 {
		characterID = session.selectedCharacterID
	}
	id := strconv.Itoa(int(characterID))
	now := s.gameplayNow()
	state, err := dnfonlineevent.SettleLaborDay(ctx, repos, c, dnfonlineevent.LaborDayCommand{
		AccountID: s.accountIDForSession(session), CharacterID: id, Now: now, From: from, To: to, Login: login,
		Deliver: func(mailboxes dnfrepo.MailboxRepository, r dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
			items, err := s.currentPVFItemCatalog()
			if err != nil {
				return "", err
			}
			d, err := items.ResolveItem(uint32(r.ItemID))
			if err != nil {
				return "", err
			}
			d, err = currentPVFItemDefinitionForGrantAt(d, now.UTC())
			if err != nil {
				return "", err
			}
			if d.Kind != dungeonDropItemStackable || (!d.ExpirationDate.IsZero() && !d.ExpirationDate.After(now)) {
				return "", fmt.Errorf("labor day reward %d expired or unsupported", r.ItemID)
			}
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: id, Title: "狂热劳动节豪礼大放送", Body: "劳动节活动奖励已送达，请查收附件。每日在线每满10分钟获得一次惊喜礼盒，最多10次。", Source: "labor_day_2348", CompletionKey: key, CreatedAt: now,
				Attachments: []dnfrepo.MailAttachment{{ItemID: r.ItemID, Count: r.Count, ExpireAt: d.ExpirationDate}},
			})
		},
	})
	if err != nil {
		return err
	}
	if len(state.MailIDs) > 0 {
		s.logGameEvent(session, "game-labor-day-reward", "login_days", state.LoginDays, "today_seconds", state.TodaySeconds, "today_paid", state.TodayPaid, "mail_count", len(state.MailIDs))
		// Mail has committed. An alarm failure must not prevent the observation
		// cursor advancing; the mailbox is authoritative on the next login.
		if err := s.sendMailboxAlarmForSession(session, characterID, "labor_day_2348"); err != nil {
			s.logGameEvent(session, "game-labor-day-mail-alarm-deferred", "error", err)
		}
	}
	return nil
}
