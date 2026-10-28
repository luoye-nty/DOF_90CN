package dnfbridge

import (
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

func currentCeraShopProductDefinitionForGrantAt(
	definition dungeonDropItemDefinition,
	product currentCeraShopProduct,
	now time.Time,
) (dungeonDropItemDefinition, error) {
	resolved, err := currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return dungeonDropItemDefinition{}, err
	}
	if product.DurationDays == 0 {
		return resolved, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowUnix := now.Unix()
	durationSeconds := uint64(product.DurationDays) * uint64((24*time.Hour)/time.Second)
	if nowUnix <= 0 || nowUnix > math.MaxUint32 || durationSeconds > uint64(math.MaxUint32)-uint64(nowUnix) {
		return dungeonDropItemDefinition{}, fmt.Errorf(
			"%w: commodity=%d duration_days=%d now=%d",
			errCurrentCeraShopProductUnavailable,
			product.CommodityID,
			product.DurationDays,
			nowUnix,
		)
	}
	resolved.ExpirationDate = time.Unix(nowUnix+int64(durationSeconds), 0).UTC()
	return resolved, nil
}

func consumeCurrentCeraShopAvatarCoupon(inventory *dnfrepo.InventoryRecord, couponItemID int64) (currentItemListEntry, error) {
	if inventory == nil || couponItemID <= 0 {
		return currentItemListEntry{}, errCurrentCeraShopOwnerUnavailable
	}
	bestKey := ""
	bestSlot := int16(math.MaxInt16)
	var bestStack dnfrepo.ItemStack
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || slot < 0 || dnfrepo.IsAccountSharedInventorySlot(listType, slot) {
			continue
		}
		if stack.ItemID != couponItemID || stack.Count <= 0 {
			continue
		}
		if bestKey == "" || slot < bestSlot {
			bestKey = key
			bestSlot = slot
			bestStack = stack
		}
	}
	if bestKey == "" {
		return currentItemListEntry{}, fmt.Errorf("%w: avatar coupon item=%d", errCurrentCeraShopCeraInsufficient, couponItemID)
	}
	bestStack.Count--
	if bestStack.Count <= 0 {
		delete(inventory.Slots, bestKey)
		var removed currentItemListEntry
		removed.patchCore(bestSlot, math.MaxUint32, 0)
		return removed, nil
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, bestSlot, bestStack)
	bestStack.RawEntry = append([]byte(nil), entry.data[:]...)
	inventory.Slots[bestKey] = bestStack
	return entry, nil
}

// currentCeraShopMainInventoryUpgradeStage recognizes the three main-bag
// expansion products through their runtime PVF paths. The returned value is
// the Cera-shop stage (1..3); the persisted op13 list-0 expansion is stage*8.
func currentCeraShopMainInventoryUpgradeStage(definition dungeonDropItemDefinition) (uint16, bool) {
	if definition.Kind != dungeonDropItemStackable {
		return 0, false
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	cleanPath = strings.TrimPrefix(cleanPath, "stackable/")
	switch cleanPath {
	case "cash/inven_upgradekit1.stk":
		return 1, true
	case "cash/inven_upgradekit2.stk":
		return 2, true
	case "cash/chn_20140617_new_sales_item/chn_inventory_3rd_expansion_2683675.stk":
		return 3, true
	default:
		return 0, false
	}
}

func (c *pvfCeraShopCatalog) isCharacterSlotExpansionTicket(definition dungeonDropItemDefinition) bool {
	if c == nil || c.source == nil || definition.Kind != dungeonDropItemStackable {
		return false
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	if cleanPath != "stackable/cash/character_slotkit.stk" {
		return false
	}
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return false
	}
	stackableType, typeFound := document.Text("stackable type")
	return typeFound && strings.EqualFold(strings.TrimSpace(stackableType), "[etc]")
}

// characterRenameCardKind reports whether a stackable is a character rename card.
//
// Recognition is the PVF contract, not a file-name allowlist:
//
//	[action type]
//	`[character name change]`
//	[/action type]
//
// Any item carrying that action type can rename a character. The runtime PVF
// currently declares it on 18 stackables - the Cera shop card, the event cards
// (角色更名卡 / 角色更名卡 (魔枪士) / 冒险家征集令), the account-bound cards and the
// localization-key variants whose [name] reads `name_<id>` - and all of them are
// items the client lets the player use exactly like the shop card.
//
// autoUse stays path-based on purpose: it only says whether the Cera shop
// purchase consumes the item and arms the rename directly (the 8800-Cera product)
// instead of delivering the card to the bag. Widening recognition must not change
// what any existing Cera shop product delivers.
//
// The shop product is recognized by its path alone, without the action type.
// The runtime PVF proves that requirement cannot be met by this item:
// stackable/cash/chn_character_rename.stk is 529 bytes and carries no
// [action type] block at all (unlike its bag sibling
// stackable/cash/charcter_rename_card.stk, which declares
// `[character name change]` and is used from the bag through op509). Its
// [explain] ends with `购买后将自动使用。` - the shop consumes it at checkout.
//
// Requiring the action type here is what broke the 8800-Cera purchase: the path
// matched, the declaration did not, so the product was classified as an ordinary
// stackable, delivered to the bag, and the character never received the
// pending-rename entitlement. The card is client-side unusable in a bag (the
// client only sends op509 for items that declare the action type), so the
// purchase bought nothing.
//
// Accepting the exact auto-use path is not a loosening: an item that lives at
// that path is the shop's rename product by construction, and every other
// stackable - including 宠物更名卡 at cash/creature/rename_card.stk - still has
// to declare the action type.
func (c *pvfCeraShopCatalog) characterRenameCardKind(definition dungeonDropItemDefinition) (autoUse bool, ok bool) {
	if c == nil || c.source == nil || definition.Kind != dungeonDropItemStackable {
		return false, false
	}
	autoUse = currentCharacterRenameAutoUsePVFPath(definition.PVFPath)
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return false, false
	}
	if autoUse || currentItemDeclaresActionType(document, currentItemActionTypeCharacterNameChange) {
		return autoUse, true
	}
	return false, false
}

// currentCharacterRenameAutoUsePVFPath recognizes the Cera shop rename product
// whose purchase arms the rename immediately. Every other rename card, including
// every other card the shop may deliver, is used from the bag through
// USE_RENAME_CARD.
func currentCharacterRenameAutoUsePVFPath(pvfPath string) bool {
	clean := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(pvfPath, "\\", "/")))
	return clean == "stackable/cash/chn_character_rename.stk"
}

// avatarInventoryUpgradeStage identifies a real avatar-inventory ticket from
// the PVF item that the Cera catalog resolved. Every supported ticket says it
// adds seven avatar slots and embeds the same target stage in its name and
// explanation; the item ID and Cera commodity are deliberately not part of
// the contract.
func (c *pvfCeraShopCatalog) avatarInventoryUpgradeStage(definition dungeonDropItemDefinition) (uint16, bool, error) {
	if c == nil || c.source == nil || definition.Kind != dungeonDropItemStackable {
		return 0, false, nil
	}
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return 0, false, err
	}
	name, nameFound := document.Text("name")
	stage, namedTicket := currentCeraShopAvatarInventoryStageFromName(name)
	if !nameFound || !namedTicket {
		return 0, false, nil
	}
	explain, explainFound := document.Text("explain")
	explainedStage, explainedTicket := currentCeraShopAvatarInventoryStageFromExplain(explain)
	if !explainFound || !explainedTicket || stage == 0 || stage != explainedStage {
		return 0, true, nil
	}
	return stage, true, nil
}

func currentCeraShopAvatarInventoryStageFromName(name string) (uint16, bool) {
	clean := currentCeraShopCompactPVFText(name)
	const (
		prefix = "装扮栏扩展券("
		suffix = "阶段)"
	)
	if !strings.HasPrefix(clean, prefix) || !strings.HasSuffix(clean, suffix) {
		return 0, false
	}
	stage, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(clean, prefix), suffix), 10, 16)
	if err != nil || stage == 0 || stage > 20 {
		return 0, true
	}
	return uint16(stage), true
}

func currentCeraShopAvatarInventoryStageFromExplain(explain string) (uint16, bool) {
	clean := currentCeraShopCompactPVFText(explain)
	if !strings.Contains(clean, "装扮栏") || !strings.Contains(clean, "增加7格") {
		return 0, false
	}
	for _, marker := range []string{"最多可扩展至", "最多可扩展到", "可以扩展至", "可以扩展到", "可扩展至", "可扩展到"} {
		start := strings.Index(clean, marker)
		if start < 0 {
			continue
		}
		rest := clean[start+len(marker):]
		end := strings.Index(rest, "阶段")
		if end <= 0 {
			return 0, true
		}
		stage, err := strconv.ParseUint(rest[:end], 10, 16)
		if err != nil || stage == 0 || stage > 20 {
			return 0, true
		}
		return uint16(stage), true
	}
	return 0, true
}

func currentCeraShopCompactPVFText(value string) string {
	return strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(strings.TrimSpace(value))
}

// currentCeraShopPersonalCargoUpgradeTarget recognizes the original personal
// cargo tools from their real PVF paths. The no-suffix tool is the first tier
// (8 -> 24); numeric suffixes identify later 16-slot tiers.  The later live
// catalog uses a separate numeric directory, so catalog.personalCargoUpgradeTarget
// augments this path-only recognition with the item's own PVF name/explain
// fields rather than with a copied commodity-ID table.
func currentCeraShopPersonalCargoUpgradeTarget(definition dungeonDropItemDefinition) (uint16, bool) {
	if definition.Kind != dungeonDropItemStackable {
		return 0, false
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	if cleanPath == "" || strings.Contains(cleanPath, "cash/chn_account_cargo/") {
		return 0, false
	}
	base := path.Base(cleanPath)
	if path.Ext(base) != ".stk" {
		return 0, false
	}
	stem := strings.TrimSuffix(base, ".stk")
	const prefix = "safe_upgradekit"
	if !strings.HasPrefix(stem, prefix) {
		return 0, false
	}
	tierText := strings.TrimPrefix(stem, prefix)
	if tierText != "" {
		for _, digit := range tierText {
			if digit < '0' || digit > '9' {
				return 0, false
			}
		}
	}
	tier := 0
	if tierText != "" {
		parsed, err := strconv.ParseUint(tierText, 10, 16)
		if err != nil {
			return 0, true
		}
		tier = int(parsed)
	}
	const (
		firstPersonalCargoUpgradeSlots = 24
		personalCargoUpgradeStep       = 16
		maximumPersonalCargoUpgrade    = 200
	)
	target := firstPersonalCargoUpgradeSlots + tier*personalCargoUpgradeStep
	if target > maximumPersonalCargoUpgrade {
		return 0, true
	}
	return uint16(target), true
}

// isAccountCargoUpgradeTool recognizes the account-cargo tool through the
// active PVF path, not through a copied commodity or item-template ID.  The
// ordinary personal-cargo recognizer explicitly excludes this directory.
func (c *pvfCeraShopCatalog) isAccountCargoUpgradeTool(definition dungeonDropItemDefinition) bool {
	if c == nil || definition.Kind != dungeonDropItemStackable {
		return false
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	return path.Ext(cleanPath) == ".stk" && strings.Contains(cleanPath, "cash/chn_account_cargo/account_cargo")
}

// personalCargoUpgradeTarget recognizes every real personal-cargo tool in
// the active PVF.  The post-152 products are not named safe_upgradekit*.stk:
// their stable contract is the stackable item's exact name plus an explicit
// target capacity in [explain].  Both values are read from the PVF that also
// supplied the requested Cera product; neither a client field nor a static
// commodity-ID map can turn an ordinary item into a warehouse upgrade.
func (c *pvfCeraShopCatalog) personalCargoUpgradeTarget(definition dungeonDropItemDefinition) (uint16, bool) {
	if target, isUpgrade := currentCeraShopPersonalCargoUpgradeTarget(definition); isUpgrade {
		return target, true
	}
	if c == nil || c.source == nil || definition.Kind != dungeonDropItemStackable {
		return 0, false
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	if cleanPath == "" || strings.Contains(cleanPath, "cash/chn_account_cargo/") || path.Ext(cleanPath) != ".stk" {
		return 0, false
	}
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return 0, false
	}
	name, nameFound := document.Text("name")
	if !nameFound || strings.TrimSpace(name) != "金库升级工具" {
		return 0, false
	}
	explain, explainFound := document.Text("explain")
	target, valid := currentCeraShopPersonalCargoTargetFromExplain(explain)
	if !explainFound || !valid {
		// This is positively a warehouse tool but its target is not a current
		// EXE-valid capacity. Reject it at checkout instead of silently placing
		// an unusable tool in the bag.
		return 0, true
	}
	return target, true
}

func currentCeraShopPersonalCargoTargetFromExplain(explain string) (uint16, bool) {
	const (
		prefix = "可以使金库的空间增加到"
		suffix = "格"
		max    = uint64(200)
	)
	clean := strings.TrimSpace(explain)
	if !strings.HasPrefix(clean, prefix) {
		return 0, false
	}
	remaining := strings.TrimPrefix(clean, prefix)
	end := strings.Index(remaining, suffix)
	if end <= 0 {
		return 0, false
	}
	target, err := strconv.ParseUint(strings.TrimSpace(remaining[:end]), 10, 16)
	if err != nil || target > max || target < uint64(currentExeInitialPersonalCargoSlotCount) {
		return 0, false
	}
	value := uint16(target)
	if (value-uint16(currentExeInitialPersonalCargoSlotCount))%16 != 0 {
		return 0, false
	}
	return value, true
}

// currentCeraShopPrepareContainerUpgrades validates and projects the persisted
// current EXE list-0/list-1/list-2 headers inside the Cera-shop asset transaction.
func currentCeraShopPrepareContainerUpgrades(
	record *dnfrepo.SettingsRecord,
	found bool,
	characterID string,
	purchases []currentCeraShopResolvedPurchase,
	now time.Time,
) (dnfrepo.SettingsRecord, uint16, uint16, uint16, bool, error) {
	hasUpgrade := false
	for _, purchase := range purchases {
		if purchase.MainInventoryUpgradeStage != 0 || purchase.AvatarInventoryUpgradeStage != 0 || purchase.PersonalCargoUpgradeTarget != 0 {
			hasUpgrade = true
			break
		}
	}
	if !hasUpgrade {
		return dnfrepo.SettingsRecord{}, 0, 0, 0, false, nil
	}
	if record == nil || strings.TrimSpace(characterID) == "" {
		return dnfrepo.SettingsRecord{}, 0, 0, 0, false, errCurrentCeraShopOwnerUnavailable
	}

	before := dnfrepo.CloneSettings(*record)
	currentMainExpansion := uint16(currentExeInitialMainSlotCount)
	currentAvatarExpansion := uint16(0)
	currentSlots := uint16(currentExeInitialPersonalCargoSlotCount)
	if !found {
		before = newCharacterContainerStateSettings(characterID, now)
	} else {
		state, stateErr := dnfrepo.CharacterContainerStateFromSettings(before, characterID)
		if stateErr != nil {
			return dnfrepo.SettingsRecord{}, 0, 0, 0, false, stateErr
		}
		currentMainExpansion = state.MainSlotCount
		currentAvatarExpansion = state.AvatarExpansion
		currentSlots = state.PersonalCargoSlotCount
	}

	targetMainExpansion := currentMainExpansion
	targetAvatarExpansion := currentAvatarExpansion
	targetSlots := currentSlots
	lastMainCommodityID := uint32(0)
	lastAvatarCommodityID := uint32(0)
	lastPersonalCargoCommodityID := uint32(0)
	for _, purchase := range purchases {
		if purchase.MainInventoryUpgradeStage != 0 {
			targetExpansion := purchase.MainInventoryUpgradeStage * 8
			if purchase.MainInventoryUpgradeStage > 3 || targetExpansion != targetMainExpansion+8 || targetExpansion > 24 {
				return dnfrepo.SettingsRecord{}, 0, 0, 0, false, fmt.Errorf("%w: commodity=%d target_main_expansion=%d current_main_expansion=%d", errCurrentCeraShopProductUnavailable, purchase.Product.CommodityID, targetExpansion, targetMainExpansion)
			}
			targetMainExpansion = targetExpansion
			lastMainCommodityID = purchase.Product.CommodityID
		}
		if purchase.AvatarInventoryUpgradeStage != 0 {
			targetExpansion := purchase.AvatarInventoryUpgradeStage * 7
			if purchase.AvatarInventoryUpgradeStage > 20 || targetExpansion != targetAvatarExpansion+7 || targetExpansion > 140 {
				return dnfrepo.SettingsRecord{}, 0, 0, 0, false, fmt.Errorf("%w: commodity=%d target_avatar_expansion=%d current_avatar_expansion=%d", errCurrentCeraShopProductUnavailable, purchase.Product.CommodityID, targetExpansion, targetAvatarExpansion)
			}
			targetAvatarExpansion = targetExpansion
			lastAvatarCommodityID = purchase.Product.CommodityID
		}
		if purchase.PersonalCargoUpgradeTarget != 0 {
			if purchase.PersonalCargoUpgradeTarget <= targetSlots {
				return dnfrepo.SettingsRecord{}, 0, 0, 0, false, fmt.Errorf("%w: commodity=%d target_slots=%d current_slots=%d", errCurrentCeraShopProductUnavailable, purchase.Product.CommodityID, purchase.PersonalCargoUpgradeTarget, targetSlots)
			}
			targetSlots = purchase.PersonalCargoUpgradeTarget
			lastPersonalCargoCommodityID = purchase.Product.CommodityID
		}
	}

	after := dnfrepo.CloneSettings(before)
	if after.Values == nil {
		after.Values = make(map[string]string)
	}
	mainInventoryExpansion := uint16(0)
	avatarInventoryExpansion := uint16(0)
	personalCargoSlotCount := uint16(0)
	if targetMainExpansion != currentMainExpansion {
		after.Values["main_list_param16"] = strconv.Itoa(int(targetMainExpansion))
		after.Values["main_inventory_last_upgrade_commodity"] = strconv.FormatUint(uint64(lastMainCommodityID), 10)
		after.Values["main_inventory_last_upgrade_from"] = strconv.Itoa(int(currentMainExpansion))
		after.Values["main_inventory_last_upgrade_to"] = strconv.Itoa(int(targetMainExpansion))
		mainInventoryExpansion = targetMainExpansion
	}
	if targetAvatarExpansion != currentAvatarExpansion {
		after.Values["avatar_list_param16"] = strconv.Itoa(int(targetAvatarExpansion))
		after.Values["avatar_inventory_last_upgrade_commodity"] = strconv.FormatUint(uint64(lastAvatarCommodityID), 10)
		after.Values["avatar_inventory_last_upgrade_from"] = strconv.Itoa(int(currentAvatarExpansion))
		after.Values["avatar_inventory_last_upgrade_to"] = strconv.Itoa(int(targetAvatarExpansion))
		avatarInventoryExpansion = targetAvatarExpansion
	}
	if targetSlots != currentSlots {
		after.Values["personal_cargo_list_param16"] = strconv.Itoa(int(targetSlots))
		after.Values["personal_cargo_last_upgrade_commodity"] = strconv.FormatUint(uint64(lastPersonalCargoCommodityID), 10)
		after.Values["personal_cargo_last_upgrade_from"] = strconv.Itoa(int(currentSlots))
		after.Values["personal_cargo_last_upgrade_to"] = strconv.Itoa(int(targetSlots))
		personalCargoSlotCount = targetSlots
	}
	upgradeCount := 0
	for _, value := range []uint16{mainInventoryExpansion, avatarInventoryExpansion, personalCargoSlotCount} {
		if value != 0 {
			upgradeCount++
		}
	}
	switch {
	case upgradeCount > 1:
		after.Values["source"] = "current_exe_cera_shop_container_upgrade"
	case mainInventoryExpansion != 0:
		after.Values["source"] = "current_exe_cera_shop_main_inventory_upgrade"
	case avatarInventoryExpansion != 0:
		after.Values["source"] = "current_exe_cera_shop_avatar_inventory_upgrade"
	default:
		after.Values["source"] = "current_exe_cera_shop_personal_cargo_upgrade"
	}
	after.UpdatedAt = now.UTC()
	return after, mainInventoryExpansion, avatarInventoryExpansion, personalCargoSlotCount, true, nil
}

// mainSlotCount 是必填参数，不是可选变参。
//
// 它决定 limitCurrentMainInventoryDefinitionSlots 是否被调用，从而决定新物品能否
// 落进未解锁的背包扩展行。早先它被写成 `...uint16`，结果 15 个调用点静默地一个都
// 没传，物品就此落进客户端不渲染的格子。改成必填后，任何新调用点漏传都会编译失败。
func grantCurrentCeraShopProduct(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
) ([]uint16, error) {
	return grantCurrentCeraShopProductWithCarryLimit(inventory, definition, count, mainSlotCount, true)
}

// grantCurrentVendingShopProduct 是贩卖机出奖：它没有"溢出邮件"这条兜底，超额部分
// 一旦被拒就会整笔失败（硬币已经扣掉），所以在它接入邮件溢出之前沿用旧的按行语义。
func grantCurrentVendingShopProduct(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
) ([]uint16, error) {
	return grantCurrentCeraShopProductWithCarryLimit(inventory, definition, count, mainSlotCount, false)
}

// grantCurrentCeraShopProductWithCarryLimit 把奖励放进角色背包。
// enforceCarryLimit=true 时按 PVF [stack limit] 的"每个容器里该道具总量上限"口径封顶，
// 放不下就整笔回滚交给调用方走溢出（邮件）；false 时保持旧的按行语义（仅贩卖机使用）。
func grantCurrentCeraShopProductWithCarryLimit(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
	enforceCarryLimit bool,
) ([]uint16, error) {
	definition = limitCurrentMainInventoryDefinitionSlots(definition, mainSlotCount)
	if inventory == nil || count == 0 {
		return nil, errCurrentCeraShopProductUnavailable
	}
	// Current runtime PVF and repository-backed wallet regressions establish
	// item 1 as the fixed slot-1 revive-coin wallet, while item 42
	// (cash/coin_general.stk) grants one wallet unit. Neither reward may be
	// spilled into an ordinary backpack slot.
	if dnfrevivecoin.IsReward(int64(definition.ItemID)) {
		if _, err := dnfrevivecoin.Grant(
			inventory,
			int64(count),
			"current_cera_reward_revive_coin_wallet",
		); err != nil {
			return nil, fmt.Errorf("%w: revive coin reward item=%d: %v", errCurrentCeraShopProductUnavailable, definition.ItemID, err)
		}
		return []uint16{uint16(dnfrevivecoin.WalletSlot)}, nil
	}
	remaining := count
	// PVF [stack limit] 是这个容器（角色背包 list 0）里该道具的总量上限：额度按容器里
	// 同 id 总量算，容器里已经有该道具的行时只能并进那一行，不能开第二行。
	// 放不下的部分不能在背包里留半截 —— 这里先冻结背包快照，失败时整笔回滚，
	// 由调用方按原有"溢出（邮件）"分支处理，避免"背包放进去一部分 + 邮件又发一整份"。
	snapshot := make(map[string]dnfrepo.ItemStack, len(inventory.Slots))
	for key, stack := range inventory.Slots {
		snapshot[key] = stack
	}
	restoreSnapshot := func() {
		inventory.Slots = make(map[string]dnfrepo.ItemStack, len(snapshot))
		for key, stack := range snapshot {
			inventory.Slots[key] = stack
		}
	}
	slots := make([]uint16, 0, 1)
	touched := make(map[uint16]struct{})
	appendTouched := func(slot uint16) {
		if _, duplicate := touched[slot]; duplicate {
			return
		}
		touched[slot] = struct{}{}
		slots = append(slots, slot)
	}
	// Package and booster rewards can be a full PVF stack while the player
	// already owns a partial stack. Fill every existing compatible stack
	// first, then spill only the remainder into new slots. Passing the full
	// reward directly to addCurrentDungeonPickupToInventory required the whole
	// chunk to fit and therefore left the partial row untouched.
	// PVF [stack limit] 是这个容器（角色背包 list 0）里该道具的总量上限：额度按容器里
	// 同 id 总量算，不是一个格子。没有声明的可叠加道具按 MaxInt32 处理，行为不变。
	space := dnfrepo.ContainerItemSpace(inventory.Slots, dnfrepo.MainInventoryListType, int64(definition.ItemID), definition.StackLimit)
	if !enforceCarryLimit {
		space = math.MaxInt32
	}
	fillRange := func(start, end int16) {
		if definition.Kind != dungeonDropItemStackable || remaining == 0 {
			return
		}
		for slot := start; slot <= end && remaining > 0; slot++ {
			key := currentDungeonPickupMainSlotKey(slot)
			stack, exists := inventory.Slots[key]
			if !exists ||
				stack.ItemID != int64(definition.ItemID) ||
				stack.Bind ||
				stack.Count <= 0 ||
				!currentItemStackExpirationMatches(stack, definition.ExpirationDate) {
				continue
			}
			available := int64(math.MaxInt32) - stack.Count
			if definition.StackLimit > 0 {
				if stack.Count >= definition.StackLimit {
					continue
				}
				available = definition.StackLimit - stack.Count
			}
			if available > space {
				available = space
			}
			if available <= 0 {
				continue
			}
			added := int64(remaining)
			if added > available {
				added = available
			}
			stack.Count += added
			if !definition.ExpirationDate.IsZero() {
				stack, _ = applyCurrentPVFItemExpiration(stack, definition)
			}
			entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[key] = stack
			remaining -= uint32(added)
			space -= added
			appendTouched(uint16(slot))
		}
	}
	if definition.Kind == dungeonDropItemStackable &&
		dungeonDropStackablePrefersItemQuickSlots(definition.StackableType) {
		fillRange(currentDungeonPickupQuickSlotStart, currentDungeonPickupQuickSlotEnd)
	}
	fillRange(definition.SlotStart, definition.SlotEnd)
	for remaining > 0 {
		if space <= 0 {
			// 容器里该道具已经到 [stack limit]：不能再开第二行。整笔回滚，由调用方走
			// 溢出分支（邮件），绝不留下"背包放进去一部分 + 邮件又发一整份"的半截状态。
			restoreSnapshot()
			return nil, fmt.Errorf(
				"%w: container stack limit reached item=%d limit=%d",
				errDungeonPickupInventoryFull,
				definition.ItemID,
				definition.StackLimit,
			)
		}
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
		slot, err := addCurrentDungeonPickupToInventory(inventory, definition, chunk)
		if err != nil {
			return nil, err
		}
		appendTouched(slot)
		remaining -= chunk
	}
	return slots, nil
}

func grantCurrentCeraShopAvatar(
	inventory *dnfrepo.InventoryRecord,
	source dnfpvf.Source,
	definition dungeonDropItemDefinition,
	product currentCeraShopProduct,
	attributeValue byte,
) (int16, error) {
	if inventory == nil || source == nil || definition.ItemID == 0 || definition.Kind != dungeonDropItemEquipment || product.Section != "avatar" {
		return 0, errCurrentCeraShopProductUnavailable
	}
	document, err := parseDungeonCardPVFDocument(source, definition.PVFPath)
	if err != nil {
		return 0, fmt.Errorf("%w: avatar item=%d PVF=%s: %v", errCurrentCeraShopProductUnavailable, definition.ItemID, definition.PVFPath, err)
	}
	equipmentType, found := document.Text("equipment type")
	if !found {
		return 0, fmt.Errorf("%w: avatar item=%d missing equipment type", errCurrentCeraShopProductUnavailable, definition.ItemID)
	}
	rule, ok := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !ok || rule.class != currentEquipmentPlacementClassAvatar {
		return 0, fmt.Errorf("%w: avatar item=%d type=%q", errCurrentCeraShopProductUnavailable, definition.ItemID, equipmentType)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	const avatarSlotStart int16 = 0
	const avatarSlotEnd int16 = 500
	var slot int16 = -1
	for candidate := avatarSlotStart; candidate <= avatarSlotEnd; candidate++ {
		if _, occupied := inventory.Slots[currentCeraShopInventorySlotKey(1, candidate)]; !occupied {
			slot = candidate
			break
		}
	}
	if slot < 0 {
		return 0, fmt.Errorf("%w: avatar item=%d range=%d..%d", errDungeonPickupInventoryFull, definition.ItemID, avatarSlotStart, avatarSlotEnd)
	}

	extra := map[string]string{
		"source":         "cera_shop_avatar",
		"item_kind":      "avatar",
		"pvf_path":       definition.PVFPath,
		"equipment_type": rule.pvfType,
		// The client's row selector is the avatar AbilityNo. In the current
		// item core that is the uint16 field at raw+0x0B, which ordinary
		// equipment calls durability. raw+0x0A is Attr (upgrade/reseal bits)
		// and must not receive the avatar selection.
		"avatar_ability_no":              strconv.FormatUint(uint64(attributeValue), 10),
		"durability":                     strconv.FormatUint(uint64(attributeValue), 10),
		"amount_or_count":                "0", // permanent avatars have no remaining-seconds countdown.
		"avatar_duration_days":           strconv.FormatUint(uint64(product.AvatarDurationDays), 10),
		"avatar_duration_selector_index": strconv.FormatUint(uint64(product.AvatarDurationIndex), 10),
	}
	// Logical count is 1 like every other equipment-kind stack; the wire
	// amount still renders as 0 through the amount_or_count override above
	// (permanent avatars have no remaining-seconds countdown). Consumers such
	// as avatar disjoint require Count == 1 for a single equipment instance.
	stack := dnfrepo.ItemStack{ItemID: int64(definition.ItemID), Count: 1, Extra: extra}
	now := time.Now().UTC()
	if product.AvatarDurationDays > 0 {
		stack.ExpireAt = now.Add(time.Duration(product.AvatarDurationDays) * 24 * time.Hour)
		expireUnix := strconv.FormatInt(stack.ExpireAt.Unix(), 10)
		stack.Extra["expire_time"] = expireUnix
		stack.Extra["expire_unix"] = expireUnix
	} else if !definition.ExpirationDate.IsZero() {
		stack, _ = applyCurrentPVFItemExpirationAt(stack, definition, now)
	}
	// Runtime PVF marks the National Day aura choices as [aurora avatar]
	// with [emblem socket default]. Skins and weapon avatars may also
	// explicitly author creation-time sockets with [emblem socket default];
	// honour that for those avatar classes, not only the historical 2018
	// package. The 2018 package predates the explicit default block and uses
	// [avatar type select] instead, so retain that narrow compatibility
	// fallback. Other avatar parts without an explicit default remain
	// unopened.
	flavorText, _ := document.Text("flavor text")
	pvfType := normalizeEquipmentPlacementPVFType(rule.pvfType)
	defaultSocketAvatar := pvfType == "[skin avatar]" || pvfType == "[weapon avatar]"
	if rule.targetSlot == 9 || defaultSocketAvatar {
		text, readErr := source.ReadText(definition.PVFPath)
		if readErr != nil {
			return 0, fmt.Errorf("%w: socketed avatar item=%d PVF=%s: %v", errCurrentCeraShopProductUnavailable, definition.ItemID, definition.PVFPath, readErr)
		}
		socketTypes := currentParseAvatarDefaultSocketTypes(text)
		if currentIs2018SpringFestivalSkin(rule.pvfType, flavorText) {
			socketTypes = currentParseAvatarSocketTypes(text)
		}
		if len(socketTypes) > 0 {
			var socketData [currentAvatarSocketBytes]byte
			currentSetAvatarSocketTypes(&socketData, socketTypes)
			currentApplyAvatarSocketDataToStack(&stack, 1, slot, socketData)
		}
	}
	entry := currentItemListEntryFromStack(1, slot, stack)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	inventory.Slots[currentCeraShopInventorySlotKey(1, slot)] = stack
	return slot, nil
}

func currentIs2018SpringFestivalSkin(pvfType, flavorText string) bool {
	return normalizeEquipmentPlacementPVFType(pvfType) == "[skin avatar]" &&
		strings.Contains(strings.TrimSpace(flavorText), "2018春节礼包")
}

func currentCeraShopInventorySlotKey(listType byte, slot int16) string {
	return strconv.Itoa(int(listType)) + ":" + strconv.FormatInt(int64(slot), 10)
}

// isCurrentCeraShopCreatureItem returns true when the item is a pet/creature
// egg that belongs in the Pet list (list 7) rather than the main backpack.
// PVF rule: equipment type contains [creature].
func isCurrentCeraShopCreatureItem(definition dungeonDropItemDefinition) bool {
	if definition.Kind != dungeonDropItemEquipment {
		return false
	}
	return strings.Contains(strings.ToLower(definition.EquipmentType), "creature")
}

// isCurrentCeraShopPetArtifactItem identifies creature artifact equipment.
// Those rows share list 7 with pets but occupy the dedicated 140..188 segment.
func isCurrentCeraShopPetArtifactItem(definition dungeonDropItemDefinition) bool {
	if definition.Kind != dungeonDropItemEquipment {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(definition.EquipmentType))
	pathText := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(definition.PVFPath), "\\", "/"))
	return strings.Contains(t, "artifact red") || strings.Contains(t, "artifact blue") || strings.Contains(t, "artifact green") ||
		strings.Contains(pathText, "/artifact_red/") || strings.Contains(pathText, "/artifact_blue/") || strings.Contains(pathText, "/artifact_green/")
}

// grantCurrentCeraShopPetArtifact grants a single non-stackable artifact into
// the current client's pet-equipment segment.
func grantCurrentCeraShopPetArtifact(inventory *dnfrepo.InventoryRecord, definition dungeonDropItemDefinition) (int16, error) {
	if inventory == nil || definition.ItemID == 0 || !isCurrentCeraShopPetArtifactItem(definition) {
		return 0, errCurrentCeraShopProductUnavailable
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	for slot := currentBoosterPetArtifactStart; slot <= currentBoosterPetArtifactEnd; slot++ {
		key := currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)
		if stack, occupied := inventory.Slots[key]; occupied && stack.ItemID > 0 && stack.Count > 0 {
			continue
		}
		extra := map[string]string{
			"source":          "cera_shop_pet_artifact",
			"item_kind":       string(dungeonDropItemEquipment),
			"pvf_path":        definition.PVFPath,
			"equipment_type":  definition.EquipmentType,
			"amount_or_count": "0",
		}
		if !definition.ExpirationDate.IsZero() {
			extra["expire_time"] = strconv.FormatInt(definition.ExpirationDate.Unix(), 10)
			extra["expire_unix"] = strconv.FormatInt(definition.ExpirationDate.Unix(), 10)
		}
		stack := dnfrepo.ItemStack{ItemID: int64(definition.ItemID), Count: 1, Extra: extra}
		if !definition.ExpirationDate.IsZero() {
			stack.ExpireAt = definition.ExpirationDate
		}
		entry := currentItemListEntryFromStack(currentPetInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[key] = stack
		return slot, nil
	}
	return 0, fmt.Errorf("%w: pet artifact slots %d..%d full", errCurrentCeraShopProductUnavailable, currentBoosterPetArtifactStart, currentBoosterPetArtifactEnd)
}

// isCurrentCeraShopPetConsumable identifies the runtime-PVF pet-consumable
// class.  In the active Script.pvf, creature food uses exactly
// [stackable type] [feed]; this is a type rule rather than an item-id/name
// exception. The pet rename card uses the same list-7 consumable segment.
func isCurrentCeraShopPetConsumable(definition dungeonDropItemDefinition) bool {
	if definition.Kind != dungeonDropItemStackable {
		return false
	}
	if normalizeDungeonDropStackableType(definition.StackableType) == "[feed]" {
		return true
	}
	cleanPath := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(definition.PVFPath, "\\", "/")))
	return strings.HasSuffix(cleanPath, "/creature/rename_card.stk")
}

// grantCurrentCeraShopPetConsumable places a PVF [feed] stack into list 7's
// current-EXE pet-consumable segment.  It deliberately cannot merge with an
// identically numbered row in list 0.
func grantCurrentCeraShopPetConsumable(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
) ([]int16, error) {
	if inventory == nil || count == 0 || definition.ItemID == 0 || !isCurrentCeraShopPetConsumable(definition) {
		return nil, errCurrentCeraShopProductUnavailable
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}

	remaining := count
	changedSlots := make([]int16, 0, 1)
	// PVF [stack limit] 是这个容器（宠物物品栏 list 7）里该道具的总量上限：额度按
	// 容器里同 id 总量算，容器里已经有该道具的行时不能再开第二行。
	petSpace := dnfrepo.ContainerItemSpace(
		inventory.Slots,
		currentPetInventoryListType,
		int64(definition.ItemID),
		definition.StackLimit,
	)
	for remaining > 0 {
		chunk := remaining
		if definition.StackLimit > 0 && int64(chunk) > definition.StackLimit {
			chunk = uint32(definition.StackLimit)
		}
		if definition.Kind == dungeonDropItemStackable && int64(chunk) > petSpace {
			chunk = uint32(petSpace)
		}
		if chunk == 0 {
			return nil, fmt.Errorf(
				"%w: pet consumable item=%d range=%d..%d",
				errDungeonPickupInventoryFull,
				definition.ItemID,
				currentCeraShopPetConsumableSlotStart,
				currentCeraShopPetConsumableSlotEnd,
			)
		}

		if definition.Kind == dungeonDropItemStackable {
			for slot := currentCeraShopPetConsumableSlotStart; slot <= currentCeraShopPetConsumableSlotEnd; slot++ {
				key := currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)
				stack, found := inventory.Slots[key]
				if !found || stack.ItemID != int64(definition.ItemID) || stack.Bind || stack.Count < 0 {
					continue
				}
				if !currentItemStackExpirationMatches(stack, definition.ExpirationDate) {
					continue
				}
				if !definition.ExpirationDate.IsZero() {
					stack, _ = applyCurrentPVFItemExpiration(stack, definition)
				}
				limit := int64(math.MaxInt32)
				if definition.StackLimit > 0 {
					limit = definition.StackLimit
				}
				if int64(chunk) > limit || stack.Count > limit-int64(chunk) || stack.Count > math.MaxInt64-int64(chunk) {
					continue
				}
				stack.Count += int64(chunk)
				entry := currentItemListEntryFromStack(currentPetInventoryListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[key] = stack
				changedSlots = append(changedSlots, slot)
				remaining -= chunk
				petSpace -= int64(chunk)
				chunk = 0
				break
			}
			if chunk == 0 {
				continue
			}
		}

		if !dnfrepo.CanOpenContainerItemRow(
			inventory.Slots,
			currentPetInventoryListType,
			int64(definition.ItemID),
			definition.StackLimit,
		) {
			return nil, fmt.Errorf(
				"%w: pet consumable item=%d range=%d..%d",
				errDungeonPickupInventoryFull,
				definition.ItemID,
				currentCeraShopPetConsumableSlotStart,
				currentCeraShopPetConsumableSlotEnd,
			)
		}

		var slot int16 = -1
		for candidate := currentCeraShopPetConsumableSlotStart; candidate <= currentCeraShopPetConsumableSlotEnd; candidate++ {
			if _, occupied := inventory.Slots[currentCeraShopInventorySlotKey(currentPetInventoryListType, candidate)]; !occupied {
				slot = candidate
				break
			}
		}
		if slot < 0 {
			return nil, fmt.Errorf(
				"%w: pet consumable item=%d range=%d..%d",
				errDungeonPickupInventoryFull,
				definition.ItemID,
				currentCeraShopPetConsumableSlotStart,
				currentCeraShopPetConsumableSlotEnd,
			)
		}

		extra := map[string]string{
			"source":         "cera_shop_pet_consumable",
			"item_kind":      string(definition.Kind),
			"pvf_path":       definition.PVFPath,
			"stackable_type": definition.StackableType,
		}
		if definition.StackLimit > 0 {
			extra["stack_limit"] = strconv.FormatInt(definition.StackLimit, 10)
		}
		stack := dnfrepo.ItemStack{ItemID: int64(definition.ItemID), Count: int64(chunk), Extra: extra}
		if !definition.ExpirationDate.IsZero() {
			stack, _ = applyCurrentPVFItemExpiration(stack, definition)
		}
		entry := currentItemListEntryFromStack(currentPetInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)] = stack
		changedSlots = append(changedSlots, slot)
		remaining -= chunk
		petSpace -= int64(chunk)
	}
	return changedSlots, nil
}

// grantCurrentCeraShopPet places a creature/pet egg into the Pet list (list 7)
// at the first available slot in the pet-body range (0-139).
func grantCurrentCeraShopPet(inventory *dnfrepo.InventoryRecord, definition dungeonDropItemDefinition) (int16, error) {
	if inventory == nil || definition.ItemID == 0 {
		return 0, errCurrentCeraShopProductUnavailable
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	const petBodySlotStart int16 = 0
	const petBodySlotEnd int16 = 139
	for slot := petBodySlotStart; slot <= petBodySlotEnd; slot++ {
		key := fmt.Sprintf("%d:%d", currentPetInventoryListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		stack := dnfrepo.ItemStack{
			ItemID: int64(definition.ItemID),
			Count:  1,
			Extra: map[string]string{
				"source":         "cera_shop_creature",
				"item_kind":      string(dungeonDropItemEquipment),
				"pvf_path":       definition.PVFPath,
				"equipment_type": definition.EquipmentType,
			},
		}
		if !definition.ExpirationDate.IsZero() {
			stack.ExpireAt = definition.ExpirationDate
			stack.Extra["expire_time"] = strconv.FormatInt(definition.ExpirationDate.Unix(), 10)
		}
		inventory.Slots[key] = stack
		return slot, nil
	}
	return 0, fmt.Errorf("%w: pet body slots 0-139 full", errCurrentCeraShopProductUnavailable)
}
