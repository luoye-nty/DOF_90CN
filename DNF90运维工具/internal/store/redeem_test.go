package store

// CDK 兑换「发放目标」口径的契约测试（2026-09-30 线上事故）。
//
// 这是一条**无库**测试：真正执行 SQL 需要 MySQL（其余 store 用例同样不起库），
// 但这次事故的全部后果都押在两句 SQL 与一个判据上 —— 兑换码先被消耗掉，
// 奖励却发给一张已删除的角色，玩家手里只剩一次被烧掉的兑换。所以这里把口径钉死：
// 登录器的选角列表必须过滤已删除角色，发放目标判据必须逐格拒绝。
//
// 背景（判据为什么长这样）：游戏侧删号是**软删除**，只把 dnf_characters.delete_flag
// 置 1、slot 改成负数，那一行**还在表里**。少一个 delete_flag 条件，就会凭空多出
// "玩家在游戏里看不见、却能被登录器选中"的幽灵角色。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// assertReadOnlyQuery 这两条查询都必须只读：改游戏数据一律走服务端 GM 接口，
// 直写游戏库会被在线角色的内存快照在下次存档时覆盖回去（本项目的既有边界）。
func assertReadOnlyQuery(t *testing.T, sqlText string) {
	t.Helper()
	upper := strings.ToUpper(sqlText)
	for _, bad := range []string{"UPDATE ", "DELETE ", "INSERT ", "REPLACE "} {
		if strings.Contains(upper, bad) {
			t.Fatalf("只读查询里出现了写操作 %q：%s", bad, sqlText)
		}
	}
}

// TestListGameCharactersByAccountSQLFiltersDeleted 登录器选角列表必须过滤已删除角色。
//
// 这条断言就是事故的边界：少了 delete_flag = 0，已删除的角色会照旧出现在登录器的
// 角色选择列表里；玩家选中它去兑换 CDK，归属校验一路放行 → 兑换码被消耗 →
// 奖励邮件/点券/黑钻全部发给一张服务端不认的角色。
func TestListGameCharactersByAccountSQLFiltersDeleted(t *testing.T) {
	sqlText := strings.Join(strings.Fields(listGameCharactersByAccountSQL), " ")

	if !strings.Contains(sqlText, "FROM dnf_characters") {
		t.Fatalf("必须直查游戏库 dnf_characters，实际 %s", sqlText)
	}
	if !strings.Contains(sqlText, "account_id = ?") {
		t.Fatalf("必须按账号取角色，实际 %s", sqlText)
	}
	if !strings.Contains(sqlText, "delete_flag = 0") {
		t.Fatalf("已删除的角色不能进登录器选角列表（否则 CDK 会兑换到一张已删除的角色上），实际 %s", sqlText)
	}
	if !strings.Contains(sqlText, "ORDER BY character_id") {
		t.Fatalf("排序口径不能改（登录器按它展示角色），实际 %s", sqlText)
	}
	assertReadOnlyQuery(t, sqlText)
}

// TestGameCharacterTargetSQLKeepsDeleteFlagInRow 发放目标查询必须把 delete_flag 取回来。
//
// 它**刻意不**把"已删除"写进 WHERE：那样"已删除"会与"不存在"一起退化成 sql.ErrNoRows，
// 两种状态再也分不出来。判据要在同一处逐格判出四类拒绝理由（见 judgeGameCharacterTarget），
// 所以 delete_flag 必须跟着行一起返回。
func TestGameCharacterTargetSQLKeepsDeleteFlagInRow(t *testing.T) {
	sqlText := strings.Join(strings.Fields(gameCharacterTargetSQL), " ")

	if !strings.Contains(sqlText, "FROM dnf_characters") {
		t.Fatalf("必须直查游戏库 dnf_characters，实际 %s", sqlText)
	}
	if !strings.Contains(sqlText, "account_id = ? AND character_id = ?") {
		t.Fatalf("必须一次点查同时锁定账号与角色（归属不能靠内存比对），实际 %s", sqlText)
	}
	if !strings.Contains(sqlText, "delete_flag") {
		t.Fatalf("delete_flag 必须随行取回、由判据逐格判断，实际 %s", sqlText)
	}
	assertReadOnlyQuery(t, sqlText)
}

// TestJudgeGameCharacterTarget 发放目标判据的表驱动用例：四类拒绝理由逐格钉死。
//
// 为什么值得为这个纯函数写表：它是"能不能把奖励发给这个角色"的**权威判据**，
// 兑换链路的每一个拒绝都押在它身上 —— 判据漏一格，烧掉的就是玩家手里的兑换码。
func TestJudgeGameCharacterTarget(t *testing.T) {
	const (
		accountID   = "dnf:bob"
		characterID = "1001"
	)
	usable := gameCharacterTargetRow{
		CharacterID: characterID,
		Name:        "角色甲",
		Level:       90,
		AccountID:   accountID,
		DeleteFlag:  sql.NullInt64{Int64: 0, Valid: true},
	}
	deleted := usable
	deleted.DeleteFlag = sql.NullInt64{Int64: 1, Valid: true}
	nullFlag := usable
	nullFlag.DeleteFlag = sql.NullInt64{}
	otherAccount := usable
	otherAccount.AccountID = "dnf:eve"
	otherCharacter := usable
	otherCharacter.CharacterID = "1002"

	cases := []struct {
		name  string
		row   gameCharacterTargetRow
		found bool
		want  bool
	}{
		{"存在、未删除、属于该账号（正常放行）", usable, true, true},
		{"已删除（delete_flag=1，软删除的行还在表里）", deleted, true, false},
		{"delete_flag 为 NULL（保守按已删除处理）", nullFlag, true, false},
		{"不属于该账号（A 的角色被 B 拿去兑换）", otherAccount, true, false},
		{"不存在（查不到行）", gameCharacterTargetRow{}, false, false},
		{"取回的行不是请求的那个角色", otherCharacter, true, false},
	}
	for _, testCase := range cases {
		got, ok := judgeGameCharacterTarget(testCase.row, testCase.found, accountID, characterID)
		if ok != testCase.want {
			t.Fatalf("%s：可用 = %v，期望 %v", testCase.name, ok, testCase.want)
		}
		if ok {
			if got.CharacterID != testCase.row.CharacterID || got.Name != testCase.row.Name || got.Level != testCase.row.Level {
				t.Fatalf("%s：返回的角色行 = %+v，期望 %+v", testCase.name, got, testCase.row)
			}
			continue
		}
		if got != (GameCharacter{}) {
			t.Fatalf("%s：拒绝时不能返回半点角色信息（调用方会拿它当发放目标），实际 %+v", testCase.name, got)
		}
	}
}

// TestGameCharacterActiveRejectsEmptyInput 空账号/空角色直接拒绝，连查询都不发。
//
// 故意用 nil *Store 调用：这条路径一旦碰了 s.db 就会 panic —— 等于把
// "空输入不能放行、也不能拿空 account_id 去查库"钉死在测试里。
func TestGameCharacterActiveRejectsEmptyInput(t *testing.T) {
	var empty *Store
	for _, testCase := range []struct{ accountID, characterID string }{
		{"", "1001"},
		{"dnf:bob", ""},
		{"   ", "1001"},
		{"dnf:bob", "   "},
	} {
		got, ok, err := empty.GameCharacterActive(context.Background(), testCase.accountID, testCase.characterID)
		if err != nil || ok || got != (GameCharacter{}) {
			t.Fatalf("GameCharacterActive(%q, %q) = (%+v, %v, %v)，期望零值/false/nil",
				testCase.accountID, testCase.characterID, got, ok, err)
		}
	}
}

// ---------- NormalizeRedeemCode：容忍"带中文标签的整段粘贴"（2026-10 玩家反馈） ----------

// TestNormalizeRedeemCodeStandardShapes 标准形状与老口径必须一字不差地通过。
//
// 这是**兼容性底线**：登录器、GUI 查流水（ListRedeemRedemptions）、作废
// （DisableRedeemCode）都依赖同一个函数，标准形状一旦被改坏，存量兑换码全线失效。
func TestNormalizeRedeemCodeStandardShapes(t *testing.T) {
	const want = "ABCD-EFGH-JKLM-NPQR-STUV"
	for _, raw := range []string{
		"ABCD-EFGH-JKLM-NPQR-STUV",
		"ABCDEFGHJKLMNPQRSTUV",
		"abcd-efgh-jklm-npqr-stuv",
		"  ABCD-EFGH-JKLM-NPQR-STUV  ",
		"ABCD EFGH JKLM NPQR STUV",
		"ABCD\r\nEFGH-JKLM-NPQR-STUV",
		"ＡＢＣＤ－ＥＦＧＨ－ＪＫＬＭ－ＮＰＱＲ－ＳＴＵＶ", // 全角码 + 全角横线
	} {
		got, err := NormalizeRedeemCode(raw)
		if err != nil {
			t.Fatalf("NormalizeRedeemCode(%q) 不应报错：%v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeRedeemCode(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

// TestNormalizeRedeemCodeExtractsFromLabels 玩家反馈的原始场景：发卡网一键复制带中文标签。
//
// 老实现的两处硬伤在这里同时被钉住：
//   - 中文一个字 3 字节，len(cleaned) != 20 → 一律失败；
//   - 只去半角空格与半角横线，中文标签留着不走 → 字符集断言也过不去。
func TestNormalizeRedeemCodeExtractsFromLabels(t *testing.T) {
	const want = "ABCD-EFGH-JKLM-NPQR-STUV"
	cases := map[string]string{
		"卡号+密码":     "卡号：ABCD-EFGH-JKLM-NPQR-STUV 密码：1234",
		"兑换码+尾巴":    "兑换码：ABCD-EFGH-JKLM-NPQR-STUV 请及时使用",
		"全角标签+全角码":  "兑换码：ＡＢＣＤ－ＥＦＧＨ－ＪＫＬＭ－ＮＰＱＲ－ＳＴＵＶ",
		"订单号+码":     "订单20260930 ABCD-EFGH-JKLM-NPQR-STUV",
		"订单号与码粘连":   "订单20260930ABCD-EFGH-JKLM-NPQR-STUV",
		"码+CRLF":    "ABCD-EFGH-JKLM-NPQR-STUV\r\n",
		"全角空格+NBSP": "　ABCD-EFGH-JKLM-NPQR-STUV\u00A0",
		"码前粘中文":     "兑换码ABCD-EFGH-JKLM-NPQR-STUV",
		"码后粘中文":     "ABCDEFGHJKLMNPQRSTUV请及时使用",
		"【】标签":      "【兑换码】ABCD-EFGH-JKLM-NPQR-STUV",
		"多行整段":      "感谢购买\n卡号：ABCD-EFGH-JKLM-NPQR-STUV\n密码：1234\n祝您游戏愉快",
	}
	for name, raw := range cases {
		got, err := NormalizeRedeemCode(raw)
		if err != nil {
			t.Fatalf("%s：NormalizeRedeemCode(%q) 应当能抠出兑换码，实际报错 %v", name, raw, err)
		}
		if got != want {
			t.Fatalf("%s：NormalizeRedeemCode(%q) = %q，期望 %q", name, raw, got, want)
		}
	}
}

// TestNormalizeRedeemCodeRejectsNoise 抠不出来就必须拒：字符集断言一点没放宽。
func TestNormalizeRedeemCodeRejectsNoise(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"没有兑换码只有中文标签",
		"ABCD-EFGH-JKLM-NPQR-STU",   // 19 位
		"ABCD-EFGH-JKLM-NPQR-STUVX", // 21 位
		"ABCD-EFGH-JKLM-NPQR-STUO",  // 含被排除的 O
		"ABCD-EFGH-JKLM-NPQR-STU0",  // 含被排除的 0
		"ABCD-EFGH-JKLM-NPQR-STU1",  // 含被排除的 1
		"ABCD-EFGH-JKLM-NPQR-STUI",  // 含被排除的 I
		"订单20260930 密码12345678",     // 只有数字噪声
		"ABCABCDEFGHJKLMNPQRSTUV",   // 前缀是合法字符 → 多个合法窗口，有歧义不猜
	} {
		if got, err := NormalizeRedeemCode(raw); err == nil {
			t.Fatalf("NormalizeRedeemCode(%q) 应当被拒，实际返回 %q", raw, got)
		}
	}
}

// TestNormalizeRedeemCodePrefersFirstOnTie 多个"看不出高低"的候选时取文本顺序第一个。
//
// 这是与登录器 CdkCodeExtractor.TryExtract 对齐的收敛规则：不猜内容，只按出现先后取，
// 免得两端（客户端抠一个、服务端抠另一个）给出不同的码。
func TestNormalizeRedeemCodePrefersFirstOnTie(t *testing.T) {
	got, err := NormalizeRedeemCode("ABCDEFGHJKLMNPQRSTUV ABCDEFGHJKLMNPQRSTVW")
	if err != nil {
		t.Fatalf("两个候选时应取第一个，实际报错 %v", err)
	}
	if got != "ABCD-EFGH-JKLM-NPQR-STUV" {
		t.Fatalf("取到的不是文本顺序第一个：%q", got)
	}
}

// TestNormalizeRedeemCodeIdempotentOnGeneratedCodes 生成端产出的码必须是不动点。
//
// generateRedeemCode 与 normalize 是"写入 / 读出"的两端：只要 normalize 把一张
// 刚生成的码改成别的形状，查流水与核销就会集体找不到那一行（而且这种坏法只在
// 线上才会暴露）。所以拿真实生成器跑一批，逐个喂回去比对。
func TestNormalizeRedeemCodeIdempotentOnGeneratedCodes(t *testing.T) {
	for i := 0; i < 200; i++ {
		generated, err := generateRedeemCode()
		if err != nil {
			t.Fatalf("generateRedeemCode 失败：%v", err)
		}
		got, err := NormalizeRedeemCode(generated)
		if err != nil {
			t.Fatalf("NormalizeRedeemCode(%q) 对生成端产出的码不应报错：%v", generated, err)
		}
		if got != generated {
			t.Fatalf("生成端的不动点被破坏：%q → %q", generated, got)
		}
		// 顺带钉住生成形状本身（5×4、24 字符）—— 它是存量码的契约，不能改。
		if len([]rune(generated)) != redeemCodeSegmentCount*redeemCodeSegmentLen+(redeemCodeSegmentCount-1) {
			t.Fatalf("生成的码形状不是 5×4：%q", generated)
		}
	}
}
