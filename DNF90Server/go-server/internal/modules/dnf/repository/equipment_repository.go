// 本文件定义 DNF 穿戴装备仓储接口、记录和字段保存入口。
package repository

import (
	"context"
	"strings"
	"time"

	"longheng.io/server/internal/platform/db"
)

// EquipmentRepository 保存角色穿戴装备 raw entry。
type EquipmentRepository interface {
	db.Store[EquipmentRecord]
}

// EquipmentRecord 是角色穿戴装备的权威记录。
type EquipmentRecord struct {
	CharacterID string                    `json:"character_id"`
	Entries     map[string]EquipmentEntry `json:"entries,omitempty"`
	UpdatedAt   time.Time                 `json:"updated_at,omitempty"`
}

// EquipmentEntry 是单个穿戴槽位的 raw entry。
// RawEntry 按旧客户端 USERINFO/穿戴链路保存原始槽位数据，修理耐久只补 raw[10..11]。
type EquipmentEntry struct {
	SlotIndex int16             `json:"slot_index"`
	ItemID    int64             `json:"item_id"`
	Bind      bool              `json:"bind,omitempty"`
	ExpireAt  time.Time         `json:"expire_at,omitempty"`
	RawEntry  []byte            `json:"raw_entry,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// CloneAvatarAppearanceExtraKey stores the ordinary avatar template copied by
// a worn clone-avatar instance. The equipped ItemID remains the real clone
// item; only appearance projections substitute this linked template.
const CloneAvatarAppearanceExtraKey = "clone_avatar_appearance_id"

// AuraLinkedTemplateExtraKey stores the aura template that a worn 转职光环
// (aura look-replace avatar) covered. It is the OPPOSITE of the clone key: the
// 转职光环 keeps its own appearance, and the covered aura is only the stat/link
// source the client resolves from the USERINFO equipment row's link field.
// Never render this value as the worn item's appearance.
const AuraLinkedTemplateExtraKey = "aura_linked_template_id"

// AuraLinkedSourceExtraKey records where the covered aura physically went, as
// the inventory record's own slot key ("<listType>:<slotIndex>", e.g. "1:31").
//
// The link field above only carries the covered aura's TEMPLATE id, and the
// client applies the covered INSTANCE's socket bytes from the conditional block
// that follows it. Those bytes therefore have to be resolved from a live
// inventory row at projection time -- a snapshot taken when the 转职光环 was
// worn would miss emblems socketed afterwards. A row that no longer holds the
// linked template means the binding is dead and no block is emitted; guessing
// from the template or the bag is forbidden.
const AuraLinkedSourceExtraKey = "aura_linked_source"

// EquipmentField 表示穿戴装备记录可局部保存的字段。
type EquipmentField string

const (
	EquipmentFieldEntries EquipmentField = "entries"
)

// SaveEquipmentFields 保存穿戴装备指定字段；底层不支持局部保存时退化为整条保存。
func SaveEquipmentFields(ctx context.Context, repo EquipmentRepository, record EquipmentRecord, fields ...EquipmentField) error {
	return db.SaveFields(ctx, repo, record, EquipmentFields.Normalize, fields...)
}

// CloneEquipment 深拷贝穿戴装备记录，避免 raw entry 和 Extra map 被调用方共享污染。
func CloneEquipment(record EquipmentRecord) EquipmentRecord {
	if len(record.Entries) == 0 {
		record.Entries = nil
		return record
	}
	out := make(map[string]EquipmentEntry, len(record.Entries))
	for key, entry := range record.Entries {
		entry.RawEntry = append([]byte(nil), entry.RawEntry...)
		entry.Extra = cloneStringMap(entry.Extra)
		out[key] = entry
	}
	record.Entries = out
	return record
}

func EquipmentKey(record EquipmentRecord) string {
	return strings.TrimSpace(record.CharacterID)
}
