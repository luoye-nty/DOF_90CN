package adventuregroup

import (
	"math"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	DailyLoginStateMetadataKey     = "adventure_group_daily_login"
	RecommendedDungeonClearStatKey = "adventure_recommended_dungeon_clears"
	ContentTypeRecommendedDungeon  = 0
	// ContentTypeCount is the number of u32 content counters in the native
	// payload (raw offsets 16/32/24/28/36/40). It is NOT the profession row
	// count: the 20 profession rows are ProfessionTypeCount below.
	ContentTypeCount              = 6
	ProfessionTypeCount           = 20
	adventureGroupContentCountMax = math.MaxUint32
)

// Projection contains only account activity that has an authoritative local
// owner. Unsupported raid and PVP counters deliberately stay zero; the tower
// counters are filled by the tower owner (indexed by TowerType).
type Projection struct {
	ConsecutiveLoginDays uint32
	ContentCounts        [ContentTypeCount]uint32
	TowerRemaining       [TowerTypeCount]uint32
	TowerLimits          [TowerTypeCount]uint32
	// ProfessionCounts is what the native 20-row descriptor block carries: one
	// per-character count for each profession classification, not a content
	// counter. Row N is the classification returned by
	// RuntimeConfig.ProfessionClassification.
	ProfessionCounts       [ProfessionTypeCount]uint16
	Runtime                RuntimeState
	Representatives        RepresentativeState
	RepresentativesEnabled bool
	// CapsuleGauge 是成长胶囊**一格**的容量（PVF `[capsule max exp]`），也就是
	// 客户端那条进度条的分母。发出去的进度必须是"累计值 mod 这一格"，否则
	// 攒过 100% 之后客户端会算出 >100% 的数（或直接顶在 100% 不动）。
	CapsuleGauge uint64
	// CapsuleClaimCount 是"现在可以领取几次经验值"，单位是客户端那条说明里的
	// 10% 档（一格 = 10 次）。**它必须是一个不大的次数**：客户端会按它建列表
	// 逐个拼字符串，见 adventuregroup.CapsuleClaimCount 的说明。
	CapsuleClaimCount uint32
}

// Tower counters are indexed by these values in the adventure-group payload.
const (
	TowerDespairType = iota
	TowerGriefType
	TowerTypeCount
)

// ProjectActivity folds durable character-scoped activity into the
// account-wide values consumed by the current adventure-group packet.
func ProjectActivity(
	consecutiveLoginDays uint32,
	characters []dnfrepo.CharacterRecord,
) Projection {
	result := Projection{ConsecutiveLoginDays: consecutiveLoginDays}
	var recommended uint64
	for _, character := range characters {
		value := character.Stats[RecommendedDungeonClearStatKey]
		if value <= 0 {
			continue
		}
		if uint64(value) >= adventureGroupContentCountMax-recommended {
			recommended = adventureGroupContentCountMax
			break
		}
		recommended += uint64(value)
	}
	result.ContentCounts[ContentTypeRecommendedDungeon] = uint32(recommended)
	return result
}
