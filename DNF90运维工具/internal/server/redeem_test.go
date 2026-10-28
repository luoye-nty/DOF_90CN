package server

// CDK 发奖拆解测试：reward_json → 邮件附件 + 点券总额 + 黑钻天数。
//
// 这是"CDK 能不能兑换点券/黑钻"的核心纯逻辑，也是最容易出错的地方：
// 点券与黑钻不能进邮件附件（没有对应物品），物品不能在拆解中被吞掉。
//
// 文件末尾另有一组 handleRedeem 用例，钉的是另一件事：**任何拒绝都必须发生在
// 核销之前**（2026-09-30 线上事故：兑换码被消耗、奖励发给一张已删除的角色）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

func TestSplitRedeemRewards(t *testing.T) {
	cases := []struct {
		name      string
		reward    string
		wantItems int
		wantCera  int64
		wantBD    int64
	}{
		{"空奖励", "", 0, 0, 0},
		{"null", "null", 0, 0, 0},
		{"仅物品", `[{"item_id":1001,"count":2,"bind":true}]`, 1, 0, 0},
		{"仅点券", `[{"cera":5000}]`, 0, 5000, 0},
		{"仅黑钻", `[{"black_diamond_days":30}]`, 0, 0, 30},
		{"物品+点券", `[{"item_id":1001,"count":1},{"cera":3000},{"item_id":1002,"count":5}]`, 2, 3000, 0},
		{"物品+黑钻", `[{"item_id":1001,"count":1},{"black_diamond_days":30}]`, 1, 0, 30},
		{"点券+黑钻", `[{"cera":3000},{"black_diamond_days":7}]`, 0, 3000, 7},
		{
			"物品+点券+黑钻（一张 CDK 三种奖励同时发）",
			`[{"item_id":1001,"count":1},{"cera":3000},{"black_diamond_days":30},{"item_id":1002,"count":5}]`,
			2, 3000, 30,
		},
		{"多行点券累加", `[{"cera":3000},{"cera":2000}]`, 0, 5000, 0},
		{"多行黑钻累加", `[{"black_diamond_days":30},{"black_diamond_days":15}]`, 0, 0, 45},
		{"非法行被跳过", `[{"cera":0},{"item_id":0,"count":5},{"item_id":1001,"count":0},{"black_diamond_days":0}]`, 0, 0, 0},
		{"点券为负不累加", `[{"cera":-100}]`, 0, 0, 0},
		{"黑钻为负不累加", `[{"black_diamond_days":-30}]`, 0, 0, 0},
	}
	for _, testCase := range cases {
		items, cera, bd, err := splitRedeemRewards(testCase.reward)
		if err != nil {
			t.Fatalf("%s: 意外错误 %v", testCase.name, err)
		}
		if len(items) != testCase.wantItems || cera != testCase.wantCera || bd != testCase.wantBD {
			t.Fatalf("%s: items=%d cera=%d bd=%d, want %d/%d/%d",
				testCase.name, len(items), cera, bd, testCase.wantItems, testCase.wantCera, testCase.wantBD)
		}
		for _, item := range items {
			if item.ItemID == 0 || item.Count == 0 {
				t.Fatalf("%s: 拆出了空附件 %+v", testCase.name, item)
			}
		}
	}
}

func TestSplitRedeemRewardsRejectsBrokenJSON(t *testing.T) {
	for _, raw := range []string{"not-json", "{}", `{"item_id":1}`} {
		if _, _, _, err := splitRedeemRewards(raw); err == nil {
			t.Fatalf("splitRedeemRewards(%q) 应当报错", raw)
		}
	}
}

// TestSplitRedeemRewardsRejectsCeraOverflow：点券累加不能超客户端可表示范围
// （否则入账时服务端会拒，等于玩家兑换成功后拿不到点券）。
func TestSplitRedeemRewardsRejectsCeraOverflow(t *testing.T) {
	reward := `[{"cera":2147483647},{"cera":1}]`
	if _, _, _, err := splitRedeemRewards(reward); err == nil {
		t.Fatal("点券总额越过 2147483647 时应当报错")
	}
	ok := `[{"cera":2147483647}]`
	if _, cera, _, err := splitRedeemRewards(ok); err != nil || cera != 2147483647 {
		t.Fatalf("边界值应通过: cera=%d err=%v", cera, err)
	}
}

// TestSplitRedeemRewardsBlackDiamondBounds：黑钻天数必须在服务端单次激活上限内
// （服务端 GMActivateBlackDiamond 只接受 1..3650 天；超上限若放到兑换时才失败，
// 玩家会白扣一次兑换次数）。
func TestSplitRedeemRewardsBlackDiamondBounds(t *testing.T) {
	if _, _, bd, err := splitRedeemRewards(`[{"black_diamond_days":3650}]`); err != nil || bd != 3650 {
		t.Fatalf("边界值应通过: bd=%d err=%v", bd, err)
	}
	for _, raw := range []string{
		`[{"black_diamond_days":3651}]`,
		`[{"black_diamond_days":3000},{"black_diamond_days":1000}]`,
	} {
		if _, _, _, err := splitRedeemRewards(raw); err == nil {
			t.Fatalf("splitRedeemRewards(%q) 应当因黑钻天数超上限报错", raw)
		}
	}
}

// TestBuildMailAttachmentsSkipsCeraAndBlackDiamond：补发邮件用的附件解析不能把
// 点券行/黑钻行变成 {item_id:0,count:0} 的空附件。
func TestBuildMailAttachmentsSkipsCeraAndBlackDiamond(t *testing.T) {
	service := &Service{}
	got := service.buildMailAttachments(
		`[{"item_id":1001,"count":2},{"cera":5000},{"black_diamond_days":30},{"item_id":1002,"count":1,"bind":true}]`)
	if len(got) != 2 {
		t.Fatalf("附件数 = %d, want 2（点券/黑钻行必须被跳过）: %+v", len(got), got)
	}
	for _, att := range got {
		if att.ItemID == 0 || att.Count == 0 {
			t.Fatalf("出现了空附件: %+v", att)
		}
	}
	if atts := service.buildMailAttachments(`[{"cera":5000},{"black_diamond_days":30}]`); len(atts) != 0 {
		t.Fatalf("纯点券+黑钻奖励不应产生附件: %+v", atts)
	}
}

// TestBlackDiamondExpireText 到期时间文案：0/负数不能格式化成 1970 年。
func TestBlackDiamondExpireText(t *testing.T) {
	if got := blackDiamondExpireText(0); !strings.Contains(got, "未知") {
		t.Fatalf("blackDiamondExpireText(0) = %q, want 含「未知」", got)
	}
	want := time.Unix(1800000000, 0).Local().Format("2006-01-02 15:04:05")
	if got := blackDiamondExpireText(1800000000); got != want {
		t.Fatalf("blackDiamondExpireText(1800000000) = %q, want %q", got, want)
	}
}

// TestGMActivateBlackDiamondRejectsBadDays：本地先挡住非法天数，避免打无意义的回环请求。
func TestGMActivateBlackDiamondRejectsBadDays(t *testing.T) {
	service := &Service{}
	for _, days := range []int64{0, -1, redeemMaxBlackDiamondDays + 1} {
		if err := service.gmActivateBlackDiamond(t.Context(), "1", days, nil); err == nil {
			t.Fatalf("days=%d 应当被拒", days)
		}
	}
	if err := service.gmActivateBlackDiamond(t.Context(), "  ", 30, nil); err == nil {
		t.Fatal("空 character_id 应当被拒")
	}
}

// TestTruncateErrKeepsValidUTF8：三路奖励的错误明细更容易顶到 255 字节上限，
// 截断必须落在 UTF-8 边界上，否则 MySQL 会以 Incorrect string value 拒绝整条回写。
func TestTruncateErrKeepsValidUTF8(t *testing.T) {
	long := errors.New(strings.Repeat("黑钻: 服务端拒绝激活; ", 40))
	got := truncateErr(long)
	if len(got) > 255 {
		t.Fatalf("截断后长度 = %d, want ≤255", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断把多字节字符切坏了: %q", got)
	}
	if short := errors.New("物品邮件: refused"); truncateErr(short) != "物品邮件: refused" {
		t.Fatalf("短文案不应被改动: %q", truncateErr(short))
	}
}

// ---------- handleRedeem：任何拒绝都必须发生在核销之前（2026-09-30 线上事故） ----------

// redeemTestCode 一个格式合法的 CDK（XXXX-XXXX-XXXX-XXXX-XXXX，字符集不含 0/O/1/I）。
const redeemTestCode = "ABCD-EFGH-JKLM-NPQR-STUV"

// fakeRedeemStore 只实现 handleRedeem 需要的那几个方法。
//
// 为什么要替身：真实 *store.Store 需要 MySQL，单测起不了库；而这条链路最要命的约束是
// "任何拒绝都必须发生在核销之前" —— 只有替身才能直接观察到 RedeemCodeTx 有没有被调用
// （兑换码有没有被烧掉）。
type fakeRedeemStore struct {
	accountID    string
	account      *store.Account
	character    store.GameCharacter
	characterOK  bool
	characterErr error

	consumeCalls int
	consumeArgs  []string
	receipt      *store.RedeemReceipt
	// consumeErr 非 nil 时核销事务直接返回该错误（用于钉住"码不存在 → 404 invalid-code"）。
	consumeErr error

	mailStatus []string
}

func (f *fakeRedeemStore) ResolveAccountIDByTicket(_ context.Context, ticket string) (string, error) {
	if strings.TrimSpace(ticket) == "" {
		return "", store.ErrTicketNotFound
	}
	return f.accountID, nil
}

func (f *fakeRedeemStore) GetAccountByID(_ context.Context, _ string) (*store.Account, error) {
	if f.account == nil {
		return nil, store.ErrUserNotFound
	}
	return f.account, nil
}

// GameCharacterActive 这里就是"权威判据"的替身：characterOK=false 同时代表
// "角色已删除 / 不属于该账号 / 根本不存在"三种情况 —— 这三种在 handler 眼里是同一格
// （该拒），逐格区分由 store 侧的 TestJudgeGameCharacterTarget 负责。
func (f *fakeRedeemStore) GameCharacterActive(_ context.Context, _, _ string) (store.GameCharacter, bool, error) {
	return f.character, f.characterOK, f.characterErr
}

func (f *fakeRedeemStore) RedeemCodeTx(_ context.Context, code, accountID, username, characterID, characterName, remoteIP string) (*store.RedeemReceipt, error) {
	f.consumeCalls++
	f.consumeArgs = []string{code, accountID, username, characterID, characterName, remoteIP}
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	if f.receipt != nil {
		return f.receipt, nil
	}
	return &store.RedeemReceipt{RedemptionID: 1, Name: "测试CDK"}, nil
}

func (f *fakeRedeemStore) SetRedemptionMailResult(_ context.Context, _ int64, status, _ string) error {
	f.mailStatus = append(f.mailStatus, status)
	return nil
}

// newRedeemTestService 组装只跑本 handler 的 Service：日志丢弃、cfg 留空。
// cfg 空是故意的 —— 发奖阶段必然连不上服务端 GM 口，于是走到"发奖失败"分支；
// 那正说明核销已经发生，后续失败不影响"校验在核销之前"这条断言（也免得单测去连网）。
func newRedeemTestService(fake *fakeRedeemStore) *Service {
	return &Service{log: discardLogger(), cfg: &config.Config{}, redeemData: fake}
}

// redeemTestRequest 构造 POST /launcher/v1/redeem 请求。
func redeemTestRequest(characterID string) *http.Request {
	body := fmt.Sprintf(`{"ticket":"tk-1","code":%q,"character_id":%q}`, redeemTestCode, characterID)
	return httptest.NewRequest(http.MethodPost, "/launcher/v1/redeem", strings.NewReader(body))
}

// TestHandleRedeemNeverConsumesOnRejectedRequest 该拒的请求一条都不能消耗兑换码。
//
// 事故现场就是这样烧掉玩家兑换码的：校验先放行了已删除的角色 → 兑换码被消耗
// （落流水 + redeemed_count+1）→ 奖励发不出去。所以这里把"拒绝"和"零消耗"绑在一条断言里，
// 三格（已删除 / 不属于本账号 / 查库失败）任一格回归都会红。
func TestHandleRedeemNeverConsumesOnRejectedRequest(t *testing.T) {
	cases := []struct {
		name        string
		fake        *fakeRedeemStore
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			"角色已被删除（dnf_characters 里那行还在，delete_flag=1）",
			&fakeRedeemStore{accountID: "dnf:bob", account: &store.Account{Username: "bob"}},
			http.StatusForbidden, "character-not-owned", "已被删除",
		},
		{
			"角色不属于本账号（判据查不到该账号下的这一行）",
			&fakeRedeemStore{accountID: "dnf:bob", account: &store.Account{Username: "bob"}},
			http.StatusForbidden, "character-not-owned", "已被删除",
		},
		{
			"角色校验查库失败",
			&fakeRedeemStore{accountID: "dnf:bob", account: &store.Account{Username: "bob"},
				characterErr: errors.New("db down")},
			http.StatusInternalServerError, "internal-error", "",
		},
	}
	for _, testCase := range cases {
		service := newRedeemTestService(testCase.fake)
		rec := httptest.NewRecorder()
		service.handleRedeem(rec, redeemTestRequest("1001"))

		// 最要紧的一条先断言：**兑换码一次都不能被消耗**。
		// 放前面是为了让回归时的报错直接说"事故重演"，而不是一句看不出严重性的 HTTP 状态不符。
		if testCase.fake.consumeCalls != 0 {
			t.Fatalf("%s：兑换码被消耗了 %d 次 —— 事故重演（先消耗、后失败，响应 %s）",
				testCase.name, testCase.fake.consumeCalls, rec.Body.String())
		}
		if len(testCase.fake.mailStatus) != 0 {
			t.Fatalf("%s：还没核销就回写了流水状态 %v", testCase.name, testCase.fake.mailStatus)
		}
		if rec.Code != testCase.wantStatus {
			t.Fatalf("%s：HTTP 状态 = %d，期望 %d（响应 %s）",
				testCase.name, rec.Code, testCase.wantStatus, rec.Body.String())
		}
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s：响应不是 JSON: %v（%s）", testCase.name, err, rec.Body.String())
		}
		// code 沿用 character-not-owned：外网版登录器 Services\RemoteAuthService.cs 的
		// ExtractRedeemApiError 只认这个 code（映射成"所选角色不属于当前账号，请重新选择"），
		// 新造 code 玩家只会看到一句泛化错误。
		if payload.Code != testCase.wantCode {
			t.Fatalf("%s：code = %q，期望 %q", testCase.name, payload.Code, testCase.wantCode)
		}
		// message 仍要能看出真实原因（运营/日志排查用）：登录器会用自己的文案覆盖，
		// 但服务端与 curl 排查的人只能看这一句。
		if testCase.wantMessage != "" && !strings.Contains(payload.Message, testCase.wantMessage) {
			t.Fatalf("%s：message = %q，期望含 %q", testCase.name, payload.Message, testCase.wantMessage)
		}
	}
}

// ---------- handleRedeem：抠不出码要给"可自查"的 400（2026-10 玩家反馈） ----------

// TestHandleRedeemUnrecognizedCode 老版本登录器（码框 MaxLength=24）会把
// 「卡号：XXXX-… 」整段粘贴截断后再提交；旧实现把"格式非法"与"码不存在"混成同一个
// 404 invalid-code，玩家看到"兑换码无效、不存在或已停用"，既不知道贴错了、也不知道怎么贴。
//
// 这里钉住三件事：
//  1. 抠不出来 → 400 + code-unrecognized（与"码不存在"的 404 invalid-code 分开）；
//  2. message 是中文 —— 登录器 RemoteAuthService.ExtractRedeemApiError 对
//     "未知 code + message 含中文"会原样透出，所以不发新登录器也能让玩家看到这句话；
//  3. 同样零消耗（连角色校验都不该走到，更不能核销）。
func TestHandleRedeemUnrecognizedCode(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"老登录器被 MaxLength 截断的整段粘贴", "卡号：ABCD-EFGH-JKLM-NPQR-STU 密码：1234"},
		{"只有中文标签", "兑换码：请复制完整内容"},
		{"前半段是订单号", "订单20260930 密码12345678"},
		{"只有 19 位", "ABCD-EFGH-JKLM-NPQR-STU"},
	}
	for _, testCase := range cases {
		fake := &fakeRedeemStore{
			accountID:   "dnf:bob",
			account:     &store.Account{Username: "bob"},
			character:   store.GameCharacter{CharacterID: "1001", Name: "角色甲"},
			characterOK: true,
		}
		service := newRedeemTestService(fake)
		body := fmt.Sprintf(`{"ticket":"tk-1","code":%q,"character_id":"1001"}`, testCase.code)
		rec := httptest.NewRecorder()
		service.handleRedeem(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/redeem", strings.NewReader(body)))

		if fake.consumeCalls != 0 {
			t.Fatalf("%s：兑换码被消耗了 %d 次", testCase.name, fake.consumeCalls)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s：HTTP 状态 = %d，期望 400（响应 %s）", testCase.name, rec.Code, rec.Body.String())
		}
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s：响应不是 JSON: %v（%s）", testCase.name, err, rec.Body.String())
		}
		if payload.Code != "code-unrecognized" {
			t.Fatalf("%s：code = %q，期望 code-unrecognized（与「码不存在」必须分开）", testCase.name, payload.Code)
		}
		if !utf8.ValidString(payload.Message) || !strings.Contains(payload.Message, "识别") {
			t.Fatalf("%s：message = %q，期望中文且说明「没能识别出兑换码」", testCase.name, payload.Message)
		}
	}
}

// TestHandleRedeemAcceptsChineseLabeledCode 对照组：带中文标签的**完整**码必须能抠出来并走到核销。
//
// 老版本登录器把中文丢掉、再拼凑 20 位，结果送到服务端的往往是"看着像码、其实是密码数字"
// 的字符串；服务端这一层现在能自己从带标签的整段文本里抠码，等于给老客户端兜底。
func TestHandleRedeemAcceptsChineseLabeledCode(t *testing.T) {
	fake := &fakeRedeemStore{
		accountID:   "dnf:bob",
		account:     &store.Account{Username: "bob"},
		character:   store.GameCharacter{CharacterID: "1001", Name: "角色甲"},
		characterOK: true,
		receipt:     &store.RedeemReceipt{RedemptionID: 9, Name: "测试CDK", RewardJSON: `[{"cera":100}]`},
	}
	service := newRedeemTestService(fake)
	body := fmt.Sprintf(`{"ticket":"tk-1","code":%q,"character_id":"1001"}`,
		"卡号：ABCD-EFGH-JKLM-NPQR-STUV 密码：1234")
	rec := httptest.NewRecorder()
	service.handleRedeem(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/redeem", strings.NewReader(body)))

	if fake.consumeCalls != 1 {
		t.Fatalf("核销调用次数 = %d，期望 1（响应 %s）", fake.consumeCalls, rec.Body.String())
	}
	// 核销拿到的必须是**规范化后的标准形状**，不能是把中文滤掉后拼出来的字符串。
	if len(fake.consumeArgs) == 0 || fake.consumeArgs[0] != redeemTestCode {
		t.Fatalf("核销用的码 = %v，期望 %q", fake.consumeArgs, redeemTestCode)
	}
}

// TestHandleRedeemUnrecognizedIsNotNotFound 把"没识别出来"与"码不存在"的差异焊死。
//
// 两者以前都是 404 invalid-code 同一句文案；现在前者 400 code-unrecognized、
// 后者 404 invalid-code。这个差异是玩家自查的唯一线索，被合并回去就等于玩家又看不到原因了。
func TestHandleRedeemUnrecognizedIsNotNotFound(t *testing.T) {
	const wanted = "ABCD-EFGH-JKLM-NPQR-STUV"

	// ① 没识别出来：400 code-unrecognized。
	fakeBad := &fakeRedeemStore{accountID: "dnf:bob", account: &store.Account{Username: "bob"},
		character: store.GameCharacter{CharacterID: "1001", Name: "角色甲"}, characterOK: true}
	serviceBad := newRedeemTestService(fakeBad)
	recBad := httptest.NewRecorder()
	serviceBad.handleRedeem(recBad, httptest.NewRequest(http.MethodPost, "/launcher/v1/redeem",
		strings.NewReader(`{"ticket":"tk-1","code":"没有兑换码","character_id":"1001"}`)))
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("没识别出来：HTTP = %d，期望 400（%s）", recBad.Code, recBad.Body.String())
	}

	// ② 格式合法但库里没有：仍然必须是 404 invalid-code（核销事务返回 not-found）。
	fakeMissing := &fakeRedeemStore{
		accountID:   "dnf:bob",
		account:     &store.Account{Username: "bob"},
		character:   store.GameCharacter{CharacterID: "1001", Name: "角色甲"},
		characterOK: true,
		consumeErr:  store.ErrRedeemCodeNotFound,
	}
	serviceMissing := newRedeemTestService(fakeMissing)
	recMissing := httptest.NewRecorder()
	body := fmt.Sprintf(`{"ticket":"tk-1","code":%q,"character_id":"1001"}`, wanted)
	serviceMissing.handleRedeem(recMissing, httptest.NewRequest(http.MethodPost, "/launcher/v1/redeem", strings.NewReader(body)))
	if recMissing.Code != http.StatusNotFound {
		t.Fatalf("码不存在：HTTP = %d，期望 404（%s）", recMissing.Code, recMissing.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recMissing.Body.Bytes(), &payload); err != nil {
		t.Fatalf("码不存在：响应不是 JSON: %v（%s）", err, recMissing.Body.String())
	}
	if payload.Code != "invalid-code" {
		t.Fatalf("码不存在：code = %q，期望 invalid-code", payload.Code)
	}
}

// TestHandleRedeemConsumesAfterCharacterCheckPasses 对照组：判据放行时必须真的走到核销。
//
// 没有这一条，上面那条"零消耗"可能是假绿 —— 比如请求根本没跑通（ticket 没过、
// body 解析失败），断言就永远成立却毫无意义。这里要求 RedeemCodeTx 恰好被调用一次，
// 且流水落在**权威判据返回的角色**上（角色名来自判据，不是请求里的字符串）。
func TestHandleRedeemConsumesAfterCharacterCheckPasses(t *testing.T) {
	fake := &fakeRedeemStore{
		accountID:   "dnf:bob",
		account:     &store.Account{Username: "bob"},
		character:   store.GameCharacter{CharacterID: "1001", Name: "角色甲", Level: 90},
		characterOK: true,
		receipt:     &store.RedeemReceipt{RedemptionID: 7, Name: "测试CDK", RewardJSON: `[{"cera":100}]`},
	}
	service := newRedeemTestService(fake)
	rec := httptest.NewRecorder()
	service.handleRedeem(rec, redeemTestRequest("1001"))

	if fake.consumeCalls != 1 {
		t.Fatalf("核销调用次数 = %d，期望 1（校验通过就该消耗兑换码）", fake.consumeCalls)
	}
	wantArgs := []string{redeemTestCode, "dnf:bob", "bob", "1001", "角色甲"}
	if len(fake.consumeArgs) < len(wantArgs) {
		t.Fatalf("核销参数 = %v，期望至少 %v", fake.consumeArgs, wantArgs)
	}
	for i, want := range wantArgs {
		if fake.consumeArgs[i] != want {
			t.Fatalf("核销参数[%d] = %q，期望 %q（全部：%v）", i, fake.consumeArgs[i], want, fake.consumeArgs)
		}
	}
	// cfg.ServerRoot 为空 → 点券入账必然连不上服务端 GM 口 → 走到"发奖全失败"分支：
	// 502 grant-failed + 流水回写 failed。发奖阶段的失败是本用例的预期结果，
	// 它同时证明核销确实发生在校验之后（而不是反过来）。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("HTTP 状态 = %d，期望 502（点券发放连不上 GM 口）: %s", rec.Code, rec.Body.String())
	}
	if len(fake.mailStatus) != 1 || fake.mailStatus[0] != "failed" {
		t.Fatalf("流水回写 = %v，期望 [failed]", fake.mailStatus)
	}
}
