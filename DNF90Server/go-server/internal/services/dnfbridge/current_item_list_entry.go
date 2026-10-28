package dnfbridge

import (
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (e *currentItemListEntry) patchCore(slot int16, itemID uint32, amount uint32) {
	binary.LittleEndian.PutUint16(e.data[0x00:0x02], uint16(slot))
	binary.LittleEndian.PutUint32(e.data[0x02:0x06], itemID)
	binary.LittleEndian.PutUint32(e.data[0x06:0x0A], amount)
}

func (e *currentItemListEntry) setByte(offset int, value byte) {
	if value == 0 || offset < 0 || offset >= len(e.data) {
		return
	}
	e.data[offset] = value
}

func (e *currentItemListEntry) setUint16(offset int, value uint16) {
	if value == 0 || offset < 0 || offset+2 > len(e.data) {
		return
	}
	binary.LittleEndian.PutUint16(e.data[offset:offset+2], value)
}

func (e *currentItemListEntry) setUint32(offset int, value uint32) {
	if value == 0 || offset < 0 || offset+4 > len(e.data) {
		return
	}
	binary.LittleEndian.PutUint32(e.data[offset:offset+4], value)
}

func (e *currentItemListEntry) clearLegacyWrongExpiration(expire uint32) {
	if expire == 0 || binary.LittleEndian.Uint32(e.data[legacyWrongCurrentItemListExpireTimeOffset:legacyWrongCurrentItemListExpireTimeOffset+4]) != expire {
		return
	}
	binary.LittleEndian.PutUint32(e.data[legacyWrongCurrentItemListExpireTimeOffset:legacyWrongCurrentItemListExpireTimeOffset+4], 0)
}

func (e *currentItemListEntry) copyFixed(offset int, value []byte) {
	if len(value) == 0 || offset < 0 || offset+len(value) > len(e.data) {
		return
	}
	copy(e.data[offset:], value)
}

func sortCurrentItemListEntries(entries []currentItemListEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return binary.LittleEndian.Uint16(entries[i].data[0x00:0x02]) < binary.LittleEndian.Uint16(entries[j].data[0x00:0x02])
	})
}

func sortCurrentEquipmentUpdateEntries(entries []currentEquipmentUpdateEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return binary.LittleEndian.Uint16(entries[i].data[0x00:0x02]) < binary.LittleEndian.Uint16(entries[j].data[0x00:0x02])
	})
}

// currentItemListBindFlag 给出物品行 +0x0D 的值：装备在客户端就是靠这一位显示"封装 / 可交易"。
//
// 判定顺序：**显式存在**的封装别名（seal_flag / seal / bind_flag / bind）说了算，
// 别名一个都没有时才退回持久化的 bind 位。
//
// 为什么不能直接用 sceneInventoryExtraByte 的返回值：它把"解析出 0"和"键不存在"都归成 0，
// 于是**显式的 seal_flag="0"（= 未封装）会被当成没设置**，再被 bind 兜底顶成 1 —— 明明
// 解封了却显示封装。现场 2026-10-03（圣物转换书产出的圣物，用品级箱调整后显示封装、
// 小退恢复）就是这条：库里 seal_flag=0（bind_flag=1），投影却发 0x0D=1。
// 这里改成按"键是否存在"判断，0 就是 0。
func currentItemListBindFlag(bind bool, extra map[string]string) byte {
	for _, key := range []string{"seal_flag", "seal", "bind_flag", "bind"} {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			// 值存在但读不出来：保守按"已封装"，与旧行为一致（旧行为会退回 bind）。
			return 1
		}
		if value > 255 {
			return 1
		}
		return byte(value)
	}
	if bind {
		return 1
	}
	return 0
}

func currentItemListStackExpire(stack dnfrepo.ItemStack) uint32 {
	if value := sceneInventoryExtraUint32(stack.Extra, "expire_time", "expire_unix"); value != 0 {
		return value
	}
	if !stack.ExpireAt.IsZero() && stack.ExpireAt.Unix() > 0 {
		return sceneInventoryUint32FromInt64(stack.ExpireAt.Unix())
	}
	return 0
}

func currentPetRemainingSecondsAt(expire uint32, now time.Time) uint32 {
	if expire == 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	remaining := int64(expire) - now.Unix()
	if remaining <= 0 {
		return 0
	}
	return uint32(remaining)
}

func currentItemListStackableUsePeriod(stack dnfrepo.ItemStack) (uint16, bool) {
	if stack.ItemID <= 0 {
		return 0, false
	}
	kind := strings.TrimSpace(stack.Extra["item_kind"])
	stackableType := strings.TrimSpace(stack.Extra["stackable_type"])
	if !strings.EqualFold(kind, string(dungeonDropItemStackable)) && stackableType == "" {
		return 0, false
	}
	expire := currentItemListStackExpire(stack)
	if expire == 0 {
		return 0, false
	}
	return currentPVFStackableUsePeriodSeconds(time.Unix(int64(expire), 0).UTC(), time.Now().UTC()), true
}

func currentItemListEquipmentExpire(equipped dnfrepo.EquipmentEntry) uint32 {
	if value := sceneInventoryExtraUint32(equipped.Extra, "expire_time", "expire_unix"); value != 0 {
		return value
	}
	if !equipped.ExpireAt.IsZero() && equipped.ExpireAt.Unix() > 0 {
		return sceneInventoryUint32FromInt64(equipped.ExpireAt.Unix())
	}
	return 0
}

func currentItemListFixedExtraBytes(extra map[string]string, length int, keys ...string) []byte {
	if length <= 0 {
		return nil
	}
	for _, key := range keys {
		decoded, ok := currentItemListDecodeExtraHex(extra, key)
		if !ok {
			continue
		}
		out := make([]byte, length)
		copy(out, decoded)
		return out
	}
	return nil
}

// currentItemListFullRowExtraBytes reads a stored complete current item row.
// Unlike currentItemListFixedExtraBytes it refuses a short stored row instead
// of zero-padding it. A row shorter than the current wire size is the legacy
// constructor summary (buildInitialEquipmentRawEntry), whose slot is one u8
// where the current row has a u16; every field after it therefore sits one
// byte low. Padding such a row into the current frame reinterprets the low
// durability byte as the packed reinforce byte at +0x0A, which projects a
// level-1 starter item as a reinforced one. Refusing it here lets the caller
// fall back to the explicit per-field projection, which knows both layouts.
func currentItemListFullRowExtraBytes(extra map[string]string, keys ...string) []byte {
	for _, key := range keys {
		decoded, ok := currentItemListDecodeExtraHex(extra, key)
		if !ok || len(decoded) < currentItemListEntryWireSize {
			continue
		}
		out := make([]byte, currentItemListEntryWireSize)
		copy(out, decoded)
		return out
	}
	return nil
}

func currentItemListDecodeExtraHex(extra map[string]string, key string) ([]byte, bool) {
	raw := strings.TrimSpace(extra[key])
	if raw == "" {
		return nil, false
	}
	raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return nil, false
	}
	return decoded, true
}

func currentItemListAvatarSocketData(extra map[string]string) []byte {
	raw := currentItemListFixedExtraBytes(extra, currentAvatarSocketBytes, "avatar_socket_data", "reserved2", "jewel_socket", "jewelSocket")
	if len(raw) != currentAvatarSocketBytes {
		return raw
	}
	var data [currentAvatarSocketBytes]byte
	copy(data[:], raw)
	data = currentNormalizeAvatarSocketData(data)
	return append([]byte(nil), data[:]...)
}

func currentItemListAvatarColorData(extra map[string]string) []byte {
	return currentItemListVariableExtraBytes(extra, "avatar_color_data", "color_data", "tail_data", "tailData")
}

func currentItemListVariableExtraBytes(extra map[string]string, keys ...string) []byte {
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) == 0 {
			continue
		}
		return decoded
	}
	return nil
}
