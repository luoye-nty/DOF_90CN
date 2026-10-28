package dnfbridge

import "testing"

// 「清完自动回城」的绑定那半条：本人开的局、以及 **adopt 队友开的局** 都要认。
//
// 超时空一个区域共用一条 run，而 prepareRuntimePartyDungeonEntry **先备队友**，
// 所以常常是**队友**开局、驱动者 adopt —— 驱动者手上那份 binding 带的是开局人的
// token，永远不等于自己的本地 token。
//
// 2026-10-01 晚上这一段被真机来回翻了两遍，正是这个用例要钉死的东西：
//
//	23:38  开局人那一笔收尾失败（objective is not open）⇒ 账本里那条 run 还在
//	       ⇒ 用"账本里还留着这条 run"兜底的版本让驱动者回了城
//	       ⇒ 操作员："队长自动回城了 队员没回去"
//	23:54  开局人那一笔收尾成功 ⇒ 通关处理把那条 run 从账本里删掉
//	       ⇒ 同一个驱动者又回不去了
//	       ⇒ 操作员："队员回了 队长没回去"
//
// 所以判据里**不能**出现账本。剩下这条是实打实的：一个会话不可能拿着 A 图的绑定
// 站在 B 图里（runtimeRaidCurrentRaidDungeonRun 用的是同一条不变式）。
func TestFiendWarClearReturnBindingAcceptsBothTheOpenerAndTheAdopter(t *testing.T) {
	const (
		raidKey = uint32(12058625)
		mapID   = 4964
	)
	binding := runtimeRaidDungeonBinding{RaidKey: raidKey, Phase: 1, MapID: mapID, LifecycleToken: 1}

	newRuntime := func(localToken uint64, bindingMapID int) *runtimeDungeonState {
		runtime := &runtimeDungeonState{lifecycleToken: localToken}
		runtime.Dungeon.ID = int64(mapID) // 人站在这一张图里
		runtime.raidDungeon = binding
		runtime.raidDungeon.MapID = bindingMapID
		return runtime
	}

	// 1) 开局人本人：binding token 与本地 token 相等。
	if !runtimeFiendWarClearReturnBindingAccepts(newRuntime(1, mapID)) {
		t.Fatal("开局人本人的绑定被判为不成立")
	}

	// 2) adopt 队友那一局的驱动者：token 不等（binding 是开局人的）——
	//    **这才是 23:54 真机翻车的那一条**，必须成立。
	if !runtimeFiendWarClearReturnBindingAccepts(newRuntime(2, mapID)) {
		t.Fatal("adopt 队友 run 的驱动者被判为不成立 —— 他会卡在副本里不回城" +
			"（真机 23:38/23:54 两次相反现象的共同原因）")
	}

	// 3) 绑定指向的不是我站的那张图：不成立（这是唯一保留的硬约束）。
	if runtimeFiendWarClearReturnBindingAccepts(newRuntime(1, mapID+1)) {
		t.Fatal("绑定指向别的图却放行了")
	}

	// 4) 没有绑定 / 零本地 token：不成立。
	noBinding := &runtimeDungeonState{lifecycleToken: 1}
	noBinding.Dungeon.ID = int64(mapID)
	if runtimeFiendWarClearReturnBindingAccepts(noBinding) {
		t.Fatal("没有绑定（RaidKey=0）被放行了")
	}
	if runtimeFiendWarClearReturnBindingAccepts(newRuntime(0, mapID)) {
		t.Fatal("本地 token 为 0 被放行了")
	}
	if runtimeFiendWarClearReturnBindingAccepts(nil) {
		t.Fatal("nil runtime 被放行了")
	}
}
