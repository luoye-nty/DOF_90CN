package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// currentQuestDropCandidate is one rolled quest-item drop from a PVF
// [monster reward item] or [enemy reward item] entry.
type currentQuestDropCandidate struct {
	QuestID  int64
	ItemID   uint32
	Count    int64
	DropRate int64
	MaxStack int64
}

type currentDungeonQuestDropPlan struct {
	sequence            uint64
	characterID         uint16
	characterGeneration uint64
	characterKey        string
	accountID           string
	dungeonID           int64
	difficulty          int64
	enemyCode           int64
	enemyType           int64
	includeMonster      bool
}

const currentDungeonQuestEnemyTypeAICharacter = int64(2)

// checkCurrentDungeonQuestMonsterDrops implements the 86JP
// QuestDropService.CheckMonsterDrop path: after a monster dies, every
// active quest's PVF [monster reward item] table is checked for the
// killed monster code. Matched entries are rolled (per-attempt
// percentage) and granted directly to the character backpack, followed
// by an op14 incremental refresh. Quest progress sync for "get item" /
// "seeking" quests is left to the existing pickup/progress owners.
func (s *Service) checkCurrentDungeonQuestMonsterDrops(
	session *gameSession,
	runtime *runtimeDungeonState,
	monsterID int64,
) {
	s.checkCurrentDungeonQuestEnemyDrops(session, runtime, monsterID, 1, true)
}

// checkCurrentDungeonQuestEnemyDrops owns the common task-item grant path for
// current-PVF enemy categories. Ordinary monsters also consult the legacy
// [monster reward item] table; passive objects consult only type-3
// [enemy reward item] rows.
func (s *Service) checkCurrentDungeonQuestEnemyDrops(
	session *gameSession,
	runtime *runtimeDungeonState,
	enemyCode int64,
	enemyType int64,
	includeMonsterRewards bool,
) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || runtime == nil || enemyCode <= 0 ||
		currentDungeonRuntimeIsPractice(runtime) || s.currentDungeonRuntimeIsRaidGate(runtime) {
		return
	}
	plan := currentDungeonQuestDropPlan{
		characterID:         session.selectedCharacterID,
		characterGeneration: session.characterGeneration,
		characterKey:        strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		accountID:           s.accountIDForSession(session),
		dungeonID:           runtime.Dungeon.ID,
		difficulty:          int64(runtime.Request.Difficulty),
		enemyCode:           enemyCode,
		enemyType:           enemyType,
		includeMonster:      includeMonsterRewards,
	}
	plan.sequence = reserveCurrentDungeonQuestProgress(session)
	if session.events != nil && session.events.loop != nil && session.events.active.Load() {
		go s.runCurrentDungeonQuestDrops(session, plan)
		return
	}
	runCurrentDungeonQuestProgressOrdered(session, plan.sequence, func() {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		s.executeCurrentDungeonQuestDrops(ctx, session, plan, func(body []byte) error {
			return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
		})
	})
}

func (s *Service) runCurrentDungeonQuestDrops(session *gameSession, plan currentDungeonQuestDropPlan) {
	runCurrentDungeonQuestProgressOrdered(session, plan.sequence, func() {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		s.executeCurrentDungeonQuestDrops(ctx, session, plan, func(body []byte) error {
			body = append([]byte(nil), body...)
			return s.postGameSessionCharacterEvent(
				session,
				"dungeon-quest-drop-persistence-complete",
				plan.characterID,
				plan.characterGeneration,
				func() error {
					return s.sendGameUpperRawClass(
						session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0,
					)
				},
			)
		})
	})
}

func (s *Service) executeCurrentDungeonQuestDrops(
	ctx context.Context,
	session *gameSession,
	plan currentDungeonQuestDropPlan,
	deliver func([]byte) error,
) {
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil || catalog == nil {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil || repositories.CharacterItems == nil {
		return
	}
	questHelperActive := false
	growthQuestDropPercent := int64(0)
	if repositories.Account != nil {
		account, accountFound, accountErr := repositories.Account.Load(ctx, plan.accountID)
		questHelperActive = accountErr == nil &&
			accountFound &&
			premium.Active(account, premium.DevilSlotType(premium.DevilSlotQuestHelper), time.Now().UTC())
	}
	if growthEffect, growthActive := s.currentGrowthContractEffect(ctx, plan.accountID); growthActive {
		growthQuestDropPercent = growthEffect.QuestItemDropRatePercent
	}
	questRecord, found, err := repositories.Quest.Load(ctx, plan.characterKey)
	if err != nil || !found {
		return
	}
	activeIDs := currentQuestDropActiveIDs(questRecord)
	if len(activeIDs) == 0 {
		return
	}
	var candidates []currentQuestDropCandidate
	if plan.includeMonster {
		candidates = s.currentQuestDropMatchMonster(catalog, activeIDs, plan.dungeonID, plan.difficulty, plan.enemyCode)
	} else {
		candidates = currentQuestDropMatchEnemy(catalog, activeIDs, plan.dungeonID, plan.difficulty, plan.enemyCode, plan.enemyType)
	}
	if len(candidates) == 0 {
		return
	}
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return
	}
	dropCatalog, catalogErr := s.currentPVFItemCatalog()
	rng := newCurrentDungeonDropLCG(uint32(time.Now().UnixNano()))
	var grantedSlots []int16
	var grantedAccountSlots []int16
	// 落点循环只写一份：账号优先（account != nil）与改造前的只进背包（account == nil）
	// 共用它，区别只在事务边界。
	drops := func(account *dnfrepo.AccountInventoryRecord, inventory *dnfrepo.InventoryRecord) (bool, bool, error) {
		accountChanged := false
		characterChanged := false
		for _, candidate := range candidates {
			// [max stack] 的"已持有"口径保持改造前原样：**只数背包**。
			// 这里刻意不改。
			//
			// 原因不是"账号仓那份不算数"，而是实测真实 Script.pvf：9487 个
			// .qst 里 [enemy reward item] / [monster reward item] 共 8900 行，
			// 其中指向六个晶块/六个灵魂的行是 **0 行** —— 这个上限对账号共享
			// 道具根本不可能被触发，把账号仓并进来只会改变 4363 这类普通任务
			// 道具（它们不在账号仓，但固定格里若被塞进外来件就会多算）的行为，
			// 属于没有收益的风险。真正给晶块的任务奖励走的是任务通关奖励那条
			// 路（[reward int data]，见 currentQuestFinishItemAllocator）。
			held := currentQuestDropHeldCount(*inventory, int64(candidate.ItemID))
			if candidate.MaxStack > 0 && held >= candidate.MaxStack {
				s.logGameEvent(session, "game-dungeon-quest-drop-skipped-max-stack",
					"quest_id", candidate.QuestID,
					"item_id", candidate.ItemID,
					"held", held,
					"max_stack", candidate.MaxStack)
				continue
			}
			dropRate := currentQuestItemDropRate(candidate.DropRate, growthQuestDropPercent, questHelperActive)
			dropCount := currentQuestDropRoll(int(rng.NextUint32()), candidate.Count, dropRate, candidate.MaxStack, held)
			if dropCount <= 0 {
				continue
			}
			if catalogErr != nil || dropCatalog == nil {
				s.logGameEvent(session, "game-dungeon-quest-drop-skipped-catalog",
					"quest_id", candidate.QuestID,
					"item_id", candidate.ItemID,
					"error", catalogErr)
				continue
			}
			definition, resolveErr := dropCatalog.ResolveItem(candidate.ItemID)
			if resolveErr != nil {
				s.logGameEvent(session, "game-dungeon-quest-drop-skipped-resolve",
					"quest_id", candidate.QuestID,
					"item_id", candidate.ItemID,
					"error", resolveErr)
				continue
			}
			// 账号优先：无色/灵魂类掉落先落账号共享仓的固定格，余量再走角色背包。
			// 落点规则只有一份实现（currentQuestDropPlaceCandidate），祭坛那条
			// op255 批量路径共用它。
			placement, placeErr := currentQuestDropPlaceCandidate(inventory, account, definition, uint32(dropCount))
			if placement.AccountChanged {
				accountChanged = true
				grantedAccountSlots = append(grantedAccountSlots, placement.AccountSlot)
				s.logGameEvent(session, "game-dungeon-quest-drop-granted",
					"quest_id", candidate.QuestID,
					"monster_id", plan.enemyCode,
					"enemy_code", plan.enemyCode,
					"enemy_type", plan.enemyType,
					"item_id", candidate.ItemID,
					"count", dropCount,
					"base_drop_rate", candidate.DropRate,
					"effective_drop_rate", dropRate,
					"growth_contract_drop_percent", growthQuestDropPercent,
					"quest_helper", questHelperActive,
					"slot", placement.AccountSlot,
					"new_total", placement.AccountTotal,
					"owner", "account_shared")
			}
			if placeErr != nil {
				s.logGameEvent(session, "game-dungeon-quest-drop-skipped-grant",
					"quest_id", candidate.QuestID,
					"item_id", candidate.ItemID,
					"count", placement.Remainder,
					"error", placeErr)
				continue
			}
			if !placement.CharacterChanged {
				continue
			}
			grantedSlots = append(grantedSlots, placement.CharacterSlot)
			characterChanged = true
			s.logGameEvent(session, "game-dungeon-quest-drop-granted",
				"quest_id", candidate.QuestID,
				"monster_id", plan.enemyCode,
				"enemy_code", plan.enemyCode,
				"enemy_type", plan.enemyType,
				"item_id", candidate.ItemID,
				"count", placement.Remainder,
				"base_drop_rate", candidate.DropRate,
				"effective_drop_rate", dropRate,
				"growth_contract_drop_percent", growthQuestDropPercent,
				"quest_helper", questHelperActive,
				"slot", placement.CharacterSlot,
				"new_total", placement.CharacterTotal,
				"owner", "character")
		}
		return accountChanged, characterChanged, nil
	}
	// 账号仓那条事务只在**确实能用**时才走：账号仓事务实现缺失、plan 没有 accountID、
	// 或角色记录不属于该账号，都会让 MutateAccountCharacterInventory 整体失败 ——
	// 那等于把"任务怪掉落"整条打没了（被动物件掉落就是这么被闸门抓出来的）。
	// 这种情况退回改造前的事务边界 MutateInventory（只给角色背包），账号侧 account=nil。
	var (
		resultInventory  dnfrepo.InventoryRecord
		resultAccount    dnfrepo.AccountInventoryRecord
		accountChanged   bool
		characterChanged bool
	)
	if currentQuestDropAccountSharedReady(ctx, repositories, plan) {
		mutation, mutationErr := owner.MutateAccountCharacterInventory(ctx, dnfdungeon.AccountCharacterInventoryMutationCommand{
			AccountID:   plan.accountID,
			CharacterID: plan.characterKey,
			UpdatedAt:   time.Now().UTC(),
			Apply:       drops,
		})
		if mutationErr != nil {
			s.logGameEvent(session, "game-dungeon-quest-drop-save-failed",
				"monster_id", plan.enemyCode,
				"enemy_code", plan.enemyCode,
				"enemy_type", plan.enemyType,
				"error", mutationErr)
			return
		}
		resultInventory, resultAccount = mutation.Inventory, mutation.AccountInventory
		accountChanged, characterChanged = mutation.AccountChanged, mutation.CharacterChanged
	} else {
		mutation, mutationErr := owner.MutateInventory(ctx, dnfdungeon.InventoryMutationCommand{
			CharacterID: plan.characterKey,
			UpdatedAt:   time.Now().UTC(),
			Apply: func(inventory *dnfrepo.InventoryRecord) (bool, error) {
				_, characterDirty, applyErr := drops(nil, inventory)
				return characterDirty, applyErr
			},
		})
		if mutationErr != nil {
			s.logGameEvent(session, "game-dungeon-quest-drop-save-failed",
				"monster_id", plan.enemyCode,
				"enemy_code", plan.enemyCode,
				"enemy_type", plan.enemyType,
				"error", mutationErr)
			return
		}
		resultInventory = mutation.Inventory
		characterChanged = mutation.Changed
	}
	if !accountChanged && !characterChanged {
		return
	}
	updates := make([]currentItemListEntry, 0, len(grantedSlots)+len(grantedAccountSlots))
	for _, slot := range grantedSlots {
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		if stack, stackFound := resultInventory.Slots[key]; stackFound {
			updates = append(updates, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack))
		}
	}
	// 账号共享槽也是 list-0 线地址：ACK 行必须从账号记录里取，才报得出真正落库的数量。
	for _, slot := range grantedAccountSlots {
		key := dnfrepo.AccountSharedInventorySlotKey(slot)
		if stack, stackFound := resultAccount.Slots[key]; stackFound {
			updates = append(updates, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack))
		}
	}
	if len(updates) == 0 {
		return
	}
	sortCurrentItemListEntries(updates)
	body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, updates)
	if deliver == nil {
		return
	}
	if sendErr := deliver(body); sendErr != nil {
		s.logGameEvent(session, "game-dungeon-quest-drop-refresh-failed",
			"monster_id", plan.enemyCode,
			"enemy_code", plan.enemyCode,
			"enemy_type", plan.enemyType,
			"error", sendErr)
		return
	}
	s.logGameEvent(session, "game-dungeon-quest-drop-refresh-send",
		"monster_id", plan.enemyCode,
		"enemy_code", plan.enemyCode,
		"enemy_type", plan.enemyType,
		"entries", len(updates),
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember))
}

// currentQuestDropMatchMonster keeps the explicit PVF reward rows
// authoritative, then consults the narrowly-scoped fallback relationships
// that the current archive stores outside those rows.
func (s *Service) currentQuestDropMatchMonster(
	catalog *dnfquest.Catalog,
	activeIDs []int64,
	dungeonID int64,
	difficulty int64,
	monsterID int64,
) []currentQuestDropCandidate {
	candidates := currentQuestDropMatchMonster(catalog, activeIDs, dungeonID, difficulty, monsterID)
	itemCatalog, itemErr := s.currentPVFItemCatalog()
	monsterCatalog, monsterErr := s.dungeonMonsterCatalog()
	if itemErr != nil || monsterErr != nil || itemCatalog == nil || monsterCatalog == nil {
		return candidates
	}
	return append(candidates, currentQuestDropMatchImplicitMonster(
		catalog, itemCatalog, monsterCatalog, activeIDs, dungeonID, difficulty, monsterID,
	)...)
}

// currentQuestDropMatchImplicitMonster recovers two current-PVF relationships
// that are not represented by [monster reward item]/[enemy reward item]:
//
//   - a plain seeking material scoped by [dungeon info], including event
//     copies that inherit the scope from an identically named/itemed sibling;
//   - a quest item whose own [explain] names the killed Dimension Crack hidden
//     boss.
//
// Both paths remain bounded by the active quest's required count. Definitions
// carrying an explicit reward table never enter this fallback.
func currentQuestDropMatchImplicitMonster(
	questCatalog *dnfquest.Catalog,
	itemCatalog *pvfDungeonDropCatalog,
	monsterCatalog *pvfDungeonMonsterCatalog,
	activeIDs []int64,
	dungeonID int64,
	difficulty int64,
	monsterID int64,
) []currentQuestDropCandidate {
	if questCatalog == nil || itemCatalog == nil || monsterCatalog == nil || dungeonID <= 0 || monsterID <= 0 {
		return nil
	}
	monster, monsterFound, monsterErr := monsterCatalog.Find(monsterID)
	if monsterErr != nil || !monsterFound {
		return nil
	}
	hiddenDimensionBoss := currentQuestDropIsDimensionHiddenBoss(monster.Path, monster.Sections)
	monsterName := normalizeCurrentQuestDropProse(monster.Name)
	var candidates []currentQuestDropCandidate
	for _, questID := range activeIDs {
		definition, found := questCatalog.Find(questID)
		if !found || len(definition.MonsterRewardItems) != 0 || len(definition.EnemyRewardItems) != 0 {
			continue
		}
		targets := questCatalog.SeekingItemTargets(questID)
		if len(targets) == 0 {
			continue
		}
		scopeMatch := currentQuestDropScopeMatches(questCatalog.SeekingDungeonScopes(questID), dungeonID, difficulty)
		for _, target := range targets {
			if target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count <= 0 {
				continue
			}
			item, itemErr := itemCatalog.ResolveItem(uint32(target.ItemID))
			if itemErr != nil || item.Kind != dungeonDropItemStackable {
				continue
			}
			typeName := normalizeDungeonPVFSymbol(item.StackableType)
			matched := typeName == "material" && scopeMatch
			if !matched && typeName == "quest" && hiddenDimensionBoss && monsterName != "" {
				matched = strings.Contains(normalizeCurrentQuestDropProse(item.Explain), monsterName)
			}
			if !matched {
				continue
			}
			candidates = append(candidates, currentQuestDropCandidate{
				QuestID: questID, ItemID: uint32(target.ItemID), Count: 1, DropRate: 100, MaxStack: target.Count,
			})
		}
	}
	return candidates
}

func currentQuestDropScopeMatches(scopes []dnfquest.DungeonScope, dungeonID int64, difficulty int64) bool {
	for _, scope := range scopes {
		if scope.DungeonID != -1 && scope.DungeonID != dungeonID {
			continue
		}
		if scope.Difficulty >= 0 && scope.Difficulty != difficulty {
			continue
		}
		return true
	}
	return false
}

func currentQuestDropIsDimensionHiddenBoss(monsterPath string, sections []string) bool {
	pathValue := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(monsterPath), "\\", "/"))
	if !strings.Contains(pathValue, "/dimensioncrack/specialboss/") &&
		!strings.Contains(pathValue, "dimensioncrack/specialboss/") {
		return false
	}
	for _, section := range sections {
		if normalizeDungeonPVFSymbol(section) == "dimension hidden boss" {
			return true
		}
	}
	return false
}

func normalizeCurrentQuestDropProse(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), ""))
}

// currentQuestHelperDropRate applies the original task-assistant behavior:
// quest-item probability is raised by one fifth, capped at certainty. The
// current EXE has no slot-2 local call site, so this belongs on the server
// roll that owns quest-item drops.
func currentQuestHelperDropRate(base int64, active bool) int64 {
	return currentQuestItemDropRate(base, 0, active)
}

func currentQuestItemDropRate(base int64, growthContractPercent int64, questHelperActive bool) int64 {
	const questHelperPercent int64 = 20
	if base <= 0 {
		return base
	}
	bonusPercent := growthContractPercent
	if bonusPercent < 0 {
		bonusPercent = 0
	}
	if questHelperActive {
		bonusPercent += questHelperPercent
	}
	if bonusPercent <= 0 {
		return base
	}
	if bonusPercent >= 10000 || base > math.MaxInt64/(100+bonusPercent) {
		return 100
	}
	boosted := (base*(100+bonusPercent) + 99) / 100
	if boosted > 100 {
		return 100
	}
	return boosted
}

// currentQuestDropActiveIDs extracts quest IDs whose status is "active"
// from the persisted quest record.
func currentQuestDropActiveIDs(record dnfrepo.QuestRecord) []int64 {
	if len(record.States) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(record.States))
	for questID, state := range record.States {
		if strings.EqualFold(strings.TrimSpace(state.Status), "active") {
			ids = append(ids, questID)
		}
	}
	return ids
}

// currentQuestDropMatchMonster checks every active quest's PVF
// [monster reward item] entries for the killed monster, applying the
// 86JP dungeon/difficulty wildcard rules.
func currentQuestDropMatchMonster(
	catalog *dnfquest.Catalog,
	activeIDs []int64,
	dungeonID int64,
	difficulty int64,
	monsterID int64,
) []currentQuestDropCandidate {
	var candidates []currentQuestDropCandidate
	for _, questID := range activeIDs {
		definition, found := catalog.Find(questID)
		if !found {
			continue
		}
		for _, entry := range definition.MonsterRewardItems {
			if entry.MonsterCode != monsterID {
				continue
			}
			if entry.DungeonID != -1 && entry.DungeonID != dungeonID {
				continue
			}
			if entry.Difficulty >= 0 && entry.Difficulty != difficulty {
				continue
			}
			if entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.Count <= 0 || entry.DropRate <= 0 {
				continue
			}
			candidates = append(candidates, currentQuestDropCandidate{
				QuestID:  questID,
				ItemID:   uint32(entry.ItemID),
				Count:    entry.Count,
				DropRate: entry.DropRate,
				MaxStack: entry.MaxStack,
			})
		}
		for _, entry := range definition.EnemyRewardItems {
			if entry.EnemyType != 1 || entry.EnemyCode != monsterID {
				continue
			}
			if entry.DungeonID != -1 && entry.DungeonID != dungeonID {
				continue
			}
			if entry.Difficulty >= 0 && entry.Difficulty != difficulty {
				continue
			}
			if entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.Count <= 0 || entry.DropRate <= 0 {
				continue
			}
			candidates = append(candidates, currentQuestDropCandidate{
				QuestID:  questID,
				ItemID:   uint32(entry.ItemID),
				Count:    entry.Count,
				DropRate: entry.DropRate,
				MaxStack: entry.MaxStack,
			})
		}
	}
	return candidates
}

// currentQuestDropMatchEnemy strictly applies the eight-field
// [enemy reward item] identity. Type 3 is reserved for passive objects; the
// caller must provide independently proven object identity and map scope.
func currentQuestDropMatchEnemy(
	catalog *dnfquest.Catalog,
	activeIDs []int64,
	dungeonID int64,
	difficulty int64,
	enemyCode int64,
	enemyType int64,
) []currentQuestDropCandidate {
	var candidates []currentQuestDropCandidate
	for _, questID := range activeIDs {
		definition, found := catalog.Find(questID)
		if !found {
			continue
		}
		for _, entry := range definition.EnemyRewardItems {
			if entry.EnemyType != enemyType || entry.EnemyCode != enemyCode {
				continue
			}
			if entry.DungeonID != -1 && entry.DungeonID != dungeonID {
				continue
			}
			if entry.Difficulty >= 0 && entry.Difficulty != difficulty {
				continue
			}
			if entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.Count <= 0 || entry.DropRate <= 0 {
				continue
			}
			candidates = append(candidates, currentQuestDropCandidate{
				QuestID: questID, ItemID: uint32(entry.ItemID), Count: entry.Count,
				DropRate: entry.DropRate, MaxStack: entry.MaxStack,
			})
		}
	}
	return candidates
}

// currentQuestDropRoll implements the 86JP QuestDropProvider.RollDrop:
// for k in 0..count-1, if rand(100) < dropRate then actual++.
// The result is clamped to maxStack - held.
func currentQuestDropRoll(seed int, count int64, dropRate int64, maxStack int64, held int64) int64 {
	if count <= 0 || dropRate <= 0 {
		return 0
	}
	if maxStack > 0 && held >= maxStack {
		return 0
	}
	var actual int64
	rng := newCurrentDungeonDropLCG(uint32(seed))
	for k := int64(0); k < count; k++ {
		if int64(rng.Next(100)) < dropRate {
			actual++
		}
	}
	if actual <= 0 {
		return 0
	}
	if actual > 999 {
		actual = 999
	}
	if maxStack > 0 && held+actual > maxStack {
		actual = maxStack - held
	}
	if actual < 0 {
		return 0
	}
	return actual
}

// currentQuestDropHeldCount counts how many of itemID the character
// currently holds across all backpack slots.
func currentQuestDropHeldCount(inventory dnfrepo.InventoryRecord, itemID int64) int64 {
	var total int64
	for _, stack := range inventory.Slots {
		if stack.ItemID == itemID {
			total += stack.Count
		}
	}
	return total
}

// currentQuestDropAccountSharedReady 判断"账号优先"这条事务能不能走。
//
// 需要三件事同时成立：账号仓事务实现存在、plan 里有 accountID、角色记录确实属于该账号。
// 任何一条不成立都必须**退回改造前的事务边界（只进背包）**，而不是让
// MutateAccountCharacterInventory 报错把整条掉落打没 —— 2026-09-21 的教训：
// 被动物件掉落（enemy_type=3）的既有夹具里角色没有 AccountID，
// 于是那条路整体发放为 0，被 baseline 闸门抓出来（4 个新失败）。
func currentQuestDropAccountSharedReady(
	ctx context.Context,
	repositories dnfrepo.Group,
	plan currentDungeonQuestDropPlan,
) bool {
	if repositories.AccountAssets == nil || repositories.Character == nil ||
		strings.TrimSpace(plan.accountID) == "" || strings.TrimSpace(plan.characterKey) == "" {
		return false
	}
	character, found, err := repositories.Character.Load(ctx, plan.characterKey)
	if err != nil || !found {
		return false
	}
	return strings.TrimSpace(character.AccountID) == strings.TrimSpace(plan.accountID)
}

// currentQuestDropAccountSharedSource 是这条路写进账号仓行 Extra["source"] 的溯源串。
const currentQuestDropAccountSharedSource = "dungeon_quest_drop"

// currentQuestDropPlacement 是"这一次掉落最后落到哪儿去了"的完整结果。
type currentQuestDropPlacement struct {
	// AccountSlot 是账号共享仓里的固定格（354..365）；0 表示该道具没有固定格。
	AccountSlot int16
	// AccountChanged 表示账号记录这一次真的被并入了量。
	AccountChanged bool
	// AccountTotal 是并入后账号仓该格的总量（仅在 AccountChanged 时有意义）。
	AccountTotal int64
	// CharacterSlot / CharacterChanged / CharacterTotal 是背包侧的结果。
	CharacterSlot    int16
	CharacterChanged bool
	CharacterTotal   int64
	// Remainder 是账号仓装不下、按容器规则溢出到背包的那部分。恒为 0 表示全部进了账号仓。
	Remainder uint32
}

// currentQuestDropPlaceCandidate 是任务怪掉落**唯一**的落点实现：无色/灵魂先并进
// 账号共享仓的固定格（统一入口见 account_shared_grant.go），装不下的余量才按调用方
// 原有的容器规则落角色背包。
//
// # 为什么必须是"唯一"
//
// 这条路历史上被写了两遍 —— 普通地图杀怪（executeCurrentDungeonQuestDrops）与祭坛
// op255 批量死亡（commitCurrentDungeonAltarQuestDrops）各有一份落点判定。2026-09-21
// 的晶块事故就是这么来的：同一个语义散成多份，改一份漏一份。落点规则只允许有一份，
// 两条调用方都必须走这里。
//
// # 失败语义
//
// 背包侧失败（背包满）**不会吞掉账号侧已经并进去的量**：账号那部分照常返回，由调用方
// 决定继续还是回滚（祭坛那条按原有策略回滚整个事务，普通那条跳过本次）。
func currentQuestDropPlaceCandidate(
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
) (currentQuestDropPlacement, error) {
	var placement currentQuestDropPlacement
	if count == 0 {
		return placement, nil
	}
	shared := grantCurrentAccountSharedFirst(account, definition, count, currentQuestDropAccountSharedSource)
	placement.AccountSlot = shared.Slot
	placement.AccountChanged = shared.Changed
	placement.Remainder = shared.Remainder
	if shared.Changed && account != nil {
		if row, found := account.Slots[dnfrepo.AccountSharedInventorySlotKey(shared.Slot)]; found {
			placement.AccountTotal = row.Count
		}
	}
	if shared.Remainder == 0 {
		return placement, nil
	}
	slot, err := currentQuestDropGrantStack(inventory, definition, shared.Remainder)
	if err != nil {
		return placement, err
	}
	key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
	stack := inventory.Slots[key]
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	inventory.Slots[key] = stack
	placement.CharacterSlot = slot
	placement.CharacterChanged = true
	placement.CharacterTotal = stack.Count
	return placement, nil
}

// currentQuestDropGrantStack adds count of the item to the character
// backpack, merging with an existing stack when possible (86JP
// QuestDropService.TryPickupItemToInventory). It returns the slot
// that received the items.
func currentQuestDropGrantStack(inventory *dnfrepo.InventoryRecord, definition dungeonDropItemDefinition, count uint32) (int16, error) {
	if inventory == nil || count == 0 || definition.ItemID == 0 {
		return 0, errDungeonPickupItemInvalid
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	// Revive coins go to the fixed slot-1 wallet, never to an ordinary bag
	// row -- same rule as dungeon.PlacePickupItem. This is the chokepoint for
	// quest monster drops, altar drops and guild contents.
	if dnfrevivecoin.IsReward(int64(definition.ItemID)) {
		if _, _, err := dnfrevivecoin.GrantSlots(
			inventory.Slots, int64(count), "revive_coin_quest_drop"); err != nil {
			return 0, err
		}
		return dnfrevivecoin.WalletSlot, nil
	}
	itemID := int64(definition.ItemID)
	stackLimit := int64(definition.StackLimit)
	if definition.Kind == dungeonDropItemEquipment {
		stackLimit = 1
	}
	// An omitted [stack limit] is the current client's unlimited signed-32-bit
	// stack contract. Treating it as 999 caused valid quest materials to split
	// into extra rows after the first stack reached that invented threshold.
	if definition.Kind == dungeonDropItemStackable && stackLimit <= 0 {
		stackLimit = math.MaxInt32
	}
	if int64(count) > stackLimit {
		return 0, fmt.Errorf("%w: item=%d amount=%d stack_limit=%d", errDungeonPickupItemInvalid, definition.ItemID, count, stackLimit)
	}
	// Use the PVF-defined slot range for the item type. Quest items
	// ([quest] stackable) land in slots 177..232 (the quest backpack
	// page), materials in 121..176, etc.
	slotStart := definition.SlotStart
	slotEnd := definition.SlotEnd
	if slotStart <= 0 || slotEnd < slotStart {
		slotStart, slotEnd = 0, dnfrepo.CrystalWarehouseFirstSlot-1
	}
	// Try to merge with an existing stack in the item's slot range.
	// PVF [stack limit] 是这个容器（角色背包 list 0）里该道具的总量上限：额度按容器里
	// 同 id 总量算，容器里已经有该道具的行时不能再开第二行。没写上限的道具沿用旧行为。
	newRowStackLimit := int64(0)
	questSpace := int64(math.MaxInt32)
	if definition.Kind == dungeonDropItemStackable && definition.StackLimit > 0 {
		newRowStackLimit = definition.StackLimit
		questSpace = dnfrepo.ContainerItemSpace(
			inventory.Slots,
			dnfrepo.MainInventoryListType,
			itemID,
			definition.StackLimit,
		)
	}
	if definition.Kind == dungeonDropItemStackable {
		for slot := slotStart; slot <= slotEnd; slot++ {
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
			stack, exists := inventory.Slots[key]
			if !exists || stack.ItemID != itemID || stack.Bind ||
				!currentItemStackExpirationMatches(stack, definition.ExpirationDate) {
				continue
			}
			room := stackLimit - stack.Count
			if room < int64(count) || int64(count) > questSpace {
				continue
			}
			stack, _ = normalizeCurrentPVFItemStack(stack, definition, time.Now().UTC())
			stack.Count += int64(count)
			entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			if stack.Extra == nil {
				stack.Extra = make(map[string]string, 1)
			}
			stack.Extra["last_grant_source"] = "quest_monster_reward"
			inventory.Slots[key] = stack
			return slot, nil
		}
	}
	// Find the first empty slot in the item's range.
	for slot := slotStart; slot <= slotEnd; slot++ {
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(inventory.Slots, dnfrepo.MainInventoryListType, itemID, newRowStackLimit) {
			break
		}
		if _, err := insertCurrentDungeonPickup(inventory, definition, count, slot); err != nil {
			return 0, err
		}
		stack := inventory.Slots[key]
		stack.Extra["source"] = "quest_monster_reward"
		inventory.Slots[key] = stack
		return slot, nil
	}
	return 0, fmt.Errorf("%w: item=%d", errDungeonPickupInventoryFull, definition.ItemID)
}
