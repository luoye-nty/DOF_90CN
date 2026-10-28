//go:build paid

package manage

// 后台批量任务（群发邮件 / 在线福利）的纯逻辑测试。
//
// 这块最容易出错的是**进度计数**：管理工具靠 Done/Total 判断"还要不要继续轮询"、
// 靠 Status 判断结束，一旦收尾没置终态，界面会永远停在"进行中"。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatchTaskProgressAccounting(t *testing.T) {
	reg := newTaskRegistry()
	task := reg.create("grant-mail", batchScopeOnlineChars, func() {})

	if !task.isRunning() {
		t.Fatal("新建任务应为 running")
	}
	// 收集阶段的账号级统计随后要清零（否则进度提前满格）。
	task.addResult(false, "账号 a：查角色失败")
	task.addResult(true, "")
	task.resetCollectCounters()
	snap := task.snapshot()
	if snap.Done != 0 || snap.OK != 0 || snap.Failed != 0 || snap.Skipped != 0 {
		t.Fatalf("收集计数未清零: %+v", snap)
	}
	if len(snap.Errors) != 1 {
		t.Fatalf("失败原因应保留，实际 %v", snap.Errors)
	}

	task.setPhase(taskPhaseSending, 3)
	task.addResult(false, "")
	task.addResult(true, "")
	task.addResult(false, "角色 b：boom")

	snap = task.snapshot()
	if snap.Total != 3 || snap.Done != 3 || snap.OK != 1 || snap.Skipped != 1 || snap.Failed != 1 {
		t.Fatalf("进度统计错误: %+v", snap)
	}
	if snap.Phase != taskPhaseSending {
		t.Fatalf("phase = %q", snap.Phase)
	}

	task.finish(taskStatusDone, "发放完成")
	snap = task.snapshot()
	if snap.Status != taskStatusDone || snap.Phase != taskPhaseFinished || snap.FinishedAt == "" {
		t.Fatalf("收尾状态错误: %+v", snap)
	}
	if task.isRunning() {
		t.Fatal("收尾后不应再是 running（管理工具会一直轮询下去）")
	}
}

// TestBatchTaskErrorsCapped：失败原因只保留前若干条，避免一个全挂的任务把响应撑爆。
func TestBatchTaskErrorsCapped(t *testing.T) {
	reg := newTaskRegistry()
	task := reg.create("grant-mail", batchScopeAllAccounts, func() {})
	for i := 0; i < batchTaskMaxErrors+10; i++ {
		task.addResult(false, "失败原因")
	}
	snap := task.snapshot()
	if len(snap.Errors) != batchTaskMaxErrors {
		t.Fatalf("失败原因条数 = %d，期望上限 %d", len(snap.Errors), batchTaskMaxErrors)
	}
	if snap.Failed != batchTaskMaxErrors+10 {
		t.Fatalf("失败计数 = %d，期望 %d（计数不能被截断）", snap.Failed, batchTaskMaxErrors+10)
	}
}

func TestTaskRegistryCreateGetListCancel(t *testing.T) {
	reg := newTaskRegistry()
	t1 := reg.create("grant-mail", batchScopeAllAccounts, func() {})
	t2 := reg.create("welfare", batchScopeOnlineChars, func() {})

	if got := reg.get(t1.snapshot().ID); got == nil || got.snapshot().ID != t1.snapshot().ID {
		t.Fatal("按 id 取任务失败")
	}
	if reg.get("not-exist") != nil {
		t.Fatal("不存在的任务应返回 nil")
	}
	list := reg.list(10)
	if len(list) != 2 {
		t.Fatalf("任务列表长度 = %d，期望 2", len(list))
	}
	// 新 → 旧：最后创建的在最前面。
	if list[0].ID != t2.snapshot().ID {
		t.Fatalf("列表顺序应为新在前，实际首个 = %s（期望 %s）", list[0].ID, t2.snapshot().ID)
	}

	if !reg.cancel(t1.snapshot().ID) {
		t.Fatal("取消运行中的任务应返回 true")
	}
	if reg.cancel(t1.snapshot().ID) {
		t.Fatal("重复取消应返回 false")
	}
	if reg.cancel("not-exist") {
		t.Fatal("取消不存在的任务应返回 false")
	}
	if got := reg.get(t1.snapshot().ID); got == nil || got.snapshot().Status != taskStatusCanceled {
		t.Fatalf("取消后状态 = %+v", got.snapshot())
	}
}

// TestCreateIfNoRunning：同类任务只允许一个在跑（防止连点两次按钮变成每人收两封）。
//
// 这条守卫必须是"查 + 建"同一把锁内的原子操作：群发在登记任务之前还要构建执行端
// PVF 索引（数百毫秒起），两步式写法在这个窗口里两次点击都会通过检查。
func TestCreateIfNoRunning(t *testing.T) {
	reg := newTaskRegistry()

	first, running := reg.createIfNoRunning("grant-mail", batchScopeAllAccounts, func() {})
	if first == nil {
		t.Fatalf("首次占位应当成功，却返回了在跑任务 %+v", running)
	}
	if first.snapshot().Status != taskStatusRunning {
		t.Fatalf("新建任务状态 = %q", first.snapshot().Status)
	}

	// 第二次占位：必须被拒，并把在跑的那个任务回给调用方（用于 409 文案）。
	second, busy := reg.createIfNoRunning("grant-mail", batchScopeOnlineChars, func() {})
	if second != nil {
		t.Fatal("同类任务在跑时不应再占位成功（TOCTOU 守卫失效）")
	}
	if busy.ID != first.snapshot().ID {
		t.Fatalf("冲突任务 = %+v，期望 %s", busy, first.snapshot().ID)
	}

	// 不同 kind 不互相阻挡。
	if other, _ := reg.createIfNoRunning("welfare", batchScopeAllAccounts, func() {}); other == nil {
		t.Fatal("不同 kind 的任务不应互相阻挡")
	}

	// 任务结束后守卫释放（否则一次失败就会永久占着群发名额）。
	first.finish(taskStatusDone, "done")
	again, _ := reg.createIfNoRunning("grant-mail", batchScopeAllAccounts, func() {})
	if again == nil {
		t.Fatal("任务结束后应能再次占位")
	}
}

// TestBatchGrantScopeValidation：scope 非法必须在**调用 GM 之前**就被拒
// （否则会先去做一次执行端 PVF 校验，白白打一次回环请求）。
func TestBatchGrantScopeValidation(t *testing.T) {
	s := &Server{tasks: newTaskRegistry()}
	body := `{"scope":"whatever","item_id":1,"amount":1}`
	rec := httptest.NewRecorder()
	s.handleAccountItemsGrantBatch(rec, httptest.NewRequest(http.MethodPost, "/manage/v1/account/items/grant-batch", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 scope 应返回 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "scope") {
		t.Fatalf("错误信息应点明 scope：%s", rec.Body.String())
	}

	// 数量为 0 同样要挡在 GM 之前。
	body = `{"scope":"all-accounts","item_id":1,"amount":0}`
	rec = httptest.NewRecorder()
	s.handleAccountItemsGrantBatch(rec, httptest.NewRequest(http.MethodPost, "/manage/v1/account/items/grant-batch", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("amount=0 应返回 400，实际 %d", rec.Code)
	}
}
