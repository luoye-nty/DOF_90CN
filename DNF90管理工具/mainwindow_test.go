package main

// 主窗口建窗回归测试：把"登录之后主界面能不能出来"这件事固化成一条用例。
//
// 为什么需要它（两个真实事故，都是建窗这一下才炸、而且都在 windowsgui 构建里
// 表现为"进程静默消失"）：
//
//  1. `MainWindow.Create()` 失败时 main.go 走的是 `log.Fatalf`，而本工具用
//     `-H windowsgui` 构建、没有控制台，那句日志谁都看不到 —— 用户看到的就是
//     "登录成功后主界面不出现"。
//  2. declarative 的 `NumberEdit.Value` 是 Property，最终由 walk 的 "Value"
//     属性 setter 处理成 `SetValue(assertFloat64Or(v, 0.0))`；`assertFloat64Or`
//     只认 float64，传 int 会返回 0.0，于是 `SetValue(0)` 落在
//     `[MinValue, MaxValue]` 之外 → Create() 报 "value out of range"。
//
// 所以这条用例断言的是：**整个主窗口（含所有标签页）能建出来**，并且几个
// "曾经取值不合法就会炸/取错"的控件确实是合法值。用例会真的创建一个窗口
// （在桌面会话里跑；跑完立刻 Dispose，不进入消息循环）。

import (
	"testing"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

func TestMainWindowBuildsWithAllTabs(t *testing.T) {
	a := &App{}
	a.store = NewConfigStore(DefaultConfigPath())
	cfg, err := a.store.Load()
	if err != nil {
		cfg = DefaultLocalConfig()
	}
	a.cfg = cfg

	var mw *walk.MainWindow
	if err := (decl.MainWindow{
		AssignTo: &mw,
		Title:    "DNF90 后台管理工具（建窗自检）",
		MinSize:  decl.Size{Width: 1000, Height: 700},
		Size:     decl.Size{Width: 1280, Height: 768},
		Layout:   decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			decl.TabWidget{AssignTo: &a.tab, Pages: buildAllPages(a)},
		},
	}).Create(); err != nil {
		t.Fatalf("MainWindow.Create 失败（登录后主界面会直接不出现）: %v", err)
	}
	defer mw.Dispose()
	a.mw = mw

	// 建窗后必须执行的默认值初始化（与 runMainWindow 保持一致）。
	if err := applyRecordTabDefaults(); err != nil {
		t.Fatalf("记录页默认值初始化失败: %v", err)
	}

	// ① 记录查询页：条数默认值必须是 recDefaultLoad，不能是 1
	//    （控件停在 MinValue=1 时，查询只会回 1 条 —— 这就是"只能查到1条"的根因）。
	if recW.limitEdit == nil {
		t.Fatal("记录页「最多加载」控件没有建出来")
	}
	if got := recW.limitEdit.Value(); got != float64(recDefaultLoad) {
		t.Fatalf("记录页「最多加载」= %v，期望 %d", got, recDefaultLoad)
	}
	if got := recLoadTarget(); got != recDefaultLoad {
		t.Fatalf("recLoadTarget() = %d，期望 %d", got, recDefaultLoad)
	}

	// ② 下拉框：walk 只在模型恰好 1 项时才自动选中第 0 项，多项目模型的
	//    CurrentIndex() 会是 -1，拿去索引数组就是越界 panic。
	combos := map[string]*walk.ComboBox{
		"记录页-类别":   recW.kindCb,
		"背包页-容器":   invW.containerCb,
		"账号页-资产模式": acctW.assetModeCb,
	}
	for name, combo := range combos {
		if combo == nil {
			t.Fatalf("%s 下拉框没有建出来", name)
		}
		if index := combo.CurrentIndex(); index < 0 {
			t.Fatalf("%s 下拉框 CurrentIndex()=%d（-1 会让后续索引取值越界 panic）", name, index)
		}
	}

	// ③ 记录页表格模型必须能安全取值（空表时任何行列都返回 nil 而不是 panic）。
	model := &recTableModel{}
	if model.RowCount() != 0 {
		t.Fatalf("空记录表行数 = %d", model.RowCount())
	}
	if value := model.Value(-1, 0); value != nil {
		t.Fatalf("越界行取值 = %v，期望 nil", value)
	}
	if value := model.Value(0, 99); value != nil {
		t.Fatalf("越界列取值 = %v，期望 nil", value)
	}
}
