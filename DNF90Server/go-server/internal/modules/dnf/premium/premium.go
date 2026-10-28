// Package premium owns the account-level premium contract state shared by the
// inventory, skill, equipment and cera-shop modules: metadata storage keys,
// expiry math, and the current-EXE wire bodies for contract notifications.
// PVF resolution stays in dnfbridge; this package only sees typed premium
// types and durations.
package premium

import (
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Premium types come from the runtime PVF premiumlist_new.etc [type] blocks:
// 22 霸王契约(over-equip), 27 达人契约(over-skill), 84 成长契约(bonus-exp),
// 97 晶之契约, 33 遗忘河水契约. The 魔王契约 selectable perks use internal
// types DevilTypeBase+slot (86JP offset to avoid PVF type collisions) and are
// folded into the single display type DevilFolded in the select-character ACK.
const (
	// TypePrivateStore is the current-client 阿拉德大陆商贸许可证. The
	// auction coupon (TypeAuction) includes this permission, but the client
	// keeps the town companion/private-store license as a distinct premium
	// row and effect resource.
	TypePrivateStore int64 = 8
	TypeAuction      int64 = 29

	TypeOverEquip int64 = 22
	TypeOverSkill int64 = 27
	TypeBonusExp  int64 = 84
	TypeCrystal   int64 = 97
	TypeLethe     int64 = 33

	// Black Diamond is deliberately split into a server-owned entitlement and
	// a current-client presentation type. Types 1, 17 and 29 are independent
	// PVF contracts and must never be treated as Black Diamond.
	TypeBlackDiamondEntitlement int64 = 1000
	TypeBlackDiamondWire        int64 = 56

	DevilTypeBase  int64 = 580
	DevilFolded    int64 = 58
	DevilSlotCount int64 = 8
)

// Devil slot indexes from cerashop.etc [selectable character premium].
const (
	DevilSlotGoldCard     int64 = 0
	DevilSlotDoubleJar    int64 = 1
	DevilSlotQuestHelper  int64 = 2
	DevilSlotSevenBuff    int64 = 3
	DevilSlotHpMpRecover  int64 = 4
	DevilSlotFreeWeakness int64 = 5
	DevilSlotAutoRepair   int64 = 6
	DevilSlotFastJarOpen  int64 = 7
)

const (
	dailyUsageDayKey    = "premium_service_day_id"
	dailyUsageKeyPrefix = "premium_service_used_"
)

func DevilSlotType(slot int64) int64 {
	return DevilTypeBase + slot
}

func IsDevilSlotType(premiumType int64) bool {
	return premiumType >= DevilTypeBase && premiumType < DevilTypeBase+DevilSlotCount
}

// CanNotifyActivation reports whether a premium type fits the current EXE
// class0/op66 activation body. That handler reads the type as one byte.
// Devil-contract service slots are internal account types (580..587) and must
// never be truncated into this notification.
func CanNotifyActivation(premiumType int64) bool {
	return premiumType > 0 && premiumType <= 0xff && !IsDevilSlotType(premiumType)
}

func MetadataKey(premiumType int64) string {
	return "premium_expire_" + strconv.FormatInt(premiumType, 10)
}

// IsBlackDiamondStorageType reports the sole durable Black Diamond identity.
// Wire type 56 is presentation-only and is never a second durable contract.
func IsBlackDiamondStorageType(premiumType int64) bool {
	return premiumType == TypeBlackDiamondEntitlement
}

// CanonicalType maps the presentation type to the dedicated entitlement when
// an internal caller accidentally supplies the wire value. Other contracts
// remain unchanged.
func CanonicalType(premiumType int64) int64 {
	if premiumType == TypeBlackDiamondEntitlement || premiumType == TypeBlackDiamondWire {
		return TypeBlackDiamondEntitlement
	}
	return premiumType
}

// ActivationTypes returns the durable/current-client premium rows activated
// by one contract. The auction coupon includes the independently rendered
// private-store license; every other contract activates only itself.
func ActivationTypes(premiumType int64) []int64 {
	premiumType = CanonicalType(premiumType)
	if premiumType == TypeAuction {
		return []int64{TypeAuction, TypePrivateStore}
	}
	return []int64{premiumType}
}

// BlackDiamondExpireAt reads only the dedicated server-owned entitlement.
func BlackDiamondExpireAt(account dnfrepo.AccountRecord) int64 {
	return ExpireAtRaw(account, TypeBlackDiamondEntitlement)
}

func BlackDiamondActive(account dnfrepo.AccountRecord, now time.Time) bool {
	return BlackDiamondExpireAt(account) > now.Unix()
}

func ExpireAtRaw(account dnfrepo.AccountRecord, premiumType int64) int64 {
	raw := strings.TrimSpace(account.Metadata[MetadataKey(premiumType)])
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// ExpireAt returns the unix-second expiry of one premium type, 0 when absent
// or malformed. Expired entries stay in metadata (86JP lazy expiry: no sweep).
func ExpireAt(account dnfrepo.AccountRecord, premiumType int64) int64 {
	if premiumType == TypeBlackDiamondEntitlement || premiumType == TypeBlackDiamondWire {
		return BlackDiamondExpireAt(account)
	}
	return ExpireAtRaw(account, premiumType)
}

func Active(account dnfrepo.AccountRecord, premiumType int64, now time.Time) bool {
	expire := ExpireAt(account, premiumType)
	return expire > 0 && expire > now.Unix()
}

// Upsert renews one contract per the 86JP rule: the new expiry is
// max(now, current expiry) + duration*count, so renewals stack. Duration is
// in seconds. The caller owns the account-scoped transaction and the Save.
func Upsert(account *dnfrepo.AccountRecord, premiumType int64, durationSeconds int64, count int64, now time.Time) {
	premiumType = CanonicalType(premiumType)
	if account.Metadata == nil {
		account.Metadata = make(map[string]string, 8)
	}
	if count < 1 {
		count = 1
	}
	expire := now.Unix()
	if current := ExpireAt(*account, premiumType); current > expire {
		expire = current
	}
	expire += durationSeconds * count
	account.Metadata[MetadataKey(premiumType)] = strconv.FormatInt(expire, 10)
	if premiumType == TypeAuction && ExpireAtRaw(*account, TypePrivateStore) < expire {
		// The auction coupon's advertised private-store permission is valid for
		// the full coupon term, including renewals stacked on an existing coupon.
		account.Metadata[MetadataKey(TypePrivateStore)] = strconv.FormatInt(expire, 10)
	}
}

// ServiceDayID identifies the DNF service day that resets at 06:00 Beijing
// time. 06:00 UTC+8 is 22:00 UTC on the previous civil day, hence the two-hour
// unix shift before dividing into 24-hour buckets.
func ServiceDayID(now time.Time) int64 {
	return (now.Unix() + int64(2*time.Hour/time.Second)) / int64(24*time.Hour/time.Second)
}

func DailyUsageKey(slot int64) string {
	return dailyUsageKeyPrefix + strconv.FormatInt(slot, 10)
}

// DailyLimit returns the PVF-defined daily cap for one devil-contract slot.
// Zero means that the service has no character-day counter in this state
// block. HP/MP recovery is once per dungeon and fast jar opening is uncapped.
func DailyLimit(slot int64) int64 {
	switch slot {
	case DevilSlotGoldCard:
		return 10
	case DevilSlotDoubleJar:
		return 8
	case DevilSlotSevenBuff:
		return 10
	case DevilSlotFreeWeakness:
		return 10
	case DevilSlotAutoRepair:
		return 6
	default:
		return 0
	}
}

// DailyUsage reads one character's current service-day use count. Stale
// counters are treated as zero without mutating repository state.
func DailyUsage(character dnfrepo.CharacterRecord, slot int64, now time.Time) int64 {
	if slot < 0 || slot >= DevilSlotCount || character.Stats == nil {
		return 0
	}
	if character.Stats[dailyUsageDayKey] != ServiceDayID(now) {
		return 0
	}
	used := character.Stats[DailyUsageKey(slot)]
	if used < 0 {
		return 0
	}
	return used
}

// TryConsumeDaily increments one capped service atomically in the caller's
// character transaction. Uncapped services return true without changing
// character state.
func TryConsumeDaily(character *dnfrepo.CharacterRecord, slot int64, now time.Time) bool {
	limit := DailyLimit(slot)
	if character == nil || slot < 0 || slot >= DevilSlotCount {
		return false
	}
	if limit == 0 {
		return true
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 16)
	}
	dayID := ServiceDayID(now)
	if character.Stats[dailyUsageDayKey] != dayID {
		character.Stats[dailyUsageDayKey] = dayID
		for currentSlot := int64(0); currentSlot < DevilSlotCount; currentSlot++ {
			delete(character.Stats, DailyUsageKey(currentSlot))
		}
	}
	key := DailyUsageKey(slot)
	used := character.Stats[key]
	if used < 0 {
		used = 0
	}
	if used >= limit {
		return false
	}
	character.Stats[key] = used + 1
	return true
}

// SelectAckEntries builds the select-character ACK premium section:
// ActivatedNotificationMessageID is the current-EXE class0 premium activation
// notification (sub_1D61460, registered at 0x1D7EBA8). Every path that extends
// an entitlement has to send it, or the account keeps the new expiry while the
// client goes on drawing the old one.
const ActivatedNotificationMessageID uint16 = 0x42

// u8 count + count*(u8 type + i64LE remaining seconds). The eight devil slots
// fold into one entry at DevilFolded with the longest remaining duration
// (86JP SelectCharacterAckBodyBuilder order).
func SelectAckEntries(account dnfrepo.AccountRecord, now time.Time) []byte {
	entries := make([]byte, 0, 54)
	emittedTypes := make(map[int64]struct{}, 8)
	appendEntry := func(premiumType int64, remaining int64) {
		if remaining <= 0 || premiumType <= 0 || premiumType > 0xff {
			return
		}
		if _, exists := emittedTypes[premiumType]; exists {
			return
		}
		emittedTypes[premiumType] = struct{}{}
		entries = append(entries, byte(premiumType))
		for shift := uint(0); shift < 8; shift++ {
			entries = append(entries, byte(remaining>>(8*shift)))
		}
	}
	for _, premiumType := range []int64{
		TypeOverEquip,
		TypeOverSkill,
		TypeBonusExp,
		TypeCrystal,
		TypeLethe,
	} {
		appendEntry(premiumType, ExpireAt(account, premiumType)-now.Unix())
	}
	appendEntry(TypeBlackDiamondWire, BlackDiamondExpireAt(account)-now.Unix())
	// Old saves may contain only the auction row because earlier server builds
	// treated its bundled private-store permission as the same type. Project a
	// type-8 row for the longest of the explicit license and the active auction
	// coupon so those accounts regain the native town companion on login.
	privateStoreExpire := ExpireAtRaw(account, TypePrivateStore)
	if auctionExpire := ExpireAtRaw(account, TypeAuction); auctionExpire > privateStoreExpire {
		privateStoreExpire = auctionExpire
	}
	// premiumlist_new.etc contains additional current-client contract families
	// (for example types 29, 80, 88, 92 and 100). Activations are stored under
	// the same premium_expire_<type> contract, so include every active u8 type
	// deterministically instead of losing those durations on character select.
	// Devil slots are internal account types above u8 range and remain folded
	// into the one native type-58 entry below.
	extraTypes := make([]int64, 0, len(account.Metadata))
	for key := range account.Metadata {
		if !strings.HasPrefix(key, "premium_expire_") {
			continue
		}
		premiumType, err := strconv.ParseInt(strings.TrimPrefix(key, "premium_expire_"), 10, 64)
		if err != nil || premiumType <= 0 || premiumType > 0xff || premiumType == DevilFolded ||
			IsBlackDiamondStorageType(premiumType) || premiumType == TypeBlackDiamondWire {
			continue
		}
		if _, exists := emittedTypes[premiumType]; exists {
			continue
		}
		extraTypes = append(extraTypes, premiumType)
	}
	if privateStoreExpire > now.Unix() {
		found := false
		for _, premiumType := range extraTypes {
			if premiumType == TypePrivateStore {
				found = true
				break
			}
		}
		if !found {
			extraTypes = append(extraTypes, TypePrivateStore)
		}
	}
	sort.Slice(extraTypes, func(i, j int) bool {
		return extraTypes[i] < extraTypes[j]
	})
	for _, premiumType := range extraTypes {
		expireAt := ExpireAt(account, premiumType)
		if premiumType == TypePrivateStore && privateStoreExpire > expireAt {
			expireAt = privateStoreExpire
		}
		appendEntry(premiumType, expireAt-now.Unix())
	}
	devilRemaining := int64(0)
	for slot := int64(0); slot < DevilSlotCount; slot++ {
		if remaining := ExpireAt(account, DevilSlotType(slot)) - now.Unix(); remaining > devilRemaining {
			devilRemaining = remaining
		}
	}
	appendEntry(DevilFolded, devilRemaining)
	out := []byte{byte(len(entries) / 9)}
	return append(out, entries...)
}

// BuildActivatedBody is the NOTI 0x0042 body emitted for one wire-compatible
// activation: u16(2) + u8 type + i64LE remaining seconds. Callers must first
// require CanNotifyActivation; this function remains allocation-only so it can
// be used by the aligned response builders.
func BuildActivatedBody(premiumType int64, remainingSeconds int64) []byte {
	body := []byte{2, 0, byte(premiumType)}
	for shift := uint(0); shift < 8; shift++ {
		body = append(body, byte(remainingSeconds>>(8*shift)))
	}
	return body
}

// ServiceType values the current EXE's class1/op903 handler 0x01CFE1D0 branches
// on (its u16 read sits at body[1:3]).
const (
	// ServiceTypePanel applies the body to the contract singleton the coupon box
	// and Cera checkout read and, when the client's own one-shot "show the panel"
	// flag is still armed (armed by its class1/op4 SELECT_CHARACTER handler,
	// cleared by the first type-1 body), opens the contract panel.
	ServiceTypePanel uint16 = 1
	// ServiceTypeSilent stores the body through the client's plain memcpy path
	// and performs no UI action at all.
	ServiceTypeSilent uint16 = 4
)

// BuildServiceDataBody is the current EXE class1/op903 premium-service body:
// u8(a2=1) + u16(serviceType) + 74-byte blob. Each devil slot owns u32LE
// absolute Unix expiry at blob offset 6+slot*9 and u32LE current-day usage at
// 10+slot*9. The current client subtracts its CHANNELINFO/native server clock
// when rendering the remaining duration, so a relative duration here is
// immediately treated as expired.
//
// serviceType is not cosmetic: the handler treats 1 as "apply and refresh the
// contract UI" (and pops the panel while its flag is armed) and every other
// value -- 4 is the one the client itself asks with -- as a silent store.
func BuildServiceDataBody(
	account dnfrepo.AccountRecord,
	character dnfrepo.CharacterRecord,
	now time.Time,
	serviceType uint16,
) []byte {
	blob := make([]byte, 74)
	for slot := int64(0); slot < DevilSlotCount; slot++ {
		expireAt := ExpireAt(account, DevilSlotType(slot))
		if expireAt <= now.Unix() {
			expireAt = 0
		}
		if expireAt > int64(^uint32(0)) {
			expireAt = int64(^uint32(0))
		}
		offset := 6 + slot*9
		binary.LittleEndian.PutUint32(blob[offset:offset+4], uint32(expireAt))
		usedOffset := 10 + slot*9
		used := uint32(DailyUsage(character, slot, now))
		if usedOffset+4 <= int64(len(blob)) {
			binary.LittleEndian.PutUint32(blob[usedOffset:usedOffset+4], used)
		} else {
			// Slot 7 ends at the 74-byte blob tail. The current EXE reads
			// only its low usage byte from that final position.
			blob[usedOffset] = byte(used)
		}
	}
	body := []byte{1, byte(serviceType), byte(serviceType >> 8)}
	return append(body, blob...)
}
