package guild

import (
	"errors"
	"fmt"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// 公会勋章合成 / 守护珠合成的配方表。
//
// 来源 etc/guilddungeoncompound.etc，两段：
//
//	[compound info]             -> 守护珠 [flag gem]
//	0 4 2000  1 4 1500  2 4 750  3 2 0
//	[guild flag compound info]  -> 公会勋章 [flag]
//	0 4 2500  1 4 2000  2 4 1000  3 4 500  6 2 0
//
// 每三个一组：**稀有度、材料件数上限、每件材料的成功率(1/10000)**。稀有度就是
// 物品自己的 [rarity]（无该字段记 0），两条阶梯分别是：
//
//	勋章   0 -> 1 -> 2 -> 3 -> 6（6 是"完美的X图腾勋章"，封顶）
//	守护珠 0 -> 1 -> 2 -> 3（封顶）
//
// 成功率为 0 的那一行是**封顶行**，不是"永远失败"：它是阶梯的最后一档，没有
// 更高一档可以产出，所以直接判不可合成。
//
// # 件数与成功率的关系（2026-09-15 按玩家实测界面修正）
//
// 第二列是**上限**不是定值：客户端允许放 2..4 件，**放得越多成功率越高**。
// 所以第三列是**每件**材料的贡献，总成功率 = 件数 × 第三列，封顶 100%：
//
//	勋章   稀有度 0  2件 50%  3件 75%  4件 100%
//	       稀有度 1  2件 40%  3件 60%  4件 80%
//	       稀有度 2  2件 20%  3件 30%  4件 40%
//	       稀有度 3  2件 10%  3件 15%  4件 20%
//	守护珠 稀有度 0  2件 40%  3件 60%  4件 80%
//	       稀有度 1  2件 30%  3件 45%  4件 60%
//	       稀有度 2  2件 15%  3件 22.5% 4件 30%
//
// 支持这个读法的一个信号：勋章最低档 2500 × 4 恰好等于 10000，也就是满件数必成，
// 是个不像巧合的整数。
//
// 最初的实现把第二列当成"必须正好这么多件"，于是玩家放 2 件被一句
//「无法进行合成。」挡掉 —— 那是这一版修正的起因。
//
// 注意这张表**不在客户端里**：DNF.exe 的任何节里都搜不到 "guilddungeoncompound"
// 或 "guild flag compound" 这两个串。所以客户端不校验件数，玩家往格子里放几个就
// 发几个（实测勋章那次只放了 2 个就发了 count=2），规则全部由服务端裁决。
//
// 第三列的语义（成功率 1/10000）是推断而非反汇编证据：客户端不读这张表，服务端
// 原本也没有实现，所以没有任何一端能直接证明。运营 2026-09-15 确认按成功率实现，
// 且失败照常消耗材料。要改语义只需改这里。
var (
	ErrCompoundCatalogUnavailable = errors.New("guild compound catalog is unavailable")
	ErrCompoundCatalogInvalid     = errors.New("guild compound catalog is malformed")
)

const (
	compoundPath          = "etc/guilddungeoncompound.etc"
	compoundGemKey        = "compound info"
	compoundFlagKey       = "guild flag compound info"
	compoundRateDenominat = 10000
)

// CompoundKind 区分两条阶梯。
type CompoundKind byte

const (
	CompoundGem CompoundKind = iota
	CompoundFlag
)

func (k CompoundKind) String() string {
	if k == CompoundFlag {
		return "flag"
	}
	return "gem"
}

// CompoundMinMaterialCount 是任何一档的最少件数。客户端的合成界面允许放 2..4 件，
// 配方表只写了上限，下限只能从界面行为来。
const CompoundMinMaterialCount uint32 = 2

// CompoundRecipe 是一档的配方。
type CompoundRecipe struct {
	// Rarity 是被消耗的材料档位。
	Rarity uint32
	// MaxMaterialCount 是这一档最多能放几件材料。
	MaxMaterialCount uint32
	// RatePerMaterial 是**每件**材料贡献的万分率成功率。0 表示封顶、不可再合成。
	RatePerMaterial uint32
}

// Compoundable 报告这一档能不能继续往上合。
func (r CompoundRecipe) Compoundable() bool {
	return r.RatePerMaterial > 0 && r.MaxMaterialCount >= CompoundMinMaterialCount
}

// AcceptsMaterialCount 报告这一档收不收这么多件。
func (r CompoundRecipe) AcceptsMaterialCount(count uint32) bool {
	return r.Compoundable() && count >= CompoundMinMaterialCount && count <= r.MaxMaterialCount
}

// SuccessRateFor 是放这么多件时的万分率成功率，封顶 100%。
func (r CompoundRecipe) SuccessRateFor(count uint32) uint32 {
	if !r.AcceptsMaterialCount(count) {
		return 0
	}
	rate := r.RatePerMaterial * count
	if rate > CompoundRateDenominator {
		return CompoundRateDenominator
	}
	return rate
}

// CompoundLadder 是一条完整的阶梯，按稀有度升序。
type CompoundLadder struct {
	rows []CompoundRecipe
}

// Recipe 返回该稀有度的配方。
func (l CompoundLadder) Recipe(rarity uint32) (CompoundRecipe, bool) {
	for _, row := range l.rows {
		if row.Rarity == rarity {
			return row, true
		}
	}
	return CompoundRecipe{}, false
}

// NextRarity 返回这一档合成出来的稀有度，也就是阶梯里紧挨着的下一档。
// 勋章的 3 -> 6 这一跳正是靠"表里的下一行"得到的，不是 rarity+1。
func (l CompoundLadder) NextRarity(rarity uint32) (uint32, bool) {
	for index, row := range l.rows {
		if row.Rarity != rarity {
			continue
		}
		if !row.Compoundable() || index+1 >= len(l.rows) {
			return 0, false
		}
		return l.rows[index+1].Rarity, true
	}
	return 0, false
}

// Rarities lists the ladder in order, lowest first.
func (l CompoundLadder) Rarities() []uint32 {
	out := make([]uint32, 0, len(l.rows))
	for _, row := range l.rows {
		out = append(out, row.Rarity)
	}
	return out
}

// CompoundCatalog holds both ladders.
type CompoundCatalog struct {
	gem  CompoundLadder
	flag CompoundLadder
}

// Ladder picks the ladder for a kind.
func (c CompoundCatalog) Ladder(kind CompoundKind) CompoundLadder {
	if kind == CompoundFlag {
		return c.flag
	}
	return c.gem
}

// LoadCompoundCatalog parses etc/guilddungeoncompound.etc.
func LoadCompoundCatalog(source dnfpvf.Source) (CompoundCatalog, error) {
	if source == nil {
		return CompoundCatalog{}, ErrCompoundCatalogUnavailable
	}
	text, err := source.ReadText(compoundPath)
	if err != nil {
		return CompoundCatalog{}, fmt.Errorf("read %s: %w", compoundPath, err)
	}
	document, err := dnfpvf.Parse(compoundPath, text)
	if err != nil {
		return CompoundCatalog{}, fmt.Errorf("parse %s: %w", compoundPath, err)
	}
	gem, err := parseCompoundLadder(document.Ints(compoundGemKey), compoundGemKey)
	if err != nil {
		return CompoundCatalog{}, err
	}
	flag, err := parseCompoundLadder(document.Ints(compoundFlagKey), compoundFlagKey)
	if err != nil {
		return CompoundCatalog{}, err
	}
	return CompoundCatalog{gem: gem, flag: flag}, nil
}

func parseCompoundLadder(values []int64, key string) (CompoundLadder, error) {
	if len(values) < 3 || len(values)%3 != 0 {
		return CompoundLadder{}, fmt.Errorf("%w: %s has %d values", ErrCompoundCatalogInvalid, key, len(values))
	}
	rows := make([]CompoundRecipe, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		rarity, count, rate := values[index], values[index+1], values[index+2]
		if rarity < 0 || count < 0 || rate < 0 ||
			rarity > int64(^uint32(0)) || count > int64(^uint32(0)) || rate > compoundRateDenominat {
			return CompoundLadder{}, fmt.Errorf("%w: %s row %d = %d %d %d",
				ErrCompoundCatalogInvalid, key, index/3, rarity, count, rate)
		}
		if index > 0 && uint32(rarity) <= rows[len(rows)-1].Rarity {
			return CompoundLadder{}, fmt.Errorf("%w: %s rarities are not ascending at row %d",
				ErrCompoundCatalogInvalid, key, index/3)
		}
		rows = append(rows, CompoundRecipe{
			Rarity:           uint32(rarity),
			MaxMaterialCount: uint32(count),
			RatePerMaterial:  uint32(rate),
		})
	}
	return CompoundLadder{rows: rows}, nil
}

// CompoundRateDenominator 是成功率的分母。
const CompoundRateDenominator = compoundRateDenominat
