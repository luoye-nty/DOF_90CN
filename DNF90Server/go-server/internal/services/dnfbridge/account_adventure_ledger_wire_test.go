package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 冒险团经验值在报文里的单位是 1/10 点原始经验。现场取证（2026-09-29，冒险团
// 5 级）：客户端把 `[adventure exp table]` 第 6 项 631,238,480 显示成
// 63,123,848（表值 ÷10），而"当前值"原样打印报文里的 u64。我们过去直接发原始
// 经验，分子就是客户端的 10 倍 —— 条一到表值 ÷10 就冲破 100%（实测 360%、10 格
// 全亮），服务端却要涨满表值才升级。这条用例把现场那两个数字钉死在换算上。
func TestAdventureWirePointMatchesTheClientUnitOnTheRealTableRow(t *testing.T) {
	// 6 级表值 631,238,480 ÷ 10 = 63,123,848（截图里客户端显示的分母）。
	if got := currentAdventureWirePoint(631_238_480); got != 63_123_848 {
		t.Fatalf("wire point = %d, want 63123848", got)
	}
	// 整数除法即截断：客户端单位的换算不能四舍五入，否则条会在升级点前后抖动。
	if got := currentAdventureWirePoint(63_123_849); got != 6_312_384 {
		t.Fatalf("wire point = %d, want 6312384 (截断)", got)
	}
}

// 回归（2026-09-29）：冒险团等级的**所有权在账号成长账本**，它必须对所有报文路径
// 生效，而不是只对冒险团窗口生效。
//
// 现场：冒险团 30 级，角色面板（mode1/mode3 的 adventure level）却一直是 13 级的
// 四围 —— 因为面板路径读的是名册刻度，而名册刻度被真实 charactermanage.etc
// `[manage level point]` 的 13 条阈值钳死（tables.go ManageLevelForPoint：表不够长
// 时最高只能到最后一个阈值）。
//
// 需要真实 PVF：只有 LoadComplete 之后的运行时配置里才有 Growth.MaximumLevel，
// 才会走账本覆写。
func TestRealAdventureSummaryUsesTheGrowthLedgerOverTheRosterCap(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE for the real adventure-group ledger")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	tables, err := adventuregroup.LoadComplete(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if tables.Runtime().Growth.MaximumLevel != 30 {
		t.Fatalf("real [max adventure level] = %d, want 30", tables.Runtime().Growth.MaximumLevel)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   "dnf:1",
		Slot:        0,
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	// 账号成长账本：30 级（真实表的上限），进度 0。
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "dnf:1",
		Metadata: map[string]string{
			adventuregroup.GrowthMetadataKey: `{"level":30,"experience":0,"remainder":0}`,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "dnf:1"},
		adventureGroupTable: tables,
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	selected, found, err := repositories.Character.Load(context.Background(), "20")
	if err != nil || !found {
		t.Fatalf("selected character found=%t err=%v", found, err)
	}

	// 面板/场景等所有报文走的都是这一层（currentAccountAdventureGroupSummaryForPacket
	// -> State），所以这里直接钉 State 的返回值。
	summary, err := service.currentAccountAdventureGroupSummary(context.Background(), selected, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ManageLevel != 30 {
		t.Fatalf("报文用的冒险团等级 = %d, want 30（账本等级，不是被 13 条阈值钳住的名册刻度）", summary.ManageLevel)
	}
	// 30 级对应的四围来源：`[attribute] 30` 的 `[stat] 30 str 260`。
	if summary.ManageOption != 260 {
		t.Fatalf("冒险团 30 级的属性加成 = %d, want 260", summary.ManageOption)
	}
}
