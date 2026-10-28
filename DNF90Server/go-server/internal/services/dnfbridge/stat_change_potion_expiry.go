package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 城镇增益药剂（史诗Buff药剂一族）的**到期清除**定时器。
//
// 为什么必须有它：buff 注册（class0/op487）本身**不带时长** —— 三个状态字段参考实现
// 一律填 0，也就是说"这条 buff 什么时候结束"只有服务端知道。参考服务端的做法正是
// `EpicBuffPotionBuffNotifier.SendAddAsync` 之后紧跟 `ScheduleRemoveForCurrentEffect`
// （到期定时器 → CHARACTER_DEL_BUFF）。
//
// 我们原来只在"登录 / 重登 / 换频道 / 回城 / 重选"这些检查点上清残留图标，后果是：
// 人一直待在城里或副本里不换场景时，效果早就到点了、buff 栏图标却还挂着。
//
// 定时器是**会话级**的（药剂可以在城镇生效，不止副本），并在每次"使用 / 恢复补发"之后
// 重新武装：换频道与重登会新建会话 → 走恢复路径重新武装；同族换档（后来者覆盖窗口）
// 会武装到**新的**到期时间（上一发被 Cancel 掉，并用 generation 防止旧回调误触发）。
//
// 到点时**必须重新读一次角色**再决定：窗口可能已经被另一瓶续上了。只有确实过期才清图标
//（清图标 = CHARACTER_DEL_BUFF(这一族全部 id) + CHARACTER_BUFF_DUNGEON(空列表)）。
//
// 注意"效果时间结束"那行聊天文字不归服务端管：客户端本地登记的效果道具自带时长
//（首次使用按 PVF 的 [stat change duration]，重登由 op174 给出剩余秒），到期时由客户端
// 自己打 dstr 42036「[%s]的效果时间结束」。服务端这一发只负责把**图标**摘掉。

const currentStatChangePotionTimerSource = "stat-change-potion-expiry"

func nextCurrentStatChangePotionTimerGeneration(previous uint64) uint64 {
	next := previous + 1
	if next == 0 {
		next = 1
	}
	return next
}

// armCurrentStatChangePotionTimer 把"到期清图标"排进游戏时间队列。
//
// expireAt 来自角色记录里的窗口（statbuff.Effect.ExpireAt），不是猜的时长。
// 排不进去也不致命：检查点（登录/回城/重选）上的兜底清除仍然在。
func (s *Service) armCurrentStatChangePotionTimer(
	session *gameSession,
	characterID uint16,
	expireAt time.Time,
	reason string,
) {
	if s == nil || session == nil || characterID == 0 || expireAt.IsZero() {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		s.logGameEvent(session, "game-stat-change-potion-timer-unavailable",
			"char_id", characterID, "reason", reason)
		return
	}
	// 定时器回调必须能证明"还是同一个角色、同一个会话代"。这里取一次身份，
	// 之后由 postGameSessionCharacterEvent 在会话事件循环上复核。
	eventCharacterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, characterID)
	if err != nil {
		s.logGameEvent(session, "game-stat-change-potion-timer-skipped",
			"char_id", characterID, "reason", reason, "error", err)
		return
	}

	now := s.gameplayNow().UTC()
	delay := expireAt.UTC().Sub(now)
	if delay < 0 {
		delay = 0
	}

	session.statChangePotionTimerMu.Lock()
	if session.statChangePotionTimerActive && session.statChangePotionTimerName != "" {
		queue.Cancel(session.statChangePotionTimerName)
	}
	session.statChangePotionTimerGeneration = nextCurrentStatChangePotionTimerGeneration(
		session.statChangePotionTimerGeneration)
	generation := session.statChangePotionTimerGeneration
	timerName := fmt.Sprintf("dnf-stat-change-potion:%s:char:%d:gen:%d",
		session.connID, characterID, generation)
	session.statChangePotionTimerActive = true
	session.statChangePotionTimerName = timerName
	session.statChangePotionTimerCharacterID = characterID
	session.statChangePotionTimerDueAt = now.Add(delay)
	session.statChangePotionTimerMu.Unlock()

	s.logGameEvent(session, "game-stat-change-potion-timer-scheduled",
		"char_id", characterID,
		"reason", reason,
		"timer_generation", generation,
		"timer_name", timerName,
		"due_at_utc", expireAt.UTC().Format(time.RFC3339Nano),
		"delay_seconds", int64(delay/time.Second))

	if err := queue.ScheduleAfter(timerName, delay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			currentStatChangePotionTimerSource,
			eventCharacterID,
			characterGeneration,
			func() error {
				s.fireCurrentStatChangePotionTimer(session, generation, characterID)
				return nil
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", currentStatChangePotionTimerSource,
				"char_id", characterID,
				"timer_generation", generation,
				"error", postErr)
		}
	}); err != nil {
		s.cancelCurrentStatChangePotionTimer(session, "timer_schedule_failed")
		s.logGameEvent(session, "game-stat-change-potion-timer-schedule-failed",
			"char_id", characterID, "reason", reason, "error", err)
	}
}

// fireCurrentStatChangePotionTimer 在窗口到期的那一刻跑：确认真的过期了就摘图标。
//
// 三种情况：
//   - generation 已被更新的武装/取消作废 → 什么都不做（旧回调）；
//   - 窗口被另一瓶续上了 → 按新的到期时间重新武装，图标不动；
//   - 确实过期 → CHARACTER_DEL_BUFF + CHARACTER_BUFF_DUNGEON(空)，图标消失。
func (s *Service) fireCurrentStatChangePotionTimer(
	session *gameSession,
	generation uint64,
	characterID uint16,
) {
	if s == nil || session == nil || characterID == 0 {
		return
	}
	session.statChangePotionTimerMu.Lock()
	stale := !session.statChangePotionTimerActive ||
		session.statChangePotionTimerGeneration != generation ||
		session.statChangePotionTimerCharacterID != characterID
	if !stale {
		session.statChangePotionTimerActive = false
		session.statChangePotionTimerName = ""
		session.statChangePotionTimerDueAt = time.Time{}
	}
	session.statChangePotionTimerMu.Unlock()
	if stale {
		return
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		s.logGameEvent(session, "game-stat-change-potion-timer-abandoned",
			"char_id", characterID, "reason", "repositories_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.FormatUint(uint64(characterID), 10))
	if err != nil || !found {
		s.logGameEvent(session, "game-stat-change-potion-timer-abandoned",
			"char_id", characterID, "reason", "character_unavailable", "error", err)
		return
	}
	// 重新读角色，并且**逐族**判断：这一族现在有五个 family（掉落率 / 强化 / 增幅 /
	// 通关经验 / 客户端自施放），各族互不覆盖、各按自己的到期时间结束。
	//
	//	还有族没到期 → 只摘**已经过期那些**的图标（精确到 buff id），并按剩下最早的那个
	//	                到期时间重新武装；
	//	全都没有了   → 一次清干净（含兜底：客户端可能还残留着清过的旧图标）。
	now := s.gameplayNow().UTC()
	remaining := s.currentStatChangePotionActiveWindows(character, now)
	expiredBuffIDs := s.currentStatChangePotionExpiredBuffIDs(character, now)
	if len(remaining) > 0 {
		next := remaining[0].ExpireAt
		for _, window := range remaining {
			if window.ExpireAt.Before(next) {
				next = window.ExpireAt
			}
		}
		if len(expiredBuffIDs) > 0 {
			if err := s.sendCurrentCharacterRemoveBuffIDs(session, expiredBuffIDs, "expiry_timer_partial"); err != nil {
				s.logGameEvent(session, "game-stat-change-potion-expiry-clear-failed",
					"char_id", characterID, "error", err)
				return
			}
		}
		s.logGameEvent(session, "game-stat-change-potion-timer-rearmed",
			"char_id", characterID,
			"remaining_windows", len(remaining),
			"expired_buffs", len(expiredBuffIDs),
			"next_expire_at_utc", next.Format(time.RFC3339Nano),
			"reason", "another_family_still_active")
		s.armCurrentStatChangePotionTimer(session, characterID, next, "rearmed_after_partial_expiry")
		return
	}
	if err := s.clearCurrentStatChangePotionBuffs(session, "expiry_timer"); err != nil {
		s.logGameEvent(session, "game-stat-change-potion-expiry-clear-failed",
			"char_id", characterID, "error", err)
		return
	}
	s.logGameEvent(session, "game-stat-change-potion-timer-fired",
		"char_id", characterID,
		"reason", "window_expired")
}

// currentStatChangePotionExpiredBuffIDs 返回"窗口已经过期"的那些 buff id（精确摘图标用）。
//
// 为什么不能一律用全量清除：玩家身上可能同时挂着两族（例如掉落率 10 分钟 + 城镇移速
// 30 分钟），掉落率到点时若把全量 buff 都删掉，会把还没到期的移速图标一起误摘。
// 每条窗口在生效时都记了掷到的 buff id，所以这里能精确列出该摘的那几个。
func (s *Service) currentStatChangePotionExpiredBuffIDs(
	character dnfrepo.CharacterRecord,
	now time.Time,
) []int32 {
	if character.Stats == nil {
		return nil
	}
	// 先列出所有"到期时间已经过去"的族，再取它们各自记下的 buff id。
	expired := make([]int32, 0, 4)
	appendIfExpired := func(buffID int64, expireUnix int64) {
		if buffID <= 0 || buffID > math.MaxInt32 || expireUnix <= 0 {
			return
		}
		if time.Unix(expireUnix, 0).UTC().After(now) {
			return
		}
		expired = append(expired, int32(buffID))
	}
	appendIfExpired(character.Stats[statbuff.EpicDropRateBuffIDStatKey],
		character.Stats[statbuff.EpicDropRateExpireStatKey])
	for _, kind := range []statbuff.Kind{statbuff.KindUpgradeSuccess, statbuff.KindAmplifySuccess,
		statbuff.KindDungeonClearExp} {
		expireUnix, buffID, ok := statbuff.RawRateWindowKeys(character, kind)
		if !ok {
			continue
		}
		appendIfExpired(buffID, expireUnix)
	}
	for _, family := range s.currentClientBuffFamilies() {
		expireUnix, buffID, ok := statbuff.RawClientBuffKeys(character, family)
		if !ok {
			continue
		}
		appendIfExpired(buffID, expireUnix)
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i] < expired[j] })
	return expired
}

// sendCurrentCharacterRemoveBuffIDs 发一发 CHARACTER_DEL_BUFF（精确列表）。
func (s *Service) sendCurrentCharacterRemoveBuffIDs(
	session *gameSession,
	buffIDs []int32,
	source string,
) error {
	body := buildCurrentCharacterRemoveBuffBody(buffIDs)
	if len(body) == 0 {
		return nil
	}
	s.logPacketEvent("game-character-remove-buff-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"buff_count", int(body[0]),
		"msg_id", currentCharacterRemoveBuffMsgID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "partial_expiry_removes_only_the_windows_that_ended")
	return s.sendGameUpperRawClass(session, currentCharacterRemoveBuffMsgID, body, 0)
}

// cancelCurrentStatChangePotionTimer 作废当前定时器（换档、窗口已过期、排不进队列等）。
func (s *Service) cancelCurrentStatChangePotionTimer(session *gameSession, reason string) {
	if s == nil || session == nil {
		return
	}
	session.statChangePotionTimerMu.Lock()
	timerName := session.statChangePotionTimerName
	session.statChangePotionTimerActive = false
	session.statChangePotionTimerName = ""
	session.statChangePotionTimerDueAt = time.Time{}
	session.statChangePotionTimerGeneration = nextCurrentStatChangePotionTimerGeneration(
		session.statChangePotionTimerGeneration)
	session.statChangePotionTimerMu.Unlock()
	if timerName == "" {
		return
	}
	if queue := s.ensureGameplayTimeQueue(); queue != nil {
		queue.Cancel(timerName)
	}
	s.logGameEvent(session, "game-stat-change-potion-timer-cancelled",
		"timer_name", timerName, "reason", reason)
}
