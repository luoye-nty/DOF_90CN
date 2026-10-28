package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/premium"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

var (
	errCurrentDungeonSettlementOwnerUnavailable = dnfdungeon.ErrOwnerUnavailable
	errCurrentDungeonSettlementStateInvalid     = dnfdungeon.ErrSettlementStateInvalid
	errCurrentDungeonSettlementPVFInvalid       = errors.New("current dungeon settlement PVF reward source is invalid")
)

const (
	currentDungeonBlackDiamondBonusRate = 0.10
	// Current NoPack sub_1D4D380 stores the op35 quest rows in its task map.
	// The same client represents a completed active task with trigger/progress
	// zero, which is the value used by the settlement result UI.
	currentDungeonClearRewardQuestCompletedState uint32 = 0
)

type currentDungeonSettlementRewardResources struct {
	Progression       *progression.Tables
	ExperienceSources *progression.MonsterExperienceSources
	ClearRankCatalog  currentDungeonClearRankCatalog
}

type currentDungeonSettlementCommitResult struct {
	Character          dnfrepo.CharacterRecord
	Skill              dnfrepo.SkillRecord
	Inventory          dnfrepo.InventoryRecord
	ExperienceGain     uint32
	HonorExpertGain    uint32
	SPGain             int
	TPGain             int
	LuckyStarBefore    uint32
	LuckyStarApplied   uint32
	LuckyStarAfter     uint32
	LuckyStarLimit     uint32
	LuckyStarAttempted bool
	Replayed           bool
}

// currentDungeonClearExperiencePresentation 是"这一次通关的经验分类"的冻结视图：
// Base 四格 + Bonus 全表。它在第一次出包时算好并钉在 runtime 上，重试直接复用。
type currentDungeonClearExperiencePresentation struct {
	Base  [4]uint32
	Bonus [currentDungeonClearRewardBonusFieldCount]uint32
}

// produceCurrentDungeonSettlementPlanLocked is the missing owner between the
// accepted current-EXE op46 and the frozen op34 -> op37 -> op35 send plan.
// The caller holds session.dungeon.mu, so consumed-drop receipts cannot change
// while the database snapshot and packet plan are frozen.
func (s *Service) produceCurrentDungeonSettlementPlanLocked(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime ||
		!runtime.settlementPlayResultReceived {
		return errCurrentDungeonSettlementOwnerUnavailable
	}
	if runtime.clearMapCompletionKey == "" {
		if err := ensureCurrentDungeonCompletionReceiptKey(
			session,
			runtime,
			runtime.Session.Snapshot(),
			s.gameplayNow(),
			"settlement_plan_without_matching_clear_map_quest",
		); err != nil {
			return err
		}
	}
	if runtime.settlementResultPlan != nil {
		return nil
	}
	resources, err := s.currentDungeonSettlementRewardResources(ctx)
	if err != nil {
		return err
	}
	return s.produceCurrentDungeonSettlementPlanWithResourcesLocked(ctx, session, runtime, resources)
}

func (s *Service) produceCurrentDungeonSettlementPlanWithResourcesLocked(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	resources currentDungeonSettlementRewardResources,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime ||
		!runtime.settlementPlayResultReceived {
		return errCurrentDungeonSettlementOwnerUnavailable
	}
	if runtime.clearMapCompletionKey == "" {
		if err := ensureCurrentDungeonCompletionReceiptKey(
			session,
			runtime,
			runtime.Session.Snapshot(),
			s.gameplayNow(),
			"settlement_plan_without_matching_clear_map_quest",
		); err != nil {
			return err
		}
	}
	if runtime.settlementResultPlan != nil {
		return nil
	}
	gain, err := currentDungeonClearExperience(runtime, resources)
	if err != nil {
		return err
	}
	// A raid gate never pays clear experience. Real Anton runs settle each gate
	// through the raid module (86JP RaidHandler.HandleDungeonClearedAsync owns the
	// phase result and never enters the ordinary dungeon-clear settlement), so the
	// full clear-exp package this path builds does not belong to one: live
	// 2026-09-16 20:28Z granted 20,312,851 EXP plus a growth-contract bonus for
	// clearing gate 210. Every derived bonus is computed from this base, so zeroing
	// it here removes the whole package in one place.
	if s.currentDungeonRuntimeIsRaidGate(runtime) {
		gain = 0
	}
	// 成长契约 (premium type 84): +20% clear experience while active. The
	// total is committed and receipted, so an idempotent replay re-applies
	// exactly the same amount (86JP ComputeClearExp growth part).
	accountID := s.accountIDForSession(session)
	growthBonus := s.currentGrowthContractBonusExp(ctx, accountID, gain)
	// 评分奖励: base × ClearRankRates[bonusIndex] (86JP scoreBonusExp).
	clientRankPoint := runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset]
	settlementRankPoint, debugRankForced := s.localDebugSettlementRankPoint(resources.ClearRankCatalog, clientRankPoint)
	scoreBonus := currentDungeonScoreBonusExp(gain, resources, settlementRankPoint)
	// 黑钻加成：base × 10%（专用权益类型 1000）。
	blackDiamondBonus := s.currentBlackDiamondBonusExpForSession(session, gain)
	ancientElixirBonus, ancientElixirEffect, ancientElixirActive := s.currentDungeonExperienceConsumableBonusExp(
		ctx,
		accountID,
		strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		gain,
	)
	adventureBonus, adventureLevel, adventurePercent := s.currentDungeonAdventureGroupClearBonusExp(ctx, session, runtime, gain)
	attendanceMarbleBonus := currentAttendanceMarbleClearExperienceBonus(runtime.attendanceMarbleBuffs, gain)
	// 幸运角色（回归活动的角色表）的通关经验加成：单人只有 Clear，组队再叠加 Party。
	//
	// 参考实现把 Party 那一半另外拆进 Base[2] 做"组队通关经验"展示行；本树的
	// Base[4] 是另一套线上布局（[0]=经验总额、[1]=评分、[2]/[3]=详情分类），
	// 没有可用的组队槽，所以两部分合在一起进小计——与本文件里其它"没有已验证
	// 专属行"的加成（幸运单人、欢迎经验、热舞）同一口径，保证原生面板求和与
	// 持久化回执一致。
	// 幸运角色（回归活动的角色表）的通关经验加成：单人只有 Clear，组队再叠加 Party。
	// 两段合起来算一次（保持一次性取整）；本树 Base[4] 没有"组队通关经验"的专属行，
	// 所以整段并入小计。
	var luckyCharacterBonus uint32
	if lucky := s.currentLuckyCharacterEffect(session, runtime.Character); lucky != nil {
		percent := lucky.Clear
		if runtime.partyDungeonOwner != nil && len(runtime.partyDungeonOwner.roster()) > 1 {
			percent += lucky.Party
		}
		luckyCharacterBonus = currentPercentageBonusUint32(gain, percent)
	}
	// 城镇增益药剂：地下城通关经验 +N%（赛丽亚的幸运经验药剂 10008222 的三档
	// 50/100/200）。与上面几项同一口径：按 base 算加成再累加；窗口过期由
	// statbuff.ActiveRate 自己判掉（返回 0）。
	statChangePotionBonus := s.currentStatChangePotionClearExperienceBonus(runtime.Character, gain)
	totalGain := gain
	for _, bonus := range []uint32{growthBonus, scoreBonus, blackDiamondBonus, ancientElixirBonus, adventureBonus, attendanceMarbleBonus, luckyCharacterBonus, statChangePotionBonus} {
		totalGain = saturatingCurrentDungeonUint32Add(totalGain, bonus)
	}
	// 回归冒险家（onlineevent returnee，+20% 通关经验）。一旦算过就钉在 runtime 上：
	// 出包计划可能重试，重试期间账号的回归资格/离线时长可能已经变了，重新计算会让
	// 同一个通关拿到两份不同的经验，而底层已经按第一份落了账。
	var welcomeBonus uint32
	if runtime.settlementClearWelcomeBonus != nil {
		welcomeBonus = *runtime.settlementClearWelcomeBonus
	} else {
		welcomeBonus, err = s.currentReturneeWelcomeExperienceBonus(ctx, session, runtime.Character, gain)
		if err != nil {
			return fmt.Errorf("resolve settlement welcome experience: %w", err)
		}
		welcomeBonus = min(welcomeBonus, math.MaxUint32-totalGain)
	}
	totalGain = saturatingCurrentDungeonUint32Add(totalGain, welcomeBonus)
	storyQuestBase, storyQuestTotal, storyQuestID, err := s.currentDungeonSettlementStoryQuestExperience(
		ctx, runtime, resources.Progression, totalGain,
	)
	if err != nil {
		return fmt.Errorf("resolve settlement story quest experience: %w", err)
	}
	presentation, err := currentDungeonSettlementPresentationForRuntime(
		runtime,
		resources.ClearRankCatalog,
		clientRankPoint,
	)
	if err != nil {
		return fmt.Errorf("resolve settlement presentation: %w", err)
	}
	result, err := s.commitCurrentDungeonSettlement(
		ctx,
		session,
		runtime,
		resources.Progression,
		totalGain,
	)
	if err != nil {
		return fmt.Errorf("commit settlement progression: %w", err)
	}
	// 钉住本次通关的欢迎加成分类，供出包计划重试复用（与上面 totalGain 同一理由）。
	runtime.settlementClearWelcomeBonus = &welcomeBonus
	// A tower commits its floor advance and ten reward slots inside the same
	// settlement transaction; nothing extra is committed out of band here.
	towerSettlement := currentDungeonTowerRewardMsgID(runtime) != 0
	permissionBody, permissionState, permissionUpdated, permissionErr := s.commitCurrentDungeonPermission(
		ctx,
		session,
		runtime,
	)
	if permissionErr != nil {
		s.logGameEvent(session, "game-dungeon-permission-clear-state-update-failed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"difficulty", runtime.Request.Difficulty,
			"source", "settlement_plan_after_reward_commit",
			"error", permissionErr)
	}
	if !towerSettlement {
		if err := s.freezeCurrentDungeonCardRewardStateLocked(session, runtime); err != nil {
			s.logGameEvent(session, "game-dungeon-card-reward-plan-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"completion_key", runtime.clearMapCompletionKey,
				"source", "settlement_plan_pre_op35_freeze",
				"error", err)
		}
	}
	drops, err := currentDungeonCommittedDropReceipts(runtime, result.Inventory)
	if err != nil {
		// Pickup rows in op35 are presentation receipts only. The pickup owner
		// already committed the actual item and sent the authoritative op14 slot
		// update. A stale runtime receipt (for example, a later pickup reused the
		// same slot) must therefore be omitted instead of suppressing the entire
		// dungeon settlement after EXP/SP and card state have been committed.
		s.logGameEvent(session, "game-dungeon-settlement-drop-receipt-omitted",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"valid_receipt_count", len(drops),
			"source", "database_inventory_is_authoritative_for_op35_presentation",
			"error", err)
	}
	// 现场 2026-09-18（魔界地图）：本局捡过的材料（诞生之芽/时空石/金刚石/碎布片…）在
	// 通关结算瞬间翻倍显示（50 → 100），小退即恢复。排除法锁定来源：结算那一串只发了
	// op34/op37/op35/op1000/op985，没有任何物品行——客户端把 op35 的 Drops 每一行当成
	// "本次结算发放的数量"累加进背包。捡取本身在捡起那一刻就已经下发（op14 绝对行），
	// 这里再报一次等于二次发放。所以 Drops 段一律留空；收据只用于日志核对。
	s.logGameEvent(session, "game-dungeon-settlement-drop-receipts-suppressed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"completion_key", runtime.clearMapCompletionKey,
		"receipt_count", len(drops),
		"reason", "op35_drops_are_granted_again_by_the_client_pickups_already_sent_at_pickup_time")
	questRows, err := s.currentDungeonSettlementPendingQuestRows(ctx, session, runtime)
	if err != nil {
		return fmt.Errorf("resolve settlement quest rows: %w", err)
	}
	cardSlots := currentDungeonClearRewardCardSlotsFromRuntime(runtime)
	blackDiamondCardSlots := currentDungeonClearRewardBlackDiamondSlotsFromRuntime(runtime)
	// 先钉住本次通关的经验分类，再在其上叠加本局怪物侧的分项。顺序不能反：
	// bonusPresentation 就是冻结表的别名，提前展开会让"冻结"落在一份还没写完的副本上。
	runtime.settlementClearWelcomeBonus = &welcomeBonus
	if runtime.settlementClearExperiencePresentation == nil {
		exp := &currentDungeonClearExperiencePresentation{}
		// Base[4] 是"把这次通关经验拆开展示"的视图，沿用本树已验证的口径：
		// Base[0] = **通关基础经验**（= gain，不含任何加成），Base[1] = 评分奖励。
		// 活动带来的加成（幸运角色、回归欢迎、热舞）**不改写 Base[0]**：它们要么走
		// 各自的展示通道（欢迎经验走 op37 category_9 + Bonus[14]），要么并入已提交
		// 总额。参考实现的 take() 口径（从 committed 里按顺序扣）在本树会得到
		// 240 而它自己的断言要 200，所以不采用。
		exp.Base[currentDungeonClearRewardBaseClearExperienceIndex] = gain
		exp.Base[currentDungeonClearRewardBaseScoreBonusIndex] = scoreBonus
		// Bonus 表里属于"通关"的那几格一并冻结。
		exp.Bonus[currentDungeonClearRewardBonusClearBlackDiamondIndex] = blackDiamondBonus
		exp.Bonus[currentDungeonClearRewardBonusClearAncientElixirIndex] = ancientElixirBonus
		exp.Bonus[currentDungeonClearRewardBonusClearGrowthContractIndex] = growthBonus
		runtime.settlementClearExperiencePresentation = exp
	}
	// 怪物侧的分项是本局累计值，每次出包都按当前 runtime 重算；通关侧的分项
	// 取自上面那张冻结表。
	bonusPresentation := runtime.settlementClearExperiencePresentation.Bonus
	bonusPresentation[currentDungeonClearRewardBonusMonsterGrowthContractIndex] = runtime.settlementMonsterGrowthContractBonus
	bonusPresentation[currentDungeonClearRewardBonusMonsterFatigueBuffIndex] = runtime.settlementMonsterFatigueBatteryBonus
	bonusPresentation[currentDungeonClearRewardBonusMonsterEquipmentIndex] = runtime.settlementMonsterEquipmentBonus
	// 原生怪物详情 5（DSTR 49141）会把 Bonus[7] 与 Bonus[14] 相加，所以回归冒险家
	// 的欢迎经验只能落在其中一半，否则结果窗会把它显示成两倍。
	bonusPresentation[currentDungeonClearRewardBonusMonsterReturnedHeroPart2Index] = runtime.settlementMonsterWelcomeBonus
	monsterTotalExperience := currentDungeonSettlementMonsterTotalExperience(runtime)
	notice := currentDungeonPlayResultNotice{
		RankGrade:      presentation.RankGrade,
		ClearTimeMS:    presentation.ClearTimeMS,
		TimeBonusPoint: presentation.TimeBonusPoint,
		// Current NoPack requires this result-state byte to echo C2S op46. The
		// debug reward threshold remains server-side; replacing this byte makes
		// the client fall back to an F result even when the numeric display rises.
		RankPoint:       clientRankPoint,
		AllVisitedClear: presentation.AllVisitedClear,
		Participants: []currentDungeonPlayResultParticipant{{
			ObjectKey:   session.selectedCharacterID,
			ClearTimeMS: presentation.ClearTimeMS,
		}},
	}
	reward := currentDungeonClearRewardSnapshot{
		CharacterID:             session.selectedCharacterID,
		CompletionKey:           runtime.clearMapCompletionKey,
		Source:                  "runtime_pvf_dungeon_clear_and_committed_pickups",
		Committed:               true,
		AllowZeroReward:         currentDungeonSettlementAllowsZeroReward(runtime, result.LuckyStarAttempted),
		CommittedExperienceGain: result.ExperienceGain,
		CommittedSPGain:         result.SPGain,
		CommittedTPGain:         result.TPGain,
		// Base 直接取冻结表：重试之间资格变化（幸运/回归到期、队伍解散）不能改写
		// 已经提交的分类，否则面板数字与真实入账对不上。
		Base:  runtime.settlementClearExperiencePresentation.Base,
		Bonus: bonusPresentation,
		// The active client maps BonusGroupsB key=0 to an unrelated banquet label,
		// adventure-group experience. Keep the bonus in committed total EXP and
		// logs, but do not project an unverified category into the result panel.
		BonusGroupsB:                 nil,
		Score:                        currentDungeonSettlementScore(runtime),
		MonsterExperienceByObjectKey: currentDungeonSettlementMonsterExperienceRows(runtime),
		Quest:                        questRows,
		// Drops 段刻意留空：见上面 game-dungeon-settlement-drop-receipts-suppressed 的说明，
		// 客户端会把这一段的 StackCount 当成结算发放量再加一次（现场 2026-09-18 材料翻倍）。
		Drops:     nil,
		CardSlots: cardSlots,
		// op35's PAID-CARD COMMISSION field, the gold-card commission the
		// current EXE prints under 黄金卡牌 N. Presentation only: freezing it
		// here does not charge the flip, exactly like the 86JP reference, which
		// resolves the same number from [gold card cost table] and writes it
		// into DungeonNotificationBuilder's "PAID-CARD COMMISSION" slot after
		// the eight free-card seats.
		TotalReward: runtime.settlementPaidCardCost,
		AuxGroupsB:  blackDiamondCardSlots,
		Tail: currentDungeonClearRewardTail{
			ShowResult:      true,
			MonsterTotalExp: monsterTotalExperience,
			Value2:          storyQuestBase,
			Value3:          storyQuestTotal,
		},
	}
	plan, err := buildCurrentDungeonSettlementPacketPlanWithWelcome(
		clientRankPoint,
		notice,
		reward,
		result.Character,
		result.Skill.Points,
		// 本次通关已提交的欢迎经验分类（op37 的 category_9）。指针为 nil 时表示
		// 这笔通关没有欢迎加成，出包体里那一行写 0。
		runtime.settlementClearWelcomeBonus,
	)
	if err != nil {
		return err
	}
	if len(permissionBody) != 0 {
		plan.DungeonPermissionBody = append([]byte(nil), permissionBody...)
	}
	// 已婚角色(4)通关推荐副本后回婚姻成长胶囊进度。这里读的 result.Character 是
	// 本次结算**已提交**的记录，所以进度已经含上这一把的累加值——与上游同口径。
	if result.Character.Stats["wedding_state"] == 4 && currentDungeonIsRecommendedClear(runtime) {
		var w packetWriter
		w.writeUint32(uint32(result.Character.Stats["wedding_capsule_progress"]))
		plan.WeddingCapsuleBody = w.bytes()
	}
	if towerSettlement {
		// The tower window carries exactly ten reward slots resolved from the
		// PVF. Despair shows what this run actually picked up; grief shows the
		// frozen draw committed with the receipt.
		towerFloor, err := s.currentDungeonTowerFloor(runtime)
		if err != nil {
			return err
		}
		towerRewards, err := currentDungeonTowerDisplayRewards(runtime, result.Character, currentDungeonTowerPickupRewards(runtime, drops))
		if err != nil {
			return err
		}
		plan.TowerRewardMsgID = currentDungeonTowerRewardMsgID(runtime)
		plan.TowerRewardBody, err = buildCurrentDungeonTowerRewardBody(presentation.ClearTimeMS, towerFloor, towerRewards)
		if err != nil {
			return err
		}
	}
	runtime.Character = dnfrepo.CloneCharacter(result.Character)
	if result.LuckyStarAttempted {
		runtime.settlementLuckyStarResolved = true
		runtime.settlementLuckyStarRefreshPending = true
		runtime.settlementLuckyStarBefore = result.LuckyStarBefore
		runtime.settlementLuckyStarApplied = result.LuckyStarApplied
		runtime.settlementLuckyStarAfter = result.LuckyStarAfter
		runtime.settlementLuckyStarLimit = result.LuckyStarLimit
		s.logGameEvent(session, "game-dungeon-lucky-star-settlement-applied",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"points_before", result.LuckyStarBefore,
			"points_applied", result.LuckyStarApplied,
			"points_after", result.LuckyStarAfter,
			"replayed", result.Replayed)
	}
	runtime.settlementResultPlan = &plan
	s.logGameEvent(session, "game-dungeon-settlement-plan-frozen",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"completion_key", runtime.clearMapCompletionKey,
		"experience_gain", result.ExperienceGain,
		"growth_contract_bonus", growthBonus,
		"sp_gain", result.SPGain,
		"tp_gain", result.TPGain,
		"remaining_sp", result.Skill.Points.RemainingSP,
		"remaining_tp", result.Skill.Points.RemainingTP,
		"op34_rank_grade", presentation.RankGrade,
		"client_op46_rank_point", clientRankPoint,
		"settlement_reward_rank_point", settlementRankPoint,
		"op34_rank_point", clientRankPoint,
		"local_debug_rank_forced", debugRankForced,
		"op34_clear_time_ms", presentation.ClearTimeMS,
		"op34_all_visited_clear", presentation.AllVisitedClear,
		"op34_time_bonus_point", presentation.TimeBonusPoint,
		"picked_drop_receipts", len(drops),
		"replayed", result.Replayed,
		"op35_clear_base_exp", gain,
		"op35_clear_score_bonus_exp", scoreBonus,
		"op35_clear_growth_contract_bonus", growthBonus,
		"op35_clear_black_diamond_bonus", blackDiamondBonus,
		"op35_clear_ancient_elixir_bonus", ancientElixirBonus,
		"op35_clear_ancient_elixir_active", ancientElixirActive,
		"op35_clear_ancient_elixir_item_id", ancientElixirEffect.ItemID,
		"op35_clear_ancient_elixir_percent", ancientElixirEffect.BonusPercent,
		"op35_clear_adventure_group_level", adventureLevel,
		"op35_clear_adventure_group_percent", adventurePercent,
		"op35_clear_adventure_group_bonus", adventureBonus,
		"op35_clear_attendance_marble_bonus", attendanceMarbleBonus,
		"op35_monster_base_exp", runtime.settlementMonsterExperienceTotal,
		"op35_monster_growth_contract_bonus", runtime.settlementMonsterGrowthContractBonus,
		"op35_monster_equipment_bonus", runtime.settlementMonsterEquipmentBonus,
		"op35_monster_attendance_marble_bonus", runtime.settlementMonsterAttendanceMarbleBonus,
		"op35_monster_fatigue_battery_bonus", runtime.settlementMonsterFatigueBatteryBonus,
		"op35_monster_total_exp", monsterTotalExperience,
		"op35_story_quest_id", storyQuestID,
		"op35_story_quest_base_exp", storyQuestBase,
		"op35_story_quest_total_exp", storyQuestTotal,
		"op35_show_result", true,
		"op35_pending_quest_pairs", len(questRows),
		"op35_monster_detail_pairs", len(currentDungeonSettlementMonsterExperienceRows(runtime)),
		"op35_free_card_pairs", len(cardSlots[0]),
		"op35_black_diamond_card_seats", currentDungeonClearRewardNonEmptyGroupCount(blackDiamondCardSlots),
		"op35_champion_experience", runtime.settlementChampionExperience,
		"op35_super_champion_experience", runtime.settlementSuperChampionExperience,
		"op35_boss_experience", runtime.settlementBossExperience,
		"card_rewards", runtime.settlementCardRewardState != nil,
		"dungeon_permission_updated", permissionUpdated,
		"dungeon_permission_state", permissionState,
		"unknown_op35_business_fields", "zero_except_proved_result_gate_score_experience_and_card_reward_context")
	return nil
}

// currentDungeonSettlementStoryQuestExperience resolves the selected maze's
// active story connection and computes the split Base/Total values consumed by
// the right-side op35 result panel. This is presentation metadata only: the
// already committed dungeon gain is the sole database experience mutation.
func (s *Service) currentDungeonSettlementStoryQuestExperience(
	ctx context.Context,
	runtime *runtimeDungeonState,
	tables *progression.Tables,
	totalGain uint32,
) (base, total uint32, questID int64, err error) {
	if runtime == nil {
		return 0, 0, 0, errCurrentDungeonSettlementStateInvalid
	}
	if runtime.settlementStoryQuestBaseResolved {
		return runtime.settlementStoryQuestBase, runtime.settlementStoryQuestTotal,
			currentDungeonStoryQuestID(runtime), nil
	}
	questID = currentDungeonStoryQuestID(runtime)
	if questID == 0 {
		runtime.settlementStoryQuestBaseResolved = true
		return 0, 0, 0, nil
	}
	if s == nil || tables == nil || runtime.Character.Level <= 0 {
		return 0, 0, questID, errCurrentDungeonSettlementPVFInvalid
	}
	currentExperience, present := runtime.Character.Stats["exp"]
	if !present || currentExperience < 0 || uint64(currentExperience) > math.MaxUint32 {
		return 0, 0, questID, errCurrentDungeonSettlementStateInvalid
	}
	projectionMaxLevel := currentAdventureCharacterLevelCap
	if snapshot := tables.Snapshot(); snapshot.ExperienceThresholds > 0 && snapshot.ExperienceThresholds+1 < projectionMaxLevel {
		// Test and compatibility PVFs may intentionally expose a shorter
		// progression table; do not ask the pure table rule to index beyond it.
		projectionMaxLevel = snapshot.ExperienceThresholds + 1
	}
	projected, err := tables.ApplyExperience(
		runtime.Character.Level,
		uint32(currentExperience),
		totalGain,
		projectionMaxLevel,
	)
	if err != nil {
		return 0, 0, questID, fmt.Errorf("project story quest settlement level: %w", err)
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return 0, 0, questID, fmt.Errorf("load story quest catalog: %w", err)
	}
	definition, found := catalog.Find(questID)
	if !found {
		return 0, 0, questID, fmt.Errorf("story quest %d is absent from PVF catalog", questID)
	}
	if definition.NoExperience {
		runtime.settlementStoryQuestBaseResolved = true
		return 0, 0, questID, nil
	}
	questDifficulty := 'G'
	if values := []rune(strings.TrimSpace(definition.Difficulty)); len(values) == 1 {
		questDifficulty = values[0]
	}
	base, total, err = tables.QuestExperienceAward(
		projected.NewLevel,
		definition.LevelMin,
		questDifficulty,
		definition.IgnoreQuestLevel4Exp,
	)
	if err != nil {
		return 0, 0, questID, fmt.Errorf("calculate story quest %d settlement experience: %w", questID, err)
	}
	runtime.settlementStoryQuestBase = base
	runtime.settlementStoryQuestTotal = total
	runtime.settlementStoryQuestBaseResolved = true
	return base, total, questID, nil
}

func currentDungeonStoryQuestID(runtime *runtimeDungeonState) int64 {
	if runtime == nil || runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		return 0
	}
	connection := runtime.Dungeon.Mazes[runtime.MazeIndex].QuestConnection
	if len(connection) < 2 || connection[0] != 0 || connection[1] <= 0 {
		return 0
	}
	return connection[1]
}

func currentDungeonSettlementMonsterExperienceRows(runtime *runtimeDungeonState) []currentDungeonClearRewardPair {
	if runtime == nil || len(runtime.settlementMonsterExperienceByObjectKey) == 0 {
		return nil
	}
	keys := make([]uint32, 0, len(runtime.settlementMonsterExperienceByObjectKey))
	for key, value := range runtime.settlementMonsterExperienceByObjectKey {
		if key != 0 && value != 0 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
	rows := make([]currentDungeonClearRewardPair, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, currentDungeonClearRewardPair{
			Key:   key,
			Value: runtime.settlementMonsterExperienceByObjectKey[key],
		})
	}
	return rows
}

// currentDungeonSettlementPendingQuestRows returns only the task completions
// belonging to this settlement's durable clear-map receipt. The client uses
// these op35 rows to construct the left-side quest result panel from its PVF;
// they are presentation metadata and do not grant any additional reward.
func (s *Service) currentDungeonSettlementPendingQuestRows(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) ([]currentDungeonClearRewardPair, error) {
	if session == nil || runtime == nil || runtime.clearMapCompletionKey == "" ||
		len(runtime.clearMapCompletionPendingQuestIDs) == 0 {
		return nil, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return nil, errCurrentDungeonSettlementOwnerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, found, err := repositories.Quest.Load(ctx, strconv.FormatUint(uint64(session.selectedCharacterID), 10))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	rows := make([]currentDungeonClearRewardPair, 0, len(runtime.clearMapCompletionPendingQuestIDs))
	seen := make(map[int64]struct{}, len(runtime.clearMapCompletionPendingQuestIDs))
	for _, questID := range runtime.clearMapCompletionPendingQuestIDs {
		if questID <= 0 || questID > math.MaxUint32 {
			continue
		}
		if _, duplicate := seen[questID]; duplicate {
			continue
		}
		seen[questID] = struct{}{}
		state, present := currentDungeonClearMapQuestState(record, questID)
		if !present || state.ProgressValue != 0 || !stringsEqualFoldTrim(state.Status, "active") ||
			state.Extra == nil || state.Extra["completion_key"] != runtime.clearMapCompletionKey ||
			state.Extra["reward_state"] != "pending" {
			continue
		}
		rows = append(rows, currentDungeonClearRewardPair{
			Key:   uint32(questID),
			Value: currentDungeonClearRewardQuestCompletedState,
		})
	}
	sort.Slice(rows, func(left, right int) bool {
		return rows[left].Key < rows[right].Key
	})
	return rows, nil
}

func currentDungeonAdventureGroupBonusGroup(bonus uint32) []currentDungeonClearRewardPair {
	// No current-EXE slot for this category has been proven. Returning nil is
	// preferable to displaying the bonus under that unrelated label.
	return nil
}

func currentDungeonSettlementMonsterTotalExperience(runtime *runtimeDungeonState) uint32 {
	if runtime == nil {
		return 0
	}
	total := runtime.settlementMonsterExperienceTotal
	total = saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterGrowthContractBonus)
	total = saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterEquipmentBonus)
	total = saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterAttendanceMarbleBonus)
	total = saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterWelcomeBonus)
	total = saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterLuckyCharacterBonus)
	return saturatingCurrentDungeonUint32Add(total, runtime.settlementMonsterFatigueBatteryBonus)
}

func currentDungeonSettlementScore(runtime *runtimeDungeonState) [currentDungeonClearRewardScoreFieldCount]uint32 {
	if runtime == nil {
		return [currentDungeonClearRewardScoreFieldCount]uint32{}
	}
	// Current NoPack consumes Score[1..3] as the champion,
	// super-champion, and boss result breakdown. Score[0] remains neutral;
	// aggregate monster EXP has its dedicated tail field. Clamp every
	// classified subtotal to that aggregate so damaged runtime state cannot
	// overstate it.
	return [currentDungeonClearRewardScoreFieldCount]uint32{
		0,
		min(runtime.settlementChampionExperience, runtime.settlementMonsterExperienceTotal),
		min(runtime.settlementSuperChampionExperience, runtime.settlementMonsterExperienceTotal),
		min(runtime.settlementBossExperience, runtime.settlementMonsterExperienceTotal),
	}
}

func currentDungeonClearRewardCardSlotsFromRuntime(runtime *runtimeDungeonState) [currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair {
	var slots [currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair
	if runtime == nil || runtime.settlementCardRewardState == nil {
		return slots
	}
	runtime.settlementCardRewardState.mu.Lock()
	plan := cloneDungeonCardRewardPlan(runtime.settlementCardRewardState.plan)
	runtime.settlementCardRewardState.mu.Unlock()
	currentDungeonProjectCardBundleToClearRewardSlots(&slots,
		int(runtimePartyDungeonCardMemberIndex(runtime)), plan.Sides[dungeonCardSideFree])
	return slots
}

func currentDungeonClearRewardBlackDiamondSlotsFromRuntime(runtime *runtimeDungeonState) [currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair {
	var slots [currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair
	if runtime == nil || runtime.settlementCardRewardState == nil {
		return slots
	}
	runtime.settlementCardRewardState.mu.Lock()
	plan := cloneDungeonCardRewardPlan(runtime.settlementCardRewardState.plan)
	runtime.settlementCardRewardState.mu.Unlock()
	for member := range plan.BlackDiamond {
		currentDungeonProjectCardBundleToClearRewardSlots(&slots, member, plan.BlackDiamond[member])
	}
	return slots
}

func currentDungeonClearRewardNonEmptyGroupCount(groups [currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair) int {
	count := 0
	for _, group := range groups {
		if len(group) > 0 {
			count++
		}
	}
	return count
}

func currentDungeonProjectCardBundleToClearRewardSlots(
	slots *[currentDungeonClearRewardGroupCount][]currentDungeonClearRewardPair,
	base int,
	bundle dungeonCardRewardBundle,
) {
	if slots == nil || base < 0 || base >= len(slots) {
		return
	}
	displayItems := currentDungeonCardDisplayItems(bundle)
	pairs := make([]currentDungeonClearRewardPair, 0, 1+len(displayItems))
	if bundle.Gold > 0 && bundle.Gold <= math.MaxUint32 {
		pairs = append(pairs, currentDungeonClearRewardPair{Key: 0, Value: uint32(bundle.Gold)})
	}
	for _, item := range displayItems {
		if item.ItemID <= 0 || item.ItemID > math.MaxUint32 || item.Count <= 0 {
			continue
		}
		count := item.Count
		if count > math.MaxUint32 {
			count = math.MaxUint32
		}
		pairs = append(pairs, currentDungeonClearRewardPair{
			Key:   uint32(item.ItemID),
			Value: uint32(count),
		})
	}
	// The eight op35 groups are party-member seats (free 0..3, paid 4..7),
	// not separate gold/item card faces. Keep every reward tuple for one side
	// in that member's single group so the free-row reveal can render the item
	// while retaining the accompanying gold amount.
	slots[base] = pairs
}

func (s *Service) currentDungeonSettlementRewardResources(ctx context.Context) (currentDungeonSettlementRewardResources, error) {
	if s == nil {
		return currentDungeonSettlementRewardResources{}, errCurrentDungeonSettlementOwnerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return currentDungeonSettlementRewardResources{}, err
	}
	tables, err := progression.Load(ctx, archive)
	if err != nil {
		return currentDungeonSettlementRewardResources{}, err
	}
	sources, err := progression.LoadMonsterExperienceSources(archive)
	if err != nil {
		return currentDungeonSettlementRewardResources{}, err
	}
	clearRankCatalog, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		return currentDungeonSettlementRewardResources{}, err
	}
	return currentDungeonSettlementRewardResources{
		Progression:       tables,
		ExperienceSources: sources,
		ClearRankCatalog:  clearRankCatalog,
	}, nil
}

func currentDungeonClearExperience(
	runtime *runtimeDungeonState,
	resources currentDungeonSettlementRewardResources,
) (uint32, error) {
	if runtime == nil || resources.Progression == nil || resources.ExperienceSources == nil ||
		!runtime.Dungeon.Metadata.BasisLevel.Set || runtime.Dungeon.Metadata.BasisLevel.Value <= 0 {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	// The dungeon field is optional in the runtime PVF: ordinary dungeon 3
	// (幽暗密林) omits it. The established domain rule is neutral weight 1.0
	// when absent or negative; an explicit non-negative value remains exact.
	weight := 1.0
	if runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Set &&
		runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value >= 0 {
		weight = runtime.Dungeon.Metadata.ExperienceIncreasingPoint.Value
	}
	if math.IsNaN(weight) || math.IsInf(weight, 0) {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	// A PVF tutorial may declare an explicit zero weight, which means "this
	// clear pays no clear experience". Live 2026-09-23 (character 123456, male
	// Priest): 7113 (dungeon/Cataclysm/NewTutorial/7113_Priest_M_Tutorial.dgn)
	// declares `[experience increasing point] 0`, and rejecting it blocked the
	// whole settlement plan -- op34/op37/op35 were never sent, so the client
	// never showed the result, never auto-returned, and every town-return op42
	// was answered with completed_run_missing_play_result_or_statistic_barrier.
	// A tutorial is a mandatory one-time handshake whose settlement panel is the
	// client's own exit condition, so a legitimately zero reward must still
	// settle. Every other dungeon keeps the strict rule, because the same value
	// also scales monster experience and the local drop rate.
	if weight == 0 && isPVFTutorialDungeon(runtime) {
		return 0, nil
	}
	if weight <= 0 {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	base, err := resources.Progression.DungeonClearBaseExperience(int(runtime.Dungeon.Metadata.BasisLevel.Value))
	if err != nil || base == 0 {
		return 0, fmt.Errorf("%w: base=%d: %v", errCurrentDungeonSettlementPVFInvalid, base, err)
	}
	modifiers := resources.ExperienceSources.RawModifiers()
	difficulty := int(runtime.Request.Difficulty)
	if difficulty < 0 || difficulty >= len(modifiers.DifficultyRates) {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	difficultyRate := modifiers.DifficultyRates[difficulty]
	if math.IsNaN(difficultyRate) || math.IsInf(difficultyRate, 0) || difficultyRate <= 0 {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	value := math.Floor(float64(base) * weight * difficultyRate)
	if value <= 0 || value > math.MaxUint32 {
		return 0, fmt.Errorf("%w: computed=%v", errCurrentDungeonSettlementPVFInvalid, value)
	}
	return uint32(value), nil
}

// currentDungeonScoreBonusExp calculates the score bonus experience from the
// PVF [clear rank exp bonusrate] table. The bonus index is the number of rank
// grade thresholds the client rank point meets or exceeds (86JP
// MonsterRewardTable.GetClearRankBonusIndex).
// currentDungeonClearRankBonusRate resolves the PVF [clear rank exp bonusrate]
// term for a client rank point. The clear-card gold formula applies the same
// rate, so both rewards move together with the run's grade.
//
// RankSystemInfo contains more display grades than the five clear-rank rates.
// The client/server rule is therefore not "count every matched grade": only
// the first five thresholds participate, with the highest threshold using the
// last rate and the fifth threshold using the first rate. Counting all eight
// thresholds made a 99-point clear produce an out-of-range index and silently
// dropped the score reward.
func currentDungeonClearRankBonusRate(resources currentDungeonSettlementRewardResources, clientRankPoint byte) (float64, bool) {
	if resources.ExperienceSources == nil {
		return 0, false
	}
	modifiers := resources.ExperienceSources.RawModifiers()
	bonusIndex := -1
	limit := len(resources.ClearRankCatalog.gradeThresholds)
	if limit > len(modifiers.ClearRankRates) {
		limit = len(modifiers.ClearRankRates)
	}
	for thresholdIndex := 0; thresholdIndex < limit; thresholdIndex++ {
		if clientRankPoint >= resources.ClearRankCatalog.gradeThresholds[thresholdIndex] {
			bonusIndex = len(modifiers.ClearRankRates) - 1 - thresholdIndex
			break
		}
	}
	if bonusIndex < 0 || bonusIndex >= len(modifiers.ClearRankRates) {
		return 0, false
	}
	rate := modifiers.ClearRankRates[bonusIndex]
	if rate <= 0 {
		return 0, false
	}
	return rate, true
}

func currentDungeonScoreBonusExp(base uint32, resources currentDungeonSettlementRewardResources, clientRankPoint byte) uint32 {
	if base == 0 || resources.ExperienceSources == nil {
		return 0
	}
	rate, ok := currentDungeonClearRankBonusRate(resources, clientRankPoint)
	if !ok {
		return 0
	}
	bonus := math.Floor(float64(base) * rate)
	if bonus <= 0 || bonus > math.MaxUint32 {
		return 0
	}
	return uint32(bonus)
}

// currentBlackDiamondBonusExp returns +10% clear experience when the account
// has an active dedicated Black Diamond entitlement (storage type 1000). The
// current client sees that entitlement only as wire type 56.
func (s *Service) currentBlackDiamondBonusExp(ctx context.Context, accountID string, base uint32) uint32 {
	if s == nil || base == 0 || strings.TrimSpace(accountID) == "" {
		return 0
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return 0
	}
	account, found, err := repositories.Account.Load(ctx, strings.TrimSpace(accountID))
	if err != nil || !found {
		return 0
	}
	now := time.Now().UTC()
	if !premium.BlackDiamondActive(account, now) {
		return 0
	}
	bonus := math.Floor(float64(base) * currentDungeonBlackDiamondBonusRate)
	if bonus <= 0 || bonus > math.MaxUint32-float64(base) {
		return 0
	}
	return uint32(bonus)
}

func (s *Service) currentBlackDiamondBonusExpForSession(session *gameSession, base uint32) uint32 {
	if base == 0 || !s.currentBlackDiamondActive(session) {
		return 0
	}
	return uint32(uint64(base) * 10 / 100)
}

// currentDungeonSettlementAllowsZeroExperience reports a completion that
// legitimately pays no clear experience. The dungeon module's settlement owner
// rejects Experience == 0 unless the caller proves the zero is intended:
//   - a raid gate, whose reward is the raid phase settlement, not the map clear;
//   - a PVF tutorial that declares an explicit zero experience weight (male
//     Priest tutorial 7113), whose clear reward is the tutorial handshake the
//     op143 checkpoint already paid.
//
// Everything else keeps the strict guard, so an accidental zero can never enter
// the settlement ledger.
func (s *Service) currentDungeonSettlementAllowsZeroExperience(runtime *runtimeDungeonState) bool {
	return s.currentDungeonRuntimeIsRaidGate(runtime) || isPVFTutorialDungeon(runtime)
}

// currentDungeonSettlementAllowsZeroReward mirrors the
// currentDungeonClearRewardSnapshot.AllowZeroReward ownership proof. It only
// skips the "committed snapshot contains no reward value" shape rejection, so it
// cannot change one byte of a non-zero reward. Recommended-dungeon Lucky Stars
// already use it; a zero-weight PVF tutorial is the second owner, because its
// clear experience, monster experience, drops and cards are all legitimately
// absent while the result panel is still the client's exit condition.
func currentDungeonSettlementAllowsZeroReward(runtime *runtimeDungeonState, luckyStarAttempted bool) bool {
	return luckyStarAttempted || isPVFTutorialDungeon(runtime)
}

func (s *Service) commitCurrentDungeonSettlement(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	tables *progression.Tables,
	gain uint32,
) (currentDungeonSettlementCommitResult, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil || tables == nil {
		return currentDungeonSettlementCommitResult{}, errCurrentDungeonSettlementOwnerUnavailable
	}
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return currentDungeonSettlementCommitResult{}, errCurrentDungeonSettlementOwnerUnavailable
	}
	if runtime == nil {
		return currentDungeonSettlementCommitResult{}, errCurrentDungeonSettlementStateInvalid
	}
	runtimeExperience, present := runtime.Character.Stats["exp"]
	if !present || runtimeExperience < 0 || uint64(runtimeExperience) > math.MaxUint32 {
		return currentDungeonSettlementCommitResult{}, errCurrentDungeonSettlementStateInvalid
	}
	honorExpertGain, err := currentHonorExpertExperienceGain(
		tables,
		runtime.Character.Level,
		uint32(runtimeExperience),
		gain,
	)
	if err != nil {
		return currentDungeonSettlementCommitResult{}, err
	}
	// Only load the independent PVF honor table for an award that actually
	// reaches the character-level-cap split.
	var honorTables *dnfhonor.Tables
	if honorExpertGain > 0 {
		loaded, loadErr := s.loadHonorTable(ctx)
		if loadErr != nil {
			return currentDungeonSettlementCommitResult{}, loadErr
		}
		honorTables = loaded
	}
	accountID := s.accountIDForSession(session)
	adventureRuntime := s.adventureGroupTable.Runtime()
	rentalPointAward, rentalPointLimit := uint32(0), uint32(0)
	if currentDungeonIsRecommendedClear(runtime) {
		if rentalCatalog, rentalErr := s.currentRentalCatalogOnly(ctx); rentalErr != nil {
			s.logGameEvent(session, "game-dungeon-lucky-star-pvf-blocked",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"reason", rentalErr)
			return currentDungeonSettlementCommitResult{}, fmt.Errorf("load recommended-dungeon rental catalog: %w", rentalErr)
		} else {
			rentalPointAward = rentalCatalog.DungeonClearPoints
			rentalPointLimit = rentalCatalog.Limit
		}
	}
	// A tower clear carries its ten frozen reward slots and the entry pool it
	// consumes, all committed with this receipt.
	towerClear, err := s.currentDungeonTowerClear(runtime)
	if err != nil {
		return currentDungeonSettlementCommitResult{}, err
	}
	// 婚姻成长胶囊只在"已婚角色 + 推荐副本通关"时累加，所以先按同一判据决定是否
	// 加载 PVF 规则；条件不满足时保持 nil，结算 owner 会整段跳过（见
	// wedding.AccumulateCapsuleClear 的调用点）。
	recommendedClear := currentDungeonIsRecommendedClear(runtime)
	var weddingCapsuleRules *wedding.CapsuleCatalog
	if recommendedClear && runtime.Character.Stats["wedding_state"] == 4 {
		weddingCapsuleRules, err = s.currentWeddingCapsuleCatalog()
		if err != nil {
			return currentDungeonSettlementCommitResult{}, fmt.Errorf("load wedding growth capsule catalog: %w", err)
		}
	}
	// 签到类活动的进度**只在副本结算里累加**，没有第二个写入方，所以这里必须把
	// 目录备齐并传下去：
	//   - 月度签到（活动 2370）：只要推荐副本通关就记一次日计数
	//   - 周周签到（活动 2208）：需要目录判定当周与补签
	//   - 签到弹珠：通关清次数决定额外骰子
	// 目录拿不到（PVF 缺失/未加载）时保持零值，结算 owner 会整段跳过而不报错——
	// 不能让一个没开的活动的目录问题把普通通关卡死。
	var (
		monthlyCatalog *dnfonlineevent.MonthlyCatalog
		weeklyCatalog  *dnfonlineevent.WeeklyCatalog
		marbleCatalog  *dnfonlineevent.MarbleCatalog
	)
	if recommendedClear {
		if catalog, catalogErr := s.currentMonthlyCatalog(ctx); catalogErr != nil {
			s.logGameEvent(session, "game-dungeon-monthly-attendance-pvf-blocked",
				"char_id", session.selectedCharacterID, "dungeon_id", runtime.Dungeon.ID, "reason", catalogErr)
		} else {
			monthlyCatalog = catalog
		}
		if catalog, catalogErr := s.currentWeeklyCatalog(ctx); catalogErr != nil {
			s.logGameEvent(session, "game-dungeon-weekly-attendance-pvf-blocked",
				"char_id", session.selectedCharacterID, "dungeon_id", runtime.Dungeon.ID, "reason", catalogErr)
		} else {
			weeklyCatalog = catalog
		}
		if catalog, catalogErr := s.currentAttendanceMarbleCatalog(); catalogErr != nil {
			s.logGameEvent(session, "game-dungeon-attendance-marble-pvf-blocked",
				"char_id", session.selectedCharacterID, "dungeon_id", runtime.Dungeon.ID, "reason", catalogErr)
		} else {
			marbleCatalog = catalog
		}
	}
	// 公会贡献度: assemble this clear's contribution side. Nil for non-members, for
	// practice runs, and for clears that never took an entry snapshot.
	contributionClear, err := s.currentGuildContributionClear(ctx, runtime)
	if err != nil {
		return currentDungeonSettlementCommitResult{}, err
	}
	// Keep the guild registry read-modify-write of the contribution path on the
	// same lock as every other guild mutation, or a concurrent guild change on
	// another session can interleave and lose one side's update. Upstream's third
	// conjunct here was runtime.guildDungeonID != 0; this tree identifies a 公会地下城
	// by reverse lookup instead (decision D5).
	//
	// The lock is taken and released around the commit alone rather than defer'd:
	// applySettlementContribution goes through the transaction's own settings
	// repository (module-level guild.Load/Save), never through Service.guildMu, so
	// it cannot re-enter this lock -- but holding it through the unrelated
	// post-settlement projection below would still be gratuitous.
	guildLocked := contributionClear != nil || runtime.guildWarID != 0 || s.currentGuildDungeonRun(runtime)
	if guildLocked {
		s.guildMu.Lock()
	}
	result, err := owner.CommitSettlement(ctx, dnfdungeon.SettlementCommand{
		AccountID:               accountID,
		CharacterID:             fmt.Sprintf("%d", session.selectedCharacterID),
		CompletionKey:           runtime.clearMapCompletionKey,
		Tables:                  tables,
		Experience:              gain,
		RecommendedDungeonClear: recommendedClear,
		WeddingCapsule:          weddingCapsuleRules,
		MonthlyAttendance:       monthlyCatalog != nil,
		WeeklyAttendance:        weeklyCatalog,
		AttendanceMarble:        marbleCatalog,
		AdventureRuntime:        &adventureRuntime,
		HonorExpertTables:       honorTables,
		MaximumCharacterLevel:   currentAdventureCharacterLevelCap,
		RentalPointAward:        rentalPointAward,
		RentalPointLimit:        rentalPointLimit,
		Tower:                   towerClear,
		// A raid gate settles with zero clear experience on purpose; without this
		// the zero-experience guard rejected the whole settlement and every op72
		// afterwards answered settlement_result_not_ready. A PVF tutorial whose
		// own weight is zero is the second legitimate zero-experience completion
		// (male Priest tutorial 7113), and it must settle for the same reason:
		// the client's exit handshake is the settlement panel itself.
		AllowZeroExperience: s.currentDungeonSettlementAllowsZeroExperience(runtime),
		GuildContribution:   contributionClear,
		UpdatedAt:           time.Now().UTC(),
	})
	if guildLocked {
		s.guildMu.Unlock()
	}
	if err != nil {
		return currentDungeonSettlementCommitResult{}, err
	}
	if towerClear != nil {
		// Publish only the rows this clear actually granted, so the client's
		// list-0 update matches the committed inventory.
		runtime.towerRewardUpdates = nil
		rewards, err := dnfdungeon.TowerLastRewards(result.Character, towerClear.Kind)
		if err != nil {
			return currentDungeonSettlementCommitResult{}, err
		}
		for key, stack := range result.Inventory.Slots {
			list, slot, valid := parseSceneInventorySlotKey(key)
			if !valid || list != dnfrepo.MainInventoryListType {
				continue
			}
			for _, reward := range rewards {
				if reward.ItemID != 0 && stack.ItemID == int64(reward.ItemID) {
					runtime.towerRewardUpdates = append(runtime.towerRewardUpdates, currentItemListEntryFromStack(list, slot, stack))
					break
				}
			}
		}
	}
	if contributionClear != nil {
		// The delivered 贡献币 rows, captured here so the result send can replay them
		// as one op14 batch instead of re-reading the inventory. Read the committed
		// inventory, not the pre-settlement copy.
		runtime.guildContributionUpdates = currentGuildContributionInventoryEntries(result.Inventory)
	}
	return currentDungeonSettlementCommitResult{
		Character:          result.Character,
		Skill:              result.Skill,
		Inventory:          result.Inventory,
		ExperienceGain:     result.ExperienceGain,
		HonorExpertGain:    result.HonorExpertGain,
		SPGain:             result.SPGain,
		TPGain:             result.TPGain,
		LuckyStarBefore:    result.LuckyStarBefore,
		LuckyStarApplied:   result.LuckyStarApplied,
		LuckyStarAfter:     result.LuckyStarAfter,
		LuckyStarLimit:     rentalPointLimit,
		LuckyStarAttempted: result.LuckyStarAttempted,
		Replayed:           result.Replayed,
	}, nil
}

func planCurrentDungeonSettlementProgression(
	tables *progression.Tables,
	level int,
	total uint32,
	gain uint32,
	points dnfrepo.SkillPointState,
) (progression.ExperienceSkillPointPlan, error) {
	return dnfdungeon.PlanSettlementProgressionAtCap(
		tables,
		level,
		total,
		gain,
		points,
		currentAdventureCharacterLevelCap,
	)
}

func currentDungeonCommittedDropReceipts(
	runtime *runtimeDungeonState,
	inventory dnfrepo.InventoryRecord,
) ([]currentDungeonClearRewardDrop, error) {
	if runtime == nil || runtime.DropOwner == nil {
		return nil, nil
	}
	bySlot := make(map[uint16]currentDungeonClearRewardDrop)
	activeDiscardAmounts := currentDungeonActiveDiscardAmounts(runtime)
	objectKeys := make([]uint32, 0, len(runtime.DropOwner.byObjectKey))
	for objectKey := range runtime.DropOwner.byObjectKey {
		objectKeys = append(objectKeys, objectKey)
	}
	sort.Slice(objectKeys, func(left, right int) bool { return objectKeys[left] < objectKeys[right] })
	invalid := make([]string, 0)
	for _, objectKey := range objectKeys {
		drop := runtime.DropOwner.byObjectKey[objectKey]
		if drop == nil || drop.Status != runtimeDungeonDropConsumed {
			continue
		}
		if drop.Item.ItemID == 0 {
			continue
		}
		// Epic fragments are committed to the account-owned Epic Book. Their
		// destination slot is only a transient client pickup slot which is
		// cleared immediately, so it must not be validated or projected as a
		// character-inventory receipt in op35.
		if len(drop.PickupEpicBookBody) > 0 {
			continue
		}
		if drop.DestinationSlot == 0 {
			invalid = append(invalid, fmt.Sprintf("drop=%d item=%d destination=0", drop.ObjectKey, drop.Item.ItemID))
			continue
		}
		stack, found := inventory.Slots[currentDungeonPickupMainSlotKey(int16(drop.DestinationSlot))]
		if !found || stack.ItemID != int64(drop.Item.ItemID) || stack.Count <= 0 || stack.Count > math.MaxUint32 {
			// Pickup receipts live for the complete run, but the player may
			// legally discard that pickup back onto the current dungeon floor.
			// In that case the original destination slot is intentionally gone;
			// do not reject settlement or invent a reward row for the absent item.
			key := currentDungeonDiscardReceiptKey{
				SourceSlot: int16(drop.DestinationSlot),
				ItemID:     drop.Item.ItemID,
			}
			amount := uint64(drop.Amount)
			if amount > 0 && activeDiscardAmounts[key] >= amount {
				activeDiscardAmounts[key] -= amount
				continue
			}
			invalid = append(invalid, fmt.Sprintf(
				"drop=%d destination=%d runtime_item=%d found=%t inventory_item=%d inventory_count=%d",
				drop.ObjectKey, drop.DestinationSlot, drop.Item.ItemID, found, stack.ItemID, stack.Count,
			))
			continue
		}
		bySlot[drop.DestinationSlot] = currentDungeonClearRewardDrop{
			ObjectKey: drop.DestinationSlot, TemplateID: drop.Item.ItemID, StackCount: uint32(stack.Count),
		}
	}
	result := make([]currentDungeonClearRewardDrop, 0, len(bySlot))
	for _, drop := range bySlot {
		result = append(result, drop)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ObjectKey < result[j].ObjectKey })
	if len(invalid) > 0 {
		return result, fmt.Errorf("%w: stale pickup presentation receipts: %s", errCurrentDungeonSettlementStateInvalid, strings.Join(invalid, "; "))
	}
	return result, nil
}

type currentDungeonDiscardReceiptKey struct {
	SourceSlot int16
	ItemID     uint32
}

func currentDungeonActiveDiscardAmounts(runtime *runtimeDungeonState) map[currentDungeonDiscardReceiptKey]uint64 {
	amounts := make(map[currentDungeonDiscardReceiptKey]uint64)
	if runtime == nil || runtime.DropOwner == nil {
		return amounts
	}
	for _, discard := range runtime.DropOwner.byObjectKey {
		if discard == nil || discard.DiscardOrigin == nil ||
			(discard.Status != runtimeDungeonDropAvailable && discard.Status != runtimeDungeonDropPending) {
			continue
		}
		origin := discard.DiscardOrigin
		if origin.ListType != dnfrepo.MainInventoryListType || origin.SourceSlot < 0 ||
			origin.Stack.ItemID <= 0 || uint64(origin.Stack.ItemID) > math.MaxUint32 ||
			origin.Stack.Count <= 0 {
			continue
		}
		key := currentDungeonDiscardReceiptKey{
			SourceSlot: origin.SourceSlot,
			ItemID:     uint32(origin.Stack.ItemID),
		}
		amounts[key] += uint64(origin.Stack.Count)
	}
	return amounts
}
