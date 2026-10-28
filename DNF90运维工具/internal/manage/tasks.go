//go:build paid

// 后台批量任务：群发邮件 / 在线福利发放 / 批量资产邮件 / 批量清空邮箱这类"逐个玩家做一遍"的动作。
//
// 为什么必须异步：全服发送要按账号逐个查角色、逐个发 GM 邮件，上千账号要走几千次
// 回环调用，放在 HTTP 请求里必然超时；客户端一断开整批就白做了。
// 所以：请求立刻返回 task_id，实际工作在后台协程里跑，管理工具轮询进度。
//
// 任务只存在内存里（进程重启即消失）：这是"运维当场看一眼进度"的东西，
// 不是审计记录 —— 真正的发放证据在 GM 邮件与日志里。
//
// 类型分成两个（go vet 的 copies-lock 检查逼出来的正确设计）：
//   - batchTaskState：内部状态，带锁、带 cancel；
//   - BatchTask：对管理工具的 JSON 快照，纯数据、无锁、可随便拷。
package manage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// 任务状态与阶段。
const (
	taskStatusRunning  = "running"
	taskStatusDone     = "done"
	taskStatusCanceled = "canceled"
	taskStatusFailed   = "failed"

	taskPhaseCollecting = "collecting" // 正在收集目标（查账号/角色）
	taskPhaseSending    = "sending"    // 正在逐个发放
	taskPhaseFinished   = "finished"
)

// batchTaskMaxErrors 任务里最多保留多少条失败原因（够定位问题，又不至于把响应撑大）。
const batchTaskMaxErrors = 20

// 任务 kind（管理工具按它分组显示进度；也是任务互斥的粒度）。
const (
	taskKindGrantMail    = "grant-mail"    // 群发物品邮件
	taskKindWelfare      = "welfare"       // 在线福利发放
	taskKindWalletMail   = "wallet-mail"   // 批量资产邮件（金币 / 点券）
	taskKindMailboxClear = "mailbox-clear" // 批量清空全服邮箱
)

// batchPressureKinds 会"全服逐个角色压服务端回环口"的任务类型，**共用同一把互斥锁**。
//
// 为什么必须跨 kind 互斥（而不是各自只挡同类）：
//
//	这三类任务的执行方式完全一样 —— 顺序（batchGrantWorkers=1）把上千次
//	POST /local/dnf-gm/* 打到同一个回环管理口。单独一类跑起来就已经贴着服务端的承受线
//	（2026-09-25 事故：8 个并发 GM 邮件请求有 3 个被回 500），两类同时跑就是并发叠加，
//	等于自己把当年那次事故重演一遍。运营也可能真的同时点"群发"和"清邮箱"
//	（清空是为了给群发腾地方），所以这道约束必须落在服务端，不能指望界面按钮。
//
// 不在此列的例子：welfare（在线福利发放）走的是自己的到账链路，不逐个压回环口，
// 因此不受影响（见 TestCreateIfNoRunning 里对 welfare 的断言）。
var batchPressureKinds = []string{taskKindGrantMail, taskKindWalletMail, taskKindMailboxClear}

// batchConflictKinds 返回某个 kind 的互斥集合。
//
// 压回环口的三类互为冲突（任一在跑，另外两类都不许起）；其余 kind 只与自己冲突
// （保持原有语义）。这样 createIfNoRunning("grant-mail") 这种单 kind 写法也天然落在
// 同一个互斥集合里，不会出现"wallet-mail 挡得住 grant-mail、反过来挡不住"的半边锁。
func batchConflictKinds(kind string) []string {
	for _, k := range batchPressureKinds {
		if k == kind {
			return batchPressureKinds
		}
	}
	return []string{kind}
}

// batchConflictMessage 409 文案：必须点明"哪一类任务、跑到哪儿了"。
//
// 运营看到 409 的第一反应是"再点一次"，所以文案要说清是**别的**批量任务占着坑
// （历史文案只写"已有群发任务"，同时跑批量资产邮件时运营会以为程序坏了）。
func batchConflictMessage(busy BatchTask) string {
	return fmt.Sprintf("已有批量任务在执行中（%s，进度 %d/%d，任务 %s），请等它结束或先取消",
		busy.Kind, busy.Done, busy.Total, busy.ID)
}

// taskSeq 任务序号：同一秒内连续发起多个任务也不会撞 id。
var taskSeq atomic.Int64

// BatchTask 后台批量任务进度快照（对管理工具的 JSON 视图）。纯数据，无锁。
type BatchTask struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`  // grant-mail / welfare / wallet-mail / mailbox-clear
	Scope  string `json:"scope"` // all-accounts / online-characters / all-characters / server-first-character
	Phase  string `json:"phase"` // collecting / sending / finished
	Total  int    `json:"total"` // 目标总数（收集完成后才知道）
	Done   int    `json:"done"`
	OK     int    `json:"ok"`
	Failed int    `json:"failed"`
	// Skipped 跳过的目标（账号没有任何角色 / 玩家已离线等）。
	Skipped int `json:"skipped"`
	// Status running / done / canceled / failed
	Status  string `json:"status"`
	Message string `json:"message"`
	// Errors 前若干条失败原因（形如 "账号 xxx：原因"）。
	Errors     []string `json:"errors"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
}

// batchTaskState 任务内部状态（带锁；外部只通过 snapshot 读取）。
type batchTaskState struct {
	mu sync.Mutex

	id, kind, scope       string
	phase                 string
	total, done           int
	ok, failed, skipped   int
	status, message       string
	errors                []string
	startedAt, finishedAt string
	cancel                context.CancelFunc
}

// snapshot 加锁读一份纯数据副本。
func (t *batchTaskState) snapshot() BatchTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	return BatchTask{
		ID: t.id, Kind: t.kind, Scope: t.scope, Phase: t.phase,
		Total: t.total, Done: t.done, OK: t.ok, Failed: t.failed, Skipped: t.skipped,
		Status: t.status, Message: t.message,
		Errors:    append([]string(nil), t.errors...),
		StartedAt: t.startedAt, FinishedAt: t.finishedAt,
	}
}

// setPhase 进入下一阶段（收集完目标后调用，此时才知道 Total）。
func (t *batchTaskState) setPhase(phase string, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = phase
	t.total = total
}

// addResult 记录一个目标的处理结果。errText 非空时计入失败并保留原因（最多 batchTaskMaxErrors 条）。
func (t *batchTaskState) addResult(skipped bool, errText string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done++
	switch {
	case errText != "":
		t.failed++
		if len(t.errors) < batchTaskMaxErrors {
			t.errors = append(t.errors, errText)
		}
	case skipped:
		t.skipped++
	default:
		t.ok++
	}
}

// resetCollectCounters 收集阶段结束后把"账号层面"的计数清零：
// 收集时记的失败/跳过（账号不存在、没角色）与发放阶段的逐角色统计会写进同一个 task，
// 不清零的话进度会显示成"已处理 N/总数 N"提前满格。失败原因文本保留（那是给运营看的线索）。
func (t *batchTaskState) resetCollectCounters() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done, t.ok, t.failed, t.skipped = 0, 0, 0, 0
}

// finish 收尾：写终态与结束时间。
func (t *batchTaskState) finish(status, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = status
	t.phase = taskPhaseFinished
	t.message = message
	t.finishedAt = time.Now().Format(time.RFC3339)
}

// isRunning 是否仍在跑（管理工具据此决定还要不要继续轮询）。
func (t *batchTaskState) isRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status == taskStatusRunning
}

// runCanceled 任务是否已被取消（工作循环每轮检查一次，尽快停下来）。
func (t *batchTaskState) runCanceled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status == taskStatusCanceled
}

// taskRegistryKeep 保留多少个历史任务供查看。
const taskRegistryKeep = 50

// taskRegistry 任务登记表（内存，最多保留最近 taskRegistryKeep 个）。
type taskRegistry struct {
	mu    sync.Mutex
	items map[string]*batchTaskState
	order []string // 新 → 旧
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{items: make(map[string]*batchTaskState)}
}

// create 新建并登记一个 running 任务。
func (r *taskRegistry) create(kind, scope string, cancel context.CancelFunc) *batchTaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(kind, scope, cancel)
}

// createIfNoRunning 单 kind 包装：按 batchConflictKinds 展开成互斥集合后占位。
//
// 保留这个签名是为了不动既有调用点（群发邮件）与既有测试；语义上它已经是
// "按这个 kind 所属的互斥集合占位"，所以群发在跑时批量清空/批量资产邮件同样起不来。
func (r *taskRegistry) createIfNoRunning(kind, scope string, cancel context.CancelFunc) (*batchTaskState, BatchTask) {
	return r.createIfNoRunningKinds(kind, scope, cancel, batchConflictKinds(kind))
}

// createIfNoRunningKinds 在**同一把锁内**完成"查这些 kind 有没有 running 任务 + 新建登记"。
//
// 为什么不能沿用"先 runningTaskOfKind 再 create"两步：两步之间隔着调用方的慢操作
// （群发要先构建执行端 PVF 索引，可能几百毫秒到数秒），两次点击都能看到"没有 running"
// → 两个任务同时跑 → 每个玩家收两封邮件，正是那道守卫要防的事。
//
// 参数：kind = 本次要新建任务的类型（写进任务快照，管理工具据此显示进度）；
// kinds = 互斥集合，任一类在跑都算冲突（见 batchPressureKinds：三类批量任务都会逐个
// 角色压服务端回环口，同时跑会互相夹击）。
//
// 返回 (task, BatchTask{}) = 占位成功；(nil, 在跑的任务快照) = 互斥集合里已有任务在跑。
func (r *taskRegistry) createIfNoRunningKinds(kind, scope string, cancel context.CancelFunc, kinds []string) (*batchTaskState, BatchTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 集合里必须含自身 kind，否则"连着点两次同一个按钮"就没人挡了。
	// 调用方漏写时在这里补上：让守卫静默失效比在这里多一次比较危险得多。
	if !kindInList(kind, kinds) {
		kinds = append(append([]string(nil), kinds...), kind)
	}
	for _, id := range r.order {
		t, ok := r.items[id]
		if !ok || !kindInList(t.kind, kinds) {
			continue
		}
		if t.isRunning() {
			return nil, t.snapshot()
		}
	}
	return r.createLocked(kind, scope, cancel), BatchTask{}
}

// kindInList 目标 kind 是否落在互斥集合里。
//
// 注意比对的是**在跑任务的 kind**：两类不同的批量任务只要共用同一个集合
// （batchPressureKinds），谁在跑都能挡住对方，锁就不会只挡住半边。
func kindInList(kind string, kinds []string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// createLocked 登记一个新任务；调用方必须已持有 r.mu。
func (r *taskRegistry) createLocked(kind, scope string, cancel context.CancelFunc) *batchTaskState {
	now := time.Now()
	// 序号 + 秒级时间戳：同一秒内连续发起多个任务也不会撞 id。
	id := fmt.Sprintf("%s-%d-%d", kind, now.Unix(), taskSeq.Add(1))
	task := &batchTaskState{
		id: id, kind: kind, scope: scope, phase: taskPhaseCollecting,
		status: taskStatusRunning, startedAt: now.Format(time.RFC3339),
		cancel: cancel,
	}
	r.items[id] = task
	r.order = append([]string{id}, r.order...)
	// 超出保留量：删掉最旧的（跑着的任务不删，避免管理工具轮询到一半 404）。
	for len(r.order) > taskRegistryKeep {
		oldest := r.order[len(r.order)-1]
		if t, ok := r.items[oldest]; ok && t.isRunning() {
			break
		}
		delete(r.items, oldest)
		r.order = r.order[:len(r.order)-1]
	}
	return task
}

// get 按 id 取任务状态对象（nil = 不存在）。
func (r *taskRegistry) get(id string) *batchTaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.items[id]
}

// list 返回最近的任务快照（新 → 旧）。
func (r *taskRegistry) list(limit int) []BatchTask {
	if limit <= 0 || limit > taskRegistryKeep {
		limit = 20
	}
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	r.mu.Unlock()

	out := make([]BatchTask, 0, limit)
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		if t := r.get(id); t != nil {
			out = append(out, t.snapshot())
		}
	}
	return out
}

// cancel 取消一个还在跑的任务（已经结束的返回 false）。
func (r *taskRegistry) cancel(id string) bool {
	t := r.get(id)
	if t == nil || !t.isRunning() {
		return false
	}
	t.mu.Lock()
	cancel := t.cancel
	t.status = taskStatusCanceled
	t.message = "已被管理员取消"
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}
