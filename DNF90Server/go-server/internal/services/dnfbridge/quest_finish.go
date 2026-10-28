package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var errCurrentFinishQuestAckShape = errors.New("current EXE finish-quest ACK shape is invalid")

// currentFinishQuestReplayKey scopes one terminal receipt to the selected
// character in one TCP session. Character selection can reuse its connection,
// so a bare quest ID would incorrectly suppress another character's receipt.
type currentFinishQuestReplayKey struct {
	characterID uint16
	questID     uint16
}

func newCurrentFinishQuestReplayKey(characterID uint16, request dnfquest.FinishQuestRequest) currentFinishQuestReplayKey {
	return currentFinishQuestReplayKey{characterID: characterID, questID: request.QuestID}
}

// currentFinishQuestReplay helpers keep the durable replay receipt scoped to
// reconnect recovery. A live same-session duplicate finish request (double
// click or client retry) must not receive the receipt a second time: the
// current EXE finish handler (sub_1D32120) drops the quest UI entry after the
// first receipt and NULL-derefs on the duplicate, killing the client
// (2026-07-25 quest 3157 crash at 0x1D3340F).
func (session *gameSession) currentFinishQuestAnswered(key currentFinishQuestReplayKey) bool {
	if session == nil {
		return false
	}
	session.questReplay.finishMu.Lock()
	defer session.questReplay.finishMu.Unlock()
	_, ok := session.questReplay.finishAnswered[key]
	return ok
}

func (session *gameSession) markCurrentFinishQuestAnswered(key currentFinishQuestReplayKey) {
	if session == nil {
		return
	}
	session.questReplay.finishMu.Lock()
	defer session.questReplay.finishMu.Unlock()
	if session.questReplay.finishAnswered == nil {
		session.questReplay.finishAnswered = make(map[currentFinishQuestReplayKey]struct{}, 1)
	}
	session.questReplay.finishAnswered[key] = struct{}{}
}

func (s *Service) handleCurrentFinishQuest(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	request, err := dnfquest.DecodeFinishQuestRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"body_len", len(body),
			"reason", "current_exe_op34_requires_exact_plain_10_byte_body",
			"error", err)
		return nil
	}
	// The current live ten-byte request ends in FF FF. The field's domain
	// meaning remains unproved, so validate only the observed wire marker.
	if request.QuestID == 0 || request.Multiplier != 1 || request.Reserved != dnfquest.CurrentFinishQuestObservedTailMarker {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"multiplier", request.Multiplier,
			"reserved", request.Reserved,
			"reason", "current_exe_op34_semantic_fields_invalid")
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketFinishQuest), 22)
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "character_settlement_transaction_unavailable")
		return nil
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "quest_catalog_unavailable",
			"error", err)
		return nil
	}
	if handled, linkedErr := s.handleCurrentLinkedObjectiveFinish(ctx, session, request, repositories, catalog); handled {
		if linkedErr != nil {
			s.logGameEvent(session, "game-upper-linked-objective-finish-blocked",
				"quest_id", request.QuestID,
				"reason", "atomic_linked_progress_failed",
				"error", linkedErr)
		}
		return linkedErr
	}
	tables, itemCatalog, err := s.currentQuestFinishResources(ctx)
	if err != nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "progression_or_item_pvf_unavailable",
			"error", err)
		return nil
	}
	var professionResources []currentQuestProfessionResources
	// The client submits the canonical quest ID. Some PVF story nodes use a
	// hidden profession/awakening companion as the actual reward source, so
	// checking only catalog.Find(request.QuestID) misses the required tables and
	// makes the atomic owner reject an otherwise valid finish. Resolve the same
	// final reward source used by the owner before deciding whether to preload.
	finishCharacterID, _, finishCharacter, finishCharacterFound := s.selectedCharacterForEnter(ctx, session)
	finishQuestRecord := dnfrepo.QuestRecord{}
	if finishCharacterFound && repositories.Quest != nil {
		characterID := finishCharacter.CharacterID
		if characterID == "" {
			characterID = strconv.Itoa(int(finishCharacterID))
		}
		if loaded, found, loadErr := repositories.Quest.Load(ctx, characterID); loadErr == nil && found {
			finishQuestRecord = loaded
		}
	}
	finishJob, finishJobValid := characterJobByte(finishCharacter)
	finishEligibility := dnfquest.CharacterEligibility{
		Level:    finishCharacter.Level,
		Job:      int(finishJob),
		GrowType: int(numericCharacterStatValue(finishCharacter, "grow_type")),
	}
	if finishCharacterFound && finishJobValid && catalog.FinishRewardNeedsProfessionResources(
		finishQuestRecord,
		finishEligibility,
		int64(request.QuestID),
		request.RewardSelectIndex,
		request.HasRewardSelect,
	) {
		profiles, skillCatalog, err := s.currentProfessionResources(ctx)
		if err != nil {
			s.logGameEvent(session, "game-upper-finish-quest-blocked",
				"quest_id", request.QuestID,
				"reason", "profession_pvf_resources_unavailable",
				"error", err)
			return nil
		}
		professionResources = append(professionResources, currentQuestProfessionResources{Profiles: profiles, SkillCatalog: skillCatalog})
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	mainSlotCount := uint16(0)
	if repositories.Settings != nil {
		containerState := s.loadCurrentItemListContainerState(ctx, repositories.Settings, characterID, dnfrepo.MainInventoryListType)
		mainSlotCount = containerState.MainSlotCount
	}
	return s.handleCurrentFinishQuestWithResources(
		session,
		request,
		repositories,
		catalog,
		tables,
		currentQuestFinishItemAllocator(itemCatalog, mainSlotCount),
		professionResources...,
	)
}

type currentQuestProfessionResources struct {
	Profiles     *dnfprofession.Profiles
	SkillCatalog *dnfskill.Table
}

func (s *Service) handleCurrentFinishQuestWithResources(
	session *gameSession,
	request dnfquest.FinishQuestRequest,
	repositories dnfrepo.Group,
	catalog *dnfquest.Catalog,
	tables *progression.Tables,
	allocator dnfquest.FinishItemAllocator,
	professionResources ...currentQuestProfessionResources,
) error {
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	var professionProfiles *dnfprofession.Profiles
	var skillCatalog *dnfskill.Table
	if len(professionResources) > 0 {
		professionProfiles = professionResources[0].Profiles
		skillCatalog = professionResources[0].SkillCatalog
	}
	accountID := s.accountIDForSession(session)
	var hellPartyUnlocks []dnfquest.HellPartyUnlock
	if catalog.HasHellPartyUnlock(int64(request.QuestID)) {
		hellPartyUnlocks, err = s.currentQuestHellPartyUnlocks(request.QuestID)
		if err != nil {
			s.logGameEvent(session, "game-upper-finish-quest-blocked",
				"quest_id", request.QuestID,
				"reason", "hell_party_pvf_area_mapping_unavailable",
				"error", err)
			return nil
		}
	}
	var experienceBonusPercent int64
	if growthEffect, active := s.currentGrowthContractEffect(ctx, accountID); active {
		experienceBonusPercent = growthEffect.BonusExperiencePercent
	}
	// 幸运角色（回归活动的角色表，chn_activatecharacter.etc）的任务经验加成。
	// 它按"当前角色"判定，所以必须读一次角色行；读不到就按无加成处理，
	// 不让一个活动加成把任务交付整个卡住。
	if ch, found, err := repositories.Character.Load(ctx, characterID); err != nil {
		return err
	} else if found {
		if lucky := s.currentLuckyCharacterEffect(session, ch); lucky != nil {
			experienceBonusPercent += lucky.Quest
		}
	}
	// 冒险团运行时配置（商店点数换算 / 成长胶囊槽）：满级任务经验的溢出部分
	// 用它换算成骁勇结晶，与角色经验同事务提交。
	adventureRuntime := s.adventureGroupTable.Runtime()
	result, err := owner.ApplyFinishSettlement(ctx, catalog, dnfquest.FinishCommitInput{
		AccountID:              accountID,
		CharacterID:            characterID,
		QuestID:                int64(request.QuestID),
		RewardSelectIndex:      request.RewardSelectIndex,
		HasRewardSelect:        request.HasRewardSelect,
		Multiplier:             request.Multiplier,
		CommittedAt:            time.Now().UTC(),
		Progression:            tables,
		ExperienceBonusPercent: experienceBonusPercent,
		AllocateItem:           allocator,
		ProfessionProfiles:     professionProfiles,
		SkillCatalog:           skillCatalog,
		HellPartyUnlocks:       hellPartyUnlocks,
		// 满级任务经验的溢出部分要在同一个事务里进骁勇结晶/成长胶囊槽。
		AdventureRuntime: &adventureRuntime,
	})
	if err != nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"reward_select", request.RewardSelectIndex,
			"has_reward_select", request.HasRewardSelect,
			"reason", "pvf_db_atomic_finish_rejected",
			"error", err)
		if currentFinishQuestBusinessFailure(err) {
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketFinishQuest), 22)
		}
		return nil
	}
	payload, err := buildCurrentFinishQuestSuccessBody(result)
	if err != nil {
		s.logGameEvent(session, "game-upper-finish-quest-blocked",
			"quest_id", request.QuestID,
			"completion_key", result.CompletionKey,
			"reason", "committed_result_ack_shape_invalid",
			"error", err)
		return nil
	}
	replayKey := newCurrentFinishQuestReplayKey(session.selectedCharacterID, request)
	if result.Replayed && session.currentFinishQuestAnswered(replayKey) {
		// Same-session duplicate of an already-answered finish: the first
		// receipt completed the client's quest UI flow; resending it NULL-derefs
		// the client handler. Reconnect recovery is unaffected because a fresh
		// session has an empty answered set and still receives the replay.
		s.logGameEvent(session, "game-upper-finish-quest-replay-suppressed",
			"quest_id", request.QuestID,
			"completion_key", result.CompletionKey,
			"reason", "same_session_duplicate_receipt_would_crash_client_handler")
		return nil
	}
	s.logGameEvent(session, "game-upper-finish-quest-success-send",
		"quest_id", result.QuestID,
		"completion_key", result.CompletionKey,
		"reward_source", result.Source,
		"base_experience_delta", result.BaseExperienceDelta,
		"growth_contract_bonus", result.ExperienceBonusDelta,
		"main_continuity_bonus", result.ContinuityExperienceDelta,
		"experience_delta", result.ExperienceDelta,
		"level_before", result.PreviousLevel,
		"level_after", result.NewLevel,
		"sp_delta", result.SPDelta,
		"item_count", len(result.Items),
		"consumed_count", len(result.ConsumedItems),
		"currency_count", len(result.Currency),
		// 券直接入账号钱包的数量（0 = 本次奖励里没有券）。
		"voucher_token_cera", result.VoucherCredits.TokenCera,
		"voucher_happy_token_cera", result.VoucherCredits.HappyTokenCera,
		"has_profession", result.HasProfession,
		"profession_chain_type", result.Profession.ChainType,
		"profession_grow_number", result.Profession.GrowNumber,
		"profession_grow_before", result.Profession.PreviousGrowType,
		"profession_grow_after", result.Profession.NewGrowType,
		"profession_grant_count", len(result.ProfessionGrants),
		"has_expert_job", result.HasExpertJob,
		"expert_job_type", result.ExpertJobType,
		"replayed", result.Replayed,
		"msg_id", uint16(dnfenum.CmdPacketFinishQuest),
		"classification", dnfproto.DefaultChannelClassification,
		"plain_body_len", len(payload)+1,
		"body_source", "current_exe_sub_1D32120_and_atomic_pvf_finish_receipt")
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketFinishQuest), payload); err != nil {
		return err
	}
	// The client can safely receive no more than one terminal receipt per
	// selected character/session, including the first durable replay after a
	// reconnect. Mark only after the ACK write succeeds so a failed transport
	// remains retryable.
	session.markCurrentFinishQuestAnswered(replayKey)
	// 任务奖励里的代币券 / 欢乐代币券已经直接进账号钱包（没有占背包格），
	// 所以必须把新余额推给客户端，否则玩家看不到到账。
	// 用 result.VoucherWallet（事务里算出来的权威值）而不是重新读一次；
	// 纯展示：失败只记日志，不让一次已经提交的任务结算变成失败。
	if result.VoucherWallet != nil {
		if err := s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID,
			buildCurrentCeraShopWalletBody(result.VoucherWallet.Cera, result.VoucherWallet.TokenCera, result.VoucherWallet.HappyTokenCera),
			currentCeraShopClass0); err != nil {
			s.logGameEvent(session, "game-cera-shop-wallet-send-failed", "source", "quest_finish_voucher_credit", "err", err)
		}
	}
	if result.HasExpertJob {
		// Current NoPack sub_1D32120 consumes chain type 20 as part of the
		// finish ACK, but the auxiliary-profession domain state has its own
		// class-0/op205 reader (sub_1D89C20). Publish that typed state
		// immediately after the ACK and before the generic character/quest
		// refreshes. 86JP's legacy op2 subtype0 and current op754 are not valid
		// local-actor carriers for this EXE; its compatibility unit mirrors the
		// validated op205 type into the existing current actor without a panel.
		//
		// This is the ONE op205 send that is a genuine acquisition: completing
		// an expert-job quest is exactly when the client's native qualification
		// window is wanted. Every other op205 send restates state the client
		// already holds and is marked silent, because the native reader cannot
		// tell the two apart and raises the same window for both.
		if err := s.sendCurrentExpertJobInfoForCharacterAnnouncing(session, result.PostCommitCharacter, true); err != nil {
			return err
		}
	}
	if result.HasProfession {
		err = s.sendCurrentProfessionFinishPostCommitSnapshots(session, result)
	} else {
		err = s.sendCurrentFinishQuestPostCommitSnapshots(session, result)
	}
	if err != nil {
		return err
	}
	if err := s.sendCurrentFinishQuestStoryDigestAfterSnapshots(session, result); err != nil {
		return err
	}
	if result.HasProfession || result.NewLevel != result.PreviousLevel {
		s.notifyCurrentUnitedFriendPresence(
			result.PostCommitCharacter.Name,
			s.currentUnitedFriendPresence(selectedCharacterID(session)),
			"quest_progression_friend_absolute_refresh",
		)
	}
	return nil
}

// sendCurrentFinishQuestStoryDigestAfterSnapshots publishes the pre-existing
// accepted story level only after the finish ACK has repainted the absolute
// character, skill and quest state. The current client compares this op1378
// value with the refreshed character level and starts the matching story
// summary. Quest completion is commonly submitted from the dungeon settlement
// page, where no town actor is finalized; op1378 updates the story player, not
// a scene actor, so that valid route must receive the trigger immediately too.
func (s *Service) sendCurrentFinishQuestStoryDigestAfterSnapshots(
	session *gameSession,
	result dnfquest.FinishCommitResult,
) error {
	if result.Replayed || result.NewLevel <= result.PreviousLevel {
		return nil
	}
	if result.NewLevel <= 0 || uint64(result.NewLevel) > uint64(math.MaxUint32) {
		return errCurrentFinishQuestAckShape
	}
	lastLevel := currentStoryDigestLastLevel(result.PostCommitCharacter, true)
	if lastLevel >= uint32(result.NewLevel) {
		s.logGameEvent(session, "game-upper-finish-quest-story-digest-skipped",
			"quest_id", result.QuestID,
			"last_level", lastLevel,
			"new_level", result.NewLevel,
			"reason", "story_level_already_accepted")
		return nil
	}
	return s.sendCurrentStoryDigestLastLevel(
		session,
		result.PostCommitCharacter,
		true,
		"finish_quest_after_character_skill_and_quest_snapshots_including_dungeon_settlement",
	)
}

func (s *Service) currentQuestHellPartyUnlocks(questID uint16) ([]dnfquest.HellPartyUnlock, error) {
	if s == nil || questID == 0 {
		return nil, nil
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return nil, err
	}
	areas := table.HellPartyAreasForQuest(int64(questID))
	if len(areas) == 0 {
		return nil, nil
	}
	unlock := make([]dnfquest.HellPartyUnlock, 0, len(areas))
	for _, area := range areas {
		if area.ID <= 0 {
			return nil, errDungeonWorldMapUnavailable
		}
		unlock = append(unlock, dnfquest.HellPartyUnlock{AreaID: area.ID, QuestID: int64(questID)})
	}
	return unlock, nil
}

func currentQuestRewardIsProfession(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value == "grow type" || value == "awakening type"
}

func validateCurrentProfessionFinishResult(session *gameSession, result dnfquest.FinishCommitResult) error {
	if session == nil {
		return errCurrentFinishQuestAckShape
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if !result.AtomicCommitted || !result.HasProfession || result.CharacterID != characterID ||
		result.PostCommitCharacter.CharacterID != characterID || result.PostCommitSkill.CharacterID != characterID ||
		result.PostCommitCharacter.Stats["grow_type"] != int64(result.Profession.NewGrowType) {
		return errCurrentFinishQuestAckShape
	}
	return nil
}

// sendCurrentProfessionFinishPostCommitSnapshots applies only repository/PVF
// state returned by the committed quest owner. Current EXE sub_1D32120 owns
// the profession transition: its op34 inline two-tree grant lists update the
// local grow type, profession dialog and native skill manager together. An
// op19 before op34 closes that dialog; a full mode0/mode1 rebuild or another
// op19 after op34 replaces the newly committed skill manager and leaves the
// skill window unable to issue buy-skill requests. The current weapon-change
// trace proves that one existing-actor mode0 row with its initialization event
// suppressed is safe: it updates the bound actor's grow type without recreating
// the skill manager. Keep op34 first, then use only that in-place actor refresh
// before the absolute point ledger and quest views.
func (s *Service) sendCurrentProfessionFinishPostCommitSnapshots(session *gameSession, result dnfquest.FinishCommitResult) error {
	if err := validateCurrentProfessionFinishResult(session, result); err != nil {
		return err
	}
	if currentTownSelfActorAppearanceReady(session) {
		if err := s.sendSelectedActorMode0AppearanceRefresh(
			session,
			"finish_profession",
			"current_exe_op34_inline_profession_transition_then_existing_actor_mode0_event_suppressed_then_op37_quests",
		); err != nil {
			return err
		}
	} else {
		// The committed grow type is still durable and the next normal town actor
		// bootstrap will carry it. Do not emit mode0 across a town transition.
		s.logGameEvent(session, "game-upper-finish-profession-actor-refresh-deferred",
			"character_id", result.CharacterID,
			"grow_type", result.Profession.NewGrowType,
			"reason", "selected_town_actor_generation_not_ready")
	}
	characterBody := buildCurrentFinishLoadingCharacterStateBody(result.PostCommitCharacter, result.PostCommitSkill.Points)
	if len(characterBody) != currentFinishLoadingCharacterStateBodySize {
		return errCurrentFinishQuestAckShape
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, characterBody, 0); err != nil {
		return err
	}
	if err := s.sendCurrentActiveQuestSnapshotFromCommittedQuest(
		session,
		result.PostCommitQuest,
		"current_exe_op34_after_client_owned_profession_skill_manager_refresh",
	); err != nil {
		return err
	}
	if err := s.sendCurrentClearQuestListFromCommittedQuest(
		session,
		result.PostCommitQuest,
		"current_exe_op34_after_profession_active_snapshot_completed_quest_scene_refresh",
	); err != nil {
		return err
	}
	if err := s.sendCurrentAcceptableQuestListOnlyForSession(session, "current_exe_op34_after_profession_active_snapshot"); err != nil {
		return err
	}
	return s.sendCurrentQuestGatedTownSceneRefresh(
		session,
		result,
		"current_exe_op34_after_profession_task_snapshots",
	)
}

// sendCurrentFinishQuestPostCommitSnapshots publishes only the immutable state
// returned by the committed settlement owner. Ordinary finishes must not push
// mode1/mode3 personal-information snapshots: current mode3 opens the personal
// information panel, and both bodies rebuild the entire actor/equipment view.
// The finish ACK owns ordinary EXP/item changes. A rare slot-expansion reward
// still needs one committed mode1 projection because that is the proved online
// carrier for ex_equip_slot_stat; mode3 remains suppressed.
//
// The order is intentional: changed inventory rows first, then only genuinely
// changed character/skill state, and finally the quest views. The op34 finish
// ACK carries the EXP delta but does not repaint the HUD bar for a same-level
// reward, so every nonzero EXP reward also publishes the committed absolute
// op37 character state. Skill information remains limited to changes that can
// affect the skill tree. A committed level change replaces the ordinary delta
// pair with the current client's complete task-manual snapshot so newly
// level-gated quests appear at once.
func (s *Service) sendCurrentFinishQuestPostCommitSnapshots(session *gameSession, result dnfquest.FinishCommitResult) error {
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if !result.AtomicCommitted || result.CharacterID != characterID ||
		result.PostCommitCharacter.CharacterID != characterID ||
		result.PostCommitSkill.CharacterID != characterID {
		return errCurrentFinishQuestAckShape
	}
	if err := s.sendCurrentFinishQuestRewardItemUpdate(session, result); err != nil {
		return err
	}
	if result.HasSlotExpansion {
		if err := s.sendCurrentFinishQuestSlotExpansionMode1Snapshot(session, result); err != nil {
			return err
		}
		s.logGameEvent(session, "game-upper-finish-quest-slot-expansion",
			"quest_id", result.QuestID,
			"slot_expansion_index", result.SlotExpansionIndex,
			"slot_expansion_bit", result.SlotExpansionBit,
			"ex_equip_slot_stat", result.PostCommitCharacter.Stats["ex_equip_slot_stat"])
	}
	characterStateChanged := result.ExperienceDelta != 0 ||
		result.NewLevel != result.PreviousLevel || result.SPDelta != 0 || result.HasExpertJob
	skillStateChanged := result.NewLevel != result.PreviousLevel || result.SPDelta != 0 || result.HasExpertJob
	if characterStateChanged {
		characterBody := buildCurrentFinishLoadingCharacterStateBody(
			result.PostCommitCharacter,
			result.PostCommitSkill.Points,
		)
		if len(characterBody) != currentFinishLoadingCharacterStateBodySize {
			return errCurrentFinishQuestAckShape
		}
		if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, characterBody, 0); err != nil {
			return err
		}
	}
	if skillStateChanged {
		// 副本内交任务同样会把 SP/经验一次性推上去（可能顺带升级）。场景内 op19 对
		// 一次性 actor 被动职业（契魔者蛇腹剑等）会打掉武器形态，判据与副本内升级
		// 完全一致：见 decideCurrentMidSceneSkillRefresh。SP/TP 已由上面的角色状态
		// 包（op37）下发，跳过不丢信息。
		decision := s.decideCurrentMidSceneSkillRefresh(result.PostCommitCharacter, result.PostCommitSkill)
		if !decision.Send {
			s.logGameEvent(session, "game-upper-finish-quest-character-skill-refresh-skipped",
				"quest_id", result.QuestID,
				"completion_key", result.CompletionKey,
				"character_id", result.PostCommitCharacter.CharacterID,
				"level", result.PostCommitCharacter.Level,
				"mode", decision.Mode,
				"at_risk_skill_ids", fmt.Sprint(decision.AtRiskSkills),
				"reason", decision.Reason)
		} else if err := s.sendCurrentFinishQuestMidSceneSkillRefresh(session, result); err != nil {
			return err
		}
	}
	if !characterStateChanged && !skillStateChanged {
		s.logGameEvent(session, "game-upper-finish-quest-character-skill-refresh-skipped",
			"quest_id", result.QuestID,
			"completion_key", result.CompletionKey,
			"reason", "no_experience_level_sp_or_expert_job_delta")
	}
	// Current EXE op574 rebuilds the role quest collection. Publish the active
	// snapshot first, then apply op356 so completed-quest town gates are not
	// discarded by that rebuild. op21 remains last because the op34 callback can
	// immediately create the newly acceptable successor UI.
	if err := s.sendCurrentActiveQuestSnapshotFromCommittedQuest(
		session,
		result.PostCommitQuest,
		"current_exe_op34_after_atomic_finish_active_quest_manager_rebuild",
	); err != nil {
		return err
	}
	if err := s.sendCurrentClearQuestListFromCommittedQuest(
		session,
		result.PostCommitQuest,
		"current_exe_op34_after_active_snapshot_completed_quest_scene_refresh",
	); err != nil {
		return err
	}
	// The server never auto-accepts the successor; the client presents it from
	// PVF and sends op31 only after player consent.
	if err := s.sendCurrentAcceptableQuestListOnlyForSession(session, "current_exe_op34_after_active_snapshot_final"); err != nil {
		return err
	}
	return s.sendCurrentQuestGatedTownSceneRefresh(
		session,
		result,
		"current_exe_op34_after_atomic_finish_task_snapshots",
	)
}

// sendCurrentFinishQuestMidSceneSkillRefresh 下发交任务后的技能信息（op19）。调用方
// 必须先过 decideCurrentMidSceneSkillRefresh：对一次性 actor 被动职业跳过这次刷新，
// 否则副本内交任务升级会打掉契魔者的蛇腹剑形态。
func (s *Service) sendCurrentFinishQuestMidSceneSkillRefresh(
	session *gameSession,
	result dnfquest.FinishCommitResult,
) error {
	if session == nil {
		return errCurrentFinishQuestAckShape
	}
	activeTree, err := currentSceneActiveSkillTree(result.PostCommitCharacter)
	if err != nil {
		return err
	}
	job, ok := characterJobByte(result.PostCommitCharacter)
	if !ok {
		return errCurrentFinishQuestAckShape
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	skillBody, activeSlots, err := s.buildCurrentSceneSkillInfoRuntimeBody(
		ctx,
		result.PostCommitCharacter,
		result.PostCommitSkill,
		job,
		activeTree,
	)
	cancel()
	if err != nil {
		return err
	}
	// learned_skill_ids 与场景构建路径保持一致，方便日志核对这次刷新实际带了哪些技能。
	activeState, found := dnfrepo.SkillTree(result.PostCommitSkill, int(activeTree))
	if !found {
		activeState = dnfrepo.SkillTreeState{
			Skills: result.PostCommitSkill.Skills,
			Points: result.PostCommitSkill.Points,
		}
	}
	learnedSkillIDs := make([]int64, 0, len(activeState.Skills))
	for skillID, state := range activeState.Skills {
		if state.Enabled && state.Level > 0 {
			learnedSkillIDs = append(learnedSkillIDs, skillID)
		}
	}
	sort.Slice(learnedSkillIDs, func(left, right int) bool {
		return learnedSkillIDs[left] < learnedSkillIDs[right]
	})
	projection := currentSceneSkillInfoProjection{
		body:            skillBody,
		characterID:     result.PostCommitCharacter.CharacterID,
		job:             job,
		learnedCount:    len(activeState.Skills),
		learnedSkillIDs: learnedSkillIDs,
		activeSlots:     activeSlots,
		activeTree:      activeTree,
	}
	if job == darkcombo.Job {
		return s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(session, projection, "finish_quest_character_skill_refresh")
	}
	return s.sendCurrentSceneSkillInfoProjection(session, projection, "finish_quest_character_skill_refresh")
}

func (s *Service) sendCurrentFinishQuestSlotExpansionMode1Snapshot(
	session *gameSession,
	result dnfquest.FinishCommitResult) error {
	if session == nil || session.selectedCharacterID == 0 {
		return errCurrentFinishQuestAckShape
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if !result.AtomicCommitted || !result.HasSlotExpansion || result.CharacterID != characterID ||
		result.PostCommitCharacter.CharacterID != characterID {
		return errCurrentFinishQuestAckShape
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	if repositories, ok := s.repositoryGroup(); ok {
		legacyRepo = repositories.LegacyUserInfo
	}
	charID := session.selectedCharacterID
	summary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, result.PostCommitCharacter, true)
	adventureLevel := uint32(summary.ManageLevel)
	mode1Body := s.buildCurrentSelectedUserInfoMode1BodyWithAdventureLevelInContext(
		ctx,
		session,
		legacyRepo,
		result.PostCommitCharacter,
		true,
		charID,
		adventureLevel,
		currentTownActorOwnerContext(session),
	)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode1Body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-finish-quest-slot-expansion-mode1-send",
		"quest_id", result.QuestID,
		"mode1_body_len", len(mode1Body),
		"owner_channel", currentTownActorOwnerContext(session),
		"mode3_suppressed", true,
		"ex_equip_slot_stat", result.PostCommitCharacter.Stats["ex_equip_slot_stat"],
		"body_source", "atomic_post_commit_mode1_only_for_proved_slot_expansion_carrier")
	return nil
}

func (s *Service) sendCurrentFinishQuestRewardItemUpdate(session *gameSession, result dnfquest.FinishCommitResult) error {
	if len(result.Items) == 0 && len(result.ConsumedItems) == 0 && len(result.Currency) == 0 {
		s.logGameEvent(session, "game-upper-finish-quest-reward-item-update-skipped",
			"quest_id", result.QuestID,
			"completion_key", result.CompletionKey,
			"reason", "no_item_currency_or_consumed_material_changes")
		return nil
	}
	entries := make([]currentItemListEntry, 0, len(result.Items)+len(result.ConsumedItems)+len(result.Currency))
	seenGold := false
	for _, currency := range result.Currency {
		// 金币体只带 **PostValue**（快照余额），不带 Delta，所以这里的负 Delta 是合法的：
		// 交付金币（PVF [int data] 的 ID 0，服主口径 2026-10-03）扣完钱后正是负 Delta。
		// 早先这里判 `Delta < 0` 就整包拒绝，那会把"任务要求交 100 万金币"这条路径
		// 在 ACK 阶段打回 shape 错误。钱包余额仍然必须非负。
		if currency.PostValue < 0 {
			return errCurrentFinishQuestAckShape
		}
		switch strings.ToLower(strings.TrimSpace(currency.Name)) {
		case "gold":
			if seenGold {
				return errCurrentFinishQuestAckShape
			}
			seenGold = true
			gold := currency.PostValue
			if gold > math.MaxInt32 {
				gold = math.MaxInt32
			}
			var entry currentItemListEntry
			entry.patchCore(0, 0, uint32(gold))
			entries = append(entries, entry)
		default:
			return errCurrentFinishQuestAckShape
		}
	}
	rewardSlots := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		if item.SlotKey != "" {
			rewardSlots[item.SlotKey] = struct{}{}
		}
	}
	for _, consumed := range result.ConsumedItems {
		if consumed.SlotIndex > math.MaxInt16 || consumed.ItemID <= 0 || consumed.ItemID > math.MaxUint32 || consumed.Delta <= 0 || consumed.Delta > math.MaxUint32 || consumed.PostCount < 0 {
			return errCurrentFinishQuestAckShape
		}
		if _, replacedByReward := rewardSlots[consumed.SlotKey]; replacedByReward {
			continue
		}
		var entry currentItemListEntry
		if consumed.PostCount <= 0 {
			entry.patchCore(int16(consumed.SlotIndex), math.MaxUint32, 0)
			entries = append(entries, entry)
			continue
		}
		if len(consumed.RawEntry) == currentItemListEntryWireSize &&
			binary.LittleEndian.Uint16(consumed.RawEntry[0:2]) == consumed.SlotIndex &&
			binary.LittleEndian.Uint32(consumed.RawEntry[2:6]) == uint32(consumed.ItemID) {
			copy(entry.data[:], consumed.RawEntry)
			entries = append(entries, entry)
			continue
		}
		slots := result.PostCommitInventory.Slots
		if dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(consumed.SlotIndex)) {
			slots = result.PostCommitAccountInventory.Slots
		}
		stack, found := slots[consumed.SlotKey]
		if !found || stack.ItemID != consumed.ItemID || stack.Count != consumed.PostCount {
			return errCurrentFinishQuestAckShape
		}
		entry = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, int16(consumed.SlotIndex), stack)
		entries = append(entries, entry)
	}
	for _, item := range result.Items {
		if len(item.RawEntry) != currentItemListEntryWireSize ||
			binary.LittleEndian.Uint16(item.RawEntry[0:2]) != item.SlotIndex ||
			binary.LittleEndian.Uint32(item.RawEntry[2:6]) != uint32(item.ItemID) {
			return errCurrentFinishQuestAckShape
		}
		var entry currentItemListEntry
		copy(entry.data[:], item.RawEntry)
		entries = append(entries, entry)
	}
	body := buildCurrentItemUpdateBody(0, entries)
	s.logGameEvent(session, "game-upper-finish-quest-reward-item-update-send",
		"quest_id", result.QuestID,
		"completion_key", result.CompletionKey,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", 0,
		"entry_count", len(entries),
		"item_count", len(result.Items),
		"consumed_count", len(result.ConsumedItems),
		"currency_count", len(result.Currency),
		"voucher_token_cera", result.VoucherCredits.TokenCera,
		"voucher_happy_token_cera", result.VoucherCredits.HappyTokenCera,
		"entry_size", currentItemListEntryWireSize,
		"body_len", len(body),
		"body_source", "committed_pvf_finish_reward_consumed_material_inventory_and_gold_op14_raw77")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}

func (s *Service) currentQuestFinishResources(ctx context.Context) (*progression.Tables, *pvfDungeonDropCatalog, error) {
	if s == nil {
		return nil, nil, dnfquest.ErrFinishSettlementUnavailable
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	tables, err := progression.Load(ctx, archive)
	if err != nil {
		return nil, nil, err
	}
	var items *pvfDungeonDropCatalog
	if s.dungeonMonsterTable != nil {
		items, err = s.dungeonMonsterTable.DropCatalog()
	} else {
		items, err = newPVFDungeonDropCatalog(archive)
	}
	if err != nil {
		return nil, nil, err
	}
	return tables, items, nil
}

func currentQuestFinishItemAllocator(catalog *pvfDungeonDropCatalog, mainSlotCount uint16) dnfquest.FinishItemAllocator {
	return func(record *dnfrepo.InventoryRecord, request dnfquest.FinishItemGrantRequest) (dnfquest.FinishCommittedItem, error) {
		if catalog == nil || record == nil || request.ItemID <= 0 || request.ItemID > math.MaxUint32 || request.Count <= 0 || request.Count > math.MaxUint32 {
			return dnfquest.FinishCommittedItem{}, errDungeonPickupItemInvalid
		}
		definition, err := catalog.ResolveItem(uint32(request.ItemID))
		if err != nil {
			return dnfquest.FinishCommittedItem{}, err
		}
		definition, err = currentPVFItemDefinitionForGrantAt(definition, time.Now().UTC())
		if err != nil {
			return dnfquest.FinishCommittedItem{}, err
		}
		source := strings.TrimSpace(request.Source)
		if source == "" {
			source = "quest_pvf_finish_reward"
		}
		// 账号共享优先：无色小晶块 / 灵魂 在同一个账号里只有账号共享仓那个家
		// （唯一入口 grantCurrentAccountSharedFirst，见 account_shared_grant.go）。
		//
		// 只接受"整份都进得去"：结算收据一行只能描述一个格子，而
		// allocateFinishRewardItems 要求 Delta 就是整份数量 —— 拆行会让 PostCount
		// 对不上 Delta，校验失败会把**整笔任务结算**打挂。所以固定格只能装一部分时
		// 把那一格还原、整份改走下面的背包路径（与翻牌 P2-2 同一口径，绝不半进半留）。
		// 线上这 12 个道具都没有 [stack limit]，固定格不封顶，这条还原分支实际到不了。
		if account := request.AccountInventory; account != nil {
			if slot, member := dnfrepo.AccountSharedWarehouseSlotForItem(int64(definition.ItemID)); member {
				key := dnfrepo.AccountSharedInventorySlotKey(slot)
				before, existed := account.Slots[key]
				shared := grantCurrentAccountSharedFirst(account, definition, uint32(request.Count), source)
				if shared.Changed && shared.Remainder == 0 {
					stack, found := account.Slots[key]
					if !found || stack.ItemID != request.ItemID || stack.Count < request.Count ||
						len(stack.RawEntry) != currentItemListEntryWireSize {
						return dnfquest.FinishCommittedItem{}, errDungeonPickupItemInvalid
					}
					return dnfquest.FinishCommittedItem{
						SlotKey: key, SlotIndex: uint16(shared.Slot), ItemID: request.ItemID,
						Delta: request.Count, PostCount: stack.Count, CountOrSeed: uint32(request.Count),
						// 账号共享槽也是 list-0 线地址，ACK 与刷新直接用这一行的 0x77 原文。
						RawEntry: append([]byte(nil), stack.RawEntry...),
					}, nil
				}
				if shared.Changed {
					if existed {
						account.Slots[key] = before
					} else {
						delete(account.Slots, key)
					}
				}
			}
		}
		definition = limitCurrentMainInventoryDefinitionSlots(definition, mainSlotCount)
		occupied := make(map[string]struct{}, len(record.Slots))
		for key := range record.Slots {
			occupied[key] = struct{}{}
		}
		slot, err := addCurrentDungeonPickupToInventory(record, definition, uint32(request.Count))
		if err != nil {
			return dnfquest.FinishCommittedItem{}, err
		}
		key := currentDungeonPickupMainSlotKey(int16(slot))
		stack, found := record.Slots[key]
		if !found || stack.ItemID != request.ItemID || stack.Count < request.Count {
			return dnfquest.FinishCommittedItem{}, errDungeonPickupItemInvalid
		}
		entry := currentItemListEntryFromStack(0, int16(slot), stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 6)
		}
		if _, existed := occupied[key]; !existed {
			stack.Extra["source"] = source
		}
		stack.Extra["last_grant_source"] = source
		stack.Extra["last_grant_quest_id"] = strconv.FormatInt(request.QuestID, 10)
		if completionKey := strings.TrimSpace(request.CompletionKey); completionKey != "" {
			stack.Extra["last_grant_completion_key"] = completionKey
		}
		stack.Extra["last_grant_pvf_source"] = source
		record.Slots[key] = stack
		countOrSeed := uint32(request.Count)
		if definition.Kind == dungeonDropItemEquipment {
			countOrSeed = binary.LittleEndian.Uint32(entry.data[0x06:0x0a])
		}
		return dnfquest.FinishCommittedItem{
			SlotKey: key, SlotIndex: slot, ItemID: request.ItemID,
			Delta: request.Count, PostCount: stack.Count, CountOrSeed: countOrSeed,
			RawEntry: append([]byte(nil), entry.data[:]...),
		}, nil
	}
}

// currentFinishGoldDebitOnly 报告这次结算的货币改动"**可以**放进链路包体"：
// 空（没有货币改动）永远放行；非空时必须全是金币负 Delta（= 交付金币的扣款）。
//
// 完成包的包体本身不带货币：金币余额由 post-commit 的 op14 钱包行下发
// （见 sendCurrentFinishQuestRewardItemUpdate）。所以下面三条"链路包体"分支
// （职业变更 / 副职业 / 槽位扩展）过去一律要求 `len(Currency) == 0` 是合理的 —— 那时
// Currency 只可能来自金币**奖励**，而这三类任务不该有奖励。
//
// 但交付金币（PVF [int data] 的 ID 0，服主口径 2026-10-03）会给任何任务加上一条
// **负 Delta** 的货币条目，于是这三条链会把一笔完全正常的结算判成 ACK 形状非法：
// 现场 2026-10-03 开耳环槽位任务（2636，[slot expansion]）正是这样 —— 金币扣了、
// 槽位开了，但完成回执发不出去，玩家要小退才看见"完成"；重试又因为回执没落库而
// 报 receipt is corrupt。
//
// 判据收窄到"空，或**只**有扣款"：任何正 Delta（真奖励）仍然被这三条链拒绝，
// 保持它们原有的"不该有奖励"约束不被顺手放开。
func currentFinishGoldDebitOnly(currency []dnfquest.FinishCommittedCurrency) bool {
	for _, entry := range currency {
		if !strings.EqualFold(strings.TrimSpace(entry.Name), "gold") || entry.Delta >= 0 || entry.PostValue < 0 {
			return false
		}
	}
	return true
}

func buildCurrentFinishQuestSuccessBody(result dnfquest.FinishCommitResult) ([]byte, error) {
	if !result.AtomicCommitted || result.QuestID <= 0 || result.QuestID > math.MaxUint16 ||
		result.CompletionKey == "" || result.Source == "" || len(result.Items) > math.MaxUint8 || len(result.ConsumedItems) > math.MaxUint8 {
		return nil, errCurrentFinishQuestAckShape
	}
	var writer packetWriter
	writer.writeUint16(uint16(result.QuestID))
	writer.writeByte(0) // completionType=0: current reader consumes the normal item sections.
	writer.writeUint32(result.ExperienceDelta)
	writer.writeByte(byte(len(result.ConsumedItems)))
	for _, consumed := range result.ConsumedItems {
		if consumed.SlotIndex > math.MaxInt16 || consumed.ItemID <= 0 || consumed.ItemID > math.MaxUint32 ||
			consumed.Delta <= 0 || consumed.Delta > math.MaxUint32 || consumed.PostCount < 0 {
			return nil, errCurrentFinishQuestAckShape
		}
		writer.writeByte(0) // updateType=0: current EXE subtracts consumed count from the slot.
		writer.writeUint16(consumed.SlotIndex)
		writer.writeUint32(uint32(consumed.Delta))
	}
	if result.HasProfession {
		if len(result.Items) != 0 || !currentFinishGoldDebitOnly(result.Currency) ||
			(result.Profession.ChainType != 1 && result.Profession.ChainType != 2) ||
			result.Profession.GrowNumber == 0 || result.Profession.NewGrowType == result.Profession.PreviousGrowType ||
			len(result.ProfessionGrants) > math.MaxUint8 {
			return nil, errCurrentFinishQuestAckShape
		}
		writer.writeByte(result.Profession.ChainType)
		writer.writeByte(result.Profession.GrowNumber)
		for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
			layout := result.PostCommitSkill.Layouts[tree]
			writer.writeByte(byte(len(result.ProfessionGrants)))
			for _, grant := range result.ProfessionGrants {
				if grant.SkillID == 0 || grant.Level <= 0 || grant.Level > math.MaxUint8 {
					return nil, errCurrentFinishQuestAckShape
				}
				// Current NoPack sub_1D32120 consumes the destination skill-tree
				// slot, the skill ID, then the absolute level, once per skill tree.
				// The per-tree repetition only makes sense because the first byte is
				// that tree's slot; a plain learned flag would be identical for both
				// lists.
				//
				// Live 2026-09-15 (test1, quest 7803, 鬼剑士 -> 剑魂): the writer
				// used to emit (level, skillID, 1), i.e. the slot and the level were
				// swapped. Every grant therefore targeted tree slot 1 and the last one
				// won the cell; the grants are sorted by skill ID, so 唤醒 (501, a
				// [passive]) overwrote 上挑 in the asdfgh "s" cell. The client never
				// places a passive on a quick bar and refuses to drag one off, so that
				// cell was stuck until the next op19 rebuilt the manager. Writing the
				// slot into the trailing byte instead only moved the bug: the client
				// read the slot as the level and displayed 唤醒 at level 106. Send
				// (slot, skillID, level).
				slot, ok := finishProfessionGrantSlot(layout, grant.SkillID)
				if !ok {
					return nil, errCurrentFinishQuestAckShape
				}
				writer.writeByte(slot)
				writer.writeUint16(grant.SkillID)
				writer.writeByte(byte(grant.Level))
			}
		}
		return writer.bytes(), nil
	}
	if result.HasExpertJob {
		if len(result.Items) != 0 || !currentFinishGoldDebitOnly(result.Currency) || result.ExpertJobType == 0 {
			return nil, errCurrentFinishQuestAckShape
		}
		// Current NoPack sub_1D32120 handles chain 20 in the same branch as
		// class change/awakening: u8 type, then two inline skill-tree counts.
		// Expert-job completion has no inline skill grants; the server persists
		// expert_job_type atomically before this ACK is emitted.
		writer.writeByte(20)
		writer.writeByte(result.ExpertJobType)
		writer.writeByte(0)
		writer.writeByte(0)
		return writer.bytes(), nil
	}
	if result.HasSlotExpansion {
		expectedBit, validIndex := dnfquest.ExEquipSlotBitForPVFIndex(result.SlotExpansionIndex)
		if len(result.Items) != 0 || !currentFinishGoldDebitOnly(result.Currency) ||
			!validIndex ||
			result.SlotExpansionBit != expectedBit {
			return nil, errCurrentFinishQuestAckShape
		}
		// Current NoPack sub_1D32120 chain type 21 does read a u32, but the
		// receiving model is unrelated to the support/magic-stone/earring
		// bitset. The committed bit is projected by the slot-expansion-only
		// mode1 actor snapshot, so finish uses the ordinary empty chain.
	}
	writer.writeByte(0) // chainType=0: ordinary PVF item reward.
	writer.writeByte(byte(len(result.Items)))
	for _, item := range result.Items {
		if item.ItemID <= 0 || item.ItemID > math.MaxUint32 || item.CountOrSeed == 0 || len(item.RawEntry) != currentItemListEntryWireSize ||
			binary.LittleEndian.Uint16(item.RawEntry[0:2]) != item.SlotIndex ||
			binary.LittleEndian.Uint32(item.RawEntry[2:6]) != uint32(item.ItemID) {
			return nil, errCurrentFinishQuestAckShape
		}
		writer.writeUint16(item.SlotIndex)
		writer.writeUint32(uint32(item.ItemID))
		writer.writeUint32(item.CountOrSeed)
		writer.writeByte(item.RawEntry[0x0a])
		writer.writeUint16(binary.LittleEndian.Uint16(item.RawEntry[0x0b:0x0d]))
		writer.writeUint32(binary.LittleEndian.Uint32(item.RawEntry[0x0e:0x12]))
		writer.writeByte(item.RawEntry[0x12])
		writer.writeByte(item.RawEntry[0x13])
	}
	return writer.bytes(), nil
}

// finishProfessionGrantSlot reports the skill-tree slot the committed layout
// assigned to one granted skill. Every learned skill owns exactly one slot:
// skillcmd.EnsureLayout places whatever the class-change baseline granted and
// fails closed if it cannot, so a missing entry means the committed record and
// the grant list disagree and the ACK must not be emitted.
func finishProfessionGrantSlot(layout dnfrepo.SkillLayout, skillID uint16) (uint8, bool) {
	for slot, assigned := range layout {
		if assigned != skillID {
			continue
		}
		if slot < 0 || slot > math.MaxUint8 {
			return 0, false
		}
		return uint8(slot), true
	}
	return 0, false
}

func currentFinishQuestBusinessFailure(err error) bool {
	return errors.Is(err, dnfquest.ErrFinishQuestNotPending) ||
		errors.Is(err, dnfquest.ErrFinishQuestCompletionConflict) ||
		errors.Is(err, dnfquest.ErrFinishQuestSettlementStale) ||
		errors.Is(err, dnfquest.ErrFinishQuestSettlementCorrupt) ||
		errors.Is(err, dnfquest.ErrFinishQuestMultiplierUnsupported) ||
		errors.Is(err, dnfquest.ErrFinishProgressionUnproven) ||
		errors.Is(err, dnfquest.ErrFinishRequiredItemsMissing) ||
		errors.Is(err, dnfquest.ErrQuestDefinitionMissing) ||
		errors.Is(err, dnfquest.ErrQuestNotAcceptable) ||
		errors.Is(err, dnfquest.ErrQuestRewardUnsupported) ||
		errors.Is(err, dnfquest.ErrQuestRewardMalformed) ||
		errors.Is(err, dnfquest.ErrQuestRewardSelectionInvalid) ||
		errors.Is(err, errDungeonPickupInventoryFull)
}
