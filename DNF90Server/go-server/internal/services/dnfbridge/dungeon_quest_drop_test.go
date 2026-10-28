package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentQuestDropMatchMonsterIncludesTypeOneEnemyRewardItem(t *testing.T) {
	ctx := context.Background()
	source := bridgePVFSource{
		dnfquest.DefaultList: "3233 `enemy_reward.qst`\n3234 `monster_reward.qst`\n",
		"n_quest/enemy_reward.qst": "[grade]\n" +
			"`[epic]`\n" +
			"[type]\n" +
			"`[seeking]`\n" +
			"[int data]\n" +
			"10164861 30\n" +
			"[enemy reward item]\n" +
			"420 1 32 -1 10164861 1 100 30\n" +
			"421 1 32 2 10164861 2 50 30\n" +
			"420 2 32 -1 10164862 1 100 30\n" +
			"16111 3 102 -1 10164801 1 100 15\n" +
			"[/enemy reward item]\n",
		"n_quest/monster_reward.qst": "[grade]\n" +
			"`[normal]`\n" +
			"[type]\n" +
			"`[seeking]`\n" +
			"[int data]\n" +
			"10164863 1\n" +
			"[monster reward item]\n" +
			"420 32 -1 10164863 1 75 1\n" +
			"[/monster reward item]\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	candidates := currentQuestDropMatchMonster(catalog, []int64{3233, 3234}, 32, 0, 420)
	if len(candidates) != 2 {
		t.Fatalf("candidates=%+v", candidates)
	}
	if candidates[0] != (currentQuestDropCandidate{QuestID: 3233, ItemID: 10164861, Count: 1, DropRate: 100, MaxStack: 30}) {
		t.Fatalf("enemy reward candidate=%+v", candidates[0])
	}
	if candidates[1] != (currentQuestDropCandidate{QuestID: 3234, ItemID: 10164863, Count: 1, DropRate: 75, MaxStack: 1}) {
		t.Fatalf("monster reward candidate=%+v", candidates[1])
	}
	if got := currentQuestDropMatchMonster(catalog, []int64{3233}, 32, 0, 421); len(got) != 0 {
		t.Fatalf("difficulty-specific enemy reward matched wrong difficulty: %+v", got)
	}
	if got := currentQuestDropMatchMonster(catalog, []int64{3233}, 32, 2, 421); len(got) != 1 || got[0].Count != 2 || got[0].DropRate != 50 {
		t.Fatalf("difficulty-specific enemy reward=%+v", got)
	}
	if got := currentQuestDropMatchMonster(catalog, []int64{3233}, 31, 0, 420); len(got) != 0 {
		t.Fatalf("enemy reward matched wrong dungeon: %+v", got)
	}
	if got := currentQuestDropMatchMonster(catalog, []int64{3233}, 102, 0, 16111); len(got) != 0 {
		t.Fatalf("type-3 passive object leaked into monster matching: %+v", got)
	}
	got := currentQuestDropMatchEnemy(catalog, []int64{3233}, 102, 3, 16111, 3)
	if len(got) != 1 || got[0] != (currentQuestDropCandidate{QuestID: 3233, ItemID: 10164801, Count: 1, DropRate: 100, MaxStack: 15}) {
		t.Fatalf("type-3 passive-object reward=%+v", got)
	}
	if got := currentQuestDropMatchEnemy(catalog, []int64{3233}, 102, 3, 16111, 1); len(got) != 0 {
		t.Fatalf("type-3 passive object leaked into type-1 matching: %+v", got)
	}
	if got := currentQuestDropMatchEnemy(catalog, []int64{3233}, 101, 3, 16111, 3); len(got) != 0 {
		t.Fatalf("type-3 passive object matched wrong dungeon: %+v", got)
	}
}

func TestCurrentQuestDropMatchImplicitMonsterUsesPVFScopeAndHiddenBossExplanation(t *testing.T) {
	ctx := context.Background()
	source := bridgePVFSource{
		dnfquest.DefaultList:  "8557 `event.qst`\n13701 `regular.qst`\n12872 `hidden.qst`\n",
		"n_quest/event.qst":   "[name]\n`进攻准备`\n[type]\n`[seeking]`\n[int data]\n10094602 10\n",
		"n_quest/regular.qst": "[name]\n`进攻准备`\n[type]\n`[seeking]`\n[int data]\n10094602 3\n[dungeon info]\n3200 -1\n[/dungeon info]\n",
		"n_quest/hidden.qst":  "[name]\n`异界的异变 (2/4)`\n[type]\n`[seeking]`\n[int data]\n10000204 1\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	questCatalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	itemCatalog := &pvfDungeonDropCatalog{
		source: bridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			10094602: {ItemID: 10094602, Kind: dungeonDropItemStackable, StackableType: "[material]"},
			10000204: {ItemID: 10000204, Kind: dungeonDropItemStackable, StackableType: "[quest]", Explain: "消灭异次元裂缝的依普利乌斯时，可以获得该道具"},
		},
	}
	monsterCatalog := &pvfDungeonMonsterCatalog{
		source: bridgePVFSource{},
		cache: map[int64]dnfmonster.Monster{
			420: {ID: 420, Path: "monster/ordinary.mob", Name: "普通怪物"},
			107000256: {
				ID: 107000256, Path: "monster/newmonsters/dimensioncrack/specialboss/iflius/iflius1.mob",
				Name: "依普利乌斯", Sections: []string{"dimension hidden boss"},
			},
		},
	}

	material := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{8557}, 3200, 0, 420,
	)
	if len(material) != 1 || material[0] != (currentQuestDropCandidate{
		QuestID: 8557, ItemID: 10094602, Count: 1, DropRate: 100, MaxStack: 10,
	}) {
		t.Fatalf("scoped material candidate=%+v", material)
	}
	if got := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{8557}, 3201, 0, 420,
	); len(got) != 0 {
		t.Fatalf("scoped material leaked into wrong dungeon: %+v", got)
	}

	hiddenBoss := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{12872}, 7177, 3, 107000256,
	)
	if len(hiddenBoss) != 1 || hiddenBoss[0] != (currentQuestDropCandidate{
		QuestID: 12872, ItemID: 10000204, Count: 1, DropRate: 100, MaxStack: 1,
	}) {
		t.Fatalf("hidden boss candidate=%+v", hiddenBoss)
	}
	if got := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{12872}, 7177, 3, 420,
	); len(got) != 0 {
		t.Fatalf("quest item leaked to ordinary monster: %+v", got)
	}
}

func TestRealScriptPVFImplicitSeekingQuestDrops(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify implicit seeking quest drops")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists: []string{dnfquest.DefaultList}, Prefixes: []string{"n_quest/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	questCatalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	scopes := questCatalog.SeekingDungeonScopes(8557)
	if len(scopes) != 1 || scopes[0] != (dnfquest.DungeonScope{DungeonID: 3200, Difficulty: -1}) {
		var related []dnfquest.Definition
		for _, definition := range questCatalog.Definitions() {
			for _, target := range questCatalog.SeekingItemTargets(definition.ID) {
				if target.ItemID == 10094602 {
					related = append(related, definition)
				}
			}
		}
		t.Fatalf("quest 8557 inherited scopes=%+v related=%+v", scopes, related)
	}
	itemCatalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	material := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{8557}, 3200, 0, 107000256,
	)
	if len(material) != 1 || material[0].ItemID != 10094602 || material[0].MaxStack != 10 {
		t.Fatalf("quest 8557 material candidate=%+v", material)
	}
	hiddenBoss := currentQuestDropMatchImplicitMonster(
		questCatalog, itemCatalog, monsterCatalog, []int64{12872}, 7177, 3, 107000256,
	)
	if len(hiddenBoss) != 1 || hiddenBoss[0] != (currentQuestDropCandidate{
		QuestID: 12872, ItemID: 10000204, Count: 1, DropRate: 100, MaxStack: 1,
	}) {
		t.Fatalf("quest 12872 hidden-boss candidate=%+v", hiddenBoss)
	}
}

func TestCurrentQuestHelperDropRateRaisesOneFifthAndCaps(t *testing.T) {
	tests := []struct {
		base   int64
		active bool
		want   int64
	}{
		{base: 50, active: false, want: 50},
		{base: 50, active: true, want: 60},
		{base: 83, active: true, want: 100},
		{base: 100, active: true, want: 100},
		{base: 0, active: true, want: 0},
	}
	for _, test := range tests {
		if got := currentQuestHelperDropRate(test.base, test.active); got != test.want {
			t.Fatalf("base=%d active=%t got=%d want=%d", test.base, test.active, got, test.want)
		}
	}
}

func TestCurrentQuestItemDropRateCombinesGrowthContractAndQuestHelper(t *testing.T) {
	tests := []struct {
		base          int64
		growthPercent int64
		questHelper   bool
		want          int64
	}{
		{base: 50, growthPercent: 0, questHelper: false, want: 50},
		{base: 50, growthPercent: 20, questHelper: false, want: 60},
		{base: 50, growthPercent: 20, questHelper: true, want: 70},
		{base: 83, growthPercent: 20, questHelper: false, want: 100},
		{base: 99, growthPercent: -1, questHelper: false, want: 99},
		{base: 0, growthPercent: 20, questHelper: true, want: 0},
	}
	for _, test := range tests {
		if got := currentQuestItemDropRate(test.base, test.growthPercent, test.questHelper); got != test.want {
			t.Fatalf(
				"base=%d growth=%d helper=%t got=%d want=%d",
				test.base,
				test.growthPercent,
				test.questHelper,
				got,
				test.want,
			)
		}
	}
}
