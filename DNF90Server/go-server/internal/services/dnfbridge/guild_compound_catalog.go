package dnfbridge

import (
	"sort"
	"strings"
	"sync"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

// 公会勋章 / 守护珠的合成候选表。
//
// 合成的产物是"下一档里的随机一件"，所以需要按稀有度分组的完整候选集。这里不写
// 死 ID 段：itemRefs / stackableIDs / equipmentIDs 是从 PVF 的 .lst 全量枚举出来
// 的，逐个 ResolveItem 之后按 [flag] / [flag gem] 归类，和 list-38 的归位规则
// （currentMailboxPVFPlacement）用的是同一个判据。
//
// 结果排序后缓存：同一档里选哪一件是随机的，但候选顺序必须稳定，否则测试和日志
// 都没法复现。
type currentGuildCompoundCandidates struct {
	once  sync.Once
	err   error
	byKey map[currentGuildCompoundKey][]uint32
}

type currentGuildCompoundKey struct {
	Kind   dnfguild.CompoundKind
	Rarity uint32
}

func (s *Service) currentGuildCompoundCandidates(
	kind dnfguild.CompoundKind,
	rarity uint32,
) ([]uint32, error) {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	s.guildCompoundCandidates.once.Do(func() {
		s.guildCompoundCandidates.byKey, s.guildCompoundCandidates.err =
			buildCurrentGuildCompoundCandidates(catalog)
	})
	if s.guildCompoundCandidates.err != nil {
		return nil, s.guildCompoundCandidates.err
	}
	return s.guildCompoundCandidates.byKey[currentGuildCompoundKey{Kind: kind, Rarity: rarity}], nil
}

func buildCurrentGuildCompoundCandidates(
	catalog *pvfDungeonDropCatalog,
) (map[currentGuildCompoundKey][]uint32, error) {
	out := make(map[currentGuildCompoundKey][]uint32, 16)
	add := func(kind dnfguild.CompoundKind, itemID uint32, rarity int64) {
		if rarity < 0 {
			return
		}
		key := currentGuildCompoundKey{Kind: kind, Rarity: uint32(rarity)}
		out[key] = append(out[key], itemID)
	}
	for _, itemID := range catalog.stackableIDs {
		if !currentGuildCompoundPathMayHold(catalog, itemID) {
			continue
		}
		definition, err := catalog.ResolveItem(itemID)
		if err != nil || definition.Kind != dungeonDropItemStackable {
			continue
		}
		if !strings.HasPrefix(normalizeDungeonDropStackableType(definition.StackableType), "[flag gem]") {
			continue
		}
		add(dnfguild.CompoundGem, itemID, definition.Rarity)
	}
	for _, itemID := range catalog.equipmentIDs {
		if !currentGuildCompoundPathMayHold(catalog, itemID) {
			continue
		}
		definition, err := catalog.ResolveItem(itemID)
		if err != nil || definition.Kind != dungeonDropItemEquipment {
			continue
		}
		if normalizeEquipmentPlacementPVFType(definition.EquipmentType) != "[flag]" {
			continue
		}
		add(dnfguild.CompoundFlag, itemID, definition.Rarity)
	}
	for key := range out {
		sort.Slice(out[key], func(i, j int) bool { return out[key][i] < out[key][j] })
	}
	return out, nil
}

// currentGuildCompoundPathMayHold 是建索引时的**廉价预筛**。
//
// 没有它，建一次索引要对 PVF 里**每一件**可堆叠物与装备跑一次 ResolveItem（各自
// 都要解析一份文档）—— 实测第一次合成因此卡了 **8.66 秒**，玩家看到的就是"合成
// 失败会卡住一段时间"。
//
// 预筛只看已经加载好的引用路径，不读任何文档：守护珠在 stackable/flaggem/ 下，
// 勋章在 equipment/character/common/flag/ 下，两者的路径里都带 "flag"。判据故意
// 放宽成子串匹配而不是精确前缀 —— 宁可多解析几件，也不要因为路径换了目录而悄悄
// 丢掉候选。真正的归类判据仍然是 PVF 的 [flag] / [flag gem]，没有放松。
func currentGuildCompoundPathMayHold(catalog *pvfDungeonDropCatalog, itemID uint32) bool {
	reference, found := catalog.itemRefs[itemID]
	if !found {
		return false
	}
	return strings.Contains(strings.ToLower(reference.path), "flag")
}

// currentGuildCompoundGemFamilySuccessor 是同属性守护珠的下一档 ID。
//
// 守护珠的 ID 是极其规整的 900<属性2位><档位1位>：90000..90003 是物理防御力的
// 0..3 档，90010..90013 是魔法防御力的 0..3 档，一直到 90120..90123，13 个属性
// 各 4 档。所以"同属性升一档"就是 ID + 1。
//
// 只有在四件材料是**同一个 ID** 时才走这条路（守护珠是可堆叠的，正常操作就是从
// 同一摞里拖四个出来，实测请求也确实是四个槽位都指向同一格）。材料属性不一致时
// 没有"同属性"可言，退回随机。
//
// 这条规则是设计判断而非反汇编证据：这张配方表不在客户端里，服务端此前也没有实现，
// 两端都不存在可以证明它的代码。挑它是因为属性对玩家有意义，随机掉属性会让合成
// 变成纯赌博。勋章没有这种对应关系（稀有度 0 的"古老/破旧/耀眼"和稀有度 1 的
// "简陋/破旧/寒酸"不是同一组词），所以勋章只能随机。
func currentGuildCompoundGemFamilySuccessor(materials []uint32, nextRarity uint32, candidates []uint32) (uint32, bool) {
	if len(materials) == 0 {
		return 0, false
	}
	first := materials[0]
	for _, material := range materials[1:] {
		if material != first {
			return 0, false
		}
	}
	successor := first + 1
	for _, candidate := range candidates {
		if candidate == successor {
			return successor, true
		}
	}
	_ = nextRarity
	return 0, false
}
