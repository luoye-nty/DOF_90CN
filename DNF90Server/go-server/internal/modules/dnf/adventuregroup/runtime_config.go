package adventuregroup

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	ShopPointBrave = iota
	ShopPointGlory
	ShopPointPure
	ShopPointTypeCount
)

type ShopProduct struct {
	ItemID               uint32
	RequiredManageLevel  int
	Cost                 uint32
	MonthlyPurchaseLimit uint32
	ResetType            int
}

type ShopCategory struct {
	Index              byte
	MaxPoint           uint32
	ResetPointMonthly  bool
	ExperiencePerPoint uint64
	PointPerExperience uint32
	Products           []ShopProduct
}

type CapsuleConfig struct {
	MinimumExperience uint64
	MaximumExperience uint64
	MaximumCount      uint32
	MinimumLevel      int
	MaximumLevel      int
	GrantedExperience uint32
	UpdateAt          time.Time
}

// CapsuleTotalCapacity 返回成长胶囊槽的**总容量**。
//
// PVF 里的两个数是两件事，绝不能混：
//
//	[capsule max exp]   116,914,950 = **一格**的容量（界面上的 100%）
//	[capsule max count] 10          = 可以攒几格（界面写"累积数上限 : 10个"）
//
// 所以总容量 = 116,914,950 × 10。把 `[capsule max exp]` 当成总上限会让累计值
// 卡死在正好 100% 上：客户端那条进度条的分子是 `累计值 mod [capsule max exp]`，
// 于是永远显示 0%、玩家看到的就是"胶囊不累计"（2026-09-23 线上现场）。
func CapsuleTotalCapacity(config CapsuleConfig) uint64 {
	if config.MaximumExperience == 0 {
		return 0
	}
	count := config.MaximumCount
	if count == 0 {
		count = 1
	}
	if config.MaximumExperience > math.MaxUint64/uint64(count) {
		return math.MaxUint64
	}
	return config.MaximumExperience * uint64(count)
}

// CapsuleGaugeCapacity 返回一格（100%）的容量，也就是客户端进度条的分母。
func CapsuleGaugeCapacity(config CapsuleConfig) uint64 {
	return config.MaximumExperience
}

// CapsuleClaimCount 返回"现在可以领取几次成长胶囊经验值"。
//
// 客户端自己的说明文字（Interface2/Adventurer2018/AdvShop 的
// `capsule_explain`）把规则写死了：
//
//	经验值按照10% : 1,000,000点的比例换算。
//	(累积100%时， 可获得10,000,000点经验值)
//	角色Lv : Lv.50 ~ 86
//	累积数上限 : 10个
//
// 所以一次领取 = 消耗 `[capsule need minimum exp]`（= 一格 10%，
// 116,914,950 / 10 = 11,691,495）= 换到 `[capsule give exp]`（1,000,000），
// 可领取次数 = floor(累计经验 / 该档位)，一格 = 10 次。
//
// 上限必须是"格数 × 每格档数"（10 × 10 = 100）：客户端拿到这个数会**按它
// 建列表**（0x13F0FA0 里 `for (i = 0; i < count; i++)` 逐个拼字符串），
// 所以绝不能把"可领取的经验值总量"（1,000 万级）当成次数发出去 —— 那会让
// 客户端建上千万个字符串而卡死。2026-09-23 第一版就写错了，装机前发现并回滚。
func CapsuleClaimCount(config CapsuleConfig, accumulated uint64) uint32 {
	if config.MinimumExperience == 0 || config.MaximumExperience < config.MinimumExperience {
		return 0
	}
	chunks := accumulated / config.MinimumExperience
	maximum := uint64(config.MaximumCount)
	if maximum == 0 {
		maximum = 1
	}
	if perGauge := config.MaximumExperience / config.MinimumExperience; perGauge > 0 {
		if maximum > math.MaxUint32/perGauge {
			maximum = math.MaxUint32
		} else {
			maximum *= perGauge
		}
	}
	if chunks > maximum {
		chunks = maximum
	}
	if chunks > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(chunks)
}

type ExpeditionArea struct {
	Index               byte
	RequiredManageLevel int
	RewardRates         map[uint32]float64
	DesignatedGroups    []string
}

type RuntimeConfig struct {
	ShopCategories       []ShopCategory
	Growth               GrowthConfig
	Capsule              CapsuleConfig
	ExpeditionAreas      []ExpeditionArea
	AttributeIDs         map[string]byte
	AttributeGroups      map[string][]string
	CharacterAttributes  map[[2]byte][]string
	AttributeRewardRates []float64
	// PartyMemberRewardRates is `[party member rate]` (5 entries, indexed by
	// member count) and ExpeditionExtraRate is `[extra rate]`. Both feed the
	// native expedition reward formula.
	PartyMemberRewardRates []float64
	ExpeditionExtraRate    float64
	RotationDays           int
	professionClasses    map[[2]byte]byte
	professionGroupCount int
}

func (c RuntimeConfig) Clone() RuntimeConfig {
	out := c
	out.Growth.RequiredExperience = append([]uint64(nil), c.Growth.RequiredExperience...)
	out.Growth.LevelBenefits = append([]Benefits(nil), c.Growth.LevelBenefits...)
	out.ShopCategories = make([]ShopCategory, len(c.ShopCategories))
	for i, category := range c.ShopCategories {
		out.ShopCategories[i] = category
		out.ShopCategories[i].Products = append([]ShopProduct(nil), category.Products...)
	}
	out.ExpeditionAreas = make([]ExpeditionArea, len(c.ExpeditionAreas))
	for i, area := range c.ExpeditionAreas {
		out.ExpeditionAreas[i] = area
		out.ExpeditionAreas[i].RewardRates = make(map[uint32]float64, len(area.RewardRates))
		for duration, rate := range area.RewardRates {
			out.ExpeditionAreas[i].RewardRates[duration] = rate
		}
		out.ExpeditionAreas[i].DesignatedGroups = append([]string(nil), area.DesignatedGroups...)
	}
	out.AttributeIDs = make(map[string]byte, len(c.AttributeIDs))
	for key, value := range c.AttributeIDs {
		out.AttributeIDs[key] = value
	}
	out.AttributeGroups = make(map[string][]string, len(c.AttributeGroups))
	for key, values := range c.AttributeGroups {
		out.AttributeGroups[key] = append([]string(nil), values...)
	}
	out.CharacterAttributes = make(map[[2]byte][]string, len(c.CharacterAttributes))
	for key, values := range c.CharacterAttributes {
		out.CharacterAttributes[key] = append([]string(nil), values...)
	}
	out.AttributeRewardRates = append([]float64(nil), c.AttributeRewardRates...)
	out.PartyMemberRewardRates = append([]float64(nil), c.PartyMemberRewardRates...)
	out.professionClasses = make(map[[2]byte]byte, len(c.professionClasses))
	for key, value := range c.professionClasses {
		out.professionClasses[key] = value
	}
	return out
}

// ProfessionClassification resolves the ordered [expert list] group used by
// the current client collection UI for one job/base-grow slot.
func (c RuntimeConfig) ProfessionClassification(job, baseGrow int) (byte, bool) {
	if job < 0 || job > math.MaxUint8 || baseGrow <= 0 || baseGrow > math.MaxUint8 {
		return 0, false
	}
	value, found := c.professionClasses[[2]byte{byte(job), byte(baseGrow)}]
	return value, found
}

func (c RuntimeConfig) ProfessionGroupCount() int {
	return c.professionGroupCount
}

func (c RuntimeConfig) Shop(index byte) (ShopCategory, bool) {
	for _, category := range c.ShopCategories {
		if category.Index == index {
			copy := category
			copy.Products = append([]ShopProduct(nil), category.Products...)
			return copy, true
		}
	}
	return ShopCategory{}, false
}

func (c RuntimeConfig) Area(index byte) (ExpeditionArea, bool) {
	for _, area := range c.ExpeditionAreas {
		if area.Index == index {
			copy := area
			copy.DesignatedGroups = append([]string(nil), area.DesignatedGroups...)
			copy.RewardRates = make(map[uint32]float64, len(area.RewardRates))
			for duration, rate := range area.RewardRates {
				copy.RewardRates[duration] = rate
			}
			return copy, true
		}
	}
	return ExpeditionArea{}, false
}

// AreaAttributes resolves the PVF rotation deterministically from the UTC+8
// calendar day. Group order comes from the PVF, and duplicate attributes are
// skipped just like the client's uniqueness presentation.
func (c RuntimeConfig) AreaAttributes(index byte, now time.Time) []byte {
	area, ok := c.Area(index)
	if !ok {
		return nil
	}
	rotationDays := c.RotationDays
	if rotationDays <= 0 {
		rotationDays = 1
	}
	local := now.In(adventureGroupCalendarLocation)
	// local.Unix() is absolute UTC seconds, so dividing it by 86400 would roll
	// the rotation over at UTC midnight. Add the zone offset to get local
	// wall-clock seconds, which puts the boundary at local (UTC+8) midnight.
	_, offsetSeconds := local.Zone()
	day := int((local.Unix() + int64(offsetSeconds)) / 86400 / int64(rotationDays))
	out := make([]byte, 0, len(area.DesignatedGroups))
	seen := make(map[byte]struct{}, len(area.DesignatedGroups))
	for offset, groupName := range area.DesignatedGroups {
		group := c.AttributeGroups[normalizedPVFName(groupName)]
		if len(group) == 0 {
			if id, exists := c.AttributeIDs[normalizedPVFName(groupName)]; exists {
				group = []string{groupName}
				_ = id
			}
		}
		if len(group) == 0 {
			continue
		}
		for attempt := 0; attempt < len(group); attempt++ {
			name := group[(day+int(index)+offset+attempt)%len(group)]
			id, exists := c.AttributeIDs[normalizedPVFName(name)]
			if !exists {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			break
		}
	}
	return out
}

// ExpeditionReward follows the native current-EXE formula at 0x38327F0.
//
// Character levels are NOT an input: the reward is the area/duration rate times
// the party-size rate times the attribute-match bonus, rounded to float32 and
// truncated, then scaled by the extra rate. designated is the area's rotated
// attribute set, supplied by the caller so the value the client was shown and
// the value the server pays come from the same snapshot.
func (c RuntimeConfig) ExpeditionReward(areaIndex byte, durationSeconds uint32, members []ExpeditionMemberInput, designated []byte) (uint32, bool) {
	area, ok := c.Area(areaIndex)
	if !ok || len(members) == 0 || len(members) > 4 ||
		len(c.PartyMemberRewardRates) <= len(members) || c.ExpeditionExtraRate <= 0 {
		return 0, false
	}
	rate, ok := area.RewardRates[durationSeconds]
	if !ok || rate <= 0 {
		return 0, false
	}
	partyAttributes := make(map[byte]struct{})
	for _, member := range members {
		if member.Level <= 0 {
			return 0, false
		}
		attributes := c.CharacterAttributes[[2]byte{member.Job, member.GrowType & 0x0f}]
		for _, name := range attributes {
			if id, exists := c.AttributeIDs[normalizedPVFName(name)]; exists {
				partyAttributes[id] = struct{}{}
			}
		}
	}
	matched := 0
	for _, wanted := range designated {
		if _, found := partyAttributes[wanted]; found {
			matched++
		}
	}
	if matched >= len(c.AttributeRewardRates) {
		matched = len(c.AttributeRewardRates) - 1
	}
	bonus := 1.0
	if matched >= 0 && len(c.AttributeRewardRates) > 0 {
		bonus = c.AttributeRewardRates[matched]
	}
	// The current EXE rounds the float32 product, then truncates after the
	// extra rate. Character levels are not an input to this formula.
	product := float32(float32(rate) * float32(c.PartyMemberRewardRates[len(members)]))
	product = float32(product * float32(bonus))
	rounded := math.Trunc(float64(float32(product + 0.5)))
	value := math.Trunc(float64(float32(float32(rounded) * float32(c.ExpeditionExtraRate))))
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	if value > math.MaxUint32 {
		return math.MaxUint32, true
	}
	return uint32(value), true
}

type ExpeditionMemberInput struct {
	Level    int
	Job      byte
	GrowType byte
}

func loadRuntimeConfig(ctx context.Context, source dnfpvf.Source) (RuntimeConfig, error) {
	systemText, err := source.ReadText(AdventureSystem2018Path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read dnf adventure-group PVF %q: %w", AdventureSystem2018Path, err)
	}
	systemDoc, err := dnfpvf.Parse(AdventureSystem2018Path, systemText)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse dnf adventure-group PVF %q: %w", AdventureSystem2018Path, err)
	}
	expeditionText, err := source.ReadText(ExpeditionSystemPath)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read dnf adventure-group PVF %q: %w", ExpeditionSystemPath, err)
	}
	expeditionDoc, err := dnfpvf.Parse(ExpeditionSystemPath, expeditionText)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse dnf adventure-group PVF %q: %w", ExpeditionSystemPath, err)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RuntimeConfig{}, err
		}
	}
	shops, err := parseShopCategories(systemDoc)
	if err != nil {
		return RuntimeConfig{}, err
	}
	growth, err := parseGrowthConfig(systemDoc)
	if err != nil {
		return RuntimeConfig{}, err
	}
	capsule, err := parseCapsule(systemDoc)
	if err != nil {
		return RuntimeConfig{}, err
	}
	areas, err := parseExpeditionAreas(expeditionDoc)
	if err != nil {
		return RuntimeConfig{}, err
	}
	professionClasses, professionGroupCount, err := parseProfessionClassifications(systemDoc)
	if err != nil {
		return RuntimeConfig{}, err
	}
	config := RuntimeConfig{
		ShopCategories:       shops,
		Growth:               growth,
		Capsule:              capsule,
		ExpeditionAreas:      areas,
		AttributeIDs:         parseAttributeIDs(expeditionDoc),
		AttributeGroups:      parseAttributeGroups(expeditionDoc),
		CharacterAttributes:  parseCharacterAttributes(expeditionDoc),
		RotationDays:         int(firstSectionInt(expeditionDoc, "area attribute rotation period")),
		professionClasses:    professionClasses,
		professionGroupCount: professionGroupCount,
	}
	config.AttributeRewardRates, err = sectionFloats(expeditionDoc, "attribute achieve rate")
	if err != nil || len(config.AttributeRewardRates) == 0 {
		return RuntimeConfig{}, fmt.Errorf("%w: %s [attribute achieve rate]", ErrTableMalformed, ExpeditionSystemPath)
	}
	config.PartyMemberRewardRates, err = sectionFloats(expeditionDoc, "party member rate")
	if err != nil || len(config.PartyMemberRewardRates) != 5 {
		return RuntimeConfig{}, fmt.Errorf("%w: %s [party member rate]", ErrTableMalformed, ExpeditionSystemPath)
	}
	extraRates, err := sectionFloats(expeditionDoc, "extra rate")
	if err != nil || len(extraRates) != 1 {
		return RuntimeConfig{}, fmt.Errorf("%w: %s [extra rate]", ErrTableMalformed, ExpeditionSystemPath)
	}
	config.ExpeditionExtraRate = extraRates[0]
	if len(config.AttributeIDs) == 0 || len(config.AttributeGroups) == 0 || len(config.CharacterAttributes) == 0 {
		return RuntimeConfig{}, fmt.Errorf("%w: %s attribute configuration", ErrTableMalformed, ExpeditionSystemPath)
	}
	return config, nil
}

var adventureProfessionJobIDs = map[string]byte{
	"[swordman]":         0,
	"[fighter]":          1,
	"[gunner]":           2,
	"[mage]":             3,
	"[priest]":           4,
	"[at gunner]":        5,
	"[thief]":            6,
	"[at fighter]":       7,
	"[at mage]":          8,
	"[demonic swordman]": 9,
	"[creator mage]":     10,
	"[at swordman]":      11,
	"[knight]":           12,
	"[demonic lancer]":   13,
	"[at priest]":        14,
	"[gun blader]":       15,
}

func parseProfessionClassifications(doc *dnfpvf.Document) (map[[2]byte]byte, int, error) {
	result := make(map[[2]byte]byte)
	if doc == nil {
		return result, 0, nil
	}
	insideList := false
	currentGroup := -1
	groupCount := 0
	infoCount := 0
	for _, section := range doc.Sections {
		switch normalizedPVFName(section.Name) {
		case "expert list":
			insideList = true
			currentGroup = -1
		case "/expert list":
			if !insideList {
				continue
			}
			if currentGroup >= 0 && infoCount == 0 {
				return nil, 0, fmt.Errorf("%w: %s [expert list] group=%d has no [info]", ErrTableMalformed, AdventureSystem2018Path, currentGroup)
			}
			// The group count bounds the profession classification space, so it
			// is checked against ProfessionTypeCount (the 20-row descriptor
			// block), NOT ContentTypeCount (the 6 u32 content counters).
			if groupCount == 0 || groupCount > ProfessionTypeCount {
				return nil, 0, fmt.Errorf("%w: %s [expert list] groups=%d", ErrTableMalformed, AdventureSystem2018Path, groupCount)
			}
			return result, groupCount, nil
		case "group":
			if !insideList {
				continue
			}
			if currentGroup >= 0 && infoCount == 0 {
				return nil, 0, fmt.Errorf("%w: %s [expert list] group=%d has no [info]", ErrTableMalformed, AdventureSystem2018Path, currentGroup)
			}
			tokens := sectionTokens(doc, section)
			ordinal := int(firstInt(tokens))
			if ordinal != groupCount || ordinal > math.MaxUint8 {
				return nil, 0, fmt.Errorf("%w: %s [expert list] group ordinal=%d want=%d", ErrTableMalformed, AdventureSystem2018Path, ordinal, groupCount)
			}
			currentGroup = ordinal
			groupCount++
			infoCount = 0
		case "info":
			if !insideList || currentGroup < 0 {
				continue
			}
			tokens := sectionTokens(doc, section)
			if len(tokens) != 3 || tokens[0].Kind != dnfpvf.TokenString ||
				tokens[1].Kind != dnfpvf.TokenString || tokens[2].Kind != dnfpvf.TokenInt {
				return nil, 0, fmt.Errorf("%w: %s [expert list] group=%d malformed [info]", ErrTableMalformed, AdventureSystem2018Path, currentGroup)
			}
			job, found := adventureProfessionJobIDs[normalizedPVFName(tokens[0].Value)]
			awakening := normalizedPVFName(tokens[1].Value)
			grow := tokens[2].Int
			if !found || (awakening != "second awakening" && awakening != "second realization") ||
				grow <= 0 || grow > math.MaxUint8 {
				return nil, 0, fmt.Errorf("%w: %s [expert list] group=%d invalid [info]", ErrTableMalformed, AdventureSystem2018Path, currentGroup)
			}
			key := [2]byte{job, byte(grow)}
			if _, duplicate := result[key]; duplicate {
				return nil, 0, fmt.Errorf("%w: %s [expert list] duplicate job=%d grow=%d", ErrTableMalformed, AdventureSystem2018Path, job, grow)
			}
			result[key] = byte(currentGroup)
			infoCount++
		}
	}
	// Synthetic progression tables may omit [expert list]. Production uses the
	// current real PVF and therefore exposes the mapping to representative flows.
	if !insideList {
		return result, 0, nil
	}
	return nil, 0, fmt.Errorf("%w: %s unterminated [expert list]", ErrTableMalformed, AdventureSystem2018Path)
}

func parseShopCategories(doc *dnfpvf.Document) ([]ShopCategory, error) {
	var categories []ShopCategory
	for index := 0; index < len(doc.Sections); index++ {
		if !strings.EqualFold(strings.TrimSpace(doc.Sections[index].Name), "shop") {
			continue
		}
		end := index + 1
		for end < len(doc.Sections) && !strings.EqualFold(strings.TrimSpace(doc.Sections[end].Name), "/shop") {
			end++
		}
		category := ShopCategory{}
		for cursor := index + 1; cursor < end; cursor++ {
			section := doc.Sections[cursor]
			tokens := sectionTokens(doc, section)
			switch normalizedPVFName(section.Name) {
			case "index":
				category.Index = byte(firstInt(tokens))
			case "max shop point":
				category.MaxPoint = uint32(firstInt(tokens))
			case "reset point":
				category.ResetPointMonthly = true
			case "exp to get shop point":
				values := ints(tokens)
				if len(values) == 2 && values[0] > 0 && values[1] > 0 {
					category.ExperiencePerPoint = uint64(values[0])
					category.PointPerExperience = uint32(values[1])
				}
			case "adventurer shop purchase info":
				values := ints(tokens)
				if len(values)%5 != 0 {
					return nil, fmt.Errorf("%w: %s shop=%d purchase integers=%d", ErrTableMalformed, AdventureSystem2018Path, category.Index, len(values))
				}
				for offset := 0; offset < len(values); offset += 5 {
					if values[offset] <= 0 || values[offset+1] < 0 || values[offset+2] <= 0 || values[offset+3] <= 0 {
						return nil, fmt.Errorf("%w: %s shop=%d purchase row=%d", ErrTableMalformed, AdventureSystem2018Path, category.Index, offset/5)
					}
					category.Products = append(category.Products, ShopProduct{
						ItemID:               uint32(values[offset]),
						RequiredManageLevel:  int(values[offset+1]),
						Cost:                 uint32(values[offset+2]),
						MonthlyPurchaseLimit: uint32(values[offset+3]),
						ResetType:            int(values[offset+4]),
					})
				}
			}
		}
		if len(category.Products) > 0 {
			categories = append(categories, category)
		}
		index = end
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i].Index < categories[j].Index })
	if len(categories) < ShopPointTypeCount {
		return nil, fmt.Errorf("%w: %s shop categories=%d", ErrTableEmpty, AdventureSystem2018Path, len(categories))
	}
	return categories, nil
}

func parseCapsule(doc *dnfpvf.Document) (CapsuleConfig, error) {
	config := CapsuleConfig{
		MinimumExperience: uint64(firstSectionInt(doc, "capsule need minimum exp")),
		MaximumExperience: uint64(firstSectionInt(doc, "capsule max exp")),
		MaximumCount:      uint32(firstSectionInt(doc, "capsule max count")),
		MinimumLevel:      int(firstSectionInt(doc, "capsule limit level min")),
		MaximumLevel:      int(firstSectionInt(doc, "capsule limit level max")),
		GrantedExperience: uint32(firstSectionInt(doc, "capsule give exp")),
	}
	tokens, _ := doc.Section("capsule update date")
	if len(tokens) == 1 && tokens[0].Kind == dnfpvf.TokenString {
		config.UpdateAt, _ = time.ParseInLocation("2006-01-02 15:04:05", tokens[0].Value, adventureGroupCalendarLocation)
	}
	if config.MinimumExperience == 0 || config.MaximumExperience < config.MinimumExperience ||
		config.MaximumCount == 0 || config.MinimumLevel <= 0 || config.MaximumLevel < config.MinimumLevel ||
		config.GrantedExperience == 0 {
		return CapsuleConfig{}, fmt.Errorf("%w: %s capsule configuration", ErrTableMalformed, AdventureSystem2018Path)
	}
	return config, nil
}

func parseExpeditionAreas(doc *dnfpvf.Document) ([]ExpeditionArea, error) {
	var areas []ExpeditionArea
	for index := 0; index < len(doc.Sections); index++ {
		if normalizedPVFName(doc.Sections[index].Name) != "area" {
			continue
		}
		end := index + 1
		for end < len(doc.Sections) && normalizedPVFName(doc.Sections[end].Name) != "/area" {
			end++
		}
		area := ExpeditionArea{RewardRates: make(map[uint32]float64)}
		for cursor := index + 1; cursor < end; cursor++ {
			section := doc.Sections[cursor]
			tokens := sectionTokens(doc, section)
			switch normalizedPVFName(section.Name) {
			case "index":
				area.Index = byte(firstInt(tokens))
			case "required level":
				area.RequiredManageLevel = int(firstInt(tokens))
			case "designated attribute":
				for _, token := range tokens {
					if token.Kind == dnfpvf.TokenString {
						area.DesignatedGroups = append(area.DesignatedGroups, token.Value)
					}
				}
			case "reward info":
				if len(tokens)%2 != 0 {
					return nil, fmt.Errorf("%w: %s area=%d reward tokens=%d", ErrTableMalformed, ExpeditionSystemPath, area.Index, len(tokens))
				}
				for offset := 0; offset < len(tokens); offset += 2 {
					hours, rate := tokenNumber(tokens[offset]), tokenNumber(tokens[offset+1])
					if hours <= 0 || rate <= 0 {
						return nil, fmt.Errorf("%w: %s area=%d reward row=%d", ErrTableMalformed, ExpeditionSystemPath, area.Index, offset/2)
					}
					area.RewardRates[uint32(hours*3600)] = rate
				}
			}
		}
		if area.Index > 0 && area.RequiredManageLevel > 0 && len(area.RewardRates) > 0 {
			areas = append(areas, area)
		}
		index = end
	}
	sort.Slice(areas, func(i, j int) bool { return areas[i].Index < areas[j].Index })
	if len(areas) == 0 {
		return nil, fmt.Errorf("%w: %s expedition areas", ErrTableEmpty, ExpeditionSystemPath)
	}
	return areas, nil
}

func parseAttributeIDs(doc *dnfpvf.Document) map[string]byte {
	out := make(map[string]byte)
	tokens, _ := doc.Section("attribute data")
	for index := 0; index+1 < len(tokens); {
		if tokens[index].Kind != dnfpvf.TokenInt || tokens[index+1].Kind != dnfpvf.TokenString {
			index++
			continue
		}
		id := tokens[index].Int
		name := normalizedPVFName(tokens[index+1].Value)
		if id > 0 && id <= math.MaxUint8 && id != math.MaxUint8 {
			out[name] = byte(id)
		}
		index += 2
		for index < len(tokens) && tokens[index].Kind != dnfpvf.TokenInt {
			index++
		}
		if index < len(tokens) {
			index++
		}
	}
	return out
}

func parseAttributeGroups(doc *dnfpvf.Document) map[string][]string {
	out := make(map[string][]string)
	for _, section := range doc.Sections {
		if normalizedPVFName(section.Name) != "group" {
			continue
		}
		tokens := sectionTokens(doc, section)
		if len(tokens) < 2 || tokens[0].Kind != dnfpvf.TokenString {
			continue
		}
		key := normalizedPVFName(tokens[0].Value)
		for _, token := range tokens[1:] {
			if token.Kind == dnfpvf.TokenString {
				out[key] = append(out[key], token.Value)
			}
		}
	}
	if groupC := out[normalizedPVFName("[group C]")]; len(groupC) > 0 {
		out[normalizedPVFName("[GROUP C ALLOW]")] = append([]string(nil), groupC...)
	}
	return out
}

func parseCharacterAttributes(doc *dnfpvf.Document) map[[2]byte][]string {
	out := make(map[[2]byte][]string)
	tokens, _ := doc.Section("character attribute")
	for index := 0; index+5 < len(tokens); index += 6 {
		if tokens[index].Kind != dnfpvf.TokenInt || tokens[index+1].Kind != dnfpvf.TokenInt {
			continue
		}
		key := [2]byte{byte(tokens[index].Int), byte(tokens[index+1].Int)}
		for _, token := range tokens[index+3 : index+6] {
			if token.Kind == dnfpvf.TokenString {
				out[key] = append(out[key], token.Value)
			}
		}
	}
	return out
}

func sectionTokens(doc *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if doc == nil || section.Start < 0 || section.End > len(doc.Tokens) || section.Start > section.End {
		return nil
	}
	return doc.Tokens[section.Start:section.End]
}

func firstSectionInt(doc *dnfpvf.Document, name string) int64 {
	tokens, _ := doc.Section(name)
	return firstInt(tokens)
}

func firstInt(tokens []dnfpvf.Token) int64 {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int
		}
	}
	return 0
}

func ints(tokens []dnfpvf.Token) []int64 {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values
}

func sectionFloats(doc *dnfpvf.Document, name string) ([]float64, error) {
	tokens, ok := doc.Section(name)
	if !ok {
		return nil, ErrTableEmpty
	}
	values := make([]float64, 0, len(tokens))
	for _, token := range tokens {
		value := tokenNumber(token)
		if value <= 0 {
			return nil, ErrTableMalformed
		}
		values = append(values, value)
	}
	return values, nil
}

func tokenNumber(token dnfpvf.Token) float64 {
	switch token.Kind {
	case dnfpvf.TokenInt:
		return float64(token.Int)
	case dnfpvf.TokenFloat:
		return token.Float
	default:
		return 0
	}
}

func normalizedPVFName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
