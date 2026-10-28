package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	gmGameplaySettingsScope          = "global:dnf90:gm-gameplay"
	gmHellEpicDropRatePercentKey     = "hell_epic_drop_rate_percent"
	gmHellEpicDropRateDefaultPercent = 100
	gmHellEpicDropRateMinimumPercent = 0
	gmHellEpicDropRateMaximumPercent = 15000

	// gmEpicDropRatePotionTestMultiplierKey 是"城镇增益药剂"的**临时测试倍率**。
	//
	// 服主 2026-10-03 要求："我需要测试它真的生效，先改一下倍率吧？后面再改回从 pvf 中读取。"
	// 药剂真实倍率只有 2 倍（PVF 1136 的 [param] 100 ⇒ 100+100=200%），肉眼根本看不出
	// "史诗是不是真的掉得更多了"，所以给它一个能被 GM 随手拧大的旋钮：
	//
	//	0        = 关闭（生产口径：用 PVF 表里查出来的倍数）
	//	2000     = 测试用 20 倍
	//
	// 它**只替换倍数**，不绕过任何前置条件：药剂窗口没过期（statbuff.ActiveEpicDropRate）
	// 才可能生效，所以这个旋钮本身仍然在测"药剂到底有没有用"。
	// 测完把它设回 0 即可，不需要重新编译。
	gmEpicDropRatePotionTestMultiplierKey            = "epic_drop_rate_potion_test_multiplier_percent"
	gmEpicDropRatePotionTestMultiplierDefaultPercent = 0
	gmEpicDropRatePotionTestMultiplierMinimumPercent = 0
	gmEpicDropRatePotionTestMultiplierMaximumPercent = 100000
)

type GMDungeonAccessState struct {
	Character                   GMCharacterSummary `json:"character"`
	DifficultyDungeons          int                `json:"difficulty_dungeons"`
	DifficultyDungeonsUnlocked  int                `json:"difficulty_dungeons_unlocked"`
	DifficultyDungeonsUpdated   int                `json:"difficulty_dungeons_updated,omitempty"`
	HellAreas                   int                `json:"hell_areas"`
	HellPermissionQuests        int                `json:"hell_permission_quests"`
	HellPermissionQuestsCleared int                `json:"hell_permission_quests_cleared"`
	HellPermissionQuestsUpdated int                `json:"hell_permission_quests_updated,omitempty"`
}

type GMGameplaySettings struct {
	HellEpicDropRatePercent int       `json:"hell_epic_drop_rate_percent"`
	MinimumPercent          int       `json:"minimum_percent"`
	MaximumPercent          int       `json:"maximum_percent"`
	UpdatedAt               time.Time `json:"updated_at,omitempty"`
	// EpicDropRatePotionTestMultiplierPercent 是"城镇增益药剂"的临时测试倍率
	// （0 = 关闭，按 PVF 表查出来的倍数走）。见上面常量的说明。
	EpicDropRatePotionTestMultiplierPercent int `json:"epic_drop_rate_potion_test_multiplier_percent"`
	PotionTestMultiplierMinimumPercent      int `json:"potion_test_multiplier_minimum_percent"`
	PotionTestMultiplierMaximumPercent      int `json:"potion_test_multiplier_maximum_percent"`
}

type gmDungeonDifficultyTarget struct {
	DungeonID uint32
	MaxState  byte
}

func gmDungeonDifficultyTargets(table *worldmap.Table) []gmDungeonDifficultyTarget {
	if table == nil {
		return nil
	}
	targets := make([]gmDungeonDifficultyTarget, 0)
	seen := make(map[uint32]struct{})
	appendDungeon := func(dungeon worldmap.Dungeon) {
		if dungeon.ID <= 0 || uint64(dungeon.ID) > uint64(^uint32(0)) {
			return
		}
		id := uint32(dungeon.ID)
		if _, exists := seen[id]; exists {
			return
		}
		maxState := currentDungeonMaxClearState(dungeon)
		if maxState == 0 {
			return
		}
		seen[id] = struct{}{}
		targets = append(targets, gmDungeonDifficultyTarget{DungeonID: id, MaxState: maxState})
	}
	for _, dungeon := range table.Dungeons() {
		appendDungeon(dungeon)
	}
	sort.SliceStable(targets, func(left, right int) bool {
		return targets[left].DungeonID < targets[right].DungeonID
	})
	return targets
}

func gmHellPermissionQuestIDs(table *worldmap.Table) ([]int64, int) {
	if table == nil {
		return nil, 0
	}
	seen := make(map[int64]struct{})
	questIDs := make([]int64, 0)
	hellAreas := 0
	for _, area := range table.Areas() {
		if !area.HellDungeon.Set || area.HellDungeon.Value == 0 || len(area.HellQuestIDs) == 0 {
			continue
		}
		hellAreas++
		for _, questID := range area.HellQuestIDs {
			if questID <= 0 {
				continue
			}
			if _, exists := seen[questID]; exists {
				continue
			}
			seen[questID] = struct{}{}
			questIDs = append(questIDs, questID)
		}
	}
	sort.SliceStable(questIDs, func(left, right int) bool { return questIDs[left] < questIDs[right] })
	return questIDs, hellAreas
}

type gmHellPermissionAreaUnlock struct {
	AreaID  int64
	QuestID int64
}

// gmHellPermissionAreaUnlocks lists one durable permission row per gated Hell
// area: the area id with the first positive [hell quest] that proves it.
func gmHellPermissionAreaUnlocks(table *worldmap.Table) []gmHellPermissionAreaUnlock {
	if table == nil {
		return nil
	}
	unlocks := make([]gmHellPermissionAreaUnlock, 0)
	for _, area := range table.Areas() {
		if area.ID <= 0 || !area.HellDungeon.Set || area.HellDungeon.Value == 0 {
			continue
		}
		for _, questID := range area.HellQuestIDs {
			if questID <= 0 {
				continue
			}
			unlocks = append(unlocks, gmHellPermissionAreaUnlock{AreaID: area.ID, QuestID: questID})
			break
		}
	}
	sort.SliceStable(unlocks, func(left, right int) bool { return unlocks[left].AreaID < unlocks[right].AreaID })
	return unlocks
}

func (s *Service) GMDungeonAccess(ctx context.Context, characterID string) (GMDungeonAccessState, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.DungeonPermission == nil || repositories.Quest == nil {
		return GMDungeonAccessState{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	if !found {
		return GMDungeonAccessState{}, ErrGMCharacterNotFound
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	targets := gmDungeonDifficultyTargets(table)
	permission, _, err := repositories.DungeonPermission.Load(ctx, characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	persisted := make(map[uint32]byte, len(permission.Entries))
	for _, entry := range permission.Entries {
		if entry.ClearState > persisted[entry.DungeonID] {
			persisted[entry.DungeonID] = entry.ClearState
		}
	}
	unlocked := 0
	for _, target := range targets {
		if persisted[target.DungeonID] >= target.MaxState {
			unlocked++
		}
	}
	hellQuestIDs, hellAreas := gmHellPermissionQuestIDs(table)
	questRecord, _, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	hellCleared := 0
	for _, questID := range hellQuestIDs {
		if state, found := gmQuestState(questRecord, questID); found && gmQuestStatusCompleted(state.Status) {
			hellCleared++
		}
	}
	return GMDungeonAccessState{
		Character:                   s.gmCharacterSummary(character),
		DifficultyDungeons:          len(targets),
		DifficultyDungeonsUnlocked:  unlocked,
		HellAreas:                   hellAreas,
		HellPermissionQuests:        len(hellQuestIDs),
		HellPermissionQuestsCleared: hellCleared,
	}, nil
}

func (s *Service) GMUnlockAllDungeonDifficulties(ctx context.Context, characterID string) (GMDungeonAccessState, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMDungeonAccessState{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.DungeonPermission == nil {
		return GMDungeonAccessState{}, ErrGMUnavailable
	}
	if _, found, loadErr := repositories.Character.Load(ctx, characterID); loadErr != nil {
		return GMDungeonAccessState{}, loadErr
	} else if !found {
		return GMDungeonAccessState{}, ErrGMCharacterNotFound
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	updated := 0
	for _, target := range gmDungeonDifficultyTargets(table) {
		_, changed, upsertErr := repositories.DungeonPermission.UpsertMax(ctx, characterID, target.DungeonID, target.MaxState)
		if upsertErr != nil {
			return GMDungeonAccessState{}, fmt.Errorf("unlock dungeon %d difficulty: %w", target.DungeonID, upsertErr)
		}
		if changed {
			updated++
		}
	}
	state, err := s.GMDungeonAccess(ctx, characterID)
	state.DifficultyDungeonsUpdated = updated
	return state, err
}

func (s *Service) GMUnlockAllHellChallenges(ctx context.Context, characterID string) (GMDungeonAccessState, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMDungeonAccessState{}, ErrGMCharacterOnline
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	questIDs, _ := gmHellPermissionQuestIDs(table)
	areaUnlocks := gmHellPermissionAreaUnlocks(table)
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMDungeonAccessState{}, ErrGMUnavailable
	}
	updated := 0
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil || tx.HellPartyPermission == nil {
			return ErrGMUnavailable
		}
		if _, found, loadErr := tx.Character.Load(ctx, characterID); loadErr != nil {
			return loadErr
		} else if !found {
			return ErrGMCharacterNotFound
		}
		record, found, loadErr := tx.Quest.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			record = dnfrepo.QuestRecord{CharacterID: characterID}
		} else {
			record = dnfrepo.CloneQuest(record)
		}
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		now := time.Now().UTC()
		for _, questID := range questIDs {
			state, completed := gmQuestState(record, questID)
			if completed && gmQuestStatusCompleted(state.Status) {
				continue
			}
			if state.Extra == nil {
				state.Extra = make(map[string]string)
			}
			state.Status = "completed"
			state.ProgressValue = 0
			state.UpdatedAt = now
			state.Extra["gm_all_hell_unlocked"] = "true"
			state.Extra["gm_rewards_granted"] = "false"
			record.States[questID] = state
			delete(record.Progress, questID)
			updated++
		}
		// The dungeon-entry gate reads the durable region permission, which the
		// normal quest finish path writes. Completing the quests here bypasses
		// that path, so the same rows must be written explicitly. This also
		// repairs characters whose quests were already marked completed by an
		// earlier unlock that never wrote them.
		for _, unlock := range areaUnlocks {
			if _, _, upsertErr := tx.HellPartyPermission.Upsert(ctx, characterID, unlock.AreaID, unlock.QuestID); upsertErr != nil {
				return upsertErr
			}
		}
		if updated == 0 {
			return nil
		}
		record.UpdatedAt = now
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, record, dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress)
	})
	if err != nil {
		return GMDungeonAccessState{}, err
	}
	state, err := s.GMDungeonAccess(ctx, characterID)
	state.HellPermissionQuestsUpdated = updated
	return state, err
}

func (s *Service) GMGameplaySettings(ctx context.Context) (GMGameplaySettings, error) {
	s.gmGameplaySettingsMu.Lock()
	defer s.gmGameplaySettingsMu.Unlock()
	return s.gmGameplaySettingsLocked(ctx)
}

func (s *Service) gmGameplaySettingsLocked(ctx context.Context) (GMGameplaySettings, error) {
	if s.gmGameplaySettingsReady {
		return newGMGameplaySettings(s.gmHellEpicDropRatePct, s.gmEpicDropRatePotionTestMultiplierPct,
			s.gmGameplaySettingsAt), nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return GMGameplaySettings{}, ErrGMUnavailable
	}
	record, found, err := repositories.Settings.Load(ctx, gmGameplaySettingsScope)
	if err != nil {
		return GMGameplaySettings{}, err
	}
	percent := gmHellEpicDropRateDefaultPercent
	testMultiplier := gmEpicDropRatePotionTestMultiplierDefaultPercent
	if found {
		raw := strings.TrimSpace(record.Values[gmHellEpicDropRatePercentKey])
		if raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed < gmHellEpicDropRateMinimumPercent || parsed > gmHellEpicDropRateMaximumPercent {
				return GMGameplaySettings{}, fmt.Errorf("%w: stored hell epic drop rate is invalid", ErrGMInvalidRequest)
			}
			percent = parsed
		}
		rawTest := strings.TrimSpace(record.Values[gmEpicDropRatePotionTestMultiplierKey])
		if rawTest != "" {
			parsed, parseErr := strconv.Atoi(rawTest)
			if parseErr != nil || parsed < gmEpicDropRatePotionTestMultiplierMinimumPercent ||
				parsed > gmEpicDropRatePotionTestMultiplierMaximumPercent {
				return GMGameplaySettings{}, fmt.Errorf("%w: stored epic potion test multiplier is invalid", ErrGMInvalidRequest)
			}
			testMultiplier = parsed
		}
	}
	s.gmHellEpicDropRatePct = percent
	s.gmEpicDropRatePotionTestMultiplierPct = testMultiplier
	s.gmGameplaySettingsAt = record.UpdatedAt
	s.gmGameplaySettingsReady = true
	return newGMGameplaySettings(percent, testMultiplier, record.UpdatedAt), nil
}

// gmGameplaySettingsFromRecord 从一个设置记录里解析出两个值（掉落率 + 药剂测试倍率）。
// 加载与写入两条路都走它，避免"只写了一个键、另一个键的缓存还是旧值"这类偏差。
func gmGameplaySettingsFromRecord(record dnfrepo.SettingsRecord, found bool) (int, int, error) {
	percent := gmHellEpicDropRateDefaultPercent
	testMultiplier := gmEpicDropRatePotionTestMultiplierDefaultPercent
	if !found {
		return percent, testMultiplier, nil
	}
	raw := strings.TrimSpace(record.Values[gmHellEpicDropRatePercentKey])
	if raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < gmHellEpicDropRateMinimumPercent || parsed > gmHellEpicDropRateMaximumPercent {
			return 0, 0, fmt.Errorf("%w: stored hell epic drop rate is invalid", ErrGMInvalidRequest)
		}
		percent = parsed
	}
	rawTest := strings.TrimSpace(record.Values[gmEpicDropRatePotionTestMultiplierKey])
	if rawTest != "" {
		parsed, parseErr := strconv.Atoi(rawTest)
		if parseErr != nil || parsed < gmEpicDropRatePotionTestMultiplierMinimumPercent ||
			parsed > gmEpicDropRatePotionTestMultiplierMaximumPercent {
			return 0, 0, fmt.Errorf("%w: stored epic potion test multiplier is invalid", ErrGMInvalidRequest)
		}
		testMultiplier = parsed
	}
	return percent, testMultiplier, nil
}

func (s *Service) GMSetHellEpicDropRate(ctx context.Context, percent int) (GMGameplaySettings, error) {
	if percent < gmHellEpicDropRateMinimumPercent || percent > gmHellEpicDropRateMaximumPercent {
		return GMGameplaySettings{}, fmt.Errorf("%w: hell epic drop rate percent must be between %d and %d", ErrGMInvalidRequest, gmHellEpicDropRateMinimumPercent, gmHellEpicDropRateMaximumPercent)
	}
	s.gmGameplaySettingsMu.Lock()
	defer s.gmGameplaySettingsMu.Unlock()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return GMGameplaySettings{}, ErrGMUnavailable
	}
	record, found, err := repositories.Settings.Load(ctx, gmGameplaySettingsScope)
	if err != nil {
		return GMGameplaySettings{}, err
	}
	if !found {
		record = dnfrepo.SettingsRecord{Scope: gmGameplaySettingsScope}
	} else {
		record = dnfrepo.CloneSettings(record)
	}
	if record.Values == nil {
		record.Values = make(map[string]string)
	}
	record.Values[gmHellEpicDropRatePercentKey] = strconv.Itoa(percent)
	record.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveSettingsFields(ctx, repositories.Settings, record, dnfrepo.SettingsFieldValues); err != nil {
		return GMGameplaySettings{}, err
	}
	storedPercent, testMultiplier, err := gmGameplaySettingsFromRecord(record, true)
	if err != nil {
		return GMGameplaySettings{}, err
	}
	s.gmHellEpicDropRatePct = storedPercent
	s.gmEpicDropRatePotionTestMultiplierPct = testMultiplier
	s.gmGameplaySettingsAt = record.UpdatedAt
	s.gmGameplaySettingsReady = true
	return newGMGameplaySettings(storedPercent, testMultiplier, record.UpdatedAt), nil
}

// GMSetEpicDropRatePotionTestMultiplier 拧"城镇增益药剂临时测试倍率"那个旋钮（0 = 关闭）。
//
// 只改倍数，不动 GM 全局掉落率，也不绕过"药剂窗口必须还在生效"这个前置条件。
func (s *Service) GMSetEpicDropRatePotionTestMultiplier(ctx context.Context, percent int) (GMGameplaySettings, error) {
	if percent < gmEpicDropRatePotionTestMultiplierMinimumPercent ||
		percent > gmEpicDropRatePotionTestMultiplierMaximumPercent {
		return GMGameplaySettings{}, fmt.Errorf(
			"%w: epic potion test multiplier must be between %d and %d",
			ErrGMInvalidRequest, gmEpicDropRatePotionTestMultiplierMinimumPercent,
			gmEpicDropRatePotionTestMultiplierMaximumPercent)
	}
	s.gmGameplaySettingsMu.Lock()
	defer s.gmGameplaySettingsMu.Unlock()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return GMGameplaySettings{}, ErrGMUnavailable
	}
	record, found, err := repositories.Settings.Load(ctx, gmGameplaySettingsScope)
	if err != nil {
		return GMGameplaySettings{}, err
	}
	if !found {
		record = dnfrepo.SettingsRecord{Scope: gmGameplaySettingsScope}
	} else {
		record = dnfrepo.CloneSettings(record)
	}
	if record.Values == nil {
		record.Values = make(map[string]string)
	}
	record.Values[gmEpicDropRatePotionTestMultiplierKey] = strconv.Itoa(percent)
	record.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveSettingsFields(ctx, repositories.Settings, record, dnfrepo.SettingsFieldValues); err != nil {
		return GMGameplaySettings{}, err
	}
	storedPercent, testMultiplier, err := gmGameplaySettingsFromRecord(record, true)
	if err != nil {
		return GMGameplaySettings{}, err
	}
	s.gmHellEpicDropRatePct = storedPercent
	s.gmEpicDropRatePotionTestMultiplierPct = testMultiplier
	s.gmGameplaySettingsAt = record.UpdatedAt
	s.gmGameplaySettingsReady = true
	return newGMGameplaySettings(storedPercent, testMultiplier, record.UpdatedAt), nil
}

func newGMGameplaySettings(percent int, testMultiplier int, updatedAt time.Time) GMGameplaySettings {
	return GMGameplaySettings{
		HellEpicDropRatePercent:                 percent,
		MinimumPercent:                          gmHellEpicDropRateMinimumPercent,
		MaximumPercent:                          gmHellEpicDropRateMaximumPercent,
		EpicDropRatePotionTestMultiplierPercent: testMultiplier,
		PotionTestMultiplierMinimumPercent:      gmEpicDropRatePotionTestMultiplierMinimumPercent,
		PotionTestMultiplierMaximumPercent:      gmEpicDropRatePotionTestMultiplierMaximumPercent,
		UpdatedAt:                               updatedAt,
	}
}

func scaleGMHellEpicRarityRate(base int64, percent int) int64 {
	if base <= 0 || percent <= 0 {
		return 0
	}
	value := base * int64(percent) / 100
	if value > int64(dnfhellparty.RarityRollDenominator) {
		value = int64(dnfhellparty.RarityRollDenominator)
	}
	return value
}

func gmAdjustHellEpicRarity(rarity int64, baseEpicRate int64, percent int, roll uint32) (int64, bool) {
	if baseEpicRate < 0 {
		baseEpicRate = 0
	}
	denominator := int64(dnfhellparty.RarityRollDenominator)
	if baseEpicRate > denominator {
		baseEpicRate = denominator
	}
	target := scaleGMHellEpicRarityRate(baseEpicRate, percent)
	if rarity == 4 {
		if target >= baseEpicRate {
			return rarity, true
		}
		if baseEpicRate == 0 {
			return rarity, false
		}
		return rarity, int64(roll) < target*denominator/baseEpicRate
	}
	if target <= baseEpicRate || baseEpicRate >= denominator {
		return rarity, true
	}
	extraThreshold := (target - baseEpicRate) * denominator / (denominator - baseEpicRate)
	if int64(roll) < extraThreshold {
		return 4, true
	}
	return rarity, true
}

func (s *Service) currentGMHellEpicDropRatePercent() int {
	settings, err := s.GMGameplaySettings(context.Background())
	if err != nil {
		s.logWarn("dnf90 GM hell epic drop setting unavailable; using 100% PVF default", "error", err)
		return gmHellEpicDropRateDefaultPercent
	}
	return settings.HellEpicDropRatePercent
}
