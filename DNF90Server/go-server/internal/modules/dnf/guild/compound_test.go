package guild

import (
	"errors"
	"testing"
)

type compoundTestSource string

func (s compoundTestSource) ReadText(string) (string, error) { return string(s), nil }

// 运行时 PVF 的原文。
const liveCompoundTable = `[compound info]
0 4 2000 1 4 1500 2 4 750 3 2 0
[/compound info]

[guild flag compound info]
0 4 2500 1 4 2000 2 4 1000 3 4 500 6 2 0
[/guid flag compound info]
`

func TestLoadCompoundCatalogMatchesTheLivePVF(t *testing.T) {
	catalog, err := LoadCompoundCatalog(compoundTestSource(liveCompoundTable))
	if err != nil {
		t.Fatalf("LoadCompoundCatalog() error = %v", err)
	}

	for _, row := range []struct {
		kind        CompoundKind
		rarity      uint32
		wantCount   uint32
		wantRate    uint32
		wantNext    uint32
		wantHasNext bool
	}{
		{kind: CompoundGem, rarity: 0, wantCount: 4, wantRate: 2000, wantNext: 1, wantHasNext: true},
		{kind: CompoundGem, rarity: 1, wantCount: 4, wantRate: 1500, wantNext: 2, wantHasNext: true},
		{kind: CompoundGem, rarity: 2, wantCount: 4, wantRate: 750, wantNext: 3, wantHasNext: true},
		{kind: CompoundGem, rarity: 3, wantCount: 2, wantRate: 0},
		{kind: CompoundFlag, rarity: 0, wantCount: 4, wantRate: 2500, wantNext: 1, wantHasNext: true},
		{kind: CompoundFlag, rarity: 1, wantCount: 4, wantRate: 2000, wantNext: 2, wantHasNext: true},
		{kind: CompoundFlag, rarity: 2, wantCount: 4, wantRate: 1000, wantNext: 3, wantHasNext: true},
		// 勋章的 3 -> 6 这一跳必须靠"表里的下一行"，不是 rarity+1：
		// 稀有度 4、5 的勋章根本不存在。
		{kind: CompoundFlag, rarity: 3, wantCount: 4, wantRate: 500, wantNext: 6, wantHasNext: true},
		{kind: CompoundFlag, rarity: 6, wantCount: 2, wantRate: 0},
	} {
		ladder := catalog.Ladder(row.kind)
		recipe, found := ladder.Recipe(row.rarity)
		if !found {
			t.Fatalf("%s rarity %d is missing from the ladder", row.kind, row.rarity)
		}
		if recipe.MaxMaterialCount != row.wantCount || recipe.RatePerMaterial != row.wantRate {
			t.Fatalf("%s rarity %d = %+v, want max_count=%d rate_per_material=%d",
				row.kind, row.rarity, recipe, row.wantCount, row.wantRate)
		}
		next, hasNext := ladder.NextRarity(row.rarity)
		if hasNext != row.wantHasNext || (hasNext && next != row.wantNext) {
			t.Fatalf("%s rarity %d next = %d, %t; want %d, %t",
				row.kind, row.rarity, next, hasNext, row.wantNext, row.wantHasNext)
		}
	}
}

// 成功率 0 的那一行是封顶行，不是"永远失败"。
func TestCompoundTopRowIsNotCompoundable(t *testing.T) {
	catalog, err := LoadCompoundCatalog(compoundTestSource(liveCompoundTable))
	if err != nil {
		t.Fatalf("LoadCompoundCatalog() error = %v", err)
	}
	for _, row := range []struct {
		kind   CompoundKind
		rarity uint32
	}{
		{kind: CompoundGem, rarity: 3},
		{kind: CompoundFlag, rarity: 6},
	} {
		recipe, _ := catalog.Ladder(row.kind).Recipe(row.rarity)
		if recipe.Compoundable() {
			t.Fatalf("%s rarity %d must be the top of the ladder", row.kind, row.rarity)
		}
	}
}

func TestLoadCompoundCatalogRejectsMalformedTables(t *testing.T) {
	for name, table := range map[string]string{
		"件数不成三元组": "[compound info]\n0 4\n[/compound info]\n[guild flag compound info]\n0 4 2500\n[/x]\n",
		"稀有度不升序":  "[compound info]\n1 4 2000 0 4 1500\n[/compound info]\n[guild flag compound info]\n0 4 2500\n[/x]\n",
		"成功率超过分母": "[compound info]\n0 4 20000\n[/compound info]\n[guild flag compound info]\n0 4 2500\n[/x]\n",
	} {
		if _, err := LoadCompoundCatalog(compoundTestSource(table)); !errors.Is(err, ErrCompoundCatalogInvalid) {
			t.Fatalf("%s: error = %v, want ErrCompoundCatalogInvalid", name, err)
		}
	}
	if _, err := LoadCompoundCatalog(nil); !errors.Is(err, ErrCompoundCatalogUnavailable) {
		t.Fatalf("nil source error = %v, want ErrCompoundCatalogUnavailable", err)
	}
}

// 件数是 2..上限的区间，成功率随件数线性上涨、封顶 100%。
// 第一版把上限当成"必须正好这么多件"，玩家放 2 件被一句「无法进行合成。」挡掉。
func TestCompoundSuccessRateScalesWithMaterialCount(t *testing.T) {
	catalog, err := LoadCompoundCatalog(compoundTestSource(liveCompoundTable))
	if err != nil {
		t.Fatalf("LoadCompoundCatalog() error = %v", err)
	}
	for _, row := range []struct {
		kind   CompoundKind
		rarity uint32
		count  uint32
		want   uint32
	}{
		// 勋章最低档满件数恰好 100%，是支持"第三列是每件贡献"这个读法的信号。
		{kind: CompoundFlag, rarity: 0, count: 2, want: 5000},
		{kind: CompoundFlag, rarity: 0, count: 3, want: 7500},
		{kind: CompoundFlag, rarity: 0, count: 4, want: 10000},
		{kind: CompoundFlag, rarity: 3, count: 2, want: 1000},
		{kind: CompoundFlag, rarity: 3, count: 4, want: 2000},
		{kind: CompoundGem, rarity: 0, count: 2, want: 4000},
		{kind: CompoundGem, rarity: 0, count: 4, want: 8000},
		{kind: CompoundGem, rarity: 2, count: 2, want: 1500},
		{kind: CompoundGem, rarity: 2, count: 4, want: 3000},
		// 区间外一律 0，且 AcceptsMaterialCount 会先把请求挡掉。
		{kind: CompoundGem, rarity: 0, count: 1, want: 0},
		{kind: CompoundGem, rarity: 0, count: 5, want: 0},
		// 封顶行任何件数都不可合成。
		{kind: CompoundGem, rarity: 3, count: 2, want: 0},
		{kind: CompoundFlag, rarity: 6, count: 2, want: 0},
	} {
		recipe, found := catalog.Ladder(row.kind).Recipe(row.rarity)
		if !found {
			t.Fatalf("%s rarity %d missing", row.kind, row.rarity)
		}
		if got := recipe.SuccessRateFor(row.count); got != row.want {
			t.Fatalf("%s rarity %d with %d materials = %d, want %d",
				row.kind, row.rarity, row.count, got, row.want)
		}
		if accepted := recipe.AcceptsMaterialCount(row.count); accepted != (row.want > 0) {
			t.Fatalf("%s rarity %d accepts %d = %t, want %t",
				row.kind, row.rarity, row.count, accepted, row.want > 0)
		}
	}
}

// 成功率封顶 100%：哪怕表被改成更大的每件贡献，也不能溢出分母。
func TestCompoundSuccessRateIsCappedAtFullOdds(t *testing.T) {
	recipe := CompoundRecipe{Rarity: 0, MaxMaterialCount: 4, RatePerMaterial: 9000}
	if got := recipe.SuccessRateFor(4); got != CompoundRateDenominator {
		t.Fatalf("rate = %d, want the %d cap", got, CompoundRateDenominator)
	}
}
