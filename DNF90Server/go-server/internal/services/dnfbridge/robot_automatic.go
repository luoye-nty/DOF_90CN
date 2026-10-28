package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/jobmap"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/services/dnfrobot"
)

const (
	automaticRobotProfilePrefix      = "auto-generated"
	automaticRobotAccountPrefix      = "dnf-auto-robot"
	automaticRobotPreferredChannelID = 19
	automaticRobotTownID             = 38
	automaticRobotTownAreaID         = 0
	// The recovered 20260825 robot service provisions a durable type-8 premium
	// row before it ever attempts op85/op87. Current automatic accounts need the
	// same actor permission: op78 with permissionSlot=ffff means "reuse the
	// already projected companion", not "create a companion without a
	// contract". Renew only after expiry so restarts do not extend the row.
	automaticRobotStoreContractSeconds = int64(10 * 365 * 24 * 60 * 60)
	// The planner requires cutoff < eligibility level. This virtual level is
	// never persisted; it lets level-cap robots close PVF main quests whose
	// minimum level is exactly 90 while their durable character level stays 90.
	automaticRobotStoryEligibilityLevel = currentAdventureCharacterLevelCap + 1
)

var automaticRobotNamePrefixes = [...]string{
	"晨星", "月影", "风语", "霜华", "赤焰", "青岚", "星尘", "流光",
	"云歌", "夜航", "银羽", "苍穹", "花火", "山岚", "晴空", "远星",
}

var automaticRobotNameSuffixes = [...]string{
	"剑客", "旅人", "游侠", "勇者", "行者", "守望", "逐风", "寻梦",
	"猎手", "星使", "漫游", "冒险", "先锋", "旅团", "巡游", "歌者",
}

type automaticRobotTownPlacement struct {
	Village     uint8
	Area        uint8
	X           uint16
	Y           uint16
	MinX        uint16
	MaxX        uint16
	MinY        uint16
	MaxY        uint16
	MapPath     string
	DungeonGate bool
	StoreArea   bool
}

// EnsureAutomaticRobotProfile is the zero-touch fleet composition boundary.
// Every index owns a dedicated account and deterministic identity, so a restart
// reuses the same native character instead of manufacturing another hundred.
func (s *Service) EnsureAutomaticRobotProfile(ctx context.Context, index int) (dnfrobotdomain.Profile, error) {
	if s == nil || index < 1 || index > dnfrobot.AutomaticFleetTarget {
		return dnfrobotdomain.Profile{}, fmt.Errorf("%w: invalid automatic robot index", ErrGMRequestInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := fmt.Sprintf("%s-%03d", automaticRobotAccountPrefix, index)
	profileID := fmt.Sprintf("%s-%03d", automaticRobotProfilePrefix, index)
	seed := automaticRobotSeed(index)
	placement := s.automaticRobotTownPlacement(index)
	channels, err := s.automaticRobotChannelIDs()
	if err != nil {
		return dnfrobotdomain.Profile{}, err
	}
	channelID := channels[int(seed%uint64(len(channels)))]

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrobotdomain.Profile{}, ErrGMRepositoryUnavailable
	}
	records, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return dnfrobotdomain.Profile{}, err
	}
	for _, record := range records {
		if record.Stats != nil && record.Stats["delete_flag"] != 0 {
			continue
		}
		record, err = s.ensureAutomaticRobotTownReady(ctx, repositories, record, placement)
		if err != nil {
			return dnfrobotdomain.Profile{}, fmt.Errorf("prepare automatic robot %d existing character %s: %w", index, record.CharacterID, err)
		}
		if profile, ok := automaticRobotProfileFromRecord(profileID, accountID, channelID, byte((seed>>40)&1), placement, record); ok {
			return profile, nil
		}
	}

	// The current fitting-room PVF has complete sets for jobs 0..14. Job 15
	// exists in character.lst but has no authored eight-part set in this client,
	// so zero-touch creation stays inside the complete appearance pool.
	job := int((seed >> 8) % uint64(jobmap.MaxID))
	level := currentAdventureCharacterLevelCap
	var lastErr error
	for attempt := 0; attempt < 16; attempt++ {
		name := automaticRobotName(index, attempt, seed)
		if _, exists, findErr := repositories.Character.FindIDByName(ctx, name); findErr != nil {
			return dnfrobotdomain.Profile{}, findErr
		} else if exists {
			continue
		}
		result, provisionErr := s.GMProvisionRobotCharacter(ctx, GMRobotCharacterProvisionRequest{
			AccountID: accountID, Name: name, Job: job, GrowType: 0, Level: level,
			Village: int(placement.Village), Area: int(placement.Area), X: int(placement.X), Y: int(placement.Y),
		})
		if provisionErr == nil {
			created, found, loadErr := repositories.Character.Load(ctx, strconv.Itoa(int(result.CharacterID)))
			if loadErr != nil {
				return dnfrobotdomain.Profile{}, loadErr
			}
			if !found {
				return dnfrobotdomain.Profile{}, ErrGMRepositoryUnavailable
			}
			created, loadErr = s.ensureAutomaticRobotTownReady(ctx, repositories, created, placement)
			if loadErr != nil {
				return dnfrobotdomain.Profile{}, fmt.Errorf("prepare automatic robot %d created character %s: %w", index, created.CharacterID, loadErr)
			}
			profile, valid := automaticRobotProfileFromRecord(profileID, accountID, channelID, byte((seed>>40)&1), placement, created)
			if !valid {
				return dnfrobotdomain.Profile{}, fmt.Errorf("%w: automatic robot %d created profile projection", ErrGMRequestInvalid, index)
			}
			return profile, nil
		}
		lastErr = provisionErr
		if !errors.Is(provisionErr, ErrGMRequestInvalid) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: no unique automatic robot name is available", ErrGMRequestInvalid)
	}
	return dnfrobotdomain.Profile{}, lastErr
}

func (s *Service) ensureAutomaticRobotTownReady(ctx context.Context, repositories dnfrepo.Group, record dnfrepo.CharacterRecord, placement automaticRobotTownPlacement) (dnfrepo.CharacterRecord, error) {
	if record.Level < currentAdventureCharacterLevelCap {
		if _, err := s.GMSetCharacterLevel(ctx, record.CharacterID, currentAdventureCharacterLevelCap); err != nil {
			return dnfrepo.CharacterRecord{}, err
		}
		current, found, err := repositories.Character.Load(ctx, record.CharacterID)
		if err != nil {
			return dnfrepo.CharacterRecord{}, err
		}
		if !found || current.AccountID != record.AccountID {
			return dnfrepo.CharacterRecord{}, fmt.Errorf("%w: automatic robot level migration identity", ErrGMRequestInvalid)
		}
		record = current
	}
	job, validJob := characterJobByte(record)
	if !validJob {
		return dnfrepo.CharacterRecord{}, fmt.Errorf("%w: automatic robot job %q", ErrGMRequestInvalid, record.Job)
	}
	avatarSet, err := s.automaticRobotAvatarSet(job, uint64(numericCharacterID(record)))
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	appearanceReady := automaticRobotAvatarSetReady(dnfrepo.EquipmentRecord{}, false, avatarSet)
	if !appearanceReady {
		if repositories.Equipment == nil {
			return dnfrepo.CharacterRecord{}, ErrGMRepositoryUnavailable
		}
		equipment, found, equipmentErr := repositories.Equipment.Load(ctx, record.CharacterID)
		if equipmentErr != nil {
			return dnfrepo.CharacterRecord{}, equipmentErr
		}
		appearanceReady = automaticRobotAvatarSetReady(equipment, found, avatarSet)
	}
	storePermissionReady := false
	if repositories.Account != nil {
		account, found, accountErr := repositories.Account.Load(ctx, strings.TrimSpace(record.AccountID))
		if accountErr != nil {
			return dnfrepo.CharacterRecord{}, accountErr
		}
		storePermissionReady = found && premium.Active(account, premium.TypeAuction, time.Now().UTC()) &&
			premium.Active(account, premium.TypePrivateStore, time.Now().UTC())
	}
	if record.Stats != nil && record.Stats[currentDungeonTutorialCompletedKey] == currentDungeonTutorialCompleteFlag &&
		record.Stats["tutorial_reward_progress_38"] == 1 &&
		record.Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] >= int64(currentAdventureCharacterLevelCap) &&
		record.Stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] >= int64(dnfrepo.CurrentCharacterStoryDigestMigrationVersion) &&
		appearanceReady &&
		record.Stats["ex_equip_slot_stat"]&gmAllEquipSlotsMask == gmAllEquipSlotsMask &&
		record.Stats["town_id"] == int64(placement.Village) &&
		record.Stats["area_id"] == int64(placement.Area) &&
		record.Stats["pos_x"] == int64(placement.X) &&
		record.Stats["pos_y"] == int64(placement.Y) &&
		storePermissionReady {
		return record, nil
	}
	if repositories.CharacterSettlement == nil {
		return dnfrepo.CharacterRecord{}, ErrGMRepositoryUnavailable
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	characterID, accountID := record.CharacterID, record.AccountID
	var updated dnfrepo.CharacterRecord
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		current, found, loadErr := tx.Character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found || current.AccountID != accountID || current.Level != currentAdventureCharacterLevelCap {
			return fmt.Errorf("%w: automatic robot baseline found=%t account_match=%t level=%d", ErrGMRequestInvalid, found, current.AccountID == accountID, current.Level)
		}
		if tx.Account == nil {
			return ErrGMRepositoryUnavailable
		}
		account, accountFound, accountErr := tx.Account.Load(ctx, accountID)
		if accountErr != nil {
			return accountErr
		}
		if !accountFound || strings.TrimSpace(account.AccountID) != accountID {
			return fmt.Errorf("%w: automatic robot account identity", ErrGMRequestInvalid)
		}
		if !premium.Active(account, premium.TypeAuction, time.Now().UTC()) ||
			!premium.Active(account, premium.TypePrivateStore, time.Now().UTC()) {
			account = dnfrepo.CloneAccount(account)
			now := time.Now().UTC()
			premium.Upsert(&account, premium.TypeAuction, automaticRobotStoreContractSeconds, 1, now)
			for _, premiumType := range premium.ActivationTypes(premium.TypeAuction) {
				key := premium.MetadataKey(premiumType)
				if saveErr := dnfrepo.SaveAccountMetadataEntry(ctx, tx.Account, account, key, account.Metadata[key], now); saveErr != nil {
					return saveErr
				}
			}
		}
		current = dnfrepo.CloneCharacter(current)
		if current.Stats == nil {
			current.Stats = make(map[string]int64)
		}
		if current.Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] < int64(currentAdventureCharacterLevelCap) {
			currentJob, currentValidJob := characterJobByte(current)
			if !currentValidJob || currentJob != job {
				return fmt.Errorf("%w: automatic robot job %q", ErrGMRequestInvalid, current.Job)
			}
			quests, questFound, questErr := tx.Quest.Load(ctx, characterID)
			if questErr != nil {
				return questErr
			}
			if !questFound {
				quests = dnfrepo.QuestRecord{CharacterID: characterID}
			}
			plan, planErr := catalog.PlanAutoCompleteMainQuests(quests, dnfquest.CharacterEligibility{
				Level: automaticRobotStoryEligibilityLevel, Job: int(currentJob),
				GrowType: int(numericCharacterStatValue(current, "grow_type")),
			}, currentAdventureCharacterLevelCap, 0, time.Now().UTC())
			// A robot profile is filled in without a class change or an
			// awakening run, so the bulk pass may report "nothing eligible",
			// "class change first" or "awakening quest first". All three mean
			// the story digest simply stays where it is.
			if planErr != nil &&
				!errors.Is(planErr, dnfquest.ErrAutoCompleteTargetInvalid) &&
				!errors.Is(planErr, dnfquest.ErrAutoCompleteClassRequired) &&
				!errors.Is(planErr, dnfquest.ErrAutoCompleteAwakeningRequired) {
				return planErr
			}
			plan.Record.CharacterID = characterID
			if planErr == nil && len(plan.ChangedFields) > 0 {
				if saveErr := dnfrepo.SaveQuestFields(ctx, tx.Quest, plan.Record, plan.ChangedFields...); saveErr != nil {
					return saveErr
				}
			}
		}
		current.Stats[currentDungeonTutorialCompletedKey] = currentDungeonTutorialCompleteFlag
		current.Stats["tutorial_reward_progress_38"] = 1
		current.Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] = int64(currentAdventureCharacterLevelCap)
		current.Stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] = int64(dnfrepo.CurrentCharacterStoryDigestMigrationVersion)
		if !appearanceReady {
			if tx.Equipment == nil {
				return ErrGMRepositoryUnavailable
			}
			equipment, equipmentFound, equipmentErr := tx.Equipment.Load(ctx, characterID)
			if equipmentErr != nil {
				return equipmentErr
			}
			if !automaticRobotAvatarSetReady(equipment, equipmentFound, avatarSet) {
				if !equipmentFound {
					equipment = dnfrepo.EquipmentRecord{CharacterID: characterID}
				} else {
					equipment = dnfrepo.CloneEquipment(equipment)
				}
				if equipment.Entries == nil {
					equipment.Entries = make(map[string]dnfrepo.EquipmentEntry)
				}
				for index, itemID := range avatarSet {
					if itemID == 0 {
						continue
					}
					slot := currentAvatarFittingRoomEquipmentSlots[index]
					equipment.Entries[strconv.Itoa(int(slot))] = dnfrepo.EquipmentEntry{
						SlotIndex: slot,
						ItemID:    int64(itemID),
						RawEntry:  buildInitialEquipmentRawEntry(slot, int64(itemID), 0),
						Extra: map[string]string{
							"source":          "automatic_robot_avatar_set",
							"avatar_set_part": currentAvatarFittingRoomPartNames[index],
						},
					}
				}
				equipment.UpdatedAt = time.Now().UTC()
				if saveErr := dnfrepo.SaveEquipmentFields(ctx, tx.Equipment, equipment, dnfrepo.EquipmentFieldEntries); saveErr != nil {
					return saveErr
				}
			}
		}
		current.Stats["ex_equip_slot_stat"] |= gmAllEquipSlotsMask
		current.Stats["town_id"] = int64(placement.Village)
		current.Stats["area_id"] = int64(placement.Area)
		current.Stats["pos_x"] = int64(placement.X)
		current.Stats["pos_y"] = int64(placement.Y)
		current.UpdatedAt = time.Now().UTC()
		if saveErr := dnfrepo.SaveCharacterFields(ctx, tx.Character, current, dnfrepo.CharacterFieldStats); saveErr != nil {
			return saveErr
		}
		updated = current
		return nil
	})
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	return updated, nil
}

// automaticRobotAvatarSet selects one complete PVF-authored eight-part set for
// the robot's native job. The choice is deterministic per character so a
// restart or a failed transaction never rolls another appearance. Each item is
// resolved through the active item catalog and must map back to the exact
// current-client avatar slot expected by the fitting-room table.
func (s *Service) automaticRobotAvatarSet(job byte, seed uint64) ([currentAvatarFittingRoomPartCount]uint32, error) {
	config, err := s.automaticRobotAvatarCatalog()
	if err != nil {
		return [currentAvatarFittingRoomPartCount]uint32{}, err
	}
	candidates := make([][currentAvatarFittingRoomPartCount]uint32, 0, len(config.Sets))
	for _, set := range config.Sets {
		items, ok := set.Jobs[job]
		if ok {
			candidates = append(candidates, items)
		}
	}
	if len(candidates) == 0 {
		// Existing installations can already contain a job introduced after the
		// current fitting-room table. Keep its native creation appearance and
		// finish the remaining robot migration instead of blocking the fleet.
		return [currentAvatarFittingRoomPartCount]uint32{}, nil
	}
	selected := candidates[int(automaticRobotMix(seed, 0x6176617461727365)%uint64(len(candidates)))]
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return [currentAvatarFittingRoomPartCount]uint32{}, err
	}
	for index, itemID := range selected {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			return [currentAvatarFittingRoomPartCount]uint32{}, resolveErr
		}
		rule, ok := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType)
		if definition.Kind != dungeonDropItemEquipment || !ok || rule.class != currentEquipmentPlacementClassAvatar || rule.targetSlot != currentAvatarFittingRoomEquipmentSlots[index] {
			return [currentAvatarFittingRoomPartCount]uint32{}, fmt.Errorf(
				"%w: automatic robot job=%d part=%s item=%d type=%q target=%d",
				errCurrentAvatarFittingRoomUnavailable,
				job,
				currentAvatarFittingRoomPartNames[index],
				itemID,
				definition.EquipmentType,
				rule.targetSlot,
			)
		}
	}
	return selected, nil
}

func automaticRobotAvatarSetReady(equipment dnfrepo.EquipmentRecord, found bool, set [currentAvatarFittingRoomPartCount]uint32) bool {
	required := 0
	for index, itemID := range set {
		if itemID == 0 {
			continue
		}
		required++
		if !found || equipment.Entries[strconv.Itoa(int(currentAvatarFittingRoomEquipmentSlots[index]))].ItemID != int64(itemID) {
			return false
		}
	}
	return required == 0 || found
}

func (s *Service) automaticRobotAvatarCatalog() (currentAvatarFittingRoomConfig, error) {
	if s == nil {
		return currentAvatarFittingRoomConfig{}, errCurrentAvatarFittingRoomUnavailable
	}
	s.automaticRobotAppearanceMu.Lock()
	defer s.automaticRobotAppearanceMu.Unlock()
	if s.automaticRobotAvatarConfig != nil {
		return *s.automaticRobotAvatarConfig, nil
	}
	if s.automaticRobotAvatarLoadErr != nil {
		return currentAvatarFittingRoomConfig{}, s.automaticRobotAvatarLoadErr
	}
	catalog, err := s.currentPVFItemCatalog()
	if err == nil {
		var config currentAvatarFittingRoomConfig
		config, err = loadCurrentAvatarFittingRoomConfig(catalog.source)
		if err == nil {
			s.automaticRobotAvatarConfig = &config
			return config, nil
		}
	}
	s.automaticRobotAvatarLoadErr = err
	return currentAvatarFittingRoomConfig{}, err
}

// EnsureRobotPrivateStoreStock keeps autonomous stores useful without letting
// the robot client invent item ids. It fills only missing rows from real,
// currently tradeable PVF stackables and commits through GMStockRobotCharacter.
func (s *Service) EnsureRobotPrivateStoreStock(ctx context.Context, accountID string, characterID uint16, maximumRows int) error {
	accountID = strings.TrimSpace(accountID)
	if s == nil || accountID == "" || characterID == 0 || maximumRows <= 0 || maximumRows > currentPrivateStoreMaxRows {
		return fmt.Errorf("%w: invalid automatic robot stock request", ErrGMRequestInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	existingRows := 0
	rows, err := s.RobotPrivateStoreRows(ctx, characterID, maximumRows, 1)
	if err == nil {
		existingRows = len(rows)
		if existingRows >= maximumRows {
			return nil
		}
	} else if !errors.Is(err, errRobotPrivateStoreInventoryEmpty) {
		return err
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return ErrGMRepositoryUnavailable
	}
	inventory, found, err := repositories.Inventory.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil {
		return err
	}
	if !found {
		return ErrGMRepositoryUnavailable
	}
	excluded := make(map[uint32]struct{}, len(inventory.Slots))
	for _, stack := range inventory.Slots {
		if stack.ItemID > 0 && uint64(stack.ItemID) <= uint64(^uint32(0)) {
			excluded[uint32(stack.ItemID)] = struct{}{}
		}
	}
	items, err := s.automaticRobotStockItems(uint64(characterID), maximumRows-existingRows, excluded)
	if err != nil {
		return err
	}
	_, err = s.GMStockRobotCharacter(ctx, GMRobotStockRequest{
		AccountID: accountID, CharacterID: characterID, Items: items,
	})
	return err
}

func (s *Service) automaticRobotStockItems(seed uint64, count int, excluded map[uint32]struct{}) ([]GMRobotStockItem, error) {
	if count <= 0 {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	catalog.mu.Lock()
	ids := append([]uint32(nil), catalog.stackableIDs...)
	catalog.mu.Unlock()
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: automatic robot stock catalog is empty", ErrGMRequestInvalid)
	}
	start := int(automaticRobotMix(seed, 0x73746f636b) % uint64(len(ids)))
	items := make([]GMRobotStockItem, 0, count)
	for offset := 0; offset < len(ids) && len(items) < count; offset++ {
		itemID := ids[(start+offset)%len(ids)]
		if _, exists := excluded[itemID]; exists {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil || definition.Kind != dungeonDropItemStackable ||
			!currentDungeonDiscardTradeable(definition) || definition.StackLimit <= 0 ||
			!definition.ExpirationDate.IsZero() || definition.UsablePeriodDays > 0 {
			continue
		}
		limit := uint64(definition.StackLimit)
		amount := uint64(5) + automaticRobotMix(seed, uint64(itemID))%46
		if amount > limit {
			amount = limit
		}
		if amount == 0 || amount > uint64(^uint32(0)) {
			continue
		}
		items = append(items, GMRobotStockItem{ItemID: itemID, Count: uint32(amount)})
		excluded[itemID] = struct{}{}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: no tradeable automatic robot stock candidate", ErrGMRequestInvalid)
	}
	return items, nil
}

func (s *Service) automaticRobotChannelIDs() ([]int, error) {
	s.mu.Lock()
	catalog := s.catalog
	s.mu.Unlock()
	if catalog == nil {
		return nil, errors.New("dnf automatic robot channel catalog is not ready")
	}
	// The shipped local profile exposes channel 19 as its ordinary public town
	// entrance even though the legacy ETC type byte is 11. Prefer that concrete
	// deployment channel so autonomous residents are visible to local players
	// instead of being distributed across unrelated compatibility entrances.
	if preferred, ok := catalog.Channel(automaticRobotPreferredChannelID); ok && preferred.Port > 0 {
		return []int{preferred.ID}, nil
	}
	channels := catalog.Channels()
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		group := strings.ToLower(strings.Trim(strings.TrimSpace(channel.Group), "[]`"))
		// The current town actor and resident-channel projections carry the
		// channel id in one byte.  Higher ids are valid catalog entrances for
		// special modes, but cannot own an autonomous town resident.
		if channel.ID <= 0 || channel.ID > 0xff || channel.Port <= 0 || channel.Type == 2 || channel.Type == 3 ||
			channel.Type == dnfenum.DeathTowerChannelType || channel.Type == dnfenum.TradeChannelType ||
			channel.Type == dnfenum.RaidChannelType || channel.Type == dnfenum.HiddenRaidType ||
			group == dnfenum.GroupCrack || group == dnfenum.GroupDeathTower || group == dnfenum.GroupTrade ||
			group == dnfenum.GroupRaid || group == dnfenum.GroupAttackRaid {
			continue
		}
		ids = append(ids, channel.ID)
	}
	if len(ids) == 0 && len(channels) > 0 {
		if resident, ok := catalog.ResidentFor(channels[0]); ok {
			ids = append(ids, resident.ID)
		}
	}
	sort.Ints(ids)
	if len(ids) == 0 {
		return nil, errors.New("dnf automatic robot has no ordinary town channel")
	}
	return ids, nil
}

func automaticRobotPublicTownPosition(identity string) (int, int) {
	parsed, _ := strconv.ParseUint(strings.TrimSpace(identity), 10, 32)
	if parsed == 0 {
		parsed = automaticRobotSeed(len(identity) + 1)
	}
	return 520 + int(parsed*37%1200), 225 + int(parsed*17%75)
}

func (s *Service) automaticRobotTownPlacement(index int) automaticRobotTownPlacement {
	fallbackX, fallbackY := automaticRobotPublicTownPosition(strconv.Itoa(index))
	fallback := automaticRobotTownPlacement{
		Village: automaticRobotTownID, Area: automaticRobotTownAreaID,
		X: uint16(fallbackX), Y: uint16(fallbackY),
		MinX: uint16(maxInt(0, fallbackX-48)), MaxX: uint16(minInt(0xffff, fallbackX+48)),
		MinY: uint16(maxInt(0, fallbackY-6)), MaxY: uint16(minInt(0xffff, fallbackY+6)),
		MapPath: "fallback", StoreArea: true,
	}
	if s == nil {
		return fallback
	}
	s.townCatalogMu.RLock()
	table := s.townCatalog
	s.townCatalogMu.RUnlock()
	if table == nil {
		return fallback
	}
	placements := automaticRobotPublicTownPlacements(table.Towns())
	if len(placements) == 0 {
		return fallback
	}
	base := placements[(index-1)%len(placements)]
	ordinal := (index - 1) / len(placements)
	seed := automaticRobotSeed(index)
	base.X = automaticRobotCoordinateInRange(base.MinX, base.MaxX, uint64(ordinal)*53+(seed>>8))
	base.Y = automaticRobotCoordinateInRange(base.MinY, base.MaxY, uint64(ordinal)*7+(seed>>24))
	return base
}

func automaticRobotCoordinateInRange(minimum, maximum uint16, salt uint64) uint16 {
	if maximum <= minimum {
		return minimum
	}
	return minimum + uint16(salt%uint64(uint32(maximum)-uint32(minimum)+1))
}

func automaticRobotNPCCorridor(spawns []dnftown.NPCSpawn) (uint16, uint16, uint16, uint16, bool) {
	valid := make([]dnftown.NPCSpawn, 0, len(spawns))
	for _, spawn := range spawns {
		if spawn.X < 0 || spawn.X > 0xffff || spawn.Y < 0 || spawn.Y > 0xffff {
			continue
		}
		valid = append(valid, spawn)
	}
	if len(valid) == 0 {
		return 0, 0, 0, 0, false
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].Y != valid[j].Y {
			return valid[i].Y < valid[j].Y
		}
		return valid[i].X < valid[j].X
	})
	const floorTolerance = int64(20)
	clusters := make([][]dnftown.NPCSpawn, 0, len(valid))
	for _, spawn := range valid {
		if len(clusters) == 0 || spawn.Y-clusters[len(clusters)-1][0].Y > floorTolerance {
			clusters = append(clusters, []dnftown.NPCSpawn{spawn})
			continue
		}
		clusters[len(clusters)-1] = append(clusters[len(clusters)-1], spawn)
	}
	best := clusters[0]
	for _, candidate := range clusters[1:] {
		bestWidth := automaticRobotNPCClusterWidth(best)
		candidateWidth := automaticRobotNPCClusterWidth(candidate)
		if len(candidate) > len(best) || (len(candidate) == len(best) && candidateWidth > bestWidth) {
			best = candidate
		}
	}
	minX, maxX, totalY := best[0].X, best[0].X, int64(0)
	for _, spawn := range best {
		if spawn.X < minX {
			minX = spawn.X
		}
		if spawn.X > maxX {
			maxX = spawn.X
		}
		totalY += spawn.Y
	}
	centerY := totalY / int64(len(best))
	if maxX-minX >= 96 {
		minX += 24
		maxX -= 24
	} else {
		centerX := (minX + maxX) / 2
		minX = maxInt64(0, centerX-48)
		maxX = minInt64(0xffff, centerX+48)
	}
	minY := maxInt64(0, centerY-6)
	maxY := minInt64(0xffff, centerY+6)
	return uint16(minX), uint16(maxX), uint16(minY), uint16(maxY), true
}

func automaticRobotNPCClusterWidth(spawns []dnftown.NPCSpawn) int64 {
	if len(spawns) == 0 {
		return 0
	}
	minimum, maximum := spawns[0].X, spawns[0].X
	for _, spawn := range spawns[1:] {
		if spawn.X < minimum {
			minimum = spawn.X
		}
		if spawn.X > maximum {
			maximum = spawn.X
		}
	}
	return maximum - minimum
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func automaticRobotPublicTownPlacements(towns []dnftown.Town) []automaticRobotTownPlacement {
	// Keep the autonomous population on current public hubs. For every hub we
	// retain its first ordinary plaza plus every declared dungeon entrance;
	// historical/event/mirror/private towns would otherwise dilute 500 robots
	// over more than one hundred barely visited area records.
	publicTowns := map[int64]struct{}{
		6: {}, 9: {}, 11: {}, 12: {}, 13: {}, 14: {}, 17: {}, 20: {}, 22: {},
		30: {}, 31: {}, 38: {}, 39: {}, 40: {}, 41: {}, 42: {}, 43: {},
	}
	excludedPaths := []string{"seria", "guildagit", "fair_pvp", "twin_raid", "fiendwar", "return_town", "death_tower", "tournament", "/test", "test/"}
	seenMaps := make(map[string]struct{})
	normalTownSelected := make(map[int64]bool)
	placements := make([]automaticRobotTownPlacement, 0, 128)
	for _, town := range towns {
		if town.ID < 0 || town.ID > 0xff {
			continue
		}
		if _, public := publicTowns[town.ID]; !public {
			continue
		}
		for _, area := range town.Areas {
			if area.ID < 0 || area.ID > 0xff || area.MinLevel > currentAdventureCharacterLevelCap ||
				(area.Kind != "normal" && area.Kind != "dungeon_gate") || len(area.NPCs) == 0 {
				continue
			}
			path := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(area.MapPath), "\\", "/"))
			if path == "" {
				continue
			}
			blocked := false
			for _, marker := range excludedPaths {
				if strings.Contains(path, marker) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			if area.Kind == "normal" && normalTownSelected[town.ID] {
				continue
			}
			if _, duplicated := seenMaps[path]; duplicated {
				continue
			}
			minX, maxX, minY, maxY, ok := automaticRobotNPCCorridor(area.NPCs)
			if !ok {
				continue
			}
			seenMaps[path] = struct{}{}
			if area.Kind == "normal" {
				normalTownSelected[town.ID] = true
			}
			placements = append(placements, automaticRobotTownPlacement{
				Village: uint8(town.ID), Area: uint8(area.ID),
				X:    automaticRobotCoordinateInRange(minX, maxX, uint64(town.ID)<<8|uint64(area.ID)),
				Y:    automaticRobotCoordinateInRange(minY, maxY, uint64(town.ID)<<16|uint64(area.ID)),
				MinX: minX, MaxX: maxX, MinY: minY, MaxY: maxY, MapPath: path,
				DungeonGate: area.Kind == "dungeon_gate",
				// The current PVF marks the large Hendon Myre and West Coast
				// plazas as dungeon gates because they also own entrances.  The
				// recovered robot service used an explicit store-area whitelist;
				// retain that semantic by allowing ordinary plazas and large
				// multi-NPC composite hubs while excluding small entrance maps.
				StoreArea: area.Kind == "normal" || len(area.NPCs) >= 5,
			})
		}
	}
	sort.Slice(placements, func(i, j int) bool {
		if placements[i].Village != placements[j].Village {
			return placements[i].Village < placements[j].Village
		}
		if placements[i].Area != placements[j].Area {
			return placements[i].Area < placements[j].Area
		}
		return placements[i].MapPath < placements[j].MapPath
	})
	return placements
}

// currentAutomaticRobotPublicTownAreaAllowed limits the authenticated robot
// bootstrap shortcut to the exact public-town catalog used by the zero-touch
// scheduler. Real players must continue to prove cross-town travel through the
// current PVF portal topology; this exception only lets a private HMAC-bound
// robot materialize at its preassigned plaza or dungeon entrance after login.
func (s *Service) currentAutomaticRobotPublicTownAreaAllowed(townID, areaID byte) bool {
	if s == nil {
		return false
	}
	s.townCatalogMu.RLock()
	table := s.townCatalog
	s.townCatalogMu.RUnlock()
	if table == nil {
		return false
	}
	for _, placement := range automaticRobotPublicTownPlacements(table.Towns()) {
		if placement.Village == townID && placement.Area == areaID {
			return true
		}
	}
	return false
}

func automaticRobotProfileFromRecord(profileID, accountID string, channelID int, moveType byte, placement automaticRobotTownPlacement, record dnfrepo.CharacterRecord) (dnfrobotdomain.Profile, bool) {
	characterID, err := strconv.ParseUint(strings.TrimSpace(record.CharacterID), 10, 16)
	if err != nil || characterID == 0 || record.AccountID != accountID {
		return dnfrobotdomain.Profile{}, false
	}
	village := int64(automaticRobotTownID)
	area := int64(automaticRobotTownAreaID)
	defaultX, defaultY := automaticRobotPublicTownPosition(record.CharacterID)
	x := int64(defaultX)
	y := int64(defaultY)
	if record.Stats != nil {
		village = automaticRobotCoordinate(record.Stats["town_id"], village, 0xff)
		area = automaticRobotCoordinate(record.Stats["area_id"], area, 0xff)
		x = automaticRobotCoordinate(record.Stats["pos_x"], x, 0xffff)
		y = automaticRobotCoordinate(record.Stats["pos_y"], y, 0xffff)
	}
	return dnfrobotdomain.Profile{
		ID: profileID, AccountID: accountID, CharacterID: uint16(characterID),
		ChannelID: channelID, AutoStart: false,
		Village: uint8(village), Area: uint8(area), X: uint16(x), Y: uint16(y),
		MoveType:      moveType,
		MoveBoundsSet: true, MoveMinX: placement.MinX, MoveMaxX: placement.MaxX,
		MoveMinY: placement.MinY, MoveMaxY: placement.MaxY,
		AutomaticStoreDisabled: !placement.StoreArea,
	}, true
}

func automaticRobotCoordinate(value, fallback, maximum int64) int64 {
	if value < 0 || value > maximum || (value == 0 && fallback != 0) {
		return fallback
	}
	return value
}

func automaticRobotSeed(index int) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(fmt.Sprintf("dnf90-automatic-robot-%03d", index)))
	return hash.Sum64()
}

func automaticRobotName(index, attempt int, seed uint64) string {
	mixed := automaticRobotMix(seed, uint64(attempt+1))
	prefix := automaticRobotNamePrefixes[mixed%uint64(len(automaticRobotNamePrefixes))]
	suffix := automaticRobotNameSuffixes[(mixed>>8)%uint64(len(automaticRobotNameSuffixes))]
	return fmt.Sprintf("%s%s%03d", prefix, suffix, index)
}

func automaticRobotMix(seed, salt uint64) uint64 {
	value := seed ^ salt*0x9e3779b97f4a7c15
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	return value ^ (value >> 31)
}

var _ dnfrobot.AutomaticFleetProvider = (*Service)(nil)
var _ dnfrobot.PrivateStoreStocker = (*Service)(nil)
