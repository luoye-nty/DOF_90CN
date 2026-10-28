//go:build paid

// 批量资产邮件发放（/manage/v1/account/wallet/mail-batch）的契约测试，
// 外加**单角色接口重构后的形状回归**（handleAccountWalletMail 改成复用 walletMailToTarget，
// 管理工具还在按老字段名解析，形状变了整页就废）。
//
// 三条最要紧的契约：
//   - cera 分支**不幂等**：入账成功但通知失败时只单独计数、绝不重试（重试 = 重复发券）；
//   - dry_run 一个 GM 调用都不发（server-first-character 这一档连收集都只读库）；
//   - "全服首个角色"解析不到时必须报 failed，不能报 done（否则运营以为已经发出去了）。
//
// 假 GM 通道 fakeGM 与任务助手定义在 handlers_mailbox_batch_test.go（同一批任务的公共测试件）。
package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"dnf90ops/internal/store"
)

// accountsBridge 造"账号分页 + 每账号角色"的桥接（scope=all-accounts 用）。
func accountsBridge(fake *fakeGM, accounts []store.Account) *Bridge {
	b := fake.bridge()
	b.AcctAccountsPage = func(ctx context.Context, offset, limit int) ([]store.Account, error) {
		if offset >= len(accounts) {
			return nil, nil
		}
		end := offset + limit
		if end > len(accounts) {
			end = len(accounts)
		}
		return accounts[offset:end], nil
	}
	return b
}

// TestWalletMailBatchValidation scope / currency / amount 的校验必须在占位之前完成。
func TestWalletMailBatchValidation(t *testing.T) {
	fake := newFakeGM()
	s := newTestServer(t, fake.bridge())
	cases := []struct {
		name, body, wantInError string
	}{
		{"scope 非法", `{"scope":"all-characters","currency":"gold","amount":100}`, "scope"},
		{"scope 缺失", `{"currency":"gold","amount":100}`, "scope"},
		{"currency 非法", `{"scope":"online-characters","currency":"token_cera","amount":100}`, "currency"},
		{"amount=0", `{"scope":"online-characters","currency":"gold","amount":0}`, "amount"},
		{"amount 负数", `{"scope":"online-characters","currency":"gold","amount":-1}`, "amount"},
		{"cera 超上限", `{"scope":"online-characters","currency":"cera","amount":2147483648}`, "amount"},
		{"标题过长", `{"scope":"online-characters","currency":"gold","amount":1,"title":"` + strings.Repeat("标", 41) + `"}`, "标题"},
		{"正文过长", `{"scope":"online-characters","currency":"gold","amount":1,"body":"` + strings.Repeat("文", 401) + `"}`, "正文"},
	}
	for _, tc := range cases {
		code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch", tc.body)
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
}

// TestWalletMailBatchAmountCapMatchesSingleRole 数量上限与单角色接口**同口径**（两币种共用
// accountMaxWalletBalance）：批量不该比单角色多接受一段，否则运营没法解释"批量能过、单个过不了"。
func TestWalletMailBatchAmountCapMatchesSingleRole(t *testing.T) {
	if walletMailBatchAmountMax != accountMaxWalletBalance {
		t.Fatalf("批量上限 = %d，期望与单角色同口径的 accountMaxWalletBalance(%d)",
			walletMailBatchAmountMax, accountMaxWalletBalance)
	}

	fake := newFakeGM()
	bridge := fake.bridge()
	bridge.AcctOnlineAccounts = func(ctx context.Context, limit int) ([]store.OnlineAccount, error) {
		return nil, nil
	}
	s := newTestServer(t, bridge)

	// 超上限（两个币种都不放过）→ 400，且不留下任务。
	over := accountMaxWalletBalance + 1
	for _, currency := range []string{"gold", "cera"} {
		code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
			fmt.Sprintf(`{"scope":"online-characters","currency":%q,"amount":%d}`, currency, over))
		if code != http.StatusBadRequest {
			t.Fatalf("%s amount=%d 应回 400，实际 %d（响应 %s）", currency, over, code, raw)
		}
		if _, errMsg, _ := decodeEnvelope(t, raw); !strings.Contains(errMsg, "amount") {
			t.Fatalf("%s 的错误应点明 amount，实际 %q", currency, errMsg)
		}
	}
	// gold 特意放宽到 uint32 的那一段也必须被拒（那正是"批量偷偷多接受一段"的口子）。
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		fmt.Sprintf(`{"scope":"online-characters","currency":"gold","amount":%d}`, accountMailGoldMax))
	if code != http.StatusBadRequest {
		t.Fatalf("gold 上限之外的 %d 应回 400，实际 %d（响应 %s）", accountMailGoldMax, code, raw)
	}
	// 边界值本身是允许的（上限是闭区间，别写成开区间）。
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		fmt.Sprintf(`{"scope":"online-characters","currency":"gold","amount":%d}`, accountMaxWalletBalance))
	if code != http.StatusOK {
		t.Fatalf("边界值 amount=%d 应被接受，实际 %d（响应 %s）", accountMaxWalletBalance, code, raw)
	}
	waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
}

// TestWalletMailBatchLimitClamp max 夹取口径。
func TestWalletMailBatchLimitClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, walletMailBatchDefaultMax},
		{-1, walletMailBatchDefaultMax},
		{500, 500},
		{walletMailBatchHardMax + 1, walletMailBatchHardMax},
	}
	for _, tc := range cases {
		if got := walletMailBatchLimit(tc.in); got != tc.want {
			t.Fatalf("walletMailBatchLimit(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// TestWalletMailBatchServerFirstCharacterDryRun "全服首个角色"的预演：
// 必须把解析到的角色 + 账号列进消息，且**一个 GM 调用都不发**（这一档只读库）。
func TestWalletMailBatchServerFirstCharacterDryRun(t *testing.T) {
	fake := newFakeGM()
	bridge := fake.bridge()
	bridge.AcctServerFirstCharacter = func(ctx context.Context) (store.ServerFirstCharacter, bool, error) {
		return store.ServerFirstCharacter{CharacterID: "1001", Name: "角色A", AccountID: "dnf:a", Level: 90}, true, nil
	}
	bridge.AccountNamesByID = func(ctx context.Context, accountIDs []string) (map[string]string, error) {
		return map[string]string{"dnf:a": "acc-a"}, nil
	}
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		`{"scope":"server-first-character","currency":"cera","amount":100000,"dry_run":true}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)

	if snap.Status != taskStatusDone {
		t.Fatalf("预演任务应为 done，实际 %+v", snap)
	}
	if snap.Total != 1 || snap.OK != 0 {
		t.Fatalf("预演只应填好 Total=1，实际 %+v", snap)
	}
	if !strings.Contains(snap.Message, "预演") || !strings.Contains(snap.Message, "未执行") {
		t.Fatalf("预演消息应含“预演/未执行”，实际 %q", snap.Message)
	}
	if !strings.Contains(snap.Message, "角色A/acc-a") {
		t.Fatalf("预演消息应列出目标角色/账号（运营就是靠这行确认发的是不是测试号），实际 %q", snap.Message)
	}
	if n := fake.totalCalls(); n != 0 {
		t.Fatalf("dry_run 竟发了 %d 次 GM 调用（%+v）", n, fake.callsOfPath("/local/dnf-gm/mail"))
	}
}

// TestWalletMailBatchServerFirstCharacterNotFound 解析不到目标必须 failed：报 done 会让运营
// 以为"已经发给那个唯一的目标了"。
func TestWalletMailBatchServerFirstCharacterNotFound(t *testing.T) {
	fake := newFakeGM()
	bridge := fake.bridge()
	bridge.AcctServerFirstCharacter = func(ctx context.Context) (store.ServerFirstCharacter, bool, error) {
		return store.ServerFirstCharacter{}, false, nil
	}
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		`{"scope":"server-first-character","currency":"gold","amount":1000}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
	if snap.Status != taskStatusFailed {
		t.Fatalf("解析不到目标应报 failed，实际 %+v", snap)
	}
	if !strings.Contains(snap.Message, "全服还没有任何角色") {
		t.Fatalf("失败原因要可读，实际 %q", snap.Message)
	}
	if n := fake.totalCalls(); n != 0 {
		t.Fatalf("没有目标时不该有任何 GM 调用，实际 %d 次", n)
	}
}

// TestWalletMailBatchCeraCreditedButNotifyFailed 需求里点名的那条：
// cera 入账成功但通知失败 → 单独计数、**不重试**（钱已经到账，重试就是重复发券）。
func TestWalletMailBatchCeraCreditedButNotifyFailed(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{
		{CharacterID: "1001", Name: "角色A", Online: true},
		{CharacterID: "1002", Name: "角色B", Online: true},
	}
	// 只有角色A 的通知邮件失败（入账照样成功）。
	fake.mailErr["角色A"] = errors.New("游戏服回环口 500")
	bridge := onlineCharsBridge(fake, []store.OnlineAccount{{AccountID: "dnf:a", Username: "acc-a"}})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		`{"scope":"online-characters","currency":"cera","amount":100000}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)

	if snap.Status != taskStatusDone || snap.Total != 2 || snap.OK != 2 || snap.Failed != 0 {
		t.Fatalf("入账成功即算成功（通知失败不算失败），实际 %+v", snap)
	}
	if !strings.Contains(snap.Message, "已入账但通知失败 1 个") {
		t.Fatalf("任务消息必须点名“已入账但通知失败 N 个”，实际 %q", snap.Message)
	}
	if !strings.Contains(snap.Message, "请勿重试") {
		t.Fatalf("任务消息必须提示请勿重试，实际 %q", snap.Message)
	}

	// 每个目标**只入账一次**、通知**只尝试一次**：任何重试都会让玩家多拿一份点券。
	if n := fake.countOfPath("/local/dnf-gm/wallet/update"); n != 2 {
		t.Fatalf("入账调用次数 = %d，期望 2（每个目标一次，绝不重试）", n)
	}
	if n := fake.countOfPath("/local/dnf-gm/mail"); n != 2 {
		t.Fatalf("通知邮件调用次数 = %d，期望 2（失败也不重试）", n)
	}
}

// TestWalletMailBatchGoldSendsMailOnly gold 走邮件附件：不发钱包 update，正文标题用默认文案。
func TestWalletMailBatchGoldSendsMailOnly(t *testing.T) {
	fake := newFakeGM()
	fake.accounts["dnf:a"] = []gmCharacterView{{CharacterID: "1001", Name: "角色A"}}
	fake.accounts["dnf:b"] = []gmCharacterView{{CharacterID: "1002", Name: "角色B"}}
	bridge := accountsBridge(fake, []store.Account{
		{AccountID: "dnf:a", Username: "acc-a"},
		{AccountID: "dnf:b", Username: "acc-b"},
	})
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail-batch",
		`{"scope":"all-accounts","currency":"gold","amount":100000}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	snap := waitTaskFinished(t, s, taskFromResponse(t, raw).ID)
	if snap.Status != taskStatusDone || snap.OK != 2 || snap.Failed != 0 {
		t.Fatalf("批量金币邮件统计不对: %+v", snap)
	}
	if n := fake.countOfPath("/local/dnf-gm/wallet/update"); n != 0 {
		t.Fatalf("gold 不该动钱包，实际调用 wallet/update %d 次", n)
	}
	mails := fake.callsOfPath("/local/dnf-gm/mail")
	if len(mails) != 2 {
		t.Fatalf("金币邮件调用次数 = %d，期望 2", len(mails))
	}
	var sent gmMailRequest
	if err := json.Unmarshal([]byte(mails[0].Body), &sent); err != nil {
		t.Fatalf("金币邮件请求体解析失败: %v（%s）", err, mails[0].Body)
	}
	if sent.Gold != 100000 || sent.CharacterName != "角色A" || sent.Title != "GM 金币邮件" {
		t.Fatalf("金币邮件请求体不对: %+v", sent)
	}
}

// TestAccountWalletMailSingleRoleShapeUnchanged 单角色接口重构后的形状回归：
// handleAccountWalletMail 现在只是调用 walletMailToTarget，对外字段与文案必须一字未改。
func TestAccountWalletMailSingleRoleShapeUnchanged(t *testing.T) {
	fake := newFakeGM()
	s := newTestServer(t, fake.bridge())

	// gold：扁平结构（含 message）。
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail",
		`{"role_name":"角色A","currency":"gold","amount":1000}`)
	if code != http.StatusOK {
		t.Fatalf("gold 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("gold 应 ok=true，实际 error=%q", errMsg)
	}
	var gold map[string]any
	if err := json.Unmarshal(data, &gold); err != nil {
		t.Fatalf("gold data 不是对象: %v", err)
	}
	for _, key := range []string{"character", "currency", "mode", "amount", "before", "after",
		"cera", "gold", "online", "refreshed", "delivery", "mail", "message"} {
		if _, exists := gold[key]; !exists {
			t.Fatalf("gold 响应缺少字段 %q: %v", key, gold)
		}
	}
	if gold["currency"] != "gold" || gold["mode"] != "mail" || gold["delivery"] != "claimable_mail" {
		t.Fatalf("gold 响应字段值不对: %v", gold)
	}
	if gold["message"] != "金币邮件已发送，在线邮箱已实时刷新；领取后到账" {
		t.Fatalf("gold 文案变了: %v", gold["message"])
	}

	// cera：{"result": {...}, "message": "..."}。
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail",
		`{"role_name":"角色A","currency":"cera","amount":100000}`)
	if code != http.StatusOK {
		t.Fatalf("cera 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	_, errMsg, data = decodeEnvelope(t, raw)
	if errMsg != "" {
		t.Fatalf("cera 应 ok=true，实际 error=%q", errMsg)
	}
	var cera struct {
		Result  map[string]any `json:"result"`
		Message string         `json:"message"`
	}
	if err := json.Unmarshal(data, &cera); err != nil {
		t.Fatalf("cera data 形状不对（管理工具按 {result,message} 解析）: %v（%s）", err, string(data))
	}
	if cera.Result["delivery"] != "credited_with_notice" || cera.Result["currency"] != "cera" {
		t.Fatalf("cera result 字段值不对: %v", cera.Result)
	}
	if cera.Message != "点券已到账，通知邮件与在线邮箱均已实时刷新" {
		t.Fatalf("cera 文案变了: %q", cera.Message)
	}

	// cera + 通知失败：仍然是 200 + 那条"请勿重复操作"，并把原因放进 result.mail_error。
	fake.mailErr["角色A"] = errors.New("游戏服回环口 500")
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail",
		`{"role_name":"角色A","currency":"cera","amount":100000}`)
	if code != http.StatusOK {
		t.Fatalf("通知失败状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	_, errMsg, data = decodeEnvelope(t, raw)
	if errMsg != "" {
		t.Fatalf("通知失败仍应 ok=true，实际 error=%q", errMsg)
	}
	cera = struct {
		Result  map[string]any `json:"result"`
		Message string         `json:"message"`
	}{}
	if err := json.Unmarshal(data, &cera); err != nil {
		t.Fatalf("通知失败 data 形状不对: %v", err)
	}
	if cera.Message != "点券已到账，但通知邮件发送失败；请勿重复操作" {
		t.Fatalf("通知失败文案变了: %q", cera.Message)
	}
	if cera.Result["mail_error"] == nil {
		t.Fatalf("通知失败应带 result.mail_error，实际 %v", cera.Result)
	}

	// 单角色接口的校验口径保持不变（两个币种共用 accountMaxWalletBalance）。
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/account/wallet/mail",
		`{"role_name":"角色A","currency":"gold","amount":2147483648}`)
	if code != http.StatusBadRequest {
		t.Fatalf("超出单角色上限应回 400，实际 %d（响应 %s）", code, raw)
	}
	if _, errMsg, _ := decodeEnvelope(t, raw); !strings.Contains(errMsg, "数量上限 2147483647") {
		t.Fatalf("单角色接口的错误文案变了: %q", errMsg)
	}
}
