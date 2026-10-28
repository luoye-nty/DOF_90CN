//go:build paid

// 批量清空全服邮箱（/manage/v1/account/mailbox/clear-batch）的契约测试。
//
// 为什么这块必须钉住（做错了代价不可逆）：
//   - 这是**物理删除**：多删一个角色、或多删一次，运营没法撤销；
//   - confirm / dry_run 的语义一旦写反（比如 confirm 缺省当成 true、dry_run 真的删了），
//     界面上的"预演"会变成线上事故；
//   - 走的必须是 /local/dnf-gm/mail/clear 这条 GM 接口，请求体形状要与单角色清空一致 ——
//     直删数据库会让在线角色的邮件在下次存档时"复活"；
//   - 上限截断必须在任务消息里说出来，否则运营会以为全服清干净了。
//
// 测试直接打 mux（绕过 X-Manage-Token 校验），聚焦"路由 + 入参解析 + 任务语义 + GM 调用形状"。
// 假 GM 通道与任务等待助手定义在本文件，handlers_wallet_batch_test.go 也复用它们。
package manage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"dnf90ops/internal/store"
)

// ---------- 假 GM 直连通道 ----------

// fakeGMCall 一次 GM 直连调用的记录。
type fakeGMCall struct {
	Method string
	Path   string // 原样（含查询串）
	Body   string // 请求体的 JSON 原文；nil body 为空串
}

// pathOnly 去掉查询串的路径，便于按接口断言。
func (c fakeGMCall) pathOnly() string {
	if i := strings.IndexByte(c.Path, '?'); i >= 0 {
		return c.Path[:i]
	}
	return c.Path
}

// fakeGM 假 GM 直连（Bridge.AcctGMCall）。
//
// 它同时是"调用计数器"：批量任务最要紧的断言就是"发了几次、发的是什么" ——
// 顺序发放（不并发）、dry_run（一次都不发）、cera 通知失败（不重试）全靠它验证。
type fakeGM struct {
	mu    sync.Mutex
	calls []fakeGMCall

	// accounts 账号 → 角色列表（/local/dnf-gm/characters）。
	accounts map[string][]gmCharacterView
	// clearErr 角色 ID → 清空失败原因（模拟游戏服回 500）。
	clearErr map[string]error
	// mailErr 角色名 → 发信失败原因（模拟 cera 的"入账成功但通知失败"）。
	mailErr map[string]error
	// walletCera 钱包查询/入账返回的点券余额。
	walletCera int64
	// beforeClear 在返回前调用（可为 nil）：测试用它把清空卡住，从而在"任务运行中"取消。
	beforeClear func()
}

func newFakeGM() *fakeGM {
	return &fakeGM{
		accounts:   map[string][]gmCharacterView{},
		clearErr:   map[string]error{},
		mailErr:    map[string]error{},
		walletCera: 100,
	}
}

// bridge 只接 GM 通道；目标来源（在线账号/账号分页/全服首角色）由各用例自己补。
func (f *fakeGM) bridge() *Bridge {
	return &Bridge{AcctGMCall: f.call}
}

func (f *fakeGM) call(ctx context.Context, method, path string, body, out any, actor string) (string, error) {
	raw := ""
	if body != nil {
		if b, err := json.Marshal(body); err == nil {
			raw = string(b)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeGMCall{Method: method, Path: path, Body: raw})
	f.mu.Unlock()

	switch {
	case strings.HasPrefix(path, "/local/dnf-gm/characters"):
		accountID := ""
		if u, err := url.Parse(path); err == nil {
			accountID = u.Query().Get("account_id")
		}
		if chars, ok := out.(*[]gmCharacterView); ok {
			*chars = f.accounts[accountID]
		}
		return "", nil
	case strings.HasPrefix(path, "/local/dnf-gm/mail/clear"):
		if f.beforeClear != nil {
			f.beforeClear()
		}
		id := characterIDFromBody(raw)
		if err, bad := f.clearErr[id]; bad {
			return "", err
		}
		return "", nil
	case strings.HasPrefix(path, "/local/dnf-gm/mail"):
		name := characterNameFromBody(raw)
		if err, bad := f.mailErr[name]; bad {
			return "", err
		}
		if res, ok := out.(*gmMailResult); ok {
			res.MailID = "mail-" + name
			res.RecipientName = name
			res.Online = true
			res.Refreshed = true
		}
		return "", nil
	case strings.HasPrefix(path, "/local/dnf-gm/wallet/query"):
		if res, ok := out.(*gmWalletResult); ok {
			res.Currency = "cera"
			res.Cera = f.walletCera
			res.Gold = 1000
			res.Character = gmCharacterView{Name: characterNameFromBody(raw)}
		}
		return "", nil
	case strings.HasPrefix(path, "/local/dnf-gm/wallet/update"):
		if res, ok := out.(*gmWalletResult); ok {
			res.Currency = "cera"
			res.Mode = "add"
			res.Before = f.walletCera
			f.walletCera += amountFromBody(raw)
			res.After = f.walletCera
			res.Cera = f.walletCera
			res.Online = true
			res.Refreshed = true
			res.Character = gmCharacterView{Name: characterNameFromBody(raw)}
		}
		return "", nil
	}
	return "", nil
}

func characterIDFromBody(raw string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	id, _ := m["character_id"].(string)
	return id
}

func characterNameFromBody(raw string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	name, _ := m["character_name"].(string)
	return name
}

func amountFromBody(raw string) int64 {
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	if v, ok := m["amount"].(float64); ok {
		return int64(v)
	}
	return 0
}

// callsOfPath 取某个接口的全部调用记录（已脱掉查询串）。
func (f *fakeGM) callsOfPath(path string) []fakeGMCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeGMCall, 0, len(f.calls))
	for _, c := range f.calls {
		if c.pathOnly() == path {
			out = append(out, c)
		}
	}
	return out
}

// countOfPath 某个接口被调了多少次。
func (f *fakeGM) countOfPath(path string) int { return len(f.callsOfPath(path)) }

// totalCalls 一共调了多少次 GM（dry_run 的"零调用"断言用它）。
func (f *fakeGM) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---------- 任务助手 ----------

// taskFromResponse 解析 {"task":{...}} 信封（与管理工具 AcctItemsGrantBatch 的解析形状一致）。
func taskFromResponse(t *testing.T, raw string) BatchTask {
	t.Helper()
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("请求应成功，实际 error=%q（原文 %s）", errMsg, raw)
	}
	var env struct {
		Task BatchTask `json:"task"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("data 不是 {task:...} 形状: %v（原文 %s）", err, string(data))
	}
	if env.Task.ID == "" {
		t.Fatalf("响应里没有 task.id（管理工具拿不到任务 id 就没法轮询）: %s", string(data))
	}
	return env.Task
}

// waitTaskFinished 等任务**真正收尾**（finish 置 phase=finished）并返回快照。
//
// 为什么不能只看 status != running：取消时 taskRegistry.cancel 会立刻把状态改成 canceled，
// 此时后台协程还卡在当前那次 GM 调用上、收尾消息也还没写。按 status 判断会读到半成品快照
// （消息还是"已被管理员取消"），断言就变成了掷骰子。
func waitTaskFinished(t *testing.T, s *Server, id string) BatchTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task := s.tasks.get(id); task != nil {
			snap := task.snapshot()
			if snap.Phase == taskPhaseFinished && snap.Status != taskStatusRunning {
				return snap
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 5 秒内没有收尾", id)
	return BatchTask{}
}

// onlineCharsBridge 造"在线账号 + 每账号角色"的桥接，用于 online-characters 这一档。
func onlineCharsBridge(fake *fakeGM, accounts []store.OnlineAccount) *Bridge {
	b := fake.bridge()
	b.AcctOnlineAccounts = func(ctx context.Context, limit int) ([]store.OnlineAccount, error) {
		if limit > 0 && len(accounts) > limit {
			return accounts[:limit], nil
		}
		return accounts, nil
	}
	return b
}

// ---------- 用例 ----------

// TestMailboxClearBatchValidation confirm 与 scope 的校验必须在**占位之前**完成：
// 校验失败的请求不该留下任何任务（否则那道"同时只允许一个批量任务"的守卫会被空任务占住）。
func TestMailboxClearBatchValidation(t *testing.T) {
	fake := newFakeGM()
	s := newTestServer(t, fake.bridge())

	cases := []struct {
		name, body, wantInError string
	}{
		{"缺 confirm", `{"scope":"online-characters"}`, "confirm=true"},
		{"confirm=false", `{"scope":"online-characters","confirm":false}`, "confirm=true"},
		{"scope 非法", `{"scope":"whatever","confirm":true}`, "scope"},
		{"scope 缺失", `{"confirm":true}`, "scope"},
	}
	for _, tc := range cases {
		code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch", tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s：状态码 = %d，期望 400（响应 %s）", tc.name, code, raw)
		}
		ok, errMsg, _ := decodeEnvelope(t, raw)
		if ok || !strings.Contains(errMsg, tc.wantInError) {
			t.Fatalf("%s：错误信封不对 ok=%v error=%q（应含 %q）", tc.name, ok, errMsg, tc.wantInError)
		}
	}
	if list := s.tasks.list(10); len(list) != 0 {
		t.Fatalf("被拒的请求不该留下任务，实际 %d 个: %+v", len(list), list)
	}
	if fake.totalCalls() != 0 {
		t.Fatalf("被拒的请求不该调用 GM，实际 %d 次", fake.totalCalls())
	}
}

// TestMailboxClearBatchLimitClamp max 是安全阀：缺省用默认值、超限夹到硬上限。
func TestMailboxClearBatchLimitClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, mailboxClearBatchDefaultMax},
		{-5, mailboxClearBatchDefaultMax},
		{2000, 2000},
		{mailboxClearBatchHardMax, mailboxClearBatchHardMax},
		{mailboxClearBatchHardMax + 1, mailboxClearBatchHardMax},
		{999999, mailboxClearBatchHardMax},
	}
	for _, tc := range cases {
		if got := mailboxClearBatchLimit(tc.in); got != tc.want {
			t.Fatalf("mailboxClearBatchLimit(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
	// 上限必须是一套**独立**的常量（值可以与群发相同，但改一边不能连带改另一边）：
	// 硬上限要比群发那档宽，因为"删除全服邮件"要覆盖全服所有角色。
	if mailboxClearBatchHardMax <= batchGrantMaxItems {
		t.Fatalf("clear-batch 硬上限(%d) 应当比群发上限(%d) 宽", mailboxClearBatchHardMax, batchGrantMaxItems)
	}
	if mailboxClearBatchDefaultMax <= 0 || mailboxClearBatchDefaultMax > mailboxClearBatchHardMax {
		t.Fatalf("默认上限(%d) 必须落在 [1, 硬上限(%d)] 内", mailboxClearBatchDefaultMax, mailboxClearBatchHardMax)
	}
}

// TestMailboxClearBatchDryRunDeletesNothing dry_run 只解析目标：一封都不删，且任务消息里
// 必须出现"预演"与目标数（管理工具据此提示"这是预演"）。
func TestMailboxClearBatchDryRunDeletesNothing(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{
		{CharacterID: "1001", Name: "角色A", Online: true},
		{CharacterID: "1002", Name: "角色B", Online: true},
	}
	fake.accounts["dnf:b"] = []gmCharacterView{
		{CharacterID: "1003", Name: "角色C", Online: true},
	}
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{
		{AccountID: "dnf:a", Username: "acc-a"},
		{AccountID: "dnf:b", Username: "acc-b"},
	})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true,"dry_run":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)

	if snap.Status != taskStatusDone {
		t.Fatalf("预演任务应为 done，实际 %+v", snap)
	}
	if snap.Total != 3 || snap.OK != 0 || snap.Failed != 0 {
		t.Fatalf("预演只应填好 Total，实际 %+v", snap)
	}
	if !strings.Contains(snap.Message, "预演") || !strings.Contains(snap.Message, "3 个角色") {
		t.Fatalf("预演消息应含“预演”与目标数，实际 %q", snap.Message)
	}
	if !strings.Contains(snap.Message, "角色A/acc-a") {
		t.Fatalf("预演消息应列出命中的角色与账号，实际 %q", snap.Message)
	}
	if n := fake.countOfPath("/local/dnf-gm/mail/clear"); n != 0 {
		t.Fatalf("dry_run 竟然发了 %d 次清空调用（这是物理删除，绝不能发生）", n)
	}
}

// TestMailboxClearBatchClearsAllOnlineTargets 正常清空：N 个目标 → ok=N，
// 且每个目标发出的都是 /local/dnf-gm/mail/clear + 单角色清空那条链路**同形状**的请求体。
func TestMailboxClearBatchClearsAllOnlineTargets(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{
		{CharacterID: "1001", Name: "角色A", Online: true},
		{CharacterID: "1002", Name: "角色B", Online: false}, // 离线角色在 online-characters 档不算目标
	}
	fake.accounts["dnf:b"] = []gmCharacterView{
		{CharacterID: "1003", Name: "角色C", Online: true},
	}
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{
		{AccountID: "dnf:a", Username: "acc-a"},
		{AccountID: "dnf:b", Username: "acc-b"},
	})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
	if snap.Status != taskStatusDone || snap.OK != 2 || snap.Failed != 0 || snap.Total != 2 {
		t.Fatalf("清空任务统计不对: %+v", snap)
	}
	if !strings.Contains(snap.Message, "清空完成：成功 2") {
		t.Fatalf("完成消息应写明成功数，实际 %q", snap.Message)
	}

	clears := fake.callsOfPath("/local/dnf-gm/mail/clear")
	if len(clears) != 2 {
		t.Fatalf("清空调用次数 = %d，期望 2（%+v）", len(clears), clears)
	}
	// 请求体**只带 character_id + confirm**：故意不发 character_name ——
	// 服务端在"同时给了 ID 和名字"时会做一致性校验（不一致 → 查无此人），
	// 而批量要跑几分钟，期间改过名的角色就会因此失败。
	want := []map[string]any{
		{"confirm": true, "character_id": "1001"},
		{"confirm": true, "character_id": "1003"},
	}
	for i, call := range clears {
		var got map[string]any
		if err := json.Unmarshal([]byte(call.Body), &got); err != nil {
			t.Fatalf("第 %d 次清空请求体不是 JSON: %v（%s）", i+1, err, call.Body)
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("第 %d 次清空请求体 = %v，期望 %v", i+1, got, want[i])
		}
		if _, sent := got["character_name"]; sent {
			t.Fatalf("第 %d 次清空请求体带了 character_name（会触发服务端 ID/名字一致性校验）：%s",
				i+1, call.Body)
		}
		if call.Method != http.MethodPost {
			t.Fatalf("清空必须用 POST，实际 %s", call.Method)
		}
	}
}

// TestMailboxClearBatchTruncatedMessage 撞上限必须明说：否则运营会以为全服清干净了。
func TestMailboxClearBatchTruncatedMessage(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{
		{CharacterID: "1001", Name: "角色A", Online: true},
		{CharacterID: "1002", Name: "角色B", Online: true},
		{CharacterID: "1003", Name: "角色C", Online: true},
	}
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{{AccountID: "dnf:a", Username: "acc-a"}})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true,"max":2}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
	if snap.Total != 2 || snap.OK != 2 {
		t.Fatalf("max=2 应该只处理 2 个目标，实际 %+v", snap)
	}
	if !strings.Contains(snap.Message, "已达上限 2") || !strings.Contains(snap.Message, "请再次执行") {
		t.Fatalf("撞上限时消息必须明说，实际 %q", snap.Message)
	}
}

// TestMailboxClearBatchCancel 任务运行中取消 → canceled（工作循环每轮都要检查取消）。
func TestMailboxClearBatchCancel(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{
		{CharacterID: "1001", Name: "角色A", Online: true},
		{CharacterID: "1002", Name: "角色B", Online: true},
		{CharacterID: "1003", Name: "角色C", Online: true},
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake.beforeClear = func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{{AccountID: "dnf:a", Username: "acc-a"}})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	task := taskFromResponse(t, raw)

	// 等它真的开始清（否则取消可能发生在协程启动之前，测不到"运行中取消"）。
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("3 秒内没等到第一次清空调用")
	}
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/tasks/"+task.ID+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("取消状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	close(release)

	snap := waitTaskFinished(t, s, task.ID)
	if snap.Status != taskStatusCanceled {
		t.Fatalf("取消后状态 = %q，期望 canceled（%+v）", snap.Status, snap)
	}
	if !strings.Contains(snap.Message, "已取消") {
		t.Fatalf("取消消息不对: %q", snap.Message)
	}
	if snap.Done > 1 {
		t.Fatalf("取消后不该继续清空剩余目标，实际已处理 %d 个", snap.Done)
	}
	if n := fake.countOfPath("/local/dnf-gm/mail/clear"); n > 1 {
		t.Fatalf("取消后仍发了 %d 次清空调用", n)
	}
}

// TestMailboxClearBatchConflictOnDoubleClick 连点两次：第二次 409（同一把锁内"查+建"的守卫），
// 且文案要说清是**哪一类**任务占着坑。
func TestMailboxClearBatchConflictOnDoubleClick(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{{CharacterID: "1001", Name: "角色A", Online: true}}
	release := make(chan struct{})
	fake.beforeClear = func() { <-release }
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{{AccountID: "dnf:a", Username: "acc-a"}})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true}`)
	if code != http.StatusOK {
		t.Fatalf("第一次点击状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	task := taskFromResponse(t, raw)

	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/account/mailbox/clear-batch",
		`{"scope":"online-characters","confirm":true}`)
	if code != http.StatusConflict {
		t.Fatalf("第二次点击状态码 = %d，期望 409（响应 %s）", code, raw)
	}
	ok, errMsg, _ := decodeEnvelope(t, raw)
	if ok || !strings.Contains(errMsg, "已有批量任务在执行中") ||
		!strings.Contains(errMsg, taskKindMailboxClear) {
		t.Fatalf("409 文案应点明冲突任务类型，实际 ok=%v error=%q", ok, errMsg)
	}
	close(release)
	waitTaskFinished(t, s, task.ID)
}

// TestBatchPressureKindsShareOneLock 三类"压回环口"的批量任务共用同一把锁：
// 任一类在跑，另外两类都不许起（否则就是并发压服务端，重演 2026-09-25 的 500）。
func TestBatchPressureKindsShareOneLock(t *testing.T) {
	reg := newTaskRegistry()

	clear, _ := reg.createIfNoRunningKinds(taskKindMailboxClear, batchScopeAllChars, func() {}, batchPressureKinds)
	if clear == nil {
		t.Fatal("clear 第一次占位应当成功")
	}
	for _, kind := range []string{taskKindWalletMail, taskKindGrantMail} {
		if got, busy := reg.createIfNoRunning(kind, batchScopeOnlineChars, func() {}); got != nil {
			t.Fatalf("%s 不该在 mailbox-clear 跑着的时候占位成功", kind)
		} else if busy.Kind != taskKindMailboxClear {
			t.Fatalf("冲突快照应指向在跑的 mailbox-clear，实际 %+v", busy)
		}
	}
	// welfare 不压回环口，不受影响。
	if other, _ := reg.createIfNoRunning(taskKindWelfare, batchScopeOnlineChars, func() {}); other == nil {
		t.Fatal("welfare 不该被批量任务挡住")
	}
	// 反方向也要挡得住：wallet-mail 在跑时，单 kind 写法的 grant-mail 同样要被挡。
	clear.finish(taskStatusDone, "done")
	wallet, _ := reg.createIfNoRunningKinds(taskKindWalletMail, batchScopeOnlineChars, func() {}, batchPressureKinds)
	if wallet == nil {
		t.Fatal("wallet-mail 在 clear 结束后应当能占位")
	}
	if got, busy := reg.createIfNoRunning(taskKindGrantMail, batchScopeAllAccounts, func() {}); got != nil {
		t.Fatal("grant-mail 不该在 wallet-mail 跑着的时候占位成功（半边锁）")
	} else if busy.Kind != taskKindWalletMail {
		t.Fatalf("冲突快照应指向在跑的 wallet-mail，实际 %+v", busy)
	}
	wallet.finish(taskStatusDone, "done")
	if again, _ := reg.createIfNoRunning(taskKindGrantMail, batchScopeAllAccounts, func() {}); again == nil {
		t.Fatal("任务结束后守卫应释放")
	}
}

// TestNewBatchRoutesRegistered 两条新路由确实注册在 mux 上（漏注册只表现为 404，编译期看不出来）。
func TestNewBatchRoutesRegistered(t *testing.T) {
	fake := newFakeGM()
	bridge := fake.bridge()
	bridge.AcctOnlineAccounts = func(ctx context.Context, limit int) ([]store.OnlineAccount, error) {
		return nil, nil
	}
	s := newTestServer(t, bridge)
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/manage/v1/account/mailbox/clear-batch", `{"scope":"online-characters","confirm":true}`},
		{http.MethodPost, "/manage/v1/account/wallet/mail-batch",
			`{"scope":"online-characters","currency":"gold","amount":1000}`},
	}
	for _, c := range cases {
		code, raw := doMux(t, s, c.method, c.path, c.body)
		if code == http.StatusNotFound {
			t.Fatalf("%s %s 未注册（404）", c.method, c.path)
		}
		if code == http.StatusServiceUnavailable {
			t.Fatalf("%s %s 桥接未接上: %s", c.method, c.path, raw)
		}
		if code != http.StatusOK {
			t.Fatalf("%s %s 状态码 = %d，期望 200（响应 %s）", c.method, c.path, code, raw)
		}
		// 上一条任务要先跑完，下一条才不会被"同时只允许一个批量任务"的守卫挡成 409。
		waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
	}
}
