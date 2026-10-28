package itemdailylimit

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// dailyUseContractFiles 是允许触碰「每天可用次数」（PVF `[total usable count]`）
// 计数与判定的**非测试**文件登记表。
//
// 为什么要这张表：这个契约此前被三套互不相干的实现分别解释（op44 一套通用预留、
// 疲劳药 op515 一套内联计数、塔券一套 allowance），于是 op515 的其余 action、
// op160/op27 开箱（66 件礼盒）和 op518 礼包一件都没被计过 —— 同一份 PVF 声明，
// 走不同的路就有不同的结果。守卫测试的作用就是让这种事无法再静默发生：
// 任何新文件只要引用计数键、判定函数或投影入口，就必须在这里登记并写明它为什么
// 算"一次使用"。改动时若确实新增了消费路径，请连同它的行为测试一起加。
var dailyUseContractFiles = map[string]string{
	"internal/modules/dnf/fatigue/daily_reset.go":                         "06:00 服务日边界：与疲劳计数同界地把全部 item_daily_used_* 清零",
	"internal/modules/dnf/itemdailylimit/limit.go":                        "唯一的计数/判定实现（Consume / Release / Used / Key）",
	"internal/modules/dnf/alignedcmd/handler.go":                          "契约类型本身（StackableUseResolution.DailyUseLimit / Result.DailyUseStates）",
	"internal/modules/dnf/inventory/owner.go":                             "op44：ReserveDailyItemUse / ReleaseDailyItemUse 委托本包",
	"internal/modules/dnf/inventory/handler.go":                           "op44 与 op515 的预留在分派前完成，失败回滚",
	"internal/modules/dnf/inventory/stackable_action_daily_use.go":        "op515：统一预留 + op541 投影（action 58 让路给疲劳药事务）",
	"internal/modules/dnf/inventory/fatigue_potion.go":                    "op515 action 58：计数必须与疲劳恢复同事务",
	"internal/services/dnfbridge/stackable_use_pvf.go":                    "解析层：把 PVF [total usable count] 解析成 StackableUseResolution.DailyUseLimit",
	"internal/services/dnfbridge/gameplay_module_booster.go":              "op160/op27 开箱：同一事务内计数",
	"internal/services/dnfbridge/cera_package.go":                         "op518 礼包：同一事务内计数",
	"internal/services/dnfbridge/lottery_item.go":                         "op27 抽奖罐：计数在 commitCurrentBooster 内完成，op541 **刻意**不发（结果包后不得再有包）",
	"internal/services/dnfbridge/item_daily_use_state.go":                 "op541 投影（登录回放 + 开箱/礼包的非 aligned 路径）",
	"internal/services/dnfbridge/game_aligned.go":                         "aligned 通道命令后的 op541 投影",
	"internal/services/dnfbridge/select_character_inventory_bootstrap.go": "选角后的 op541 回放（客户端计数是内存态，必须重放）",
	"internal/services/dnfbridge/daily_fatigue_reset.go":                  "在线跨 06:00 的重置调度：读 PotionUsageDayStatKey 判断是否要 reconcile（清零 item_daily_used_* 的同一份边界）。注意：重置后**没有**重放 op541 —— 那是一条已知缺口，需单独评审（它会给 06:00 的包序加一个 op541，触碰 daily_fatigue_reset_test.go 钉死的『只发疲劳 HUD』断言）",
}

// dailyUseContractTokens 是"碰到了这份契约"的源码特征：计数键、判定函数、
// 投影入口。命中任意一个的非测试文件都必须出现在上面的登记表里。
//
// 用**裸**标识符而不是 `dnffatigue.X` 限定名：fatigue 包内部用的是不带包名的
// 常量名，只写限定名会让登记表里的 fatigue 条目被反向检查判成失效。
var dailyUseContractTokens = []string{
	"ItemUsageStatPrefix",
	"PotionUsageStatPrefix",
	"ItemUsageDayStatKey",
	"PotionUsageDayStatKey",
	"dnfitemdailylimit.",
	"dailyItemUseStatKey",
	"dailyItemUseUsed",
	"ReserveDailyItemUse",
	"ReleaseDailyItemUse",
	"DailyItemUseReservation",
	"sendCurrentItemDailyUseState",
	"sendAlignedDailyUseStates",
	"sendCurrentCharacterDailyUseStates",
}

func TestDailyUseContractTouchesOnlyRegisteredRoutes(t *testing.T) {
	root := dailyUseModuleRoot(t)
	offenders := make([]string, 0)
	walked := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		walked++
		text := string(data)
		for _, token := range dailyUseContractTokens {
			if !strings.Contains(text, token) {
				continue
			}
			if _, registered := dailyUseContractFiles[relative]; !registered {
				offenders = append(offenders, relative+"（命中 "+token+"）")
			}
			break
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码失败: %v", err)
	}
	if walked == 0 {
		t.Fatal("没有扫描到任何源码，守卫测试本身失效了")
	}
	sort.Strings(offenders)
	if len(offenders) != 0 {
		t.Fatalf("以下文件触碰了「每天可用次数」契约却没有登记。\n"+
			"如果是新增的消费路径：它必须在使用道具的同一个事务里按道具自己的 PVF 上限计数，"+
			"并在成功后投影 op541，然后把文件登记进 dailyUseContractFiles 并补一条行为测试；\n"+
			"如果只是引用而并不消费：请在登记表里写明理由。\n未登记文件：%v", offenders)
	}

	// 反向检查：登记表不得腐烂成一张列着不存在/已不再触碰契约的文件的空表。
	stale := make([]string, 0)
	for relative := range dailyUseContractFiles {
		path := filepath.Join(root, filepath.FromSlash(relative))
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			stale = append(stale, relative+"（文件不存在）")
			continue
		}
		text := string(data)
		hit := false
		for _, token := range dailyUseContractTokens {
			if strings.Contains(text, token) {
				hit = true
				break
			}
		}
		if !hit {
			stale = append(stale, relative+"（已不再触碰契约）")
		}
	}
	sort.Strings(stale)
	if len(stale) != 0 {
		t.Fatalf("登记表里有失效条目，请更新 dailyUseContractFiles: %v", stale)
	}
}

// dailyUseModuleRoot 从测试文件出发向上找到含 go.mod 的模块根目录。
func dailyUseModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("向上找不到 go.mod")
		}
		dir = parent
	}
}
