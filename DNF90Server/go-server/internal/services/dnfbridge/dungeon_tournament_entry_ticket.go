package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"sort"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

var (
	errDungeonEntryItemMissing         = errors.New("dungeon entry item is missing")
	errDungeonEntryItemRestoreConflict = errors.New("dungeon entry item restore conflicts with current inventory")
)

// currentDungeonEntryItemShortage is the structured form of
// errDungeonEntryItemMissing. The current client shows nothing for select
// failure reason 17, so the shortage has to be carried out to a visible notice
// with the exact item and counts; errors.Is on errDungeonEntryItemMissing keeps
// working for every existing caller.
type currentDungeonEntryItemShortage struct {
	CharacterID uint16
	ItemID      int64
	Required    int64
	Available   int64
}

func (e currentDungeonEntryItemShortage) Error() string {
	return fmt.Sprintf("%v: character=%d item=%d need=%d available=%d",
		errDungeonEntryItemMissing, e.CharacterID, e.ItemID, e.Required, e.Available)
}

func (e currentDungeonEntryItemShortage) Unwrap() error {
	return errDungeonEntryItemMissing
}

type currentDungeonEntryAdmissionPlan struct {
	FreePasses              []worldmap.TicketItem
	Requirements            []worldmap.TicketItem
	RequirementAlternatives []worldmap.TicketItem
	Source                  string
	// AdmissionCostSource names where the requirement count came from
	// (PVF declaration vs the client's level formula); diagnostics only.
	AdmissionCostSource string
}

type currentTowerEntryItemDefinition struct {
	ItemID        int64
	PVFPath       string
	StackableType string
}

// The current PVF authors one canonical [required item] in every layer, while
// older event/shop versions remain distinct inventory item IDs. Only direct
// [material]/[quest] invitations are alternatives here. Invitation boxes and
// [waste] items with [up layer despair dungeon] keep their separate use-item
// owner and must not be consumed again by dungeon admission.
var currentTowerOfDespairEntryItemDefinitions = []currentTowerEntryItemDefinition{
	{ItemID: 4473, PVFPath: "stackable/invitation_drapyus2.stk", StackableType: "[material]"},
	{ItemID: 490001700, PVFPath: "stackable/490001001/chn_490001700.stk", StackableType: "[material]"},
	{ItemID: 490001875, PVFPath: "stackable/490001001/chn_490001875.stk", StackableType: "[material]"},
	{ItemID: 490004423, PVFPath: "stackable/490004001/chn_490004423.stk", StackableType: "[material]"},
	{ItemID: 490004952, PVFPath: "stackable/490004001/chn_490004952.stk", StackableType: "[material]"},
	{ItemID: 490002380, PVFPath: "stackable/490002001/chn_490002380.stk", StackableType: "[quest]"},
	{ItemID: 10004955, PVFPath: "stackable/event/chn_event_20150326_simona_event/chn_simona_10004955.stk", StackableType: "[quest]"},
	{ItemID: 10005828, PVFPath: "stackable/event/chn_event_20150716_towerofhope/chn_invitation_10005828.stk", StackableType: "[quest]"},
	{ItemID: 10006020, PVFPath: "stackable/event/chn_event_20150813_sheepfarm/chn_invitation_10006020.stk", StackableType: "[quest]"},
}

var currentTowerOfGriefEntryItemDefinitions = []currentTowerEntryItemDefinition{
	{ItemID: 10161065, PVFPath: "stackable/10161001/10161065.stk", StackableType: "[material]"},
}

type currentDungeonEntryInventoryCommit struct {
	session  *gameSession
	updates  []currentItemListEntry
	restore  func() error
	restored bool
}

// currentDungeonEntryInventoryBatch is the admission boundary shared by every
// member of a party. Each character-owned inventory is persisted before any
// runtime is committed; if a later member cannot pay, or a runtime commit
// fails, the already persisted deductions are compensated in reverse order.
//
// The repositories do not expose a cross-character transaction. Each member
// therefore retains only the exact consumed-slot before/post-images. A later
// compensation can restore those deltas without replacing unrelated inventory
// changes that committed after this member left its session mailbox.
type currentDungeonEntryInventoryBatch struct {
	commits []*currentDungeonEntryInventoryCommit
}

type currentDungeonEntryInventorySlotMutation struct {
	key          string
	before       dnfrepo.ItemStack
	beforeExists bool
	after        dnfrepo.ItemStack
	afterExists  bool
}

func (batch *currentDungeonEntryInventoryBatch) updatesFor(session *gameSession) []currentItemListEntry {
	if batch == nil || session == nil {
		return nil
	}
	for _, commit := range batch.commits {
		if commit != nil && commit.session == session {
			return append([]currentItemListEntry(nil), commit.updates...)
		}
	}
	return nil
}

func (s *Service) restoreCurrentDungeonEntryInventoryBatch(
	batch *currentDungeonEntryInventoryBatch,
) error {
	if batch == nil {
		return nil
	}
	var restoreErr error
	for index := len(batch.commits) - 1; index >= 0; index-- {
		commit := batch.commits[index]
		if commit == nil || commit.restore == nil || commit.restored {
			continue
		}
		if err := commit.restore(); err != nil {
			characterID := uint16(0)
			if commit.session != nil {
				characterID = commit.session.selectedCharacterID
				s.logGameEvent(commit.session, "game-dungeon-entry-item-restore-failed",
					"char_id", characterID,
					"error", err)
			}
			restoreErr = errors.Join(restoreErr, fmt.Errorf(
				"restore dungeon admission inventory for character %d: %w", characterID, err))
			continue
		}
		commit.restored = true
	}
	return restoreErr
}

func (s *Service) consumeCurrentDungeonEntryPartyItems(
	ctx context.Context,
	leader *gameSession,
	leaderRuntime *runtimeDungeonState,
	preparedFollowers []*runtimePartyPreparedDungeonEntry,
) (*currentDungeonEntryInventoryBatch, error) {
	batch := &currentDungeonEntryInventoryBatch{
		commits: make([]*currentDungeonEntryInventoryCommit, 0, len(preparedFollowers)+1),
	}
	consume := func(session *gameSession, runtime *runtimeDungeonState) error {
		if session == nil || runtime == nil || currentDungeonRuntimeIsPractice(runtime) {
			return nil
		}
		updates, restore, err := s.consumeCurrentDungeonEntryItems(ctx, session, runtime)
		if err != nil {
			return err
		}
		if restore == nil {
			return nil
		}
		batch.commits = append(batch.commits, &currentDungeonEntryInventoryCommit{
			session: session,
			updates: append([]currentItemListEntry(nil), updates...),
			restore: restore,
		})
		return nil
	}
	if err := consume(leader, leaderRuntime); err != nil {
		return nil, err
	}
	for _, prepared := range preparedFollowers {
		if prepared == nil || prepared.session == nil || prepared.runtime == nil {
			err := errors.New("prepared party member admission is incomplete")
			if restoreErr := s.restoreCurrentDungeonEntryInventoryBatch(batch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			return nil, err
		}
		var consumeErr error
		err := s.callGameSession(ctx, prepared.session, "runtime-party-dungeon-admission-inventory", func() error {
			consumeErr = consume(prepared.session, prepared.runtime)
			return consumeErr
		})
		if err != nil {
			if consumeErr == nil {
				consumeErr = err
			} else if !errors.Is(err, consumeErr) {
				consumeErr = errors.Join(consumeErr, err)
			}
			if restoreErr := s.restoreCurrentDungeonEntryInventoryBatch(batch); restoreErr != nil {
				consumeErr = errors.Join(consumeErr, restoreErr)
			}
			return nil, fmt.Errorf(
				"consume party member %d dungeon admission: %w",
				prepared.session.selectedCharacterID,
				consumeErr,
			)
		}
	}
	for _, prepared := range preparedFollowers {
		prepared.entryItemUpdates = batch.updatesFor(prepared.session)
	}
	return batch, nil
}

func (s *Service) resolveCurrentDungeonEntryAdmissionPlan(
	runtime *runtimeDungeonState,
) (currentDungeonEntryAdmissionPlan, error) {
	if runtime == nil {
		return currentDungeonEntryAdmissionPlan{}, nil
	}
	if len(runtime.Dungeon.Metadata.EventRequiredItems) != 0 || len(runtime.Dungeon.Metadata.AddedRequiredItems) != 0 {
		return currentDungeonEntryAdmissionPlan{}, fmt.Errorf(
			"dungeon %d has event/schedule admission items without a proven current op16 selector",
			runtime.Dungeon.ID,
		)
	}
	plan := currentDungeonEntryAdmissionPlan{
		Requirements: append([]worldmap.TicketItem(nil), runtime.Dungeon.Metadata.RequiredItems...),
		Source:       "dungeon_required_item",
	}
	if alternatives, source, err := s.resolveCurrentTowerEntryItemAlternatives(
		runtime.Dungeon.ID,
		plan.Requirements,
	); err != nil {
		return currentDungeonEntryAdmissionPlan{}, err
	} else if len(alternatives) != 0 {
		plan.Requirements = nil
		plan.RequirementAlternatives = alternatives
		plan.Source = source
	}
	particularHell := normalizeDungeonPVFSymbol(runtime.Dungeon.Metadata.DungeonType) == "particular hell"
	inferno := runtime.Dungeon.Metadata.InfernoDungeon
	ordinaryHell := currentDungeonExplicitHellPartyRequest(runtime.Request, runtime.Dungeon, runtime.MazeIndex)
	if !particularHell && !inferno && !ordinaryHell {
		return validateCurrentDungeonEntryAdmissionPlan(plan)
	}

	if s == nil {
		return currentDungeonEntryAdmissionPlan{}, errDungeonWorldMapUnavailable
	}
	s.worldMapMu.RLock()
	table := s.worldMapTable
	s.worldMapMu.RUnlock()
	if table == nil {
		return currentDungeonEntryAdmissionPlan{}, errDungeonWorldMapUnavailable
	}
	area, found := table.FindAreaByDungeon(runtime.Dungeon.ID)
	if !found {
		return currentDungeonEntryAdmissionPlan{}, fmt.Errorf("%w: dungeon=%d has no owning area", errDungeonWorldMapUnavailable, runtime.Dungeon.ID)
	}
	switch {
	case particularHell:
		plan.FreePasses = append([]worldmap.TicketItem(nil), area.ParticularHellFreePassItems...)
		plan.Source = "worldmap_particularhell_freepass_or_dungeon_required_item"
	case inferno:
		plan.FreePasses = append([]worldmap.TicketItem(nil), area.InfernoFreePassItems...)
		plan.Source = "worldmap_inferno_freepass_or_item_condition"
	case ordinaryHell:
		plan.FreePasses = append([]worldmap.TicketItem(nil), area.HellFreePassItems...)
		plan.Source = "worldmap_hell_freepass_or_item_condition"
	}
	plan.FreePasses = uniqueCurrentDungeonEntryItems(plan.FreePasses)

	if len(plan.Requirements) == 0 && len(plan.RequirementAlternatives) == 0 && (inferno || ordinaryHell) {
		conditions := uniquePositiveInt64(area.ItemConditions)
		if len(conditions) != 1 {
			return currentDungeonEntryAdmissionPlan{}, fmt.Errorf(
				"invalid PVF hell admission item condition dungeon=%d item_conditions=%v",
				runtime.Dungeon.ID,
				conditions,
			)
		}
		// 194/205 个深渊区域副本没有 [hell party required item count]；没有声明值时按
		// 客户端同一条等级式推导（见 dungeon_hell_admission_cost.go），否则 193 这类副本
		// 的入场计划建立不起来，玩家点下去什么回执都收不到。
		count, costSource, err := currentDungeonHellAdmissionInvitationCount(runtime.Dungeon)
		if err != nil {
			return currentDungeonEntryAdmissionPlan{}, fmt.Errorf(
				"invalid PVF hell admission cost dungeon=%d item_conditions=%v: %w",
				runtime.Dungeon.ID,
				conditions,
				err,
			)
		}
		plan.Requirements = []worldmap.TicketItem{{ItemID: conditions[0], Count: count}}
		plan.AdmissionCostSource = costSource
	}
	return validateCurrentDungeonEntryAdmissionPlan(plan)
}

func validateCurrentDungeonEntryAdmissionPlan(plan currentDungeonEntryAdmissionPlan) (currentDungeonEntryAdmissionPlan, error) {
	plan.Requirements = uniqueCurrentDungeonEntryItems(plan.Requirements)
	plan.RequirementAlternatives = uniqueCurrentDungeonEntryItems(plan.RequirementAlternatives)
	for _, group := range [][]worldmap.TicketItem{plan.FreePasses, plan.Requirements, plan.RequirementAlternatives} {
		for _, item := range group {
			if item.ItemID <= 0 || item.Count <= 0 {
				return currentDungeonEntryAdmissionPlan{}, fmt.Errorf("invalid dungeon entry item id=%d count=%d", item.ItemID, item.Count)
			}
		}
	}
	if len(plan.FreePasses) != 0 && len(plan.Requirements) == 0 && len(plan.RequirementAlternatives) == 0 {
		return currentDungeonEntryAdmissionPlan{}, fmt.Errorf("PVF dungeon entry freepass has no fallback material contract")
	}
	if len(plan.Requirements) != 0 && len(plan.RequirementAlternatives) != 0 {
		return currentDungeonEntryAdmissionPlan{}, fmt.Errorf("dungeon entry has both conjunctive and alternative item contracts")
	}
	return plan, nil
}

func (s *Service) resolveCurrentTowerEntryItemAlternatives(
	dungeonID int64,
	requirements []worldmap.TicketItem,
) ([]worldmap.TicketItem, string, error) {
	if s == nil || dungeonID <= 0 || dungeonID > math.MaxUint32 {
		return nil, "", nil
	}
	s.worldMapMu.RLock()
	catalog := s.dungeonAdmissionCatalog
	s.worldMapMu.RUnlock()
	if catalog == nil {
		return nil, "", nil
	}
	var definitions []currentTowerEntryItemDefinition
	var source string
	for _, policy := range catalog.PoliciesForDungeon(uint32(dungeonID)) {
		switch policy.Key {
		case "tower_of_despair.character.daily", "tower_of_despair.account.daily":
			if source != "" && source != "tower_of_despair_entry_item_alternatives" {
				return nil, "", fmt.Errorf("dungeon %d belongs to conflicting tower admission families", dungeonID)
			}
			definitions = currentTowerOfDespairEntryItemDefinitions
			source = "tower_of_despair_entry_item_alternatives"
		case "tower_of_grief.character.daily", "tower_of_grief.account.daily":
			if source != "" && source != "tower_of_grief_entry_item_alternatives" {
				return nil, "", fmt.Errorf("dungeon %d belongs to conflicting tower admission families", dungeonID)
			}
			definitions = currentTowerOfGriefEntryItemDefinitions
			source = "tower_of_grief_entry_item_alternatives"
		}
	}
	if len(definitions) == 0 {
		return nil, "", nil
	}
	canonical := worldmap.TicketItem{ItemID: definitions[0].ItemID, Count: 1}
	if len(requirements) != 1 || requirements[0] != canonical {
		return nil, "", fmt.Errorf(
			"tower dungeon %d required-item contract=%v want canonical=%v",
			dungeonID,
			requirements,
			canonical,
		)
	}
	alternatives := make([]worldmap.TicketItem, 0, len(definitions))
	for _, definition := range definitions {
		alternatives = append(alternatives, worldmap.TicketItem{ItemID: definition.ItemID, Count: 1})
	}
	return alternatives, source, nil
}

func uniqueCurrentDungeonEntryItems(items []worldmap.TicketItem) []worldmap.TicketItem {
	if len(items) == 0 {
		return nil
	}
	type itemKey struct {
		itemID int64
		count  int64
	}
	seen := make(map[itemKey]struct{}, len(items))
	result := make([]worldmap.TicketItem, 0, len(items))
	for _, item := range items {
		key := itemKey{itemID: item.ItemID, count: item.Count}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	return result
}

func uniquePositiveInt64(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// consumeCurrentDungeonEntryItems applies the owning world's authored
// freepass category first, then the dungeon/area fallback cost, to the
// character-owned inventory. Account/shared warehouses are not searched
// because the client admission projection is backed by main inventory.
func (s *Service) consumeCurrentDungeonEntryItems(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) ([]currentItemListEntry, func() error, error) {
	if runtime == nil || session == nil {
		return nil, nil, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := s.resolveCurrentDungeonEntryAdmissionPlan(runtime)
	if err != nil {
		return nil, nil, err
	}
	if len(plan.FreePasses) == 0 && len(plan.Requirements) == 0 && len(plan.RequirementAlternatives) == 0 {
		return nil, nil, nil
	}
	updates := make([]currentItemListEntry, 0)
	mutations := make([]currentDungeonEntryInventorySlotMutation, 0)
	var consumed []worldmap.TicketItem
	usedFreePass := false
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return nil, nil, err
	}
	_, err = owner.MutateInventory(ctx, dnfdungeon.InventoryMutationCommand{
		CharacterID: fmt.Sprintf("%d", session.selectedCharacterID),
		Apply: func(inventory *dnfrepo.InventoryRecord) (bool, error) {
			if inventory == nil {
				return false, errDungeonEntryItemMissing
			}
			mutations = mutations[:0]
			selectedItems, freePass, selectErr := selectCurrentDungeonEntryItems(inventory.Slots, plan)
			if selectErr != nil {
				var shortage currentDungeonEntryItemShortage
				if errors.As(selectErr, &shortage) {
					shortage.CharacterID = session.selectedCharacterID
					selectErr = shortage
				}
				return false, selectErr
			}
			consumed = selectedItems
			usedFreePass = freePass
			keys := currentDungeonEntryOwnedKeys(inventory.Slots)
			requirements := make(map[int64]int64, len(consumed))
			for _, required := range consumed {
				requirements[required.ItemID] += required.Count
			}
			itemIDs := make([]int64, 0, len(requirements))
			for itemID := range requirements {
				itemIDs = append(itemIDs, itemID)
			}
			sort.Slice(itemIDs, func(i, j int) bool { return itemIDs[i] < itemIDs[j] })
			for _, itemID := range itemIDs {
				remaining := requirements[itemID]
				for _, key := range keys {
					if remaining == 0 {
						break
					}
					stack := inventory.Slots[key]
					if stack.ItemID != itemID || stack.Count <= 0 {
						continue
					}
					beforeStack := cloneCurrentDungeonEntryInventoryStack(stack)
					listType, slot, _ := parseSceneInventorySlotKey(key)
					used := stack.Count
					if used > remaining {
						used = remaining
					}
					stack.Count -= used
					remaining -= used
					if stack.Count == 0 {
						delete(inventory.Slots, key)
						mutations = append(mutations, currentDungeonEntryInventorySlotMutation{
							key: key, before: beforeStack, beforeExists: true,
						})
						var removed currentItemListEntry
						removed.patchCore(slot, math.MaxUint32, 0)
						updates = append(updates, removed)
					} else {
						inventory.Slots[key] = stack
						mutations = append(mutations, currentDungeonEntryInventorySlotMutation{
							key: key, before: beforeStack, beforeExists: true,
							after: cloneCurrentDungeonEntryInventoryStack(stack), afterExists: true,
						})
						updates = append(updates, currentItemListEntryFromStack(listType, slot, stack))
					}
				}
				if remaining != 0 {
					return false, errDungeonEntryItemMissing
				}
			}
			return len(updates) != 0, nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	if len(updates) == 0 {
		return nil, nil, nil
	}
	restore := func() error {
		restoreCtx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		_, restoreErr := owner.MutateInventory(restoreCtx, dnfdungeon.InventoryMutationCommand{
			CharacterID: fmt.Sprintf("%d", session.selectedCharacterID),
			Apply: func(inventory *dnfrepo.InventoryRecord) (bool, error) {
				if inventory == nil {
					return false, errDungeonEntryItemMissing
				}
				// Validate every post-image before changing any slot. One conflict
				// fails the repository transaction without a partial compensation.
				for _, mutation := range mutations {
					current, found := inventory.Slots[mutation.key]
					if found != mutation.afterExists ||
						(found && !sameCurrentDungeonEntryInventoryStack(current, mutation.after)) {
						return false, fmt.Errorf("%w: slot=%s", errDungeonEntryItemRestoreConflict, mutation.key)
					}
				}
				if inventory.Slots == nil && len(mutations) != 0 {
					inventory.Slots = make(map[string]dnfrepo.ItemStack, len(mutations))
				}
				for _, mutation := range mutations {
					if mutation.beforeExists {
						inventory.Slots[mutation.key] = cloneCurrentDungeonEntryInventoryStack(mutation.before)
					} else {
						delete(inventory.Slots, mutation.key)
					}
				}
				return len(mutations) != 0, nil
			},
		})
		return restoreErr
	}
	s.logGameEvent(session, "game-dungeon-entry-items-consumed",
		"dungeon_id", runtime.Dungeon.ID,
		"consumed", consumed,
		"freepass", usedFreePass,
		"update_count", len(updates),
		"source", plan.Source,
		"admission_cost_source", plan.AdmissionCostSource)
	return updates, restore, nil
}

func cloneCurrentDungeonEntryInventoryStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = maps.Clone(stack.Extra)
	return stack
}

func sameCurrentDungeonEntryInventoryStack(left, right dnfrepo.ItemStack) bool {
	return left.ItemID == right.ItemID && left.Count == right.Count && left.Bind == right.Bind &&
		left.ExpireAt.Equal(right.ExpireAt) && bytes.Equal(left.RawEntry, right.RawEntry) &&
		maps.Equal(left.Extra, right.Extra)
}

// currentDungeonEntryOwnedKeys lists the character-owned list-0 rows one entry
// may consume from, in the sorted order both the selector below and the consume
// loop walk. Account/shared warehouses are deliberately not searched: the client
// admission projection is backed by main inventory.
func currentDungeonEntryOwnedKeys(slots map[string]dnfrepo.ItemStack) []string {
	keys := make([]string, 0, len(slots))
	for key, stack := range slots {
		listType, _, valid := parseSceneInventorySlotKey(key)
		if valid && listType == dnfrepo.MainInventoryListType && stack.ItemID > 0 && stack.Count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// selectCurrentDungeonEntryItems decides which authored admission rows one
// attempt consumes: the owning area's freepass category first, then the
// dungeon/area alternatives, then the conjunctive cost -- and reports the same
// currentDungeonEntryItemShortage the consume path reports when the player
// cannot pay.
//
// It is pure on purpose. The read-only retry preflight and the consuming entry
// path both call it, so "the preflight said the player can pay" and "the consume
// found the item" can never disagree. Live 2026-09-14 22:22 is what the
// disagreement cost: 再次挑战 passed a preflight that never looked at entry
// items, retired the completed run, and only then failed the consume -- leaving
// the client on the settlement page with no runtime, so 返回城镇 did nothing.
func selectCurrentDungeonEntryItems(
	slots map[string]dnfrepo.ItemStack,
	plan currentDungeonEntryAdmissionPlan,
) ([]worldmap.TicketItem, bool, error) {
	keys := currentDungeonEntryOwnedKeys(slots)
	if selected := firstAvailableCurrentDungeonEntryItem(slots, keys, plan.FreePasses); selected.ItemID > 0 {
		return []worldmap.TicketItem{selected}, true, nil
	}
	if len(plan.RequirementAlternatives) != 0 {
		selected := firstAvailableCurrentDungeonEntryItem(slots, keys, plan.RequirementAlternatives)
		if selected.ItemID <= 0 {
			// Report the first alternative: it is the item the area's PVF names
			// first, and the one the player is told to bring.
			first := plan.RequirementAlternatives[0]
			available := int64(0)
			for _, key := range keys {
				if stack := slots[key]; stack.ItemID == first.ItemID && stack.Count > 0 {
					available += stack.Count
				}
			}
			return nil, false, currentDungeonEntryItemShortage{
				ItemID:    first.ItemID,
				Required:  first.Count,
				Available: available,
			}
		}
		return []worldmap.TicketItem{selected}, false, nil
	}
	requirements := make(map[int64]int64, len(plan.Requirements))
	for _, required := range plan.Requirements {
		requirements[required.ItemID] += required.Count
	}
	itemIDs := make([]int64, 0, len(requirements))
	for itemID := range requirements {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(i, j int) bool { return itemIDs[i] < itemIDs[j] })
	for _, itemID := range itemIDs {
		available := int64(0)
		for _, key := range keys {
			stack := slots[key]
			if stack.ItemID != itemID || stack.Count <= 0 {
				continue
			}
			available += stack.Count
		}
		if available < requirements[itemID] {
			return nil, false, currentDungeonEntryItemShortage{
				ItemID:    itemID,
				Required:  requirements[itemID],
				Available: available,
			}
		}
	}
	return append([]worldmap.TicketItem(nil), plan.Requirements...), false, nil
}

func firstAvailableCurrentDungeonEntryItem(
	slots map[string]dnfrepo.ItemStack,
	keys []string,
	candidates []worldmap.TicketItem,
) worldmap.TicketItem {
	for _, candidate := range candidates {
		available := int64(0)
		for _, key := range keys {
			stack := slots[key]
			if stack.ItemID == candidate.ItemID && stack.Count > 0 {
				available += stack.Count
			}
		}
		if available >= candidate.Count {
			return candidate
		}
	}
	return worldmap.TicketItem{}
}
