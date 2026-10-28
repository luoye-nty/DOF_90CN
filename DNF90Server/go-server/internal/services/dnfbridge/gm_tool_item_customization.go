package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type GMItemMailOptionPage struct {
	Items               []GMItemSummary `json:"items"`
	Total               int             `json:"total"`
	Limit               int             `json:"limit"`
	Offset              int             `json:"offset"`
	Mode                string          `json:"mode"`
	TargetEquipmentType string          `json:"target_equipment_type"`
	SocketType          byte            `json:"socket_type,omitempty"`
	SocketColor         string          `json:"socket_color,omitempty"`
	SocketCount         int             `json:"socket_count,omitempty"`
}

func gmMailEquipmentTag(value string) string { return gmItemTypeTag(value) }

func normalizeEquipmentPlacementItemGroup(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	return strings.TrimSpace(strings.Trim(raw, "`"))
}

func gmMailSupportsForging(equipmentType string) bool {
	return gmMailEquipmentTag(equipmentType) == "weapon"
}

func gmMailSupportsEmblems(equipmentType string) bool {
	switch gmMailEquipmentTag(equipmentType) {
	case "coat", "shoulder", "pants", "shoes", "waist", "amulet", "wrist", "ring", "support", "magic stone":
		return true
	default:
		return false
	}
}

func gmMailSupportsEnchant(equipmentType string) bool {
	switch gmMailEquipmentTag(equipmentType) {
	case "weapon", "coat", "shoulder", "pants", "shoes", "waist", "amulet", "wrist", "ring", "support", "magic stone", "earring":
		return true
	default:
		return false
	}
}

func gmSocketColor(socketType byte) string {
	switch socketType {
	case 0x01:
		return "红色"
	case 0x02:
		return "黄色"
	case 0x04:
		return "绿色"
	case 0x08:
		return "蓝色"
	case 0x10:
		return "白金"
	default:
		return ""
	}
}

func (s *Service) GMItemMailOptions(ctx context.Context, targetItemID uint32, mode, query string, rarity int64, limit, offset int) (GMItemMailOptionPage, error) {
	if targetItemID == 0 || rarity < -1 || rarity > 6 || limit < 1 || limit > 200 || offset < 0 {
		return GMItemMailOptionPage{}, ErrGMInvalidRequest
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "emblem" && mode != "enchant" {
		return GMItemMailOptionPage{}, fmt.Errorf("%w: mode must be emblem or enchant", ErrGMInvalidRequest)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMItemMailOptionPage{}, err
	}
	target, err := catalog.ResolveItem(targetItemID)
	if err != nil || target.Kind != dungeonDropItemEquipment {
		return GMItemMailOptionPage{}, fmt.Errorf("%w: target must be equipment", ErrGMInvalidRequest)
	}
	page := GMItemMailOptionPage{Mode: mode, TargetEquipmentType: gmMailEquipmentTag(target.EquipmentType), Limit: limit, Offset: offset}
	var all []GMItemSummary
	switch mode {
	case "emblem":
		if !gmMailSupportsEmblems(target.EquipmentType) {
			return GMItemMailOptionPage{}, fmt.Errorf("%w: equipment type does not support emblems", ErrGMInvalidRequest)
		}
		rule, ok := currentEquipmentPlacementRuleForPVFType(target.EquipmentType)
		if !ok {
			return GMItemMailOptionPage{}, fmt.Errorf("%w: unknown equipment type", ErrGMInvalidRequest)
		}
		page.SocketType = currentEquipmentJewelSocketType(rule)
		page.SocketColor = gmSocketColor(page.SocketType)
		page.SocketCount = currentEquipmentSocketOpenCount(rule)
		all, err = gmCachedMailOptions(ctx, catalog, targetItemID, mode, func(definition dungeonDropItemDefinition) bool {
			if gmMailEquipmentTag(definition.StackableType) != "avatar emblem" {
				return false
			}
			emblemType := currentSocketEmblemType(catalog, int64(definition.ItemID))
			return emblemType != 0 && currentCanAttachEmblem(page.SocketType, emblemType)
		})
	case "enchant":
		if !gmMailSupportsEnchant(target.EquipmentType) {
			return GMItemMailOptionPage{}, fmt.Errorf("%w: equipment type does not support enchant", ErrGMInvalidRequest)
		}
		all, err = gmCachedMailOptions(ctx, catalog, targetItemID, mode, func(definition dungeonDropItemDefinition) bool {
			if gmMailEquipmentTag(definition.StackableType) != "enchant waste" {
				return false
			}
			ok, resolveErr := gmEnchantBeadSupportsTarget(catalog, int64(definition.ItemID), int64(targetItemID))
			return resolveErr == nil && ok
		})
	}
	if err != nil {
		return GMItemMailOptionPage{}, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	filtered := make([]GMItemSummary, 0, len(all))
	for _, item := range all {
		if err := ctx.Err(); err != nil {
			return GMItemMailOptionPage{}, err
		}
		if q != "" && !strings.Contains(strings.ToLower(item.Name), q) && !strings.Contains(strings.ToLower(item.PVFPath), q) && strconv.FormatUint(uint64(item.ItemID), 10) != q {
			continue
		}
		if rarity >= 0 && item.Rarity != rarity {
			continue
		}
		filtered = append(filtered, item)
	}
	page.Total = len(filtered)
	if offset < len(filtered) {
		end := offset + limit
		if end > len(filtered) {
			end = len(filtered)
		}
		page.Items = append([]GMItemSummary(nil), filtered[offset:end]...)
	} else {
		page.Items = []GMItemSummary{}
	}
	return page, nil
}

func gmCachedMailOptions(ctx context.Context, catalog *pvfDungeonDropCatalog, targetItemID uint32, mode string, accepts func(dungeonDropItemDefinition) bool) ([]GMItemSummary, error) {
	catalog.gmMailOptionMu.Lock()
	cache := catalog.gmEmblemOptions
	if mode == "enchant" {
		cache = catalog.gmEnchantOptions
	}
	if cached, ok := cache[targetItemID]; ok {
		out := append([]GMItemSummary(nil), cached...)
		catalog.gmMailOptionMu.Unlock()
		return out, nil
	}
	catalog.gmMailOptionMu.Unlock()

	ids := append([]uint32(nil), catalog.stackableIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	items := make([]GMItemSummary, 0, 128)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		definition, err := catalog.ResolveItem(id)
		if err != nil || !accepts(definition) {
			continue
		}
		summary := gmItemSummary(definition)
		summary.MailEffects, err = gmMailOptionEffects(catalog, mode, definition)
		if err != nil {
			return nil, err
		}
		items = append(items, summary)
	}
	catalog.gmMailOptionMu.Lock()
	if mode == "enchant" {
		catalog.gmEnchantOptions[targetItemID] = append([]GMItemSummary(nil), items...)
	} else {
		catalog.gmEmblemOptions[targetItemID] = append([]GMItemSummary(nil), items...)
	}
	catalog.gmMailOptionMu.Unlock()
	return items, nil
}

func gmMailOptionEffects(catalog *pvfDungeonDropCatalog, mode string, definition dungeonDropItemDefinition) ([]GMItemMailEffect, error) {
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil {
		return nil, fmt.Errorf("parse GM %s option item=%d path=%s: %w", mode, definition.ItemID, definition.PVFPath, err)
	}
	if mode == "emblem" {
		effect := gmMailOptionEffectFromDocument("", document)
		if gmMailOptionEffectEmpty(effect) {
			return nil, nil
		}
		return []GMItemMailEffect{effect}, nil
	}

	cardIDs := document.Ints("monster card id")
	if len(cardIDs) == 0 {
		if cardID, ok := document.Int("enchant index"); ok {
			cardIDs = []int64{cardID}
		}
	}
	effects := make([]GMItemMailEffect, 0, len(cardIDs))
	seen := make(map[int64]struct{}, len(cardIDs))
	for _, cardID := range cardIDs {
		if cardID <= 0 || cardID > int64(^uint32(0)) {
			continue
		}
		if _, ok := seen[cardID]; ok {
			continue
		}
		seen[cardID] = struct{}{}
		card, resolveErr := catalog.ResolveItem(uint32(cardID))
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve GM enchant effect card=%d for bead=%d: %w", cardID, definition.ItemID, resolveErr)
		}
		cardDocument, parseErr := parseDungeonCardPVFDocument(catalog.source, card.PVFPath)
		if parseErr != nil {
			return nil, fmt.Errorf("parse GM enchant effect card=%d path=%s: %w", cardID, card.PVFPath, parseErr)
		}
		effect := gmMailOptionEffectFromDocument(strings.TrimSpace(card.Name), cardDocument)
		if effect.Name == "" {
			effect.Name = fmt.Sprintf("卡片 #%d", cardID)
		}
		if !gmMailOptionEffectEmpty(effect) {
			effects = append(effects, effect)
		}
	}
	return effects, nil
}

func gmMailOptionEffectFromDocument(name string, document *dnfpvf.Document) GMItemMailEffect {
	description := gmItemDetailText(document, "detail explain")
	if description == "" {
		description = gmItemDetailText(document, "basic explain")
	}
	if description == "" {
		description = gmItemDetailText(document, "explain")
	}
	return GMItemMailEffect{
		Name:                name,
		Description:         description,
		Attributes:          gmEquipmentSetAttributes(document),
		ElementalProperties: gmEquipmentElementalProperties(document),
		SkillLevelUps:       gmMailOptionSkillLevelUps(document),
	}
}

func gmMailOptionSkillLevelUps(document *dnfpvf.Document) []GMItemSkillLevelUp {
	if document == nil {
		return nil
	}
	tokens, ok := document.Section("skill levelup")
	if !ok {
		return nil
	}
	result := make([]GMItemSkillLevelUp, 0, 2)
	for index := 0; index < len(tokens); {
		token := tokens[index]
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			index++
			continue
		}
		job := strings.TrimSpace(token.Value)
		index++
		if index+1 >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt || tokens[index+1].Kind != dnfpvf.TokenInt {
			continue
		}
		result = append(result, GMItemSkillLevelUp{Job: job, SkillID: tokens[index].Int, Level: tokens[index+1].Int})
		index += 2
	}
	return result
}

func gmMailOptionEffectEmpty(effect GMItemMailEffect) bool {
	return effect.Description == "" && len(effect.Attributes) == 0 && len(effect.ElementalProperties) == 0 && len(effect.SkillLevelUps) == 0
}

func gmEnchantBeadSupportsTarget(catalog *pvfDungeonDropCatalog, beadItemID, targetItemID int64) (bool, error) {
	definition, err := catalog.ResolveItem(uint32(beadItemID))
	if err != nil || definition.Kind != dungeonDropItemStackable {
		return false, err
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil {
		return false, err
	}
	whitelist := document.Ints("bead limited usable item")
	if len(whitelist) == 0 {
		whitelist = document.Ints("target item id")
	}
	if len(whitelist) > 0 && !gmInt64Contains(whitelist, targetItemID) {
		return false, nil
	}
	cards := document.Ints("monster card id")
	if len(cards) == 0 {
		if card, ok := document.Int("enchant index"); ok {
			cards = []int64{card}
		}
	}
	seen := make(map[int64]struct{})
	valid := 0
	for _, cardID := range cards {
		if cardID <= 0 {
			continue
		}
		if _, ok := seen[cardID]; ok {
			continue
		}
		seen[cardID] = struct{}{}
		resolution, resolveErr := resolveCurrentEnchantCardMetadata(catalog, catalog.source, cardID, targetItemID)
		if resolveErr != nil {
			return false, resolveErr
		}
		if resolution.CardItemID != cardID || resolution.TargetKind != string(dungeonDropItemEquipment) ||
			!gmStringContainsFold(resolution.AllowedEquipmentTypes, resolution.TargetEquipmentType) ||
			(len(resolution.UpgradeCounts) > 0 && !gmInt64Contains(resolution.UpgradeCounts, 0)) {
			return false, nil
		}
		valid++
	}
	return valid > 0, nil
}

func gmStringContainsFold(values []string, needle string) bool {
	needle = gmMailEquipmentTag(needle)
	for _, value := range values {
		if gmMailEquipmentTag(value) == needle {
			return true
		}
	}
	return false
}

func gmInt64Contains(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// gmApplyMailEquipmentSeal applies the one creation rule the GM item-mail path
// used to skip. Every other grant route builds its row through
// buildCurrentDungeonPickupStack, which seals `[attach type] [sealing]`
// equipment (row byte +0x0D = 1); GMSendItemMailWithOptions hand-builds its
// stack and so delivered those pieces unsealed. The client treats an unsealed
// piece as bound: it cannot be traded, and the 合成器 refuses it outright --
// dstr 25065 is literally "解除封装的装备无法进行合成。" Rarity plays no part,
// and stackables are never sealed.
func gmApplyMailEquipmentSeal(stack *dnfrepo.ItemStack, definition dungeonDropItemDefinition) {
	if stack == nil || !currentDungeonDroppedEquipmentShouldBeSealed(definition) {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 2)
	}
	stack.Extra["seal_flag"] = "1"
	stack.Extra["seal"] = "1"
}

func gmApplyMailEquipmentCustomization(stack *dnfrepo.ItemStack, catalog *pvfDungeonDropCatalog, definition dungeonDropItemDefinition, options GMMailOptions) error {
	if stack == nil {
		return ErrGMInvalidRequest
	}
	if options.Forging < 0 || options.Forging > 8 {
		return fmt.Errorf("%w: forging must be between 0 and 8", ErrGMInvalidRequest)
	}
	if options.Forging != 0 && !gmMailSupportsForging(definition.EquipmentType) {
		return fmt.Errorf("%w: forging is supported only by weapons", ErrGMInvalidRequest)
	}
	if len(options.EmblemItemIDs) > 0 {
		if !gmMailSupportsEmblems(definition.EquipmentType) {
			return fmt.Errorf("%w: emblems are not supported by this equipment type", ErrGMInvalidRequest)
		}
		rule, ok := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType)
		if !ok || len(options.EmblemItemIDs) > currentEquipmentSocketOpenCount(rule) {
			return fmt.Errorf("%w: invalid emblem socket count", ErrGMInvalidRequest)
		}
		socketType := currentEquipmentJewelSocketType(rule)
		var data [currentEquipmentEmblemDataBytes]byte
		currentEnsureEquipmentEmblemSocketsOpen(&data, currentEquipmentSocketOpenCount(rule), currentEquipmentSocketOpenCount(rule))
		for index, emblemItemID := range options.EmblemItemIDs {
			if emblemItemID == 0 {
				continue
			}
			emblemType := currentSocketEmblemType(catalog, int64(emblemItemID))
			if emblemType == 0 || !currentCanAttachEmblem(socketType, emblemType) {
				return fmt.Errorf("%w: emblem %d does not match socket color", ErrGMInvalidRequest, emblemItemID)
			}
			currentWriteEquipmentEmblem(&data, byte(index), int64(emblemItemID))
		}
		currentSetEquipmentEmblemDataExtra(stack.Extra, data)
		currentSetEquipmentSocketTypeExtra(stack.Extra, socketType)
	}
	if options.EnchantBeadItemID != 0 {
		if !gmMailSupportsEnchant(definition.EquipmentType) {
			return fmt.Errorf("%w: enchant is not supported by this equipment type", ErrGMInvalidRequest)
		}
		resolution, err := resolveCurrentEnchantBeadMetadata(catalog, catalog.source, int64(options.EnchantBeadItemID), int64(definition.ItemID))
		if err != nil {
			return err
		}
		if resolution.CardItemID <= 0 || resolution.TargetKind != string(dungeonDropItemEquipment) ||
			(len(resolution.TargetWhitelist) > 0 && !gmInt64Contains(resolution.TargetWhitelist, int64(definition.ItemID))) ||
			!gmStringContainsFold(resolution.AllowedEquipmentTypes, resolution.TargetEquipmentType) ||
			(len(resolution.UpgradeCounts) > 0 && !gmInt64Contains(resolution.UpgradeCounts, 0)) {
			return fmt.Errorf("%w: enchant bead does not support this equipment", ErrGMInvalidRequest)
		}
		stack.Extra["value_a"] = strconv.FormatInt(resolution.CardItemID, 10)
		stack.Extra["enchant_card_id"] = strconv.FormatInt(resolution.CardItemID, 10)
		stack.Extra["byte_12"] = "0"
		stack.Extra["enchant_upgrade_count"] = "0"
		stack.Extra["gm_enchant_bead_item_id"] = strconv.FormatUint(uint64(options.EnchantBeadItemID), 10)
	}
	if options.Forging > 0 {
		// Keep GM grants on the same canonical state owned by the remote
		// separate-upgrade implementation. Its shared item-list projector writes
		// raw+0x57; GM mail does not maintain an independent forging codec.
		stack.Extra["forging_level"] = strconv.Itoa(options.Forging)
	}
	return nil
}
