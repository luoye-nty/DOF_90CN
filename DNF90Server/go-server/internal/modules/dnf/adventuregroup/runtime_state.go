package adventuregroup

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const RuntimeStateMetadataKey = "adventure_group_runtime_v1"

var (
	ErrRuntimeStateInvalid = errors.New("adventure-group runtime state is invalid")
	// ErrPurchaseLimitReached / ErrInsufficientShopPoints 是购买被拒的两种
	// 业务原因。客户端 op1414 的结果码要按原因区分（19 = 超过购买限制、
	// 22 = 购买材料不足），回一个笼统的失败码会让客户端**完全不弹提示** ——
	// 所以这两种情况必须是可识别的独立错误，不能都并进 ErrRuntimeStateInvalid。
	ErrPurchaseLimitReached   = errors.New("adventure-group shop monthly purchase limit is reached")
	ErrInsufficientShopPoints = errors.New("adventure-group shop points are insufficient")
)

type RuntimeState struct {
	Version          int                        `json:"version"`
	ShopPoints       [ShopPointTypeCount]uint32 `json:"shop_points"`
	BraveExperience  uint64                     `json:"brave_experience"`
	GrowthExperience uint64                     `json:"growth_experience"`
	Month            string                     `json:"month"`
	// CapsuleWeek 记录成长胶囊槽上一次"归属"的那一周（每周四 06:00 为界）。
	// 见 Normalize：跨周才清零，且第一次见到空值是**认领当前值**而不是清零。
	CapsuleWeek string                `json:"capsule_week,omitempty"`
	Purchases   map[string]uint32     `json:"purchases,omitempty"`
	Expeditions map[string]Expedition `json:"expeditions,omitempty"`
}

type Expedition struct {
	Area       byte               `json:"area"`
	State      byte               `json:"state"`
	StartedAt  int64              `json:"started_at"`
	EndsAt     int64              `json:"ends_at"`
	Attributes []byte             `json:"attributes,omitempty"`
	Members    []ExpeditionMember `json:"members"`
	Reward     uint32             `json:"reward"`
}

type ExpeditionMember struct {
	// PersistentCharacterID is the durable character ID. The native client
	// echoes the u16 selector, but the selector is a roster slot and moves when
	// the roster is reordered, so busy detection must key on this instead.
	PersistentCharacterID string `json:"persistent_character_id,omitempty"`
	CharacterID           uint16 `json:"character_id"`
	Name                  string `json:"name"`
	Level                 uint32 `json:"level"`
	Job                   byte   `json:"job"`
	GrowType              byte   `json:"grow_type"`
	Status                byte   `json:"status"`
}

func ParseRuntimeState(account dnfrepo.AccountRecord, config RuntimeConfig, now time.Time) (RuntimeState, error) {
	state := RuntimeState{Version: 1}
	value := strings.TrimSpace(account.Metadata[RuntimeStateMetadataKey])
	if value != "" {
		if err := json.Unmarshal([]byte(value), &state); err != nil {
			return RuntimeState{}, fmt.Errorf("%w: %v", ErrRuntimeStateInvalid, err)
		}
		if state.Version != 1 {
			return RuntimeState{}, fmt.Errorf("%w: version=%d", ErrRuntimeStateInvalid, state.Version)
		}
	}
	if state.Purchases == nil {
		state.Purchases = make(map[string]uint32)
	}
	if state.Expeditions == nil {
		state.Expeditions = make(map[string]Expedition)
	}
	state.Normalize(config, now)
	return state, nil
}

func (s *RuntimeState) Normalize(config RuntimeConfig, now time.Time) {
	if s == nil {
		return
	}
	s.Version = 1
	month := now.In(adventureGroupCalendarLocation).Format("2006-01")
	if s.Month != month {
		s.Month = month
		s.Purchases = make(map[string]uint32)
		for _, category := range config.ShopCategories {
			if category.ResetPointMonthly && int(category.Index) < len(s.ShopPoints) {
				s.ShopPoints[category.Index] = 0
			}
		}
	}
	// 成长胶囊槽按周重置：客户端界面写明"成长胶囊于每周四凌晨6点重置"。
	// 第一次读到（旧存档没有这个字段）只**认领**当前周，绝不清零 —— 否则升级
	// 部署那一刻会把玩家攒下的一整槽胶囊静默清掉。
	capsuleWeek := adventureGroupCapsuleWeek(now)
	if s.CapsuleWeek == "" {
		s.CapsuleWeek = capsuleWeek
	} else if s.CapsuleWeek != capsuleWeek {
		s.CapsuleWeek = capsuleWeek
		s.GrowthExperience = 0
	}
	if s.Purchases == nil {
		s.Purchases = make(map[string]uint32)
	}
	if s.Expeditions == nil {
		s.Expeditions = make(map[string]Expedition)
	}
	for _, category := range config.ShopCategories {
		if int(category.Index) < len(s.ShopPoints) && category.MaxPoint > 0 &&
			s.ShopPoints[category.Index] > category.MaxPoint {
			s.ShopPoints[category.Index] = category.MaxPoint
		}
	}
	if capacity := CapsuleTotalCapacity(config.Capsule); capacity > 0 && s.GrowthExperience > capacity {
		s.GrowthExperience = capacity
	}
}

// adventureGroupCapsuleWeek 返回 now 所属"胶囊周"的标识：以**每周四 06:00**
// （Asia/Shanghai）为分界，标识取该周起始那一天的日期。界面文案是
// "成长胶囊于每周四凌晨6点重置。"，所以周四 06:00 之前仍算上一周。
func adventureGroupCapsuleWeek(now time.Time) string {
	local := now.In(adventureGroupCalendarLocation)
	// time.Weekday: Sunday=0 ... Thursday=4
	daysSinceThursday := (int(local.Weekday()) - int(time.Thursday) + 7) % 7
	boundary := time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, adventureGroupCalendarLocation).
		AddDate(0, 0, -daysSinceThursday)
	if local.Before(boundary) {
		boundary = boundary.AddDate(0, 0, -7)
	}
	return boundary.Format("2006-01-02")
}

func (s RuntimeState) Marshal() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRuntimeStateInvalid, err)
	}
	return string(data), nil
}

func (s RuntimeState) PurchaseCount(category byte, itemID uint32) uint32 {
	return s.Purchases[purchaseKey(category, itemID)]
}

func (s *RuntimeState) Spend(category ShopCategory, product ShopProduct) error {
	if s == nil || int(category.Index) >= len(s.ShopPoints) {
		return ErrRuntimeStateInvalid
	}
	count := s.PurchaseCount(category.Index, product.ItemID)
	if count >= product.MonthlyPurchaseLimit {
		return ErrPurchaseLimitReached
	}
	if s.ShopPoints[category.Index] < product.Cost {
		return ErrInsufficientShopPoints
	}
	if s.Purchases == nil {
		// ParseRuntimeState/Normalize 一定会建好这张表；这里只是不让手工构造的
		// 状态把"记一次购买"变成 panic。
		s.Purchases = make(map[string]uint32)
	}
	s.ShopPoints[category.Index] -= product.Cost
	s.Purchases[purchaseKey(category.Index, product.ItemID)] = count + 1
	return nil
}

func (s *RuntimeState) AddGlory(config RuntimeConfig, value uint32) {
	if s == nil || value == 0 {
		return
	}
	category, ok := config.Shop(ShopPointGlory)
	if !ok {
		return
	}
	s.ShopPoints[ShopPointGlory] = saturatingPointAdd(s.ShopPoints[ShopPointGlory], value, category.MaxPoint)
}

// AddExperience 记入一笔**已经发给角色**的经验。
//
//   - crystalExperience（任意等级获得的经验）按 PVF `[exp to get shop point]`
//     进骁勇结晶，余数带在 BraveExperience 里，封顶 `[max shop point]`。
//     角色自己的升级经验不受影响，这只是账号账本上的镜像累计
//     （2026-09-24 服主口径：没满级的角色也要能攒结晶）。
//   - maximumLevelExperience（其中越过等级上限的部分）再进成长胶囊槽：胶囊槽
//     是满级专属（客户端说明写着"Lv90以上的角色获得经验值时，经验值会累积在
//     成长胶囊槽中"），未满级经验一律不进。
func (s *RuntimeState) AddExperience(
	config RuntimeConfig,
	crystalExperience uint64,
	maximumLevelExperience uint64,
) {
	if s == nil || (crystalExperience == 0 && maximumLevelExperience == 0) {
		return
	}
	if category, ok := config.Shop(ShopPointBrave); ok && category.ExperiencePerPoint > 0 && category.PointPerExperience > 0 {
		total := s.BraveExperience + crystalExperience
		points := total / category.ExperiencePerPoint
		s.BraveExperience = total % category.ExperiencePerPoint
		if points > math.MaxUint32 {
			points = math.MaxUint32
		}
		add := uint64(category.PointPerExperience) * points
		if add > math.MaxUint32 {
			add = math.MaxUint32
		}
		s.ShopPoints[ShopPointBrave] = saturatingPointAdd(s.ShopPoints[ShopPointBrave], uint32(add), category.MaxPoint)
	}
	if maximumLevelExperience == 0 {
		return
	}
	// 胶囊槽可以攒多格：总容量 = [capsule max exp] × [capsule max count]。
	// 只按单格上限截断会让累计值停在正好 100%，客户端那条进度条永远 0%
	// （分子的 "累计值 mod 单格" 恒为 0），现场表现为"胶囊不累计"。
	if capacity := CapsuleTotalCapacity(config.Capsule); capacity > 0 {
		remaining := capacity - minUint64(capacity, s.GrowthExperience)
		add := maximumLevelExperience
		if add > remaining {
			add = remaining
		}
		s.GrowthExperience += add
	}
}

func (s *RuntimeState) ConsumeCapsule(config RuntimeConfig) bool {
	if s == nil || config.Capsule.MinimumExperience == 0 || s.GrowthExperience < config.Capsule.MinimumExperience {
		return false
	}
	s.GrowthExperience -= config.Capsule.MinimumExperience
	return true
}

func purchaseKey(category byte, itemID uint32) string {
	return strconv.Itoa(int(category)) + ":" + strconv.FormatUint(uint64(itemID), 10)
}

func ExpeditionKey(area byte) string {
	return strconv.Itoa(int(area))
}

func saturatingPointAdd(current, value, maximum uint32) uint32 {
	total := uint64(current) + uint64(value)
	if maximum > 0 && total > uint64(maximum) {
		return maximum
	}
	if total > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(total)
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}
