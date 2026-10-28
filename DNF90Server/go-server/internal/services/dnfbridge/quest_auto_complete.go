package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentAutoCompleteMainQuestRequestSize     = 4
	currentAutoCompleteMainQuestAckSuccessSize  = 1
	currentAutoCompleteMainQuestAckFailureSize  = 2
	currentAutoCompleteMainQuestMaxLevel        = 89
	currentAutoCompleteMainQuestAckNeutralError = 0

	// op1429's client handler draws nothing at all for a rejected request - it
	// only releases the quest window - so every rejection has to carry its own
	// notice or the button reads as dead input.
	currentAutoCompleteClassChangeNoticeText = "需要先完成转职任务，转职后才能一键完成低级主线。"
	currentAutoCompleteNoTargetNoticeText    = "当前没有可以一键完成的主线任务。"
	// The rest of the story chain sits behind an awakening quest, and a bulk
	// pass never completes one for the player. Name it: the quest is still in
	// the player's list, so "没有可以一键完成的主线任务" reads as a lie.
	currentAutoCompleteAwakeningNoticeFormat = "剩下的主线卡在觉醒任务『%s』上，需要你自己完成，一键完成不会代做觉醒。"
	currentAutoCompleteAwakeningNoticeText   = "剩下的主线卡在觉醒任务上，需要你自己完成，一键完成不会代做觉醒。"
	currentAutoCompleteUnavailableNoticeText = "一键完成主线暂时不可用，请稍后再试。"
	// A [question] quest's answer decides which successors unlock, so the pass
	// must not pick one. Same reason the awakening text exists: the quest is
	// still in the list, so a bare "没有可以一键完成的主线任务" is wrong.
	currentAutoCompleteChoiceNoticeFormat = "剩下的主线卡在抉择任务『%s』上，需要你自己找 NPC 做出选择，一键完成不会替你选。"
	currentAutoCompleteChoiceNoticeText   = "剩下的主线卡在抉择任务上，需要你自己找 NPC 做出选择，一键完成不会替你选。"
)

// currentAutoCompleteMainQuestNoticeSurface is where those rejections land.
// Magenta on the 系统 tab: the message is a hint for one player rather than a
// broadcast, and the default light blue proved too easy to miss.
var currentAutoCompleteMainQuestNoticeSurface = noticeSurfaceSystemMagenta

// op1429 answers through the ordinary class-1 envelope. Its reader
// (sub_2262150) takes one u8 status; when that status is zero and the
// per-opcode filter sub_7691D0 rejects the opcode - 1429 is outside its
// 0x12..0xD7 window, so it does - it takes one more u8 error code. Only then
// does it call the registered handler, sub_225C8A0 passing (opcode, status,
// error, context).
//
// The registered op1429 handler sub_1D0F340 is a pure thunk over the quest
// window at [0x51B2798]: status != 0 jumps to sub_1A03E90, which refreshes the
// window and, for the bulk request (the window's 0x52C flag == 1, set by the
// sender at 0x19EE332), asks the server for the refreshed list with op1456;
// status == 0 jumps to sub_19FE680, which just releases the window. Neither
// reads one byte of the remaining body.
//
// The packed 13-byte struct this used to send (selector, highest quest,
// cutoff, committed) was therefore never consumed by the game. It was read off
// sub_79EB70 - which is registered for 1429 in the class-0 *logging* table and
// only prints five fields - and it made the feature unusable: the envelope
// took its first byte as the status, and that byte is the low byte of the
// request selector, which the bulk request always sends as 0. Every bulk
// auto-complete, success or not, reached the client as a silent failure.
func buildCurrentAutoCompleteMainQuestAck(committed bool) []byte {
	if committed {
		return []byte{1}
	}
	// The envelope still reads an error byte here, but op1429's handler
	// discards it - there is no client text to select - so send a neutral 0.
	return []byte{0, currentAutoCompleteMainQuestAckNeutralError}
}

// sendCurrentAutoCompleteMainQuestRejection answers one refused op1429 the way
// the client needs it: the class-1 failure envelope, then a notice carrying the
// reason. The ACK alone releases the quest window without drawing anything, so
// without the notice the player only sees an unresponsive button.
func (s *Service) sendCurrentAutoCompleteMainQuestRejection(
	session *gameSession,
	event string,
	noticeText string,
	noticeSource string,
	fields ...any,
) error {
	response := buildCurrentAutoCompleteMainQuestAck(false)
	fields = append(fields,
		"notice_source", noticeSource,
		"notice_surface", currentAutoCompleteMainQuestNoticeSurface.Name,
		"msg_id", uint16(dnfenum.CmdPacketClearQuestTicket),
		"classification", dnfproto.DefaultChannelClassification,
		"body_len", len(response))
	s.logGameEvent(session, event, fields...)
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketClearQuestTicket),
		response,
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	return s.sendNotice(session, noticeRequest{
		Surface: currentAutoCompleteMainQuestNoticeSurface,
		Text:    noticeText,
		Source:  noticeSource,
	})
}

// sendCurrentAutoCompleteMainQuestUnavailable answers the rejections that are
// not the player's doing - a repository or PVF catalog that is not ready, or a
// completion the domain refused. They used to return silently, which left the
// quest window waiting on an ACK that never came.
func (s *Service) sendCurrentAutoCompleteMainQuestUnavailable(session *gameSession, fields ...any) error {
	return s.sendCurrentAutoCompleteMainQuestRejection(
		session,
		"game-auto-complete-main-quest-blocked",
		currentAutoCompleteUnavailableNoticeText,
		"auto_complete_main_quest_unavailable",
		fields...,
	)
}

func (s *Service) handleCurrentAutoCompleteMainQuest(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if len(body) != currentAutoCompleteMainQuestRequestSize {
		s.logGameEvent(session, "game-auto-complete-main-quest-blocked",
			"body_len", len(body),
			"reason", "current_exe_op1429_requires_exact_u32_request")
		return nil
	}
	requestSelector := binary.LittleEndian.Uint32(body)

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return s.sendCurrentAutoCompleteMainQuestUnavailable(session,
			"request_selector", requestSelector,
			"reason", "character_settlement_transaction_unavailable")
	}
	_, _, character, found := s.selectedCharacterForEnter(ctx, session)
	if !found || character.Level <= 1 {
		return s.sendCurrentAutoCompleteMainQuestUnavailable(session,
			"request_selector", requestSelector,
			"character_found", found,
			"level", character.Level,
			"reason", "selected_character_or_previous_level_unavailable")
	}
	job, validJob := characterJobByte(character)
	if !validJob {
		return s.sendCurrentAutoCompleteMainQuestUnavailable(session,
			"request_selector", requestSelector,
			"job", character.Job,
			"reason", "selected_character_job_invalid")
	}
	cutoffLevel := character.Level - 1
	if cutoffLevel > currentAutoCompleteMainQuestMaxLevel {
		cutoffLevel = currentAutoCompleteMainQuestMaxLevel
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return s.sendCurrentAutoCompleteMainQuestUnavailable(session,
			"request_selector", requestSelector,
			"reason", "quest_catalog_unavailable",
			"error", err)
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return err
	}
	characterID := character.CharacterID
	if characterID == "" {
		characterID = strconv.Itoa(int(session.selectedCharacterID))
	}
	targetQuestID := int64(requestSelector)
	targetSource := "current_exe_nonzero_per_entry_selector"
	if requestSelector == 0 {
		targetSource = "zero_selector_bulk_eligible_pvf_main_quests"
	}
	input := dnfquest.AutoCompleteMainInput{
		CharacterID: characterID,
		Eligibility: dnfquest.CharacterEligibility{
			Level:    character.Level,
			Job:      int(job),
			GrowType: int(numericCharacterStatValue(character, "grow_type")),
		},
		CutoffLevel:   cutoffLevel,
		TargetQuestID: targetQuestID,
		CompletedAt:   time.Now().UTC(),
	}
	// Story items an outstanding epic still needs (3226's letter for 3610, the
	// 3509 fragments) are carried forward inside the same transaction. Without
	// the item catalog the quest side still completes; only a character that
	// actually needs such an item then fails, loudly, instead of getting stuck.
	if itemCatalog, itemCatalogErr := s.currentPVFItemCatalog(); itemCatalogErr == nil {
		mainSlotCount := uint16(0)
		if repositories.Settings != nil {
			containerState := s.loadCurrentItemListContainerState(ctx, repositories.Settings, characterID, dnfrepo.MainInventoryListType)
			mainSlotCount = containerState.MainSlotCount
		}
		input.AllocateItem = currentQuestFinishItemAllocator(itemCatalog, mainSlotCount)
	} else {
		s.logGameEvent(session, "game-auto-complete-main-quest-carry-forward-catalog-unavailable",
			"request_selector", requestSelector,
			"error", itemCatalogErr)
	}
	result, err := owner.ApplyAutoCompleteMain(ctx, catalog, input)
	if err != nil {
		if errors.Is(err, dnfquest.ErrAutoCompleteClassRequired) {
			return s.sendCurrentAutoCompleteMainQuestRejection(
				session,
				"game-auto-complete-main-quest-class-change-required",
				currentAutoCompleteClassChangeNoticeText,
				"auto_complete_main_quest_class_change_required",
				"request_selector", requestSelector,
				"job", job,
				"grow_type", input.Eligibility.GrowType,
				"cutoff_level", cutoffLevel,
				"reason", err,
				"quest_mutation", "none",
			)
		}
		var awakening *dnfquest.AwakeningRequiredError
		if errors.As(err, &awakening) {
			text := currentAutoCompleteAwakeningNoticeText
			if awakening.QuestName != "" {
				text = fmt.Sprintf(currentAutoCompleteAwakeningNoticeFormat, awakening.QuestName)
			}
			return s.sendCurrentAutoCompleteMainQuestRejection(
				session,
				"game-auto-complete-main-quest-awakening-required",
				text,
				"auto_complete_main_quest_awakening_required",
				"request_selector", requestSelector,
				"blocking_quest_id", awakening.QuestID,
				"blocking_quest_name", awakening.QuestName,
				"cutoff_level", cutoffLevel,
				"quest_mutation", "none",
			)
		}
		var choice *dnfquest.ChoiceRequiredError
		if errors.As(err, &choice) {
			text := currentAutoCompleteChoiceNoticeText
			if choice.QuestName != "" {
				text = fmt.Sprintf(currentAutoCompleteChoiceNoticeFormat, choice.QuestName)
			}
			return s.sendCurrentAutoCompleteMainQuestRejection(
				session,
				"game-auto-complete-main-quest-choice-required",
				text,
				"auto_complete_main_quest_choice_required",
				"request_selector", requestSelector,
				"blocking_quest_id", choice.QuestID,
				"blocking_quest_name", choice.QuestName,
				"cutoff_level", cutoffLevel,
				"quest_mutation", "none",
			)
		}
		if errors.Is(err, dnfquest.ErrAutoCompleteTargetAmbiguous) ||
			errors.Is(err, dnfquest.ErrAutoCompleteTargetInvalid) {
			return s.sendCurrentAutoCompleteMainQuestRejection(
				session,
				"game-auto-complete-main-quest-single-target-required",
				currentAutoCompleteNoTargetNoticeText,
				"auto_complete_main_quest_no_eligible_target",
				"request_selector", requestSelector,
				"target_quest_id", targetQuestID,
				"target_source", targetSource,
				"cutoff_level", cutoffLevel,
				"reason", err,
				"quest_mutation", "none",
			)
		}
		return s.sendCurrentAutoCompleteMainQuestUnavailable(session,
			"request_selector", requestSelector,
			"target_quest_id", targetQuestID,
			"target_source", targetSource,
			"cutoff_level", cutoffLevel,
			"reason", "pvf_db_atomic_selected_main_quest_completion_rejected",
			"error", err)
	}
	if result.HighestCompletedQuest < 0 || result.HighestCompletedQuest > int64(^uint32(0)) {
		return fmt.Errorf("auto-complete highest quest id out of wire range: %d", result.HighestCompletedQuest)
	}
	response := buildCurrentAutoCompleteMainQuestAck(true)
	s.logGameEvent(session, "game-auto-complete-main-quest-success-send",
		"request_selector", requestSelector,
		"target_quest_id", result.HighestCompletedQuest,
		"target_source", targetSource,
		"cutoff_level", cutoffLevel,
		"targeted_main_count", len(result.EligibleQuestIDs),
		"changed_main_count", len(result.ChangedQuestIDs),
		"changed_linked_sub_count", len(result.ChangedLinkedSubIDs),
		"active_completed_count", result.ActiveCompletedCount,
		"highest_completed_quest", result.HighestCompletedQuest,
		"idempotent", result.Idempotent,
		"msg_id", uint16(dnfenum.CmdPacketClearQuestTicket),
		"classification", dnfproto.DefaultChannelClassification,
		"body_len", len(response),
		"body_source", "current_exe_class1_status_envelope_sub_1D0F340_and_atomic_pvf_main_quest_completion_without_rewards")
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketClearQuestTicket),
		response,
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	if len(result.CarryForwardItems) > 0 {
		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.CarryForwardItems))
		for _, item := range result.CarryForwardItems {
			if item.SlotIndex > math.MaxInt16 {
				return fmt.Errorf("auto-complete carry-forward slot out of wire range: %d", item.SlotIndex)
			}
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
				ListType:  dnfrepo.MainInventoryListType,
				SlotIndex: int16(item.SlotIndex),
			})
		}
		if err := s.sendSelectedIncrementalItemSlotRefreshes(
			session,
			"auto_complete_main_carry_forward",
			refreshes,
		); err != nil {
			return err
		}
		s.logGameEvent(session, "game-auto-complete-main-quest-carry-forward-items-send",
			"request_selector", requestSelector,
			"item_count", len(result.CarryForwardItems),
			"source", "pvf_skipped_reward_required_by_outstanding_story_quest")
	}
	if err := s.sendCurrentClearQuestListFromCommittedQuest(
		session,
		result.PostCommitQuest,
		"current_exe_op1429_after_atomic_no_reward_main_quest_completion",
	); err != nil {
		return err
	}
	if err := s.sendCurrentActiveQuestSnapshotForSession(
		session,
		"current_exe_op1429_after_completed_quest_bitmap",
	); err != nil {
		return err
	}
	if err := s.sendCurrentAcceptableQuestListOnlyForSession(
		session,
		"current_exe_op1429_after_active_quest_snapshot",
	); err != nil {
		return err
	}
	// The bulk closure can finish a map [quest condition] or client-feature gate
	// quest inside its changed set even when that quest is not the highest one
	// completed. Rebuild the same-area town scene when ANY changed quest gates
	// the currently loaded map; an idempotent close changed nothing and needs no
	// rebuild.
	if result.Idempotent {
		return nil
	}
	return s.sendCurrentQuestGatedTownSceneRefreshForQuestIDs(
		session,
		currentAutoCompleteFinishCommitBridge(result, character),
		currentAutoCompleteChangedQuestIDs(result),
		"current_exe_op1429_after_atomic_no_reward_main_quest_completion",
	)
}

// currentAutoCompleteFinishCommitBridge adapts the op1429 bulk-completion
// receipt into the minimal committed finish shape consumed by the town scene
// refresh gate. Bulk completion never changes the character record, so the
// pre-applied character snapshot already carries the authoritative town
// location and area state for the scene check.
func currentAutoCompleteFinishCommitBridge(
	result dnfquest.AutoCompleteMainResult,
	character dnfrepo.CharacterRecord,
) dnfquest.FinishCommitResult {
	return dnfquest.FinishCommitResult{
		CharacterID:         result.CharacterID,
		QuestID:             result.HighestCompletedQuest,
		AtomicCommitted:     true,
		PostCommitCharacter: character,
		PostCommitQuest:     result.PostCommitQuest,
	}
}

// currentAutoCompleteChangedQuestIDs returns the deduplicated set of quests the
// op1429 close actually changed, including linked no-reward sub quests. The
// scene refresh gate scans this whole set so a [quest condition] belonging to a
// non-highest quest still triggers the same-area rebuild.
func currentAutoCompleteChangedQuestIDs(result dnfquest.AutoCompleteMainResult) []int64 {
	size := len(result.ChangedQuestIDs) + len(result.ChangedLinkedSubIDs)
	questIDs := make([]int64, 0, size+1)
	seen := make(map[int64]struct{}, size+1)
	appendOnce := func(questID int64) {
		if questID <= 0 {
			return
		}
		if _, duplicate := seen[questID]; duplicate {
			return
		}
		seen[questID] = struct{}{}
		questIDs = append(questIDs, questID)
	}
	for _, questID := range result.ChangedQuestIDs {
		appendOnce(questID)
	}
	for _, questID := range result.ChangedLinkedSubIDs {
		appendOnce(questID)
	}
	appendOnce(result.HighestCompletedQuest)
	return questIDs
}
