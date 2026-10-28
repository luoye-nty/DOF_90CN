package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
)

const currentHellPartyDropChanceDenominator = 1001

const (
	// The 90CN PVF owns equipment rarity and level selection, but the retail
	// server kept the invitation-orb jackpot outside Script.pvf. Contemporary
	// 90-era aggregate records put it at roughly 0.54% per Hell clear.
	currentDungeonHellOrbRollDenominator = 1_000_000
	currentDungeonHellOrbChance          = 5_400
	// 阴沉的深渊宝珠 for dungeons below level 86, 地轨中心深渊宝珠 from 86 up.
	currentDungeonHellGenericOrbItemID = uint32(10146492)
	currentDungeonHellMetroOrbItemID   = uint32(10159141)
	currentDungeonHellOrbDropSource    = "hell_invitation_orb"
)

var errCurrentHellPartyDropUnavailable = errors.New("dnf hell party drop assets are unavailable")

// currentDungeonHellPartyOrbItemID picks the era-correct orb family for the
// dungeon's actor level.
func currentDungeonHellPartyOrbItemID(level int) uint32 {
	if level >= 86 {
		return currentDungeonHellMetroOrbItemID
	}
	return currentDungeonHellGenericOrbItemID
}

type currentDungeonHellPartyFragmentCandidate struct {
	EquipmentID uint32
	FragmentID  uint32
	Weight      uint32
}

type currentDungeonHellPartySelectedDrop struct {
	ItemID uint32
	Amount uint32
	Source string
}

func (s *Service) currentHellPartyDropAssets() (*dnfhellparty.DropCatalog, map[int][]currentDungeonHellPartyFragmentCandidate, error) {
	if s == nil {
		return nil, nil, errCurrentHellPartyDropUnavailable
	}
	s.hellPartyDropMu.Lock()
	defer s.hellPartyDropMu.Unlock()
	if s.hellPartyDropCatalog != nil || s.hellPartyDropCatalogLoadErr != nil {
		return s.hellPartyDropCatalog, s.hellPartyFragmentPools, s.hellPartyDropCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.hellPartyDropCatalogLoadErr = err
		return nil, nil, err
	}
	dropCatalog, err := dnfhellparty.LoadDropCatalog(archive)
	if err != nil {
		s.hellPartyDropCatalogLoadErr = err
		return nil, nil, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		s.hellPartyDropCatalogLoadErr = err
		return nil, nil, err
	}
	epicBook, err := s.currentEpicBookCatalog()
	if err != nil {
		s.hellPartyDropCatalogLoadErr = err
		return nil, nil, err
	}
	fragmentPools := make(map[int][]currentDungeonHellPartyFragmentCandidate)
	for _, entry := range epicBook.Entries() {
		equipment, equipmentErr := items.ResolveItem(entry.EquipmentID)
		fragment, fragmentErr := items.ResolveItem(entry.FragmentID)
		if equipmentErr != nil || fragmentErr != nil || equipment.Kind != dungeonDropItemEquipment ||
			equipment.Rarity != 4 || equipment.MinimumLevel <= 0 || equipment.MinimumLevel > 200 ||
			fragment.Kind != dungeonDropItemStackable {
			err = fmt.Errorf("%w: equipment=%d fragment=%d equipment_error=%v fragment_error=%v",
				errCurrentHellPartyDropUnavailable, entry.EquipmentID, entry.FragmentID, equipmentErr, fragmentErr)
			s.hellPartyDropCatalogLoadErr = err
			return nil, nil, err
		}
		weight := equipment.CreationRate
		if weight <= 0 {
			weight = 1
		}
		if weight > math.MaxUint32 {
			weight = math.MaxUint32
		}
		level := int(equipment.MinimumLevel)
		fragmentPools[level] = append(fragmentPools[level], currentDungeonHellPartyFragmentCandidate{
			EquipmentID: entry.EquipmentID,
			FragmentID:  entry.FragmentID,
			Weight:      uint32(weight),
		})
	}
	if len(fragmentPools) == 0 {
		err = fmt.Errorf("%w: epic fragment candidate pool is empty", errCurrentHellPartyDropUnavailable)
		s.hellPartyDropCatalogLoadErr = err
		return nil, nil, err
	}
	for level := range fragmentPools {
		sort.Slice(fragmentPools[level], func(i, j int) bool {
			return fragmentPools[level][i].EquipmentID < fragmentPools[level][j].EquipmentID
		})
	}
	s.hellPartyDropCatalog = dropCatalog
	s.hellPartyFragmentPools = fragmentPools
	return s.hellPartyDropCatalog, s.hellPartyFragmentPools, nil
}

func (s *Service) preloadHellPartyDropCatalog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	catalog, pools, err := s.currentHellPartyDropAssets()
	if err != nil {
		return fmt.Errorf("preload current hell party drop catalog: %w", err)
	}
	probabilityRows, rarityRows, gradeRows := catalog.Snapshot()
	candidateCount := 0
	for _, candidates := range pools {
		candidateCount += len(candidates)
	}
	s.logPacketEvent("dnf-hell-party-drop-catalog-loaded",
		"probability_rows", probabilityRows,
		"rarity_rows", rarityRows,
		"grade_rows", gradeRows,
		"fragment_levels", len(pools),
		"fragment_candidates", candidateCount,
		"elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

func currentHellPartyEpicLevels(monsterLevel int) []int {
	if monsterLevel < 50 {
		monsterLevel = 50
	}
	// 史诗碎片池最高只有 90 档（图鉴里就是 65/70/75/80/85/90 六档）。
	// 所以「怪物等级 > 90」要按 90 处理，而不是直接返回 {90}：
	// 直接返回 {90} 会让所有 91 级以上的深渊（例如红色魔女之森 basis level = 92）
	// 永远刷不出 85 级史诗 —— 但 85 档池子里有 138 件，本来是应该掉的。
	// 钳到 90 之后，92 级副本得到 lower=85 / upper=90，与 90 级副本一致。
	if monsterLevel > 90 {
		monsterLevel = 90
	}
	upper := ((monsterLevel + 4) / 5) * 5
	if upper < 50 {
		upper = 50
	}
	lower := upper - 5
	if lower < 50 {
		lower = 50
	}
	if lower == upper {
		return []int{upper}
	}
	return []int{lower, upper}
}

func selectCurrentHellPartyFragmentCandidate(
	rng *currentDungeonDropLCG,
	pools map[int][]currentDungeonHellPartyFragmentCandidate,
	monsterLevel int,
) (currentDungeonHellPartyFragmentCandidate, bool) {
	if rng == nil {
		return currentDungeonHellPartyFragmentCandidate{}, false
	}
	candidates := make([]currentDungeonHellPartyFragmentCandidate, 0)
	for _, level := range currentHellPartyEpicLevels(monsterLevel) {
		candidates = append(candidates, pools[level]...)
	}
	return selectCurrentHellPartyFragmentCandidateFromPool(rng, candidates)
}

func selectCurrentHellPartyFragmentCandidateFromPool(
	rng *currentDungeonDropLCG,
	candidates []currentDungeonHellPartyFragmentCandidate,
) (currentDungeonHellPartyFragmentCandidate, bool) {
	if rng == nil {
		return currentDungeonHellPartyFragmentCandidate{}, false
	}
	var total uint64
	for _, candidate := range candidates {
		if candidate.FragmentID == 0 || candidate.Weight == 0 || math.MaxUint64-total < uint64(candidate.Weight) {
			continue
		}
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		return currentDungeonHellPartyFragmentCandidate{}, false
	}
	want := uint64(rng.NextUint32()) % total
	var cumulative uint64
	for _, candidate := range candidates {
		if candidate.FragmentID == 0 || candidate.Weight == 0 || math.MaxUint64-cumulative < uint64(candidate.Weight) {
			continue
		}
		cumulative += uint64(candidate.Weight)
		if want < cumulative {
			return candidate, true
		}
	}
	return currentDungeonHellPartyFragmentCandidate{}, false
}

func (s *Service) planCurrentDungeonHellPartyDrops(
	runtime *runtimeDungeonState,
	actor currentDungeonHellPartyActor,
	ownerActorObjectKey uint16,
	rollCount uint32,
) ([]currentDungeonDeathDropWire, []currentDungeonHellPartySelectedDrop, error) {
	if runtime == nil || runtime.Session == nil || runtime.Room == nil || runtime.HellParty == nil {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	return s.planCurrentDungeonHellDrops(
		runtime,
		actor.Level,
		ownerActorObjectKey,
		rollCount,
		runtime.HellParty.Mode,
	)
}

func (s *Service) planCurrentDungeonStandaloneRiftBossDrops(
	runtime *runtimeDungeonState,
	monster runtimeDungeonMonster,
	ownerActorObjectKey uint16,
) ([]currentDungeonDeathDropWire, []currentDungeonHellPartySelectedDrop, error) {
	if !currentDungeonStandaloneRiftRuntime(runtime) {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	monsterType, err := currentDungeonMonsterType(monster.Spawn.Rank)
	if err != nil {
		return nil, nil, err
	}
	if monsterType != 3 {
		return nil, nil, nil
	}
	monsterLevel, err := currentDungeonMonsterLevel(monster.Spawn, runtime.Dungeon.Metadata.BasisLevel)
	if err != nil {
		return nil, nil, err
	}
	rollCount, err := s.currentDungeonStandaloneRiftRewardRollCount()
	if err != nil {
		return nil, nil, err
	}
	return s.planCurrentDungeonHellDrops(
		runtime,
		monsterLevel,
		ownerActorObjectKey,
		rollCount,
		currentHellPartyModeA,
	)
}

func (s *Service) planCurrentDungeonHellDrops(
	runtime *runtimeDungeonState,
	actorLevel byte,
	ownerActorObjectKey uint16,
	rollCount uint32,
	hellPartyMode byte,
) ([]currentDungeonDeathDropWire, []currentDungeonHellPartySelectedDrop, error) {
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	if ownerActorObjectKey == 0 {
		return nil, nil, errDungeonDropActorMismatch
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return nil, nil, errDungeonWorldMapUnavailable
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		return nil, nil, err
	}
	hellDrops, fragmentPools, err := s.currentHellPartyDropAssets()
	if err != nil {
		return nil, nil, err
	}
	monsterCatalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return nil, nil, err
	}
	items, err := monsterCatalog.DropCatalog()
	if err != nil {
		return nil, nil, err
	}
	fallbacksBefore := items.GenericFallbackMarks()
	basisLevel := actorLevel
	if runtime.Dungeon.Metadata.BasisLevel.Set && runtime.Dungeon.Metadata.BasisLevel.Value > 0 {
		basisLevel = byte(runtime.Dungeon.Metadata.BasisLevel.Value)
	}
	if basisLevel == 0 {
		return nil, nil, fmt.Errorf("%w: basis level is zero", errCurrentHellPartyDropUnavailable)
	}
	window, windowFound := hellDrops.GradeWindow(int(basisLevel))
	rate, rateFound := hellDrops.DropRate(int(basisLevel), int(runtime.Request.Difficulty))
	if !windowFound || !rateFound {
		return nil, nil, fmt.Errorf("%w: basis=%d difficulty=%d window=%t rate=%t", errCurrentHellPartyDropUnavailable, basisLevel, runtime.Request.Difficulty, windowFound, rateFound)
	}
	rarityWeights, weightsFound := hellDrops.RarityWeights(int(hellPartyMode))
	if !weightsFound {
		return nil, nil, fmt.Errorf("%w: hell-party rarity weights are unavailable", errCurrentHellPartyDropUnavailable)
	}
	baseEpicRate := int64(rarityWeights[4])
	epicPercent := s.currentHellPartyEpicDropRatePercent(runtime.Character)
	rng := newCurrentDungeonDropLCG(visit.DropRNG)
	selected := make([]currentDungeonHellPartySelectedDrop, 0, rollCount*2)
	timeRift := currentDungeonTimeRiftRuntime(runtime)
	var wishItems []uint32
	if timeRift {
		wishItems, err = s.currentEpicWishItemsForCharacter(runtime.Character)
		if err != nil {
			s.logPacketEvent("dnf-epic-wish-drop-preference-sanitized",
				"character_id", runtime.Character.CharacterID,
				"dungeon_id", runtime.Request.DungeonID,
				"reason", err)
			wishItems = nil
		}
	}
	for index := uint32(0); index < rollCount; index++ {
		chance := rng.Next(currentHellPartyDropChanceDenominator)
		if uint32(chance) > rate {
			continue
		}
		rarityRoll := uint32(rng.Next(int(dnfhellparty.RarityRollDenominator)))
		rarity, rarityFound := hellDrops.RarityForRoll(int(hellPartyMode), rarityRoll)
		if !rarityFound {
			continue
		}
		if epicPercent != gmHellEpicDropRateDefaultPercent {
			adjustRoll := uint32(rng.Next(int(dnfhellparty.RarityRollDenominator)))
			rarity, rarityFound = gmAdjustHellEpicRarity(rarity, baseEpicRate, epicPercent, adjustRoll)
			if !rarityFound {
				continue
			}
		}
		var itemID uint32
		var itemFound bool
		itemSource := "hell_equipment"
		if rarity == 4 {
			candidate, found := currentDungeonHellPartyFragmentCandidate{}, false
			wishHit := false
			if timeRift {
				candidate, found, wishHit = selectCurrentTimeRiftEpicCandidate(
					rng,
					fragmentPools,
					int(actorLevel),
					wishItems,
				)
			} else {
				candidate, found = selectCurrentHellPartyFragmentCandidate(rng, fragmentPools, int(actorLevel))
			}
			if found {
				itemID, itemFound = candidate.EquipmentID, true
				if wishHit {
					itemSource = "hell_wish_equipment"
				}
			}
		} else {
			itemID, itemFound, err = items.SelectGenericEquipment(rng, int(basisLevel), window.Down, window.Up, rarity)
			if err != nil {
				return nil, nil, err
			}
		}
		if itemFound {
			selected = append(selected, currentDungeonHellPartySelectedDrop{ItemID: itemID, Amount: 1, Source: itemSource})
		}
	}
	for index := uint32(0); index < rollCount; index++ {
		candidate, found := selectCurrentHellPartyFragmentCandidate(rng, fragmentPools, int(actorLevel))
		if found {
			selected = append(selected, currentDungeonHellPartySelectedDrop{ItemID: candidate.FragmentID, Amount: 1, Source: "epic_fragment"})
		}
	}
	// The invitation-orb jackpot is an independent rare roll on a Hell-party
	// clear only; the standalone Time Rift boss shares this planner but never
	// paid orbs. It is rolled after the PVF-driven streams so their RNG sequence
	// stays identical to the reference with the jackpot disabled. An orb the
	// active item catalog cannot resolve is skipped rather than failing the
	// whole reward plan.
	if runtime.HellParty != nil && rng.Next(currentDungeonHellOrbRollDenominator) < currentDungeonHellOrbChance {
		orbItemID := currentDungeonHellPartyOrbItemID(int(actorLevel))
		if _, resolveErr := items.ResolveItem(orbItemID); resolveErr == nil {
			selected = append(selected, currentDungeonHellPartySelectedDrop{ItemID: orbItemID, Amount: 1, Source: currentDungeonHellOrbDropSource})
		} else {
			s.logPacketEvent("dnf-hell-party-orb-drop-skipped",
				"dungeon_id", runtime.Request.DungeonID,
				"item_id", orbItemID,
				"reason", resolveErr)
		}
	}

	roomKey := runtimeDungeonRoomKeyFromScene(scene)
	owner := runtime.DropOwner
	if owner == nil {
		owner = newRuntimeDungeonDropOwner()
	}
	nextObjectKey := owner.nextObjectKeyAtOrAbove(runtime.NextObjectKey)
	count := uint32(len(selected))
	if count > math.MaxUint8 || nextObjectKey == 0 || nextObjectKey > math.MaxUint16 ||
		count > math.MaxUint16-nextObjectKey {
		return nil, nil, fmt.Errorf("%w: next=%d count=%d", errDungeonDropObjectKeyRange, nextObjectKey, count)
	}
	if owner.nextSceneSlot == 0 || uint32(owner.nextSceneSlot)+count > math.MaxUint16 {
		return nil, nil, fmt.Errorf("%w: next=%d count=%d", errDungeonDropSceneSlotRange, owner.nextSceneSlot, count)
	}
	drops := make([]*runtimeDungeonDrop, 0, len(selected))
	wires := make([]currentDungeonDeathDropWire, 0, len(selected))
	nextSceneSlot := owner.nextSceneSlot
	for _, planned := range selected {
		definition, resolveErr := items.ResolveItem(planned.ItemID)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		itemState, stateErr := currentDungeonDeathDropItemState(nextSceneSlot, definition, planned.Amount)
		if stateErr != nil {
			return nil, nil, stateErr
		}
		qualitySeed := uint32(0)
		if definition.Kind == dungeonDropItemEquipment {
			qualitySeed = binary.LittleEndian.Uint32(itemState.data[6:10])
		}
		drop := &runtimeDungeonDrop{
			ObjectKey:           nextObjectKey,
			SceneSlot:           nextSceneSlot,
			Item:                definition,
			Amount:              planned.Amount,
			QualitySeed:         qualitySeed,
			OwnerActorObjectKey: ownerActorObjectKey,
			Room:                roomKey,
			Status:              runtimeDungeonDropAvailable,
		}
		drops = append(drops, drop)
		wires = append(wires, currentDungeonDeathDropWire{
			SceneObjectKey:      drop.ObjectKey,
			Item:                itemState,
			UnknownTailSentinel: math.MaxUint16,
			OwnerActorObjectKey: ownerActorObjectKey,
		})
		nextObjectKey++
		nextSceneSlot++
	}
	if len(drops) > 0 {
		if err := owner.registerBatch(drops, nextSceneSlot); err != nil {
			return nil, nil, err
		}
		runtime.DropOwner = owner
		runtime.NextObjectKey = nextObjectKey
	}
	visit.DropRNG = rng.Seed()
	s.reportCurrentDungeonGenericDropFallbacks(items, fallbacksBefore,
		"game-dungeon-hell-drop-empty-rarity-pool",
		"dungeon_id", runtime.Request.DungeonID,
		"maze_index", runtime.MazeIndex,
		"basis_level", basisLevel,
		"hell_party_mode", hellPartyMode,
		"roll_count", rollCount)
	return wires, selected, nil
}
