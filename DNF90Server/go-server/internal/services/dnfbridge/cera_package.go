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

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCeraPackageRequestHeaderSize = 3
	currentCeraPackageChoiceStride      = 5
	currentCeraPackageMaxChoices        = 32
	currentCeraPackageMaxRewardRows     = 128
	currentCeraPackageMaxRewardAmount   = 1_000_000
)

var (
	errCurrentCeraPackageRequestMalformed = errors.New("dnf cera package request is malformed")
	errCurrentCeraPackageOwnerUnavailable = errors.New("dnf cera package selected character is unavailable")
	errCurrentCeraPackageSourceMissing    = errors.New("dnf cera package source item is unavailable")
	errCurrentCeraPackagePVFInvalid       = errors.New("dnf cera package PVF data is invalid")
	errCurrentCeraPackageChoiceInvalid    = errors.New("dnf cera package selection is invalid")
	errCurrentCeraPackageExpired          = errors.New("dnf cera package has expired")
	// errCurrentCeraPackageDailyUseLimitReached：PVF `[total usable count]`（客户端
	// 悬浮说明里的「每天可用次数」）今天已经用满。礼包同样是消耗品，上限只由道具自己的
	// PVF 决定，所以这条路径用与 op44/op515/开箱完全相同的那份计数。
	errCurrentCeraPackageDailyUseLimitReached = errors.New("dnf cera package source item reached its daily usable count")
)

// currentCeraPackageOpenRequest is the exact current-EXE op518 body written by
// sub_1EC46D0: source slot, selected-avatar count, then item/option pairs.
// A zero count is valid for packages whose PVF rewards contain no avatars.
// Item quantities never come from the client; they are resolved from PVF.
type currentCeraPackageOpenRequest struct {
	SourceSlot int16
	Choices    []currentCeraPackageChoice
}

type currentCeraPackageChoice struct {
	ItemID uint32
	Option byte
}

type currentCeraPackageDefinition struct {
	Source          dungeonDropItemDefinition
	StackableType   string
	Rewards         []currentCeraPackageReward
	SelectionGroups [][]currentCeraPackageReward
	AvatarItemIDs   []uint32
}

type currentCeraPackageReward struct {
	Definition dungeonDropItemDefinition
	Count      uint32
	Avatar     bool
	Option     byte
}

type currentCeraPackageCommitResult struct {
	ChangedListTypes []byte
	RewardSlots      []alignedcmd.ItemSlotRefresh
	SourceRemaining  uint32
	DisplayRewards   []currentBoosterGrantedReward
	RewardRows       int
	RewardUnits      uint64
	OverflowMailID   string
	OverflowItems    int
	// SourceDailyUsed / SourceDailyLimit 是这次开启之后的「每天可用次数」进度
	// （PVF `[total usable count]`）。Limit<=0 表示该礼包没有声明上限。
	SourceDailyUsed  int64
	SourceDailyLimit int64
}

func parseCurrentCeraPackageOpenRequest(body []byte) (currentCeraPackageOpenRequest, error) {
	if len(body) < currentCeraPackageRequestHeaderSize {
		return currentCeraPackageOpenRequest{}, errCurrentCeraPackageRequestMalformed
	}
	count := int(body[2])
	if count > currentCeraPackageMaxChoices || len(body) != currentCeraPackageRequestHeaderSize+count*currentCeraPackageChoiceStride {
		return currentCeraPackageOpenRequest{}, errCurrentCeraPackageRequestMalformed
	}
	request := currentCeraPackageOpenRequest{
		SourceSlot: int16(binary.LittleEndian.Uint16(body[0:2])),
		Choices:    make([]currentCeraPackageChoice, 0, count),
	}
	seen := make(map[uint32]struct{}, count)
	for index := 0; index < count; index++ {
		offset := currentCeraPackageRequestHeaderSize + index*currentCeraPackageChoiceStride
		itemID := binary.LittleEndian.Uint32(body[offset : offset+4])
		if itemID == 0 {
			return currentCeraPackageOpenRequest{}, errCurrentCeraPackageRequestMalformed
		}
		if _, duplicate := seen[itemID]; duplicate {
			return currentCeraPackageOpenRequest{}, fmt.Errorf("%w: duplicate item=%d", errCurrentCeraPackageChoiceInvalid, itemID)
		}
		seen[itemID] = struct{}{}
		request.Choices = append(request.Choices, currentCeraPackageChoice{ItemID: itemID, Option: body[offset+4]})
	}
	return request, nil
}

func resolveCurrentCeraPackageDefinition(
	catalog *pvfDungeonDropCatalog,
	sourceItemID uint32,
) (currentCeraPackageDefinition, error) {
	if catalog == nil || catalog.source == nil || sourceItemID == 0 {
		return currentCeraPackageDefinition{}, errCurrentCeraPackagePVFInvalid
	}
	sourceDefinition, err := catalog.ResolveItem(sourceItemID)
	if err != nil {
		return currentCeraPackageDefinition{}, fmt.Errorf("%w: resolve source item=%d: %v", errCurrentCeraPackagePVFInvalid, sourceItemID, err)
	}
	definition, recognized, err := resolveCurrentCeraPackageDefinitionForSource(catalog, sourceDefinition)
	if err != nil {
		return currentCeraPackageDefinition{}, err
	}
	if !recognized {
		return currentCeraPackageDefinition{}, fmt.Errorf("%w: source item=%d stackable_type=%q", errCurrentCeraPackagePVFInvalid, sourceItemID, sourceDefinition.StackableType)
	}
	if err := validateCurrentCeraPackageRewardGraph(catalog, sourceItemID); err != nil {
		return currentCeraPackageDefinition{}, err
	}
	return definition, nil
}

// resolveCurrentCeraPackageDefinitionForSource is the single PVF parser shared
// by op64 checkout and inventory op518.  [cera package] and
// [usable cera package] have the same fixed and repeated-selection grammar;
// only their owning protocol differs.  Keeping the grammar here prevents the
// two paths from silently disagreeing about historical nested gift boxes.
func resolveCurrentCeraPackageDefinitionForSource(
	catalog *pvfDungeonDropCatalog,
	sourceDefinition dungeonDropItemDefinition,
) (currentCeraPackageDefinition, bool, error) {
	if catalog == nil || catalog.source == nil || sourceDefinition.ItemID == 0 {
		return currentCeraPackageDefinition{}, false, errCurrentCeraPackagePVFInvalid
	}
	if sourceDefinition.Kind != dungeonDropItemStackable {
		return currentCeraPackageDefinition{}, false, nil
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, sourceDefinition.PVFPath)
	if err != nil {
		return currentCeraPackageDefinition{}, false, fmt.Errorf("%w: read source item=%d: %v", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, err)
	}
	stackableType := normalizeDungeonDropStackableType(magicBoxDocumentText(document, "stackable type"))
	if stackableType != "[cera package]" && stackableType != "[usable cera package]" {
		return currentCeraPackageDefinition{}, false, nil
	}
	definition := currentCeraPackageDefinition{
		Source:        sourceDefinition,
		StackableType: stackableType,
	}
	avatarIDs := make(map[uint32]struct{})
	var total uint64
	rowCount := 0
	parseRows := func(values []int64, label string, collectFixedAvatars bool) ([]currentCeraPackageReward, error) {
		if len(values) == 0 || len(values)%2 != 0 {
			return nil, fmt.Errorf("%w: source item=%d %s_values=%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, label, len(values))
		}
		rowCount += len(values) / 2
		if rowCount > currentCeraPackageMaxRewardRows {
			return nil, fmt.Errorf("%w: source item=%d reward_rows=%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, rowCount)
		}
		rewards := make([]currentCeraPackageReward, 0, len(values)/2)
		seen := make(map[uint32]struct{}, len(values)/2)
		for offset := 0; offset < len(values); offset += 2 {
			itemValue, countValue := values[offset], values[offset+1]
			if itemValue <= 0 || itemValue > math.MaxUint32 || countValue <= 0 || countValue > currentCeraPackageMaxRewardAmount {
				return nil, fmt.Errorf("%w: source item=%d %s_pair=%d/%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, label, itemValue, countValue)
			}
			itemID := uint32(itemValue)
			if _, duplicate := seen[itemID]; duplicate && !collectFixedAvatars {
				return nil, fmt.Errorf("%w: source item=%d %s duplicate reward=%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, label, itemID)
			}
			seen[itemID] = struct{}{}
			total += uint64(countValue)
			if total > currentCeraPackageMaxRewardAmount {
				return nil, fmt.Errorf("%w: source item=%d declared_reward_total=%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, total)
			}
			rewardDefinition, resolveErr := catalog.ResolveItem(itemID)
			if resolveErr != nil {
				return nil, fmt.Errorf("%w: source item=%d reward item=%d: %v", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, itemValue, resolveErr)
			}
			avatar, avatarErr := currentCeraPackageRewardIsAvatar(catalog.source, rewardDefinition)
			if avatarErr != nil {
				return nil, fmt.Errorf("%w: source item=%d reward item=%d: %v", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, itemValue, avatarErr)
			}
			reward := currentCeraPackageReward{Definition: rewardDefinition, Count: uint32(countValue), Avatar: avatar}
			rewards = append(rewards, reward)
			if collectFixedAvatars && avatar {
				if _, exists := avatarIDs[itemID]; !exists {
					avatarIDs[itemID] = struct{}{}
					definition.AvatarItemIDs = append(definition.AvatarItemIDs, itemID)
				}
			}
		}
		return rewards, nil
	}
	if values := document.Ints("package data"); len(values) > 0 {
		definition.Rewards, err = parseRows(values, "package_data", true)
		if err != nil {
			return currentCeraPackageDefinition{}, true, err
		}
	}
	selectionItems := make(map[uint32]int)
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "package data selection") {
			continue
		}
		group, groupErr := parseRows(magicBoxSectionInts(document, section), "package_data_selection", false)
		if groupErr != nil {
			return currentCeraPackageDefinition{}, true, groupErr
		}
		groupIndex := len(definition.SelectionGroups)
		for _, reward := range group {
			if previous, duplicate := selectionItems[reward.Definition.ItemID]; duplicate {
				return currentCeraPackageDefinition{}, true, fmt.Errorf(
					"%w: source item=%d selection reward=%d appears in groups=%d/%d",
					errCurrentCeraPackagePVFInvalid,
					sourceDefinition.ItemID,
					reward.Definition.ItemID,
					previous,
					groupIndex,
				)
			}
			selectionItems[reward.Definition.ItemID] = groupIndex
		}
		definition.SelectionGroups = append(definition.SelectionGroups, group)
	}
	if len(definition.Rewards) == 0 && len(definition.SelectionGroups) == 0 {
		return currentCeraPackageDefinition{}, true, fmt.Errorf("%w: source item=%d has no rewards", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID)
	}
	if len(definition.AvatarItemIDs) > currentCeraPackageMaxChoices {
		return currentCeraPackageDefinition{}, true, fmt.Errorf("%w: source item=%d avatar_choices=%d", errCurrentCeraPackagePVFInvalid, sourceDefinition.ItemID, len(definition.AvatarItemIDs))
	}
	return definition, true, nil
}

func currentCeraPackageRewardIsAvatar(source dnfpvf.Source, definition dungeonDropItemDefinition) (bool, error) {
	if definition.Kind != dungeonDropItemEquipment {
		return false, nil
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return false, err
	}
	equipmentType, found := document.Text("equipment type")
	if !found {
		return false, nil
	}
	rule, supported := currentEquipmentPlacementRuleForPVFType(equipmentType)
	return supported && rule.class == currentEquipmentPlacementClassAvatar, nil
}

func validateCurrentCeraPackageChoices(
	definition currentCeraPackageDefinition,
	choices []currentCeraPackageChoice,
) (map[uint32]byte, error) {
	if len(choices) != len(definition.AvatarItemIDs) {
		return nil, fmt.Errorf("%w: got=%d want=%d", errCurrentCeraPackageChoiceInvalid, len(choices), len(definition.AvatarItemIDs))
	}
	expected := make(map[uint32]struct{}, len(definition.AvatarItemIDs))
	for _, itemID := range definition.AvatarItemIDs {
		expected[itemID] = struct{}{}
	}
	options := make(map[uint32]byte, len(choices))
	for _, choice := range choices {
		if _, ok := expected[choice.ItemID]; !ok {
			return nil, fmt.Errorf("%w: item=%d is not an avatar reward", errCurrentCeraPackageChoiceInvalid, choice.ItemID)
		}
		if _, duplicate := options[choice.ItemID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate item=%d", errCurrentCeraPackageChoiceInvalid, choice.ItemID)
		}
		options[choice.ItemID] = choice.Option
	}
	for itemID := range expected {
		if _, ok := options[itemID]; !ok {
			return nil, fmt.Errorf("%w: avatar item=%d was not selected", errCurrentCeraPackageChoiceInvalid, itemID)
		}
	}
	return options, nil
}

// resolveCurrentCeraPackageOpenRewards applies the op518 item/option pairs to
// the shared package definition.  A pair can serve two roles at once: select
// one row from a repeated [package data selection] group and carry the avatar
// option for that selected reward.  Singleton groups are deterministic and may
// be omitted by the client; every group with alternatives requires one explicit
// item choice.  No unconsumed choice is tolerated.
func resolveCurrentCeraPackageOpenRewards(
	definition currentCeraPackageDefinition,
	choices []currentCeraPackageChoice,
) ([]currentCeraPackageReward, error) {
	choiceByItem := make(map[uint32]currentCeraPackageChoice, len(choices))
	for _, choice := range choices {
		if choice.ItemID == 0 {
			return nil, errCurrentCeraPackageChoiceInvalid
		}
		if _, duplicate := choiceByItem[choice.ItemID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate item=%d", errCurrentCeraPackageChoiceInvalid, choice.ItemID)
		}
		choiceByItem[choice.ItemID] = choice
	}

	used := make(map[uint32]struct{}, len(choices))
	rewards := append([]currentCeraPackageReward(nil), definition.Rewards...)
	for groupIndex, group := range definition.SelectionGroups {
		if len(group) == 0 {
			return nil, fmt.Errorf("%w: selection group=%d is empty", errCurrentCeraPackagePVFInvalid, groupIndex)
		}
		selectedIndex := -1
		for index, candidate := range group {
			if _, selected := choiceByItem[candidate.Definition.ItemID]; !selected {
				continue
			}
			if selectedIndex >= 0 {
				return nil, fmt.Errorf("%w: selection group=%d has multiple choices", errCurrentCeraPackageChoiceInvalid, groupIndex)
			}
			selectedIndex = index
		}
		if selectedIndex < 0 {
			if len(group) != 1 {
				return nil, fmt.Errorf("%w: selection group=%d has no explicit choice", errCurrentCeraPackageChoiceInvalid, groupIndex)
			}
			selectedIndex = 0
		}
		selected := group[selectedIndex]
		if choice, explicit := choiceByItem[selected.Definition.ItemID]; explicit {
			used[selected.Definition.ItemID] = struct{}{}
			if selected.Avatar {
				selected.Option = choice.Option
			} else if choice.Option != 0 {
				return nil, fmt.Errorf("%w: non-avatar selection item=%d option=%d", errCurrentCeraPackageChoiceInvalid, choice.ItemID, choice.Option)
			}
		}
		rewards = append(rewards, selected)
	}

	avatarItems := make(map[uint32]struct{})
	for index := range rewards {
		if !rewards[index].Avatar {
			continue
		}
		itemID := rewards[index].Definition.ItemID
		choice, selected := choiceByItem[itemID]
		if !selected {
			return nil, fmt.Errorf("%w: avatar item=%d was not selected", errCurrentCeraPackageChoiceInvalid, itemID)
		}
		rewards[index].Option = choice.Option
		used[itemID] = struct{}{}
		avatarItems[itemID] = struct{}{}
	}
	if len(used) != len(choiceByItem) {
		for itemID := range choiceByItem {
			if _, ok := used[itemID]; !ok {
				return nil, fmt.Errorf("%w: item=%d is not required by the selected package rewards", errCurrentCeraPackageChoiceInvalid, itemID)
			}
		}
	}
	if len(avatarItems) > currentCeraPackageMaxChoices || len(rewards) > currentCeraPackageMaxRewardRows {
		return nil, errCurrentCeraPackagePVFInvalid
	}
	var total uint64
	for _, reward := range rewards {
		total += uint64(reward.Count)
		if total > currentCeraPackageMaxRewardAmount {
			return nil, fmt.Errorf("%w: selected reward total=%d", errCurrentCeraPackagePVFInvalid, total)
		}
	}
	return rewards, nil
}

func validateCurrentCeraPackageSourceExpiration(
	stack dnfrepo.ItemStack,
	source dungeonDropItemDefinition,
	now time.Time,
) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if expire := currentItemListStackExpire(stack); expire != 0 {
		if uint64(expire) <= uint64(now.Unix()) {
			return errCurrentCeraPackageExpired
		}
		return nil
	}
	if source.UsablePeriodDays > 0 {
		return errCurrentCeraPackageExpired
	}
	if !source.ExpirationDate.IsZero() && !now.Before(source.ExpirationDate) {
		return errCurrentCeraPackageExpired
	}
	return nil
}

func (s *Service) handleCurrentCeraPackageOpen(session *gameSession, body []byte) error {
	request, err := parseCurrentCeraPackageOpenRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-cera-package-open-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketOpenCerapackage), 4)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logGameEvent(session, "game-cera-package-open-rejected", "source_slot", request.SourceSlot, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketOpenCerapackage), 4)
	}
	definition, err := s.prepareCurrentCeraPackage(ctx, session, catalog, request)
	if err != nil {
		s.logGameEvent(session, "game-cera-package-open-rejected", "source_slot", request.SourceSlot, "choice_count", len(request.Choices), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketOpenCerapackage), 4)
	}
	result, err := s.commitCurrentCeraPackage(ctx, session, catalog, definition, request)
	if err != nil {
		s.logGameEvent(session, "game-cera-package-open-rejected", "source_slot", request.SourceSlot, "source_item", definition.Source.ItemID, "choice_count", len(request.Choices), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketOpenCerapackage), 4)
	}
	var success packetWriter
	success.writeUint16(uint16(request.SourceSlot))
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketOpenCerapackage), success.bytes()); err != nil {
		return err
	}
	rewardNotice := buildCurrentBoosterSuccessBody(currentBoosterCommitResult{
		SourceItemID:    definition.Source.ItemID,
		SourceSlot:      request.SourceSlot,
		SourceRemaining: result.SourceRemaining,
		Rewards:         result.DisplayRewards,
	})
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketUseBoosterItem), rewardNotice); err != nil {
		return err
	}
	for _, listType := range result.ChangedListTypes {
		listBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, listType)
		if !ok {
			return errCurrentCeraPackageOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), listBody); err != nil {
			return err
		}
	}
	s.scheduleCurrentRewardSlotHighlights(session, "open_cera_package", result.RewardSlots)
	if result.OverflowMailID != "" {
		if err := s.sendMailboxAlarmToOnlineRecipient(session.selectedCharacterID); err != nil {
			// The package, its directly placed rewards, and the overflow mail
			// have already committed together. A missed live alarm is harmless:
			// the durable mailbox snapshot will still show the attachment when
			// the player next opens the mailbox.
			s.logWarn("cera package mailbox alarm deferred to next mailbox open",
				"character_id", session.selectedCharacterID,
				"mail_id", result.OverflowMailID,
				"error", err)
		}
	}
	s.logGameEvent(session, "game-cera-package-open-committed",
		"source_slot", request.SourceSlot,
		"source_item", definition.Source.ItemID,
		"choice_count", len(request.Choices),
		"source_remaining", result.SourceRemaining,
		"reward_rows", result.RewardRows,
		"reward_units", result.RewardUnits,
		"overflow_mail_id", result.OverflowMailID,
		"overflow_items", result.OverflowItems,
		"changed_lists", fmt.Sprint(result.ChangedListTypes),
		"ack_body", "current_exe_op518_success_u16_slot_then_op160_reward_result",
		"inventory_refresh", "current_exe_op13_real_repository_lists")
	// Same reason as op160: the client's lock icons live in its op251 lock list,
	// not in the item rows, so the container rebuild after this package drops them
	// until the list is replayed (live 2026-09-15 07:38).
	if err := s.sendCurrentSelectedItemLockSnapshotIfAny(session, "open_cera_package_after_reward_grant"); err != nil {
		s.logGameEvent(session, "game-cera-package-lock-snapshot-send-failed", "error", err)
	}
	// Same for the blue sort-lock markers (see sendCurrentSortLockReplayIfAny).
	if err := s.sendCurrentSortLockReplayIfAny(session, "open_cera_package_after_reward_grant"); err != nil {
		s.logGameEvent(session, "game-cera-package-sort-lock-replay-failed", "error", err)
	}
	// 每天可用次数（PVF [total usable count]）：礼包与开箱同理 —— 不走 aligned
	// 通道，必须自己补一包 op541，否则客户端那份计数永远是 0。
	s.sendCurrentItemDailyUseState(
		session, definition.Source.ItemID, result.SourceDailyUsed, result.SourceDailyLimit, "open_cera_package")
	return nil
}

func (s *Service) prepareCurrentCeraPackage(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentCeraPackageOpenRequest,
) (currentCeraPackageDefinition, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || request.SourceSlot < 0 || catalog == nil {
		return currentCeraPackageDefinition{}, errCurrentCeraPackageOwnerUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return currentCeraPackageDefinition{}, errCurrentCeraPackageOwnerUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return currentCeraPackageDefinition{}, err
	}
	if !found || inventory.CharacterID != characterID {
		return currentCeraPackageDefinition{}, errCurrentCeraPackageOwnerUnavailable
	}
	stack, found := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.SourceSlot)]
	if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 {
		return currentCeraPackageDefinition{}, errCurrentCeraPackageSourceMissing
	}
	definition, err := resolveCurrentCeraPackageDefinition(catalog, uint32(stack.ItemID))
	if err != nil {
		return currentCeraPackageDefinition{}, err
	}
	now := time.Now().UTC()
	if err := validateCurrentCeraPackageSourceExpiration(stack, definition.Source, now); err != nil {
		return currentCeraPackageDefinition{}, err
	}
	selectedRewards, err := resolveCurrentCeraPackageOpenRewards(definition, request.Choices)
	if err != nil {
		return currentCeraPackageDefinition{}, err
	}
	if _, err := expandCurrentDeterministicNestedCeraPackages(catalog, selectedRewards); err != nil {
		return currentCeraPackageDefinition{}, err
	}
	return definition, nil
}

func (s *Service) commitCurrentCeraPackage(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	definition currentCeraPackageDefinition,
	request currentCeraPackageOpenRequest,
) (currentCeraPackageCommitResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.source == nil || definition.Source.ItemID == 0 {
		return currentCeraPackageCommitResult{}, errCurrentCeraPackageOwnerUnavailable
	}
	selectedRewards, err := resolveCurrentCeraPackageOpenRewards(definition, request.Choices)
	if err != nil {
		return currentCeraPackageCommitResult{}, err
	}
	selectedRewards, err = expandCurrentDeterministicNestedCeraPackages(catalog, selectedRewards)
	if err != nil {
		return currentCeraPackageCommitResult{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return currentCeraPackageCommitResult{}, errCurrentCeraPackageOwnerUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	now := time.Now().UTC()
	mainSlotCount := uint16(0)
	if repositories.Settings != nil {
		containerState := s.loadCurrentItemListContainerState(ctx, repositories.Settings, characterID, dnfrepo.MainInventoryListType)
		mainSlotCount = containerState.MainSlotCount
	}
	result := currentCeraPackageCommitResult{
		DisplayRewards: make([]currentBoosterGrantedReward, 0, len(selectedRewards)),
	}
	for _, reward := range selectedRewards {
		result.DisplayRewards = append(result.DisplayRewards, currentBoosterGrantedReward{
			ItemID: reward.Definition.ItemID,
			Count:  reward.Count,
		})
	}
	err = repositories.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
		_ dnfrepo.AccountRepository,
		accountInventories dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != characterID || accountID == "" || strings.TrimSpace(character.AccountID) != accountID {
			return errCurrentCeraPackageOwnerUnavailable
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || inventory.CharacterID != characterID {
			return errCurrentCeraPackageOwnerUnavailable
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			return errCurrentCeraPackageSourceMissing
		}
		// 礼包奖励里的无色/灵魂必须落账号共享仓（下面调用统一入口）。
		// accountID 已在上面校验过非空且与角色记录一致。
		accountInventory := dnfrepo.AccountInventoryRecord{}
		accountInventoryReady := false
		accountDirty := false
		loadAccountInventory := func() error {
			if accountInventoryReady || accountInventories == nil {
				return nil
			}
			record, _, loadErr := accountInventories.Load(ctx, accountID)
			if loadErr != nil {
				return loadErr
			}
			accountInventory = dnfrepo.CloneAccountInventory(record)
			accountInventory.AccountID = accountID
			if accountInventory.Slots == nil {
				accountInventory.Slots = make(map[string]dnfrepo.ItemStack)
			}
			accountInventoryReady = true
			return nil
		}
		sourceKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.SourceSlot)
		sourceStack, found := inventory.Slots[sourceKey]
		if !found || sourceStack.ItemID != int64(definition.Source.ItemID) || sourceStack.Count <= 0 {
			return errCurrentCeraPackageSourceMissing
		}
		if !definition.Source.ExpirationDate.IsZero() {
			sourceStack, _ = applyCurrentPVFItemExpirationAt(sourceStack, definition.Source, now)
		}
		if err := validateCurrentCeraPackageSourceExpiration(sourceStack, definition.Source, now); err != nil {
			return err
		}
		// 每天可用次数（PVF `[total usable count]`）：礼包同样是消耗品，上限属于道具
		// 自身，所以这里用的是与 op44/op515/开箱完全相同的那份计数。判定与自增都在
		// **这个事务**里：到上限就整笔失败、礼包不会被消耗；成功则与背包同事务落库。
		if dailyLimit := definition.Source.TotalUsableCount; dailyLimit > 0 {
			character = dnfrepo.CloneCharacter(character)
			if character.Stats == nil {
				character.Stats = make(map[string]int64)
			}
			used, chargeErr := dnfitemdailylimit.Consume(
				character.Stats, int64(definition.Source.ItemID), dailyLimit, now, character.Level)
			if chargeErr != nil {
				return fmt.Errorf(
					"%w: item=%d: %v",
					errCurrentCeraPackageDailyUseLimitReached, definition.Source.ItemID, chargeErr)
			}
			character.UpdatedAt = now
			if saveErr := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); saveErr != nil {
				return saveErr
			}
			result.SourceDailyUsed = used
			result.SourceDailyLimit = dailyLimit
		}

		changed := map[byte]struct{}{dnfrepo.MainInventoryListType: {}}
		rewardSlots := make(map[string]struct{})
		// 落到账号共享槽（0:354..365）的奖励行单独收集：它们不在 inventory 里，
		// 不能混进下面那个"必须在 inventory 里找得到"的盖章循环。
		accountRewardSlots := make(map[string]struct{})
		overflow := make([]dnfrepo.MailAttachment, 0)
		result.SourceRemaining = sceneInventoryUint32FromInt64(sourceStack.Count - 1)
		if sourceStack.Count == 1 {
			delete(inventory.Slots, sourceKey)
		} else {
			sourceStack.Count--
			entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.SourceSlot, sourceStack)
			sourceStack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[sourceKey] = sourceStack
		}

		var rewardUnits uint64
		for _, reward := range selectedRewards {
			rewardUnits += uint64(reward.Count)
			grantDefinition, expirationErr := currentPVFItemDefinitionForNestedRewardGrantAt(reward.Definition, sourceStack, now)
			if expirationErr != nil {
				return fmt.Errorf("%w: reward item=%d expiration: %v", errCurrentCeraPackagePVFInvalid, reward.Definition.ItemID, expirationErr)
			}
			grantDefinition = limitCurrentMainInventoryDefinitionSlots(grantDefinition, mainSlotCount)
			if reward.Avatar {
				for count := uint32(0); count < reward.Count; count++ {
					slot, grantErr := grantCurrentCeraShopAvatar(
						&inventory,
						catalog.source,
						grantDefinition,
						currentCeraShopProduct{ItemID: grantDefinition.ItemID, Count: 1, Section: "avatar"},
						reward.Option,
					)
					if grantErr != nil {
						return grantErr
					}
					key := currentCeraShopInventorySlotKey(1, slot)
					rewardSlots[key] = struct{}{}
				}
				changed[1] = struct{}{}
				continue
			}
			if isCurrentCeraShopCreatureItem(grantDefinition) {
				for count := uint32(0); count < reward.Count; count++ {
					slot, grantErr := grantCurrentCeraShopPet(&inventory, grantDefinition)
					if grantErr != nil {
						return grantErr
					}
					rewardSlots[currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)] = struct{}{}
				}
				changed[currentPetInventoryListType] = struct{}{}
				continue
			}
			if isCurrentCeraShopPetConsumable(grantDefinition) {
				slots, grantErr := grantCurrentCeraShopPetConsumable(&inventory, grantDefinition, reward.Count)
				if grantErr != nil {
					return grantErr
				}
				for _, slot := range slots {
					rewardSlots[currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)] = struct{}{}
				}
				changed[currentPetInventoryListType] = struct{}{}
				continue
			}
			// 无色小晶块 / 灵魂 这两族礼包奖励必须先落账号共享仓的固定格
			// （统一入口见 account_shared_grant.go）。账号侧是"并进固定格"，
			// 不需要下面的 candidate 探针；余量才交给角色侧那条探针/邮件溢出。
			if err := loadAccountInventory(); err != nil {
				return err
			}
			if accountInventoryReady {
				shared := grantCurrentAccountSharedFirst(&accountInventory, grantDefinition, reward.Count, "cera_package")
				if shared.Changed {
					accountDirty = true
					accountKey := dnfrepo.AccountSharedInventorySlotKey(shared.Slot)
					accountStack := accountInventory.Slots[accountKey]
					if accountStack.Extra == nil {
						accountStack.Extra = make(map[string]string, 5)
					}
					accountStack.Extra["source"] = "cera_package"
					accountStack.Extra["last_grant_source"] = "cera_package"
					accountStack.Extra["cera_package_source_item"] = strconv.FormatUint(uint64(definition.Source.ItemID), 10)
					accountInventory.Slots[accountKey] = accountStack
					// 账号共享槽也是 list-0 线地址：让客户端把晶体/灵魂面板那一格重画。
					accountRewardSlots[accountKey] = struct{}{}
				}
				if shared.Remainder == 0 {
					continue
				}
				// 只剩余量要落角色背包（reward.Count 是循环变量副本，改它只影响下面这几行）。
				reward.Count = shared.Remainder
			}
			// Product placement can merge part of a stack before discovering a
			// full category. Probe a clone first, so the fallback either grants
			// the whole reward directly or sends the whole row to mail, never a
			// duplicated or half-consumed remainder.
			candidate := dnfrepo.CloneInventory(inventory)
			slots, grantErr := grantCurrentCeraShopProduct(&candidate, grantDefinition, reward.Count, s.currentInventoryPlacementMainSlotCount(ctx, &candidate, ""))
			if grantErr != nil {
				if !errors.Is(grantErr, errDungeonPickupInventoryFull) {
					return grantErr
				}
				attachments, attachmentErr := currentCeraPackageOverflowAttachments(
					grantDefinition,
					reward.Count,
					definition.Source.ItemID,
				)
				if attachmentErr != nil {
					return attachmentErr
				}
				overflow = append(overflow, attachments...)
				continue
			}
			inventory = candidate
			for _, slot := range slots {
				rewardSlots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, int16(slot))] = struct{}{}
			}
		}

		for key := range rewardSlots {
			stack, found := inventory.Slots[key]
			if !found || stack.ItemID <= 0 {
				return errCurrentCeraPackagePVFInvalid
			}
			if stack.Extra == nil {
				stack.Extra = make(map[string]string, 5)
			}
			stack.Extra["source"] = "cera_package"
			stack.Extra["last_grant_source"] = "cera_package"
			stack.Extra["cera_package_source_item"] = strconv.FormatUint(uint64(definition.Source.ItemID), 10)
			listType, slot, parsed := parseSceneInventorySlotKey(key)
			if !parsed {
				return errCurrentCeraPackagePVFInvalid
			}
			entry := currentItemListEntryFromStack(listType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if accountDirty {
			accountInventory.UpdatedAt = now
			if err := accountInventories.Save(ctx, accountInventory); err != nil {
				return err
			}
		}
		if len(overflow) > 0 {
			mailID, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID,
				Title:                "背包已满：礼包奖励",
				Body:                 "背包空间不足，礼包中未能放入背包的奖励已通过邮件发送。请清理对应道具分页后领取。",
				Source:               "cera_package_main_inventory_full",
				Attachments:          overflow,
				CreatedAt:            now,
			})
			if mailErr != nil {
				return mailErr
			}
			result.OverflowMailID = mailID
			result.OverflowItems = len(overflow)
		}

		listTypes := make([]byte, 0, len(changed))
		for listType := range changed {
			listTypes = append(listTypes, listType)
		}
		sort.Slice(listTypes, func(left, right int) bool { return listTypes[left] < listTypes[right] })
		rewardRefreshes, refreshErr := currentRewardSlotRefreshesFromKeys(rewardSlots)
		if refreshErr != nil {
			return fmt.Errorf("%w: %v", errCurrentCeraPackagePVFInvalid, refreshErr)
		}
		if len(accountRewardSlots) > 0 {
			accountRefreshes, accountRefreshErr := currentRewardSlotRefreshesFromKeys(accountRewardSlots)
			if accountRefreshErr != nil {
				return fmt.Errorf("%w: %v", errCurrentCeraPackagePVFInvalid, accountRefreshErr)
			}
			rewardRefreshes = append(rewardRefreshes, accountRefreshes...)
		}
		result = currentCeraPackageCommitResult{
			ChangedListTypes: listTypes,
			RewardSlots:      rewardRefreshes,
			SourceRemaining:  result.SourceRemaining,
			DisplayRewards:   result.DisplayRewards,
			RewardRows:       len(selectedRewards),
			RewardUnits:      rewardUnits,
			OverflowMailID:   result.OverflowMailID,
			OverflowItems:    result.OverflowItems,
			// 这次整体重写结果结构体，每日次数的进度必须一起带过来，否则上面刚扣的
			// 那一次在返回时就丢了（也就不会投影给客户端）。
			SourceDailyUsed:  result.SourceDailyUsed,
			SourceDailyLimit: result.SourceDailyLimit,
		}
		return nil
	})
	if err != nil {
		return currentCeraPackageCommitResult{}, err
	}
	return result, nil
}

// currentCeraPackageOverflowAttachments makes mail-owned instances for a
// package row that cannot fit its PVF inventory category. The item instances
// retain the same expiration and PVF metadata used by the direct placement
// path, so claiming them later is equivalent to receiving them immediately.
func currentCeraPackageOverflowAttachments(
	definition dungeonDropItemDefinition,
	count uint32,
	sourceItemID uint32,
) ([]dnfrepo.MailAttachment, error) {
	if definition.ItemID == 0 || count == 0 || sourceItemID == 0 {
		return nil, errCurrentCeraPackagePVFInvalid
	}
	attachments := make([]dnfrepo.MailAttachment, 0, 1)
	remaining := count
	for remaining > 0 {
		chunk := remaining
		switch definition.Kind {
		case dungeonDropItemEquipment:
			chunk = 1
		case dungeonDropItemStackable:
			if definition.StackLimit > 0 && int64(chunk) > definition.StackLimit {
				chunk = uint32(definition.StackLimit)
			}
		default:
			return nil, errCurrentCeraShopProductUnavailable
		}
		// The mailbox wire builder replaces the location field. Build with a
		// valid PVF category slot solely to obtain the exact item-instance
		// payload and expiration fields.
		stack, err := buildCurrentDungeonPickupStack(definition.SlotStart, definition, chunk)
		if err != nil {
			return nil, err
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 4)
		}
		stack.Extra["source"] = "cera_package"
		stack.Extra["last_grant_source"] = "cera_package"
		stack.Extra["cera_package_source_item"] = strconv.FormatUint(uint64(sourceItemID), 10)
		stack.Extra["cera_package_overflow_mail"] = "true"
		attachments = append(attachments, dnfrepo.MailAttachment{
			ItemID:   stack.ItemID,
			Count:    stack.Count,
			Bind:     stack.Bind,
			ExpireAt: stack.ExpireAt,
			RawEntry: append([]byte(nil), stack.RawEntry...),
			Extra:    stack.Extra,
		})
		remaining -= chunk
	}
	return attachments, nil
}
