package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// The current 90CN enchanter.exj omits these two legacy policy tags. Keep the
	// already packet-tested compatibility defaults narrowly scoped to op604;
	// template identity, rarity, upgrade table and placement remain active-PVF
	// authoritative.
	currentMonsterCardUpgradeCalculateConst = 10
	currentMonsterCardUpgradeGoldCost       = int64(5000)
)

// alignedMonsterCardUpgradeResolverForCommand keeps the op603 catalog load
// request-scoped.  A card is eligible only when the active PVF resolves it as
// a stackable [item category] `monster card`. A zero maximum remains valid as
// sacrifice material, but cannot be selected as the upgrade target.
func (s *Service) alignedMonsterCardUpgradeResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.MonsterCardUpgradeResolver, error) {
	if opcode != dnfenum.CmdPacketUpgradeCard {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errDungeonDropSourceRequired
	}
	s.initialEquipmentMu.Lock()
	source, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	return func(itemID int64) (alignedcmd.MonsterCardUpgradeResolution, error) {
		return resolveCurrentMonsterCardUpgradeMetadata(catalog, source, itemID)
	}, nil
}

func resolveCurrentMonsterCardUpgradeMetadata(catalog *pvfDungeonDropCatalog, source dnfpvf.Source, itemID int64) (alignedcmd.MonsterCardUpgradeResolution, error) {
	resolution := alignedcmd.MonsterCardUpgradeResolution{ItemID: itemID}
	if catalog == nil || source == nil || itemID <= 0 || itemID > int64(^uint32(0)) {
		return resolution, nil
	}
	definition, resolveErr := catalog.ResolveItem(uint32(itemID))
	if resolveErr != nil {
		if errors.Is(resolveErr, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.MonsterCardUpgradeResolution{}, resolveErr
	}
	if definition.Kind != dungeonDropItemStackable {
		return resolution, nil
	}
	document, parseErr := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if parseErr != nil {
		return alignedcmd.MonsterCardUpgradeResolution{}, fmt.Errorf("parse monster card item=%d path=%s: %w", itemID, definition.PVFPath, parseErr)
	}
	category, found := document.Text("item category")
	if !found || !strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(category, "`", "")), "monster card") {
		return resolution, nil
	}
	rarity, found := document.Int("rarity")
	// Current 90CN contains upgradeable monster cards above the legacy
	// 0..3 rarity table (for example Maker Luke (Light), rarity 6). Keep the
	// exact PVF rarity for same-card and higher-card comparisons; the domain
	// clamps only the legacy probability tier when it must calculate a chance.
	if !found || rarity < 0 || rarity > 6 {
		return resolution, nil
	}
	maximum := currentMonsterCardMaximumUpgrade(document)
	if maximum < 0 || maximum > int64(^byte(0)) || definition.SlotStart < 0 || definition.SlotEnd < definition.SlotStart || definition.StackLimit < 0 {
		return resolution, nil
	}
	resolution.PVFPath = strings.ReplaceAll(definition.PVFPath, "\\", "/")
	resolution.Rarity = int(rarity)
	resolution.MaximumUpgrade = byte(maximum)
	resolution.StackLimit = definition.StackLimit
	resolution.SlotStart = definition.SlotStart
	resolution.SlotEnd = definition.SlotEnd
	resolution.GoldCost = currentMonsterCardUpgradeGoldCost
	resolution.CalculateConst = currentMonsterCardUpgradeCalculateConst
	return resolution, nil
}

// Current card documents encode one [enchant index] block per upgrade stage
// inside [enchant table]. Document.Ints intentionally returns only the first
// matching section, so it would see only stage zero and make every ordinary
// card look non-upgradeable. Walk every exact section opener instead.
func currentMonsterCardMaximumUpgrade(document *dnfpvf.Document) int64 {
	maximum := int64(-1)
	for _, value := range currentMonsterCardUpgradeIndices(document) {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

// currentMonsterCardUpgradeIndices returns the authored index from every
// exact [enchant index] stage. In current Script.pvf these sections are nested
// below [enchant table]; Document.Ints and Document.Section intentionally
// expose only the first/direct section and therefore make upgraded stages
// disappear from enchant validation.
func currentMonsterCardUpgradeIndices(document *dnfpvf.Document) []int64 {
	if document == nil {
		return nil
	}
	indices := make([]int64, 0, 4)
	seen := make(map[int64]struct{}, 4)
	for index, token := range document.Tokens {
		if !isPVFSectionToken(token, "enchant index") {
			continue
		}
		for valueIndex := index + 1; valueIndex < len(document.Tokens); valueIndex++ {
			value := document.Tokens[valueIndex]
			if value.Kind == dnfpvf.TokenSection {
				break
			}
			if value.Kind != dnfpvf.TokenInt || value.Int < 0 {
				continue
			}
			if _, duplicate := seen[value.Int]; !duplicate {
				indices = append(indices, value.Int)
				seen[value.Int] = struct{}{}
			}
		}
	}
	return indices
}

// currentMonsterCardUpgradePolicy resolves the enchanter's [monstercard exp]
// row for the acting character. op603's odds and the experience it pays belong
// to the enchanter job, not to the card, so they depend on the player's own
// enchanter level: level 1 reads 100/70/60/50/0 across the rarity tiers, level
// 11 reads 100 everywhere. An unavailable catalog returns a zero policy and the
// caller keeps its previous estimate rather than failing the upgrade.
func (s *Service) currentMonsterCardUpgradePolicy(
	ctx context.Context,
	repos dnfrepo.Group,
	session *gameSession,
) alignedcmd.MonsterCardUpgradePolicy {
	if s == nil || session == nil || session.selectedCharacterID == 0 || repos.Character == nil {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	catalog, err := s.currentExpertJobCatalog()
	if err != nil || catalog == nil {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	enchanter, ok := catalog.Enchanter()
	if !ok || enchanter == nil {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	config, ok := catalog.Config(dnfexpertjob.EnchanterType)
	if !ok || config == nil {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	character, found, err := repos.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	level := config.Level(character.Stats["expert_job_exp"])
	rule, ok := enchanter.CardExperienceByLevel[level]
	if !ok {
		return alignedcmd.MonsterCardUpgradePolicy{}
	}
	return alignedcmd.MonsterCardUpgradePolicy{
		Known:                 true,
		Level:                 level,
		SuccessRates:          rule.SuccessRates,
		MinimumExperienceGain: rule.MinimumExperienceGain,
		MaximumExperienceGain: rule.MaximumExperienceGain,
	}
}
