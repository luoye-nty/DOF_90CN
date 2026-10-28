// Package pvfindex 提供 PVF 物品目录内存索引与搜索。
// 由 DNF90GM工具_独立版 direct_items.go 抽取（2026-09-06），来源 vendor/longheng.io/server 语义：
// 仅保留纯 PVF/内存逻辑（catalog/load/build/resolve/search + 类型定义），已删除一切 MySQL 直写/背包/仓库代码。
// 维护纪律: 以 GM 工具源码为唯一事实源; 同步升级时逐函数对比 direct_items.go 对应实现。
package pvfindex

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Kind 物品大类（对照 GM directItemKind）。
type Kind string

const (
	KindStackable Kind = "stackable"
	KindEquipment Kind = "equipment"
)

const (
	// MaxItemAmount 单次发放数量上限（对照 GM directItemMaxAmount）。
	MaxItemAmount     = uint32(math.MaxInt32)
	stackableListPath = "stackable/stackable.lst"
	equipmentListPath = "equipment/equipment.lst"
)

// Definition 单物品最小定义（对照 GM directItemDefinition，仅保留搜索/展示/执行端校验所需字段）。
type Definition struct {
	ItemID           uint32
	Name             string
	Kind             Kind
	PVFPath          string
	AttachType       string
	StackableType    string
	EquipmentType    string
	StackLimit       int64
	SlotStart        int16
	SlotEnd          int16
	ListType         byte
	Route            string
	SupportsUpgrade  bool
	ReinforceLevel   byte
	AmplifyLevel     byte
	AmplifyType      byte
	AmplifyValue     uint16
	Durability       uint16
	ExpirationDate   time.Time
	UsablePeriodDays int64
}

// reference 物品 ID → PVF 列表条目（对照 GM directItemReference）。
type reference struct {
	kind Kind
	path string
}

// Validate 校验定义完整性（对照 GM validateDirectItemDefinition）。
func Validate(def Definition) error {
	if def.ItemID == 0 || def.SlotStart < 0 || def.SlotEnd < def.SlotStart || def.Route == "" {
		return errors.New("PVF 物品定义不完整")
	}
	if def.Kind == KindStackable || def.Kind == KindEquipment {
		return nil
	}
	return fmt.Errorf("物品 %d 的 PVF 类型不受支持", def.ItemID)
}

func normalizeEquipmentType(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "`"))
}

func applyEquipmentRoute(def *Definition) {
	def.ListType = 0
	def.SlotStart, def.SlotEnd = 9, 64
	def.Route = "角色装备背包"
	typ := normalizeEquipmentType(def.EquipmentType)
	switch typ {
	case "[hat avatar]", "[hair avatar]", "[face avatar]", "[coat avatar]", "[pants avatar]",
		"[shoes avatar]", "[breast avatar]", "[waist avatar]", "[skin avatar]",
		"[aurora avatar]", "[weapon avatar]":
		def.ListType = 1
		def.SlotStart, def.SlotEnd = 0, 500
		def.Route = "角色时装背包"
	case "[creature]":
		def.ListType = 7
		def.SlotStart, def.SlotEnd = 0, 139
		def.Route = "角色宠物背包"
	case "[artifact red]", "[artifact blue]", "[artifact green]":
		def.ListType = 7
		def.SlotStart, def.SlotEnd = 140, 188
		def.Route = "角色宠物装备背包"
	default:
		def.SupportsUpgrade = normalEquipmentType(typ)
	}
}

func applyStackableRoute(def *Definition) {
	def.ListType = 0
	def.SlotStart, def.SlotEnd = stackableSlotRange(def.StackableType)
	def.Route = "角色道具背包"
	if normalizeStackableType(def.StackableType) == "[feed]" {
		def.ListType = 7
		def.SlotStart, def.SlotEnd = 189, 238
		def.Route = "角色宠物消耗品背包"
	}
}

func normalizeStackableType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
}

func stackableSlotRange(value string) (int16, int16) {
	normalized := normalizeStackableType(value)
	switch {
	case strings.HasPrefix(normalized, "[material]") && strings.HasSuffix(normalized, "4"):
		return 345, 359
	case strings.HasPrefix(normalized, "[material]"):
		return 121, 176
	case strings.HasPrefix(normalized, "[quest]"):
		return 177, 232
	case strings.HasPrefix(normalized, "[material expert job]"):
		return 233, 288
	case strings.HasPrefix(normalized, "[avatar emblem]"):
		return 289, 344
	default:
		return 65, 120
	}
}

// WithUpgrade 校验并回填强化/增幅字段（对照 GM directDefinitionWithUpgrade）。
func WithUpgrade(def Definition, reinforceLevel, amplifyLevel, amplifyType int) (Definition, error) {
	if reinforceLevel < 0 || reinforceLevel > 31 {
		return Definition{}, errors.New("强化等级必须在 0 到 31 之间")
	}
	if amplifyLevel < 0 || amplifyLevel > 31 {
		return Definition{}, errors.New("增幅等级必须在 0 到 31 之间")
	}
	if amplifyType < 0 || amplifyType > 4 {
		return Definition{}, errors.New("增幅属性无效")
	}
	if !def.SupportsUpgrade {
		return def, nil
	}
	if reinforceLevel > 0 && (amplifyLevel > 0 || amplifyType > 0) {
		return Definition{}, errors.New("同一件装备不能同时设置强化和增幅")
	}
	if amplifyLevel > 0 && amplifyType == 0 {
		return Definition{}, errors.New("设置增幅等级时必须选择增幅属性")
	}
	def.ReinforceLevel = byte(reinforceLevel)
	def.AmplifyLevel = byte(amplifyLevel)
	def.AmplifyType = byte(amplifyType)
	if amplifyType > 0 {
		// 当前客户端以非零增幅值区分已经净化、可继续增幅的装备。
		def.AmplifyValue = 1
	}
	return def, nil
}

func equipmentUpgradeLevel(def Definition) byte {
	if def.AmplifyType > 0 {
		return def.AmplifyLevel
	}
	return def.ReinforceLevel
}

func normalEquipmentType(value string) bool {
	value = normalizeEquipmentType(value)
	switch value {
	case "[weapon]", "[title name]", "[coat]", "[shoulder]", "[pants]", "[shoes]", "[waist]",
		"[amulet]", "[wrist]", "[ring]", "[support]", "[magic stone]", "[support weapon]", "[earring]", "[flag]", "[name tag]":
		return true
	default:
		return false
	}
}
