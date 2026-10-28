package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// alignedMagicBoxResolversForCommand keeps PVF loading request-driven. Only
// the single-open magic-box command needs these dependencies; other aligned
// commands do not open or scan the item catalog. The resolvers mirror the
// 86JP StackableItemFile contract: [RANDOMBOX] [int data] groups (skip the
// leading triple, then (item, weight, count, ignored) quads), the
// [booster info] child-tag groups (optional leading DrawCount, then ordinary
// triples or historical avatar five-field rows),
// and the [sealing removal item] / [need material] material requirement.
func (s *Service) alignedMagicBoxResolversForCommand(opcode dnfenum.CmdPacket) (alignedcmd.MagicBoxResolver, alignedcmd.MagicBoxRewardItemResolver, error) {
	if opcode != dnfenum.CmdPacketUseRandomboxItem && opcode != dnfenum.CmdPacketUseRandomboxItemExpand {
		return nil, nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, nil, err
	}
	if catalog == nil {
		return nil, nil, errDungeonDropSourceRequired
	}
	s.initialEquipmentMu.Lock()
	source, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	// Rewards the runtime PVF defines as premium contracts never take a bag cell:
	// the bridge extends the account entitlement through the activator instead.
	contractItems := make(map[int64]struct{})
	if premiumCatalog, premiumErr := s.currentPremiumCatalog(); premiumErr == nil && premiumCatalog != nil {
		for itemID := range premiumCatalog.contractsByItem {
			contractItems[itemID] = struct{}{}
		}
	}
	return func(boxItemID int64) (alignedcmd.MagicBoxResolution, error) {
			return resolveCurrentMagicBox(catalog, source, boxItemID)
		}, func(itemID int64) (alignedcmd.MagicBoxRewardItem, error) {
			reward, err := resolveCurrentMagicBoxRewardItem(catalog, source, itemID)
			if err != nil {
				return reward, err
			}
			if _, isContract := contractItems[itemID]; isContract {
				reward.DirectActivation = true
			}
			return reward, nil
		}, nil
}

// resolveCurrentMagicBox resolves one box template through the active runtime
// PVF. Unresolvable or non-stackable templates and unsupported box families
// resolve with an empty Kind; only catalog/document read failures surface as
// errors so the open fails closed.
func resolveCurrentMagicBox(catalog *pvfDungeonDropCatalog, source dnfpvf.Source, boxItemID int64) (alignedcmd.MagicBoxResolution, error) {
	resolution := alignedcmd.MagicBoxResolution{}
	if catalog == nil || source == nil {
		return resolution, errDungeonDropSourceRequired
	}
	if boxItemID <= 0 || boxItemID > int64(^uint32(0)) {
		return resolution, nil
	}
	definition, err := catalog.ResolveItem(uint32(boxItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.MagicBoxResolution{}, fmt.Errorf("resolve magic box item=%d: %w", boxItemID, err)
	}
	if definition.Kind != dungeonDropItemStackable {
		return resolution, nil
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return alignedcmd.MagicBoxResolution{}, fmt.Errorf("parse magic box item=%d path=%s: %w", boxItemID, definition.PVFPath, err)
	}
	resolution.BoxPVFPath = definition.PVFPath

	switch normalizeMagicBoxPVFType(magicBoxDocumentText(document, "stackable type")) {
	case "upgradable legacy":
		resolution.Groups = parseMagicBoxUpgradableLegacyGroups(document)
	case "random upgradable legacy":
		resolution.Groups = parseMagicBoxRandomGroups(document)
	case "booster", "cera booster", "booster random":
		resolution.Groups = parseMagicBoxBoosterGroups(document)
	default:
		return resolution, nil
	}
	resolution.Groups = filterCurrentMagicBoxResolvableRewardGroups(catalog, resolution.Groups)
	if len(resolution.Groups) == 0 {
		return alignedcmd.MagicBoxResolution{}, nil
	}
	resolution.Kind = "random"
	resolution.MaterialItemID, resolution.MaterialCountPerUse = resolveMagicBoxNeedMaterial(document, boxItemID)
	return resolution, nil
}

// filterCurrentMagicBoxResolvableRewardGroups removes stale PVF rows whose
// item template is absent from the active equipment/stackable indexes. Some
// official legacy jars retain rows for character equipment that was removed
// from the same Script.pvf. Keeping those rows in the weighted pool makes a
// valid jar fail only when the stale row happens to win. Gold pseudo rows are
// retained because list-0 slot 0 is a wallet row rather than an item template.
func filterCurrentMagicBoxResolvableRewardGroups(
	catalog *pvfDungeonDropCatalog,
	groups []alignedcmd.MagicBoxRewardGroup,
) []alignedcmd.MagicBoxRewardGroup {
	filtered := make([]alignedcmd.MagicBoxRewardGroup, 0, len(groups))
	for _, group := range groups {
		entries := make([]alignedcmd.MagicBoxRewardEntry, 0, len(group.Entries))
		for _, entry := range group.Entries {
			if entry.Gold && entry.ItemID == 0 {
				entries = append(entries, entry)
				continue
			}
			if catalog == nil || entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 {
				continue
			}
			if _, err := catalog.ResolveItem(uint32(entry.ItemID)); err != nil {
				continue
			}
			entries = append(entries, entry)
		}
		if len(entries) == 0 {
			continue
		}
		group.Entries = entries
		filtered = append(filtered, group)
	}
	return filtered
}

// parseMagicBoxUpgradableLegacyGroups reads the older top-level [int data]
// pool used by historical event weapon/gift boxes.  Current NoPack opens this
// family through the native two-phase op27 lottery flow; each row is exactly
// (item_id, weight, count).  Zero-weight sentinel rows are ignored and each
// int-data section remains one independent weighted group.
func parseMagicBoxUpgradableLegacyGroups(document *dnfpvf.Document) []alignedcmd.MagicBoxRewardGroup {
	if document == nil {
		return nil
	}
	groups := make([]alignedcmd.MagicBoxRewardGroup, 0, 2)
	for _, section := range document.Sections {
		if normalizeMagicBoxSectionName(section.Name) != "int data" {
			continue
		}
		ints := magicBoxSectionInts(document, section)
		if len(ints) == 0 || len(ints)%3 != 0 {
			continue
		}
		group := alignedcmd.MagicBoxRewardGroup{DrawCount: 1}
		for index := 0; index+2 < len(ints); index += 3 {
			itemID, weight, count := ints[index], ints[index+1], ints[index+2]
			if itemID < 0 || weight <= 0 || count <= 0 {
				continue
			}
			group.Entries = append(group.Entries, alignedcmd.MagicBoxRewardEntry{
				ItemID: itemID,
				Weight: weight,
				Count:  count,
				Gold:   itemID == 0,
			})
		}
		if len(group.Entries) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// parseMagicBoxRandomGroups walks the [RANDOMBOX] child [int data] sections.
// Each section is one independent group with DrawCount 1; per the 86JP
// ParseRandomBoxRewards rule the leading triple is skipped and every later
// four ints form one (item, weight, count, ignored) entry. A section with no
// quad entries and at least two ints degrades to one default-weight entry.
func parseMagicBoxRandomGroups(document *dnfpvf.Document) []alignedcmd.MagicBoxRewardGroup {
	groups := make([]alignedcmd.MagicBoxRewardGroup, 0, 4)
	inRandombox := false
	for _, section := range document.Sections {
		name := normalizeMagicBoxSectionName(section.Name)
		switch name {
		case "randombox":
			inRandombox = true
			continue
		case "/randombox":
			inRandombox = false
		}
		if !inRandombox || name != "int data" {
			continue
		}
		ints := magicBoxSectionInts(document, section)
		group := alignedcmd.MagicBoxRewardGroup{DrawCount: 1}
		if len(ints) >= 7 {
			for index := 3; index+3 < len(ints); index += 4 {
				if ints[index] <= 0 {
					continue
				}
				group.Entries = append(group.Entries, alignedcmd.MagicBoxRewardEntry{
					ItemID: ints[index],
					Weight: maxMagicBoxInt(ints[index+1], 0),
					Count:  maxMagicBoxInt(ints[index+2], 1),
				})
			}
		}
		if len(group.Entries) == 0 && len(ints) >= 2 && ints[0] > 0 {
			group.Entries = append(group.Entries, alignedcmd.MagicBoxRewardEntry{
				ItemID: ints[0],
				Weight: 10000,
				Count:  maxMagicBoxInt(ints[1], 1),
			})
		}
		if len(group.Entries) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// parseMagicBoxBoosterGroups walks the [booster info] child tags. Each direct
// child tag is one independent group. Ordinary rows are (item, weight, count)
// triples. Historical [avatar] rows contain five fields and carry the avatar
// option in the fifth field; the fourth field is retained only as PVF shape.
//
// `[charactor]` is the job-gated form: its first token is a job tag (e.g.
// `[swordman]`), then minLevel/maxLevel/drawCount, then the same triples.
// It only participates for the matching job — see MagicBoxGroupsForCharacter.
func parseMagicBoxBoosterGroups(document *dnfpvf.Document) []alignedcmd.MagicBoxRewardGroup {
	groups := make([]alignedcmd.MagicBoxRewardGroup, 0, 4)
	inInfo := false
	for _, section := range document.Sections {
		name := normalizeMagicBoxSectionName(section.Name)
		switch name {
		case "booster info":
			inInfo = true
			continue
		case "/booster info":
			inInfo = false
		}
		if !inInfo || strings.HasPrefix(name, "/") {
			continue
		}
		ints := magicBoxSectionInts(document, section)
		stride := 3
		if name == "avatar" {
			stride = 5
		}
		group := alignedcmd.MagicBoxRewardGroup{DrawCount: 1}
		start := 0
		if name == "charactor" {
			// `[charactor]` 段 = `<职业标签> <minLevel> <maxLevel> <drawCount> <item> <weight> <count>...`
			// 职业标签是**字符串** token，所以不能只看整数个数，必须核对 token 形状。
			tokens := document.Tokens[section.Start:section.End]
			if len(tokens) < 7 || tokens[0].Kind != dnfpvf.TokenString ||
				strings.TrimSpace(tokens[0].Value) == "" || len(tokens) != len(ints)+1 ||
				len(ints) < 6 || (len(ints)-3)%3 != 0 ||
				ints[0] < 1 || ints[1] < ints[0] || ints[2] < 1 {
				return nil
			}
			group.Job = tokens[0].Value
			group.MinLevel, group.MaxLevel = ints[0], ints[1]
			group.DrawCount, start = ints[2], 3
		}
		if name != "charactor" && len(ints) >= stride+1 && (len(ints)-1)%stride == 0 {
			group.DrawCount = maxMagicBoxInt(ints[0], 1)
			start = 1
		}
		if (len(ints)-start)%stride != 0 {
			continue
		}
		for index := start; index+stride-1 < len(ints); index += stride {
			if ints[index] <= 0 {
				continue
			}
			entry := alignedcmd.MagicBoxRewardEntry{
				ItemID: ints[index],
				Weight: maxMagicBoxInt(ints[index+1], 0),
				Count:  maxMagicBoxInt(ints[index+2], 1),
			}
			if stride == 5 {
				if ints[index+4] < 0 || ints[index+4] > math.MaxUint8 {
					continue
				}
				entry.Option = byte(ints[index+4])
			}
			group.Entries = append(group.Entries, entry)
		}
		if len(group.Entries) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// resolveMagicBoxNeedMaterial reads the per-open material requirement. The
// explicit lottery opening cost is preferred, followed by [sealing removal
// item]: when its ints are
// 1+header*2 shaped the leading int is an entry count and skipped, the rest
// form (item, count) pairs; the first pair with a positive count that is not
// the box itself wins. Otherwise the top-level [need material] "id count"
// field is used (86JP ResolveNeedMaterial), except for PVF gift boxes. A gift
// box's top-level row is acquisition/exchange metadata and is not an opening
// cost; the owned box must remain unconditionally usable.
func resolveMagicBoxNeedMaterial(document *dnfpvf.Document, boxItemID int64) (int64, int64) {
	if itemID, count := resolveMagicBoxLotteryOpeningMaterial(document, boxItemID); itemID > 0 && count > 0 {
		return itemID, count
	}
	if currentMagicBoxHasImpossibleContent(document, "gift") {
		return 0, 0
	}
	if values := document.Ints("need material"); len(values) >= 2 && values[0] > 0 && values[1] > 0 {
		return values[0], values[1]
	}
	raw, found := document.Text("need material")
	if !found {
		return 0, 0
	}
	var itemID, count int64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d %d", &itemID, &count); err != nil || itemID <= 0 || count <= 0 {
		return 0, 0
	}
	return itemID, count
}

// resolveMagicBoxLotteryOpeningMaterial resolves the material required by the
// current client's op27 lottery path. Older event jars use the dedicated
// [lottery use need item] tag rather than [need material], while the native
// random-upgradable box family uses [sealing removal item].
func resolveMagicBoxLotteryOpeningMaterial(document *dnfpvf.Document, boxItemID int64) (int64, int64) {
	if itemID, count := resolveMagicBoxLotteryUseNeedMaterial(document, boxItemID); itemID > 0 && count > 0 {
		return itemID, count
	}
	return resolveMagicBoxSealingRemovalMaterial(document, boxItemID)
}

func resolveMagicBoxLotteryUseNeedMaterial(document *dnfpvf.Document, boxItemID int64) (int64, int64) {
	if document == nil {
		return 0, 0
	}
	values := document.Ints("lottery use need item")
	if len(values) != 2 {
		return 0, 0
	}
	itemID, count := values[0], values[1]
	if itemID <= 0 || count <= 0 || itemID == boxItemID {
		return 0, 0
	}
	return itemID, count
}

// resolveMagicBoxSealingRemovalMaterial returns only the material explicitly
// attached to the opening action. Unlike top-level [need material], this row
// is not an NPC-shop exchange price and must remain enforced for native
// random-upgradable boxes such as the Amazing Box hammer route.
func resolveMagicBoxSealingRemovalMaterial(document *dnfpvf.Document, boxItemID int64) (int64, int64) {
	ints := document.Ints("sealing removal item")
	if len(ints) > 0 && int64(len(ints)) == 1+ints[0]*2 {
		ints = ints[1:]
	}
	for index := 0; index+1 < len(ints); index += 2 {
		itemID, count := ints[index], ints[index+1]
		if itemID <= 0 || count <= 0 || itemID == boxItemID {
			continue
		}
		return itemID, count
	}
	return 0, 0
}

func currentMagicBoxHasImpossibleContent(document *dnfpvf.Document, marker string) bool {
	if document == nil {
		return false
	}
	want := normalizeMagicBoxPVFType(marker)
	for _, value := range document.Texts("impossible contents") {
		if normalizeMagicBoxPVFType(value) == want {
			return true
		}
	}
	return false
}

// resolveCurrentMagicBoxRewardItem resolves one reward template for the
// durable grant step: inventory kind, stack limit (0 = unlimited), PVF slot
// range, and the seal flag from its [attach type].
func resolveCurrentMagicBoxRewardItem(catalog *pvfDungeonDropCatalog, source dnfpvf.Source, itemID int64) (alignedcmd.MagicBoxRewardItem, error) {
	if catalog == nil || source == nil {
		return alignedcmd.MagicBoxRewardItem{}, errDungeonDropSourceRequired
	}
	if itemID <= 0 || itemID > int64(^uint32(0)) {
		return alignedcmd.MagicBoxRewardItem{}, nil
	}
	definition, err := catalog.ResolveItem(uint32(itemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return alignedcmd.MagicBoxRewardItem{}, nil
		}
		return alignedcmd.MagicBoxRewardItem{}, fmt.Errorf("resolve magic box reward item=%d: %w", itemID, err)
	}
	definition, err = currentPVFItemDefinitionForGrantAt(definition, time.Now().UTC())
	if err != nil {
		return alignedcmd.MagicBoxRewardItem{}, fmt.Errorf("resolve magic box reward item=%d expiration: %w", itemID, err)
	}
	item := alignedcmd.MagicBoxRewardItem{
		ItemID:           itemID,
		Kind:             string(definition.Kind),
		EquipmentType:    definition.EquipmentType,
		StackLimit:       definition.StackLimit,
		SlotStart:        definition.SlotStart,
		SlotEnd:          definition.SlotEnd,
		PVFPath:          definition.PVFPath,
		ExpireAt:         definition.ExpirationDate,
		UsablePeriodDays: definition.UsablePeriodDays,
	}
	if isCurrentCeraShopCreatureItem(definition) {
		item.TargetListType = currentPetInventoryListType
		item.SlotStart = 0
		item.SlotEnd = 139
		item.StackLimit = 1
	} else if isCurrentCeraShopPetConsumable(definition) {
		item.TargetListType = currentPetInventoryListType
		item.SlotStart = currentCeraShopPetConsumableSlotStart
		item.SlotEnd = currentCeraShopPetConsumableSlotEnd
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return alignedcmd.MagicBoxRewardItem{}, fmt.Errorf("parse magic box reward item=%d path=%s: %w", itemID, definition.PVFPath, err)
	}
	attachType := normalizeMagicBoxPVFType(magicBoxDocumentText(document, "attach type"))
	item.Seal = attachType == "sealing"
	if durability, found := document.Int("durability"); found && durability >= 0 && durability <= 65535 {
		item.Durability = uint16(durability)
	}
	// 装备奖励的品级在**这里**掷（和掉落掷品级同一层），Owner 侧因此仍然是纯确定性的
	// （见 alignedcmd.MagicBoxRewardItem.QualitySeed 的注释）。不掷的话 raw+0x06
	// 会落到投影的 Count 兜底上（=1），客户端把每一件奖励装备都画成"最下级"。
	// 掷失败不拦发奖：那只是品级显示，不该让玩家拿不到东西。
	if definition.Kind == dungeonDropItemEquipment {
		if seed, seedErr := newCurrentEquipmentQualitySeed(); seedErr == nil {
			item.QualitySeed = seed
		}
	}
	return item, nil
}

func magicBoxDocumentText(document *dnfpvf.Document, name string) string {
	values := document.Texts(name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func magicBoxSectionInts(document *dnfpvf.Document, section dnfpvf.Section) []int64 {
	if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
		return nil
	}
	out := make([]int64, 0, section.End-section.Start)
	for _, token := range document.Tokens[section.Start:section.End] {
		if token.Kind == dnfpvf.TokenInt {
			out = append(out, token.Int)
		}
	}
	return out
}

func normalizeMagicBoxSectionName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizeMagicBoxPVFType(value string) string {
	return strings.ToLower(strings.Trim(value, "` []"))
}

func maxMagicBoxInt(value int64, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}
