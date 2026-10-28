package inventory

import (
	"bytes"
	"context"
	"maps"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 无色小晶块 / 灵魂 只允许出现在账号共享槽 354..365。可只要哪条获取途径漏查固定槽表，
// 它们就会落进普通背包格 —— 现场 2026-09-21：角色 1111 用金币买的 20 个无色落在
// 0:154，而账号 0:358 另有 43890 个。
//
// 登录迁移过去只处理"同一个线地址"的旧数据（角色记录的 0:354..365），看不见落在
// 别的背包格里的错位行，所以那 20 个活过了小退。本文件覆盖补上的收敛兜底。
func TestMigrateAccountSharedSlotsSweepsMisplacedBackpackRowIntoItsFixedCell(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 现场形态照抄：背包 0:154 有 20 个无色，账号 0:358 已有 43890 个。
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 700, Count: 1},                  // 普通背包格，不该被动
			"0:154": {ItemID: 3037, Count: 20},                // 错位的无色
			"1:33":  {ItemID: 3037, Count: 5},                 // list 1(装扮)里的同 ID，不该被当成背包
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if _, exists := character.Slots["0:154"]; exists {
		t.Fatalf("misplaced backpack row survived migration: %+v", character.Slots)
	}
	if got := character.Slots["0:9"]; got.ItemID != 700 || got.Count != 1 {
		t.Fatalf("ordinary backpack slot changed: %+v", got)
	}
	if got := character.Slots["1:33"]; got.ItemID != 3037 || got.Count != 5 {
		t.Fatalf("non-list-0 row must not be swept: %+v", got)
	}
	account, exists, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil || !exists {
		t.Fatalf("Load account inventory exists=%t err=%v", exists, err)
	}
	if got := account.Slots["0:358"]; got.ItemID != 3037 || got.Count != 43910 {
		t.Fatalf("account crystal cell=%+v want 3037x43910 (43890 + the 20 that were in the bag)", got)
	}
}

func TestMigrateAccountSharedSlotsSweepsMisplacedSoulRowAndKeepsState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	expires := time.Date(2027, time.June, 1, 2, 3, 4, 0, time.FixedZone("CST", 8*60*60))
	// 灵魂落在普通背包格里、账号格还空着 —— 整行搬走，状态一个字段都不能丢。
	misplaced := dnfrepo.ItemStack{
		ItemID:   10100116,
		Count:    3,
		Bind:     true,
		ExpireAt: expires,
		RawEntry: []byte{0x77, 0x11, 0x22, 0x33},
		Extra:    map[string]string{"source": "legacy", "serial": "77"},
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:100": misplaced},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if len(character.Slots) != 0 {
		t.Fatalf("character slots not emptied: %+v", character.Slots)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	got := account.Slots["0:361"]
	if got.ItemID != misplaced.ItemID || got.Count != misplaced.Count || got.Bind != misplaced.Bind ||
		!got.ExpireAt.Equal(misplaced.ExpireAt) || !bytes.Equal(got.RawEntry, misplaced.RawEntry) ||
		!maps.Equal(got.Extra, misplaced.Extra) {
		t.Fatalf("swept soul = %+v, want complete legacy state %+v", got, misplaced)
	}
}

// 运营口径：个人仓库(list 2)与账号金库(list 12)是**仓库**，玩家可以把晶块/灵魂存进去，
// 兜底扫描只认背包(list 0)，不许去把仓库里的东西搬走。
//
// 用例里放了一个**控制行**（0:154 的无色）：它必须被扫走。没有控制行的话，本用例在
// "扫描整个被关掉"时也会通过 —— 那就变成了只会说"什么都没发生"的空话。
func TestMigrateAccountSharedSlotsLeavesWarehouseContainersAlone(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	personal := dnfrepo.ItemStack{ItemID: 3037, Count: 7}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:154": {ItemID: 3037, Count: 20}},
		Warehouse:   map[string]dnfrepo.ItemStack{"2:5": personal},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if got := character.Warehouse["2:5"]; got.ItemID != 3037 || got.Count != 7 {
		t.Fatalf("personal cargo row must stay untouched: %+v", got)
	}
	// 控制行：背包里那一行必须被扫走，证明扫描确实在跑。
	if _, exists := character.Slots["0:154"]; exists {
		t.Fatalf("control row in the backpack was not swept: %+v", character.Slots)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if got := account.Slots["0:358"]; got.ItemID != 3037 || got.Count != 20 {
		t.Fatalf("control row must land in the fixed cell: %+v", account.Slots)
	}
	if len(account.Slots) != 1 {
		t.Fatalf("personal cargo must not be routed into the account warehouse: %+v", account.Slots)
	}
}

// 固定格被**别的**道具占着时绝不驱赶：宁可留一行错位数据，也不能把玩家的东西挪丢。
// 控制行（0:200 的白色小晶块，固定格 0:355 是空的）必须被扫走。
func TestMigrateAccountSharedSlotsNeverEvictsAForeignOccupant(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	misplaced := dnfrepo.ItemStack{ItemID: 3037, Count: 20}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:154": misplaced,
			"0:200": {ItemID: 3034, Count: 6},
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 9999, Count: 4}},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if got := character.Slots["0:154"]; got.ItemID != 3037 || got.Count != 20 {
		t.Fatalf("misplaced row must stay put when its cell is taken by a foreign item: %+v", character.Slots)
	}
	if _, exists := character.Slots["0:200"]; exists {
		t.Fatalf("control row with a free fixed cell should have been swept: %+v", character.Slots)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if got := account.Slots["0:358"]; got.ItemID != 9999 || got.Count != 4 {
		t.Fatalf("foreign occupant was disturbed: %+v", got)
	}
	if got := account.Slots["0:355"]; got.ItemID != 3034 || got.Count != 6 {
		t.Fatalf("control row must land in the free fixed cell: %+v", got)
	}
}

// 绑定状态/到期时间与账号行不一致时不合并 —— 合并会改变物品语义（例如把绑定的并进不绑定的）。
// 控制行（0:200 的普通灵魂，固定格 0:360 是空的）必须被扫走。
func TestMigrateAccountSharedSlotsSkipsIncompatibleBindOrExpiry(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:154": {ItemID: 3037, Count: 20, Bind: false},
			"0:200": {ItemID: 10100115, Count: 2},
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 5, Bind: true}},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
		t.Fatalf("MigrateAccountSharedSlots error = %v", err)
	}

	character := loadTestInventory(t, ctx, repos, "77")
	if got := character.Slots["0:154"]; got.ItemID != 3037 || got.Count != 20 {
		t.Fatalf("incompatible row must stay put: %+v", character.Slots)
	}
	if _, exists := character.Slots["0:200"]; exists {
		t.Fatalf("control row with a free fixed cell should have been swept: %+v", character.Slots)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "dnf:1")
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if got := account.Slots["0:358"]; got.Count != 5 || !got.Bind {
		t.Fatalf("account row must not absorb an incompatible stack: %+v", got)
	}
	if got := account.Slots["0:360"]; got.ItemID != 10100115 || got.Count != 2 {
		t.Fatalf("control soul row must land in the free fixed cell: %+v", got)
	}
}

// 幂等：迁移会在每次构建 list-0 快照前调用，重复执行绝不能把同一行累加两次。
func TestMigrateAccountSharedSlotsIsIdempotentForMisplacedRows(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:154": {ItemID: 3037, Count: 20}},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		if err := owner.MigrateAccountSharedSlots(ctx, "dnf:1", "77"); err != nil {
			t.Fatalf("attempt %d: MigrateAccountSharedSlots error = %v", attempt, err)
		}
		account, _, err := repos.AccountInventory.Load(ctx, "dnf:1")
		if err != nil {
			t.Fatalf("attempt %d: Load error = %v", attempt, err)
		}
		if got := account.Slots["0:358"]; got.Count != 43910 {
			t.Fatalf("attempt %d: count=%d want 43910 (the sweep must not add the same row twice)", attempt, got.Count)
		}
	}
}
