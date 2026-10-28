package dnfbridge

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPVFWrongExpirationSource = "runtime_pvf"

const (
	currentJoustEventItemFirst uint32 = 490005585
	currentJoustEventItemLast  uint32 = 490005593
)

// currentJoustEventHasRetiredInstanceExpiration identifies the nine shop and
// source items plus the reward box in the 2017 horse-joust chain. Their
// original PVF deadline can remain in old item instances after a runtime PVF
// repair. These are static event items, not rentals or duration grants, so a
// smaller per-instance deadline cannot win over the repaired definition.
func currentJoustEventHasRetiredInstanceExpiration(
	itemID uint32,
	instanceExpire uint32,
	definitionExpire time.Time,
) bool {
	if !currentJoustAllDayEventItem(itemID) || instanceExpire == 0 || definitionExpire.IsZero() {
		return false
	}
	definitionUnix := currentPVFExpirationUnix(definitionExpire)
	return definitionUnix != 0 && instanceExpire < definitionUnix
}

func currentPVFWrongExpirationUnix(stack dnfrepo.ItemStack) uint32 {
	for _, key := range []string{"expire_time", "expire_unix"} {
		raw := strings.TrimSpace(stack.Extra[key])
		value, err := strconv.ParseUint(raw, 10, 32)
		if err == nil && value != 0 {
			return uint32(value)
		}
	}
	if !stack.ExpireAt.IsZero() && stack.ExpireAt.Unix() > 0 && stack.ExpireAt.Unix() <= math.MaxUint32 {
		return uint32(stack.ExpireAt.Unix())
	}
	return 0
}

// cleanupCurrentPVFWrongExpirationProjection removes only the synthetic state
// written by the superseded +0x6E implementation. A genuine rental expiration
// has no runtime_pvf marker and is preserved. The row tail is cleared only when
// it exactly equals the synthetic timestamp, so an unrelated real price value
// is never erased merely because the item also has a PVF expiration date.
func cleanupCurrentPVFWrongExpirationProjection(stack dnfrepo.ItemStack) (dnfrepo.ItemStack, bool) {
	if !strings.EqualFold(strings.TrimSpace(stack.Extra["expiration_source"]), currentPVFWrongExpirationSource) {
		return stack, false
	}
	wrongUnix := currentPVFWrongExpirationUnix(stack)
	changed := false
	if !stack.ExpireAt.IsZero() {
		stack.ExpireAt = time.Time{}
		changed = true
	}
	if stack.Extra != nil {
		cloned := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			cloned[key] = value
		}
		for _, key := range []string{"expire_time", "expire_unix", "expiration_source"} {
			if _, exists := cloned[key]; exists {
				delete(cloned, key)
				changed = true
			}
		}
		stack.Extra = cloned
	}
	if wrongUnix != 0 && len(stack.RawEntry) == currentItemListEntryWireSize &&
		binary.LittleEndian.Uint32(stack.RawEntry[0x6E:0x72]) == wrongUnix {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[0x6E:0x72], 0)
		changed = true
	}
	return stack, changed
}

func applyCurrentPVFStackableUsePeriodAt(stack dnfrepo.ItemStack, expirationDate time.Time, now time.Time) (dnfrepo.ItemStack, bool) {
	stack, changed := cleanupCurrentPVFWrongExpirationProjection(stack)
	patchedExpire, expireChanged := applyCurrentItemListExpireTimeRaw(stack, expirationDate)
	stack = patchedExpire
	changed = changed || expireChanged
	usePeriod := currentPVFStackableUsePeriodSeconds(expirationDate, now)
	if len(stack.RawEntry) == 0 {
		stack.RawEntry = make([]byte, currentItemListEntryWireSize)
		changed = true
	}
	if len(stack.RawEntry) == currentItemListEntryWireSize && binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]) != usePeriod {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint16(stack.RawEntry[0x0B:0x0D], usePeriod)
		changed = true
	}
	return stack, changed
}

func applyCurrentItemListExpireTimeRaw(stack dnfrepo.ItemStack, expirationDate time.Time) (dnfrepo.ItemStack, bool) {
	if expirationDate.IsZero() || expirationDate.Unix() <= 0 {
		return stack, false
	}
	changed := false
	expireUnix := sceneInventoryUint32FromInt64(expirationDate.Unix())
	if len(stack.RawEntry) == 0 {
		stack.RawEntry = make([]byte, currentItemListEntryWireSize)
		changed = true
	}
	if len(stack.RawEntry) == currentItemListEntryWireSize && (binary.LittleEndian.Uint32(stack.RawEntry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4]) != expireUnix ||
		binary.LittleEndian.Uint32(stack.RawEntry[legacyWrongCurrentItemListExpireTimeOffset:legacyWrongCurrentItemListExpireTimeOffset+4]) == expireUnix) {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		if binary.LittleEndian.Uint32(stack.RawEntry[legacyWrongCurrentItemListExpireTimeOffset:legacyWrongCurrentItemListExpireTimeOffset+4]) == expireUnix {
			binary.LittleEndian.PutUint32(stack.RawEntry[legacyWrongCurrentItemListExpireTimeOffset:legacyWrongCurrentItemListExpireTimeOffset+4], 0)
		}
		binary.LittleEndian.PutUint32(stack.RawEntry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4], expireUnix)
		changed = true
	}
	return stack, changed
}

func applyCurrentPVFStackableUsePeriod(stack dnfrepo.ItemStack, expirationDate time.Time) (dnfrepo.ItemStack, bool) {
	return applyCurrentPVFStackableUsePeriodAt(stack, expirationDate, time.Now().UTC())
}

func applyCurrentStackableExpirationAt(stack dnfrepo.ItemStack, expirationDate time.Time, now time.Time) (dnfrepo.ItemStack, bool) {
	stack, changed := cleanupCurrentPVFWrongExpirationProjection(stack)
	if expirationDate.IsZero() {
		return stack, changed
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
		changed = true
	} else {
		cloned := make(map[string]string, len(stack.Extra)+4)
		for key, value := range stack.Extra {
			cloned[key] = value
		}
		stack.Extra = cloned
	}
	expireUnix := strconv.FormatInt(expirationDate.Unix(), 10)
	for _, key := range []string{"expire_time", "expire_unix"} {
		if stack.Extra[key] != expireUnix {
			stack.Extra[key] = expireUnix
			changed = true
		}
	}
	if stack.Extra["item_kind"] == "" {
		stack.Extra["item_kind"] = string(dungeonDropItemStackable)
		changed = true
	}
	if !stack.ExpireAt.Equal(expirationDate) {
		stack.ExpireAt = expirationDate
		changed = true
	}
	patched, patchedChanged := applyCurrentPVFStackableUsePeriodAt(stack, expirationDate, now)
	return patched, changed || patchedChanged
}

func applyCurrentStackableExpiration(stack dnfrepo.ItemStack, expirationDate time.Time) (dnfrepo.ItemStack, bool) {
	return applyCurrentStackableExpirationAt(stack, expirationDate, time.Now().UTC())
}

func applyCurrentPVFItemExpirationAt(stack dnfrepo.ItemStack, definition dungeonDropItemDefinition, now time.Time) (dnfrepo.ItemStack, bool) {
	stack, changed := cleanupCurrentPVFWrongExpirationProjection(stack)
	classified, classificationChanged := applyCurrentPVFItemClassification(stack, definition)
	stack = classified
	changed = changed || classificationChanged
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// PVF `[daily delete item]` 与下面这套期限字段完全独立，而且**必须在
	// ExpirationDate 为空时也盖**：本服有意屏蔽了物品期限，带每日删除的物品基本
	// 都没有 ExpirationDate，早退会让它们永远拿不到戳。见 daily_delete_item.go。
	if stamped, dailyChanged := stampCurrentDailyDeleteItem(
		stack, definition, premium.ServiceDayID(now.UTC()),
	); dailyChanged {
		stack = stamped
		changed = true
	}
	if definition.ExpirationDate.IsZero() {
		return stack, changed
	}
	expirationDate := definition.ExpirationDate
	if instanceExpire := currentItemListStackExpire(stack); instanceExpire != 0 &&
		!currentJoustEventHasRetiredInstanceExpiration(
			sceneInventoryUint32FromInt64(stack.ItemID), instanceExpire, definition.ExpirationDate) {
		expirationDate = time.Unix(int64(instanceExpire), 0).UTC()
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 5)
		changed = true
	} else {
		cloned := make(map[string]string, len(stack.Extra)+5)
		for key, value := range stack.Extra {
			cloned[key] = value
		}
		stack.Extra = cloned
	}
	expireUnix := strconv.FormatInt(expirationDate.Unix(), 10)
	for _, key := range []string{"expire_time", "expire_unix"} {
		if stack.Extra[key] != expireUnix {
			stack.Extra[key] = expireUnix
			changed = true
		}
	}
	if definition.UsablePeriodDays > 0 {
		usablePeriod := strconv.FormatInt(definition.UsablePeriodDays, 10)
		if stack.Extra["usable_period_days"] != usablePeriod {
			stack.Extra["usable_period_days"] = usablePeriod
			changed = true
		}
		if stack.Extra["expiration_source"] != "runtime_pvf_usable_period_grant" {
			stack.Extra["expiration_source"] = "runtime_pvf_usable_period_grant"
			changed = true
		}
	}
	if !stack.ExpireAt.Equal(expirationDate) {
		stack.ExpireAt = expirationDate
		changed = true
	}
	patchedExpire, expireChanged := applyCurrentItemListExpireTimeRaw(stack, expirationDate)
	stack = patchedExpire
	changed = changed || expireChanged
	if definition.Kind == dungeonDropItemStackable {
		patched, patchedChanged := applyCurrentPVFStackableUsePeriodAt(stack, expirationDate, now)
		stack = patched
		changed = changed || patchedChanged
	}
	return stack, changed
}

func applyCurrentPVFItemClassification(stack dnfrepo.ItemStack, definition dungeonDropItemDefinition) (dnfrepo.ItemStack, bool) {
	if definition.Kind == "" {
		return stack, false
	}
	changed := false
	ensureWritableExtra := func() {
		if changed {
			return
		}
		cloned := make(map[string]string, len(stack.Extra)+5)
		for key, value := range stack.Extra {
			cloned[key] = value
		}
		stack.Extra = cloned
		changed = true
	}
	set := func(key, value string) {
		value = strings.TrimSpace(value)
		if value == "" || stack.Extra[key] == value {
			return
		}
		ensureWritableExtra()
		stack.Extra[key] = value
	}
	remove := func(key string) {
		if _, exists := stack.Extra[key]; !exists {
			return
		}
		ensureWritableExtra()
		delete(stack.Extra, key)
	}

	set("item_kind", string(definition.Kind))
	set("pvf_path", definition.PVFPath)
	switch definition.Kind {
	case dungeonDropItemStackable:
		remove("equipment_type")
		if strings.TrimSpace(definition.StackableType) == "" {
			remove("stackable_type")
		} else {
			set("stackable_type", definition.StackableType)
		}
		if definition.StackLimit > 0 {
			set("stack_limit", strconv.FormatInt(definition.StackLimit, 10))
		} else {
			remove("stack_limit")
		}
	case dungeonDropItemEquipment:
		remove("stackable_type")
		remove("stack_limit")
		if strings.TrimSpace(definition.EquipmentType) == "" {
			remove("equipment_type")
		} else {
			set("equipment_type", definition.EquipmentType)
		}
		if definition.Durability > 0 {
			set("max_durability", strconv.FormatUint(uint64(definition.Durability), 10))
			if strings.TrimSpace(stack.Extra["durability"]) == "" {
				currentDurability := definition.Durability
				if len(stack.RawEntry) == currentItemListEntryWireSize {
					if rawDurability := binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]); rawDurability > 0 {
						currentDurability = rawDurability
					}
				}
				set("durability", strconv.FormatUint(uint64(currentDurability), 10))
			}
		}
	}
	return stack, changed
}

func applyCurrentPVFItemExpiration(stack dnfrepo.ItemStack, definition dungeonDropItemDefinition) (dnfrepo.ItemStack, bool) {
	return applyCurrentPVFItemExpirationAt(stack, definition, time.Now().UTC())
}

// applyCurrentGMExpirationOverrideAt applies the explicit bounded lifetime
// selected by the trusted GM surface after the ordinary PVF expiration rules.
func applyCurrentGMExpirationOverrideAt(stack dnfrepo.ItemStack, expirationDate time.Time, expireDays int, now time.Time) (dnfrepo.ItemStack, bool) {
	if expirationDate.IsZero() || expirationDate.Unix() <= 0 {
		return stack, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	changed := false
	cloned := make(map[string]string, len(stack.Extra)+4)
	for key, value := range stack.Extra {
		cloned[key] = value
	}
	stack.Extra = cloned
	expireUnix := strconv.FormatInt(expirationDate.Unix(), 10)
	for _, key := range []string{"expire_time", "expire_unix"} {
		if stack.Extra[key] != expireUnix {
			stack.Extra[key] = expireUnix
			changed = true
		}
	}
	if stack.Extra["expiration_source"] != "gm" {
		stack.Extra["expiration_source"] = "gm"
		changed = true
	}
	if expireDays > 0 {
		days := strconv.Itoa(expireDays)
		if stack.Extra["gm_expire_days"] != days {
			stack.Extra["gm_expire_days"] = days
			changed = true
		}
	}
	if !stack.ExpireAt.Equal(expirationDate) {
		stack.ExpireAt = expirationDate
		changed = true
	}
	patched, rawChanged := applyCurrentItemListExpireTimeRaw(stack, expirationDate)
	stack = patched
	changed = changed || rawChanged
	if strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), string(dungeonDropItemStackable)) {
		patched, periodChanged := applyCurrentPVFStackableUsePeriodAt(stack, expirationDate, now)
		stack = patched
		changed = changed || periodChanged
	}
	return stack, changed
}

func normalizeCurrentPVFItemStack(stack dnfrepo.ItemStack, definition dungeonDropItemDefinition, now time.Time) (dnfrepo.ItemStack, bool) {
	classified, classificationChanged := applyCurrentPVFItemClassification(stack, definition)
	stack = classified
	_, retiredJoustDeadline := currentJoustAllDayDefinition(definition, now)
	if retiredJoustDeadline {
		cleared, clearChanged := clearCurrentJoustAllDayExpiration(stack)
		return cleared, classificationChanged || clearChanged
	}
	projected, projectionChanged := applyCurrentPVFItemExpirationAt(stack, definition, now)
	return projected, classificationChanged || projectionChanged
}

func clearCurrentJoustAllDayExpiration(stack dnfrepo.ItemStack) (dnfrepo.ItemStack, bool) {
	changed := false
	if !stack.ExpireAt.IsZero() {
		stack.ExpireAt = time.Time{}
		changed = true
	}
	if stack.Extra != nil {
		cloned := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			cloned[key] = value
		}
		for _, key := range []string{"expire_time", "expire_unix", "usable_period_days", "expiration_source"} {
			if _, exists := cloned[key]; exists {
				delete(cloned, key)
				changed = true
			}
		}
		stack.Extra = cloned
	}
	if len(stack.RawEntry) == currentItemListEntryWireSize &&
		(binary.LittleEndian.Uint32(stack.RawEntry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4]) != 0 ||
			binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]) != 0) {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4], 0)
		binary.LittleEndian.PutUint16(stack.RawEntry[0x0B:0x0D], 0)
		changed = true
	}
	return stack, changed
}

func normalizeCurrentPVFEquipmentEntry(entry dnfrepo.EquipmentEntry, definition dungeonDropItemDefinition, now time.Time) (dnfrepo.EquipmentEntry, bool) {
	stack, changed := applyCurrentPVFItemExpirationAt(dnfrepo.ItemStack{
		ItemID:   entry.ItemID,
		Bind:     entry.Bind,
		ExpireAt: entry.ExpireAt,
		RawEntry: entry.RawEntry,
		Extra:    entry.Extra,
	}, definition, now)
	entry.ExpireAt = stack.ExpireAt
	entry.RawEntry = stack.RawEntry
	entry.Extra = stack.Extra
	return entry, changed
}
