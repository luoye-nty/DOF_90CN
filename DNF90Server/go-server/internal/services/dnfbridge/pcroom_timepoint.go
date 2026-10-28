package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPCRoomTimePointEventID uint16 = 228
const currentPCRoomTimePointStateID uint16 = 561
const currentPCRoomTimePointRequestID uint16 = 632

// pcroomPeriodLoopEnabled 决定「累计 25 次」领完之后能不能重新累计。
//
//	默认：开启 —— 四个累计档全部领完的下一个服务日（06:00 换天）Total/PeriodMask 归零，
//	        每个账号按自己的进度独立重置。
//	关闭：在 /opt/dnf90/conf/server.env 里加 DNF90_PCROOM_PERIOD_LOOP=off 后重启，
//	        恢复"领满 25 次即永久结束"的封顶语义。
func (s *Service) pcroomPeriodLoopEnabled() bool {
	if s == nil {
		return true
	}
	s.pcroomPeriodLoopOnce.Do(func() {
		enabled := true
		switch strings.ToLower(strings.TrimSpace(os.Getenv("DNF90_PCROOM_PERIOD_LOOP"))) {
		case "0", "false", "off", "no", "disable", "disabled":
			enabled = false
		}
		s.pcroomPeriodLoop = enabled
	})
	return s.pcroomPeriodLoop
}

// 0x1BD5E10 reads exactly 17 bytes. With PVF daily reward loop=1, the
// zero-based daily cycle at offset 9 remains zero even on a completed day.
func buildCurrentPCRoomTimePointBody(s dnfonlineevent.PCRoomTimePointSnapshot) []byte {
	b := make([]byte, 17)
	binary.LittleEndian.PutUint32(b, s.Seconds)
	binary.LittleEndian.PutUint32(b[4:], s.Total)
	b[8] = s.DailyMask
	binary.LittleEndian.PutUint32(b[13:], s.PeriodMask)
	return b
}

func parseCurrentPCRoomTimePointRequest(body []byte) (daily byte, period int, err error) {
	period = -1
	if len(body) != 2 {
		return 0, -1, fmt.Errorf("pcroom request length=%d", len(body))
	}
	if body[0] == 0 && body[1] == 255 {
		return 0, -1, nil
	}
	if (body[0] == 1 || body[0] == 2 || body[0] == 4 || body[0] == 8) && body[1] == 255 {
		return body[0], -1, nil
	}
	if body[0] == 16 && body[1] < 4 {
		return 0, int(body[1]), nil
	}
	return 0, -1, fmt.Errorf("pcroom request action=%02x index=%02x", body[0], body[1])
}

func (s *Service) settleCurrentPCRoomTimePoint(ctx context.Context, accountID, characterID string, c *currentSpendTimeRuntimeCatalog, now time.Time, daily byte, period int) (dnfonlineevent.PCRoomTimePointSnapshot, error) {
	repos, ok := s.repositoryGroup()
	if !ok || c == nil || c.nativePCRoom == nil {
		return dnfonlineevent.PCRoomTimePointSnapshot{}, dnfonlineevent.ErrOwnerUnavailable
	}
	return dnfonlineevent.SettlePCRoomTimePoint(ctx, repos, c.nativePCRoom, dnfonlineevent.PCRoomTimePointCommand{
		AccountID: accountID, CharacterID: characterID, Now: now, ClockDefinition: c.definition, DailyClaim: daily, PeriodClaim: period,
		PeriodLoop: s.pcroomPeriodLoopEnabled(),
		Deliver: func(mailboxes dnfrepo.MailboxRepository, reward dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
			items, err := s.currentPVFItemCatalog()
			if err != nil {
				return "", err
			}
			definition, err := items.ResolveItem(uint32(reward.ItemID))
			if err != nil {
				return "", err
			}
			definition, err = currentPVFItemDefinitionForGrantAt(definition, now.UTC())
			if err != nil {
				return "", err
			}
			if definition.Kind != dungeonDropItemStackable || (!definition.ExpirationDate.IsZero() && !definition.ExpirationDate.After(now)) {
				return "", fmt.Errorf("pcroom reward %d expired or unsupported", reward.ItemID)
			}
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID, Title: "累计在线得好礼", Body: "累计在线活动奖励已发放，请查收附件。", Source: "pcroom_timepoint_228", CompletionKey: key, CreatedAt: now,
				Attachments: []dnfrepo.MailAttachment{{ItemID: reward.ItemID, Count: reward.Count, ExpireAt: definition.ExpirationDate}},
			})
		},
	})
}

func (s *Service) sendCurrentPCRoomTimePoint(session *gameSession, c *currentSpendTimeRuntimeCatalog, daily byte, period int) error {
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	state, err := s.settleCurrentPCRoomTimePoint(ctx, s.accountIDForSession(session), strconv.Itoa(int(session.selectedCharacterID)), c, s.gameplayNow(), daily, period)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentPCRoomTimePointStateID, buildCurrentPCRoomTimePointBody(state), 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-pcroom-timepoint-state", "event_id", currentPCRoomTimePointEventID, "char_id", session.selectedCharacterID, "seconds", state.Seconds, "total", state.Total, "daily_mask", state.DailyMask, "period_mask", state.PeriodMask, "mail_count", len(state.MailIDs))
	// The native buttons send 632, and the model refresh is notification 561.
	// No command-632 success-body handler was found in this EXE; notification
	// 632 belongs to an unrelated owner. Do not synthesize an ACK from that ID.
	if len(state.MailIDs) > 0 {
		return s.sendMailboxAlarmForSession(session, session.selectedCharacterID, "pcroom_timepoint_228")
	}
	return nil
}

func (s *Service) handleCurrentPCRoomTimePoint(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	daily, period, err := parseCurrentPCRoomTimePointRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-pcroom-timepoint-invalid-request", "error", err)
		s.logInfo("dnfbridge pcroom timepoint request rejected", "conn_id", session.connID, "body_len", len(body), "error", err)
		return nil
	}
	session.spendTime.mu.Lock()
	defer session.spendTime.mu.Unlock()
	c := session.spendTime.catalog
	if c == nil || c.nativePCRoom == nil || session.spendTime.characterID != session.selectedCharacterID || session.spendTime.characterGeneration != session.characterGeneration {
		// 活动时钟还没装载完（或角色已切换）。以前这里是静默返回，点击看起来就是
		// "毫无反应"，所以留一条 INFO 让现场能立刻区分"没路由"和"状态没就绪"。
		s.logInfo("dnfbridge pcroom timepoint request deferred",
			"conn_id", session.connID,
			"daily_mask", daily,
			"period", period,
			"reason", "session_online_clock_not_ready")
		return nil
	}
	s.logInfo("dnfbridge pcroom timepoint claim",
		"conn_id", session.connID,
		"daily_mask", daily,
		"period", period,
		"char_id", session.selectedCharacterID)
	settlement, err := s.settleCurrentSpendTimeClockLocked(session, s.gameplayNow())
	if err != nil {
		return err
	}
	if settlement.claimErr != nil {
		return settlement.claimErr
	}
	return s.sendCurrentPCRoomTimePoint(session, c, daily, period)
}
