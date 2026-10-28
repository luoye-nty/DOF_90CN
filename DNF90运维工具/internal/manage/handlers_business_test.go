//go:build paid

package manage

// reward_json 生成端校验测试：这张表决定了"管理工具能不能建出这张 CDK"。
//
// 校验太松 → 玩家兑换时发奖阶段才失败（白扣一次兑换次数）；
// 校验太严 → 合法的黑钻/点券/物品组合被拒，运营以为功能坏了。

import (
	"strings"
	"testing"
)

func TestValidateRedeemRewardJSONAccepts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"仅物品", `[{"item_id":1001,"count":2,"bind":true}]`},
		{"仅点券", `[{"cera":5000}]`},
		{"仅黑钻", `[{"black_diamond_days":30}]`},
		{"物品+点券+黑钻", `[{"item_id":1001,"count":1},{"cera":3000},{"black_diamond_days":30}]`},
		{"多行黑钻合计在上限内", `[{"black_diamond_days":3000},{"black_diamond_days":650}]`},
		{"黑钻恰好上限", `[{"black_diamond_days":3650}]`},
		{"点券恰好上限", `[{"cera":2147483647}]`},
	}
	for _, testCase := range cases {
		if err := validateRedeemRewardJSON(testCase.raw); err != nil {
			t.Fatalf("%s：应当通过，却报 %v", testCase.name, err)
		}
	}
}

func TestValidateRedeemRewardJSONRejects(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{"空", "", "不能为空"},
		{"null", "null", "不能为空"},
		{"非数组", `{"item_id":1}`, "不是合法 JSON 数组"},
		{"空对象行", `[{}]`, "没有任何可发放的奖励"},
		{"黑钻为负", `[{"black_diamond_days":-30}]`, "没有任何可发放的奖励"},
		{"黑钻超上限", `[{"black_diamond_days":3651}]`, "黑钻总天数"},
		{"多行黑钻合计超上限", `[{"black_diamond_days":3000},{"black_diamond_days":1000}]`, "黑钻总天数"},
		{"点券超上限", `[{"cera":2147483647},{"cera":1}]`, "点券总额"},
	}
	for _, testCase := range cases {
		err := validateRedeemRewardJSON(testCase.raw)
		if err == nil {
			t.Fatalf("%s：应当报错，却通过了", testCase.name)
		}
		if !strings.Contains(err.Error(), testCase.wantSub) {
			t.Fatalf("%s：错误文案 %q 不含 %q", testCase.name, err.Error(), testCase.wantSub)
		}
	}
}

// TestRedeemBlackDiamondLimitMatchesServer：生成端与发奖端、服务端的天数上限必须一致，
// 否则会出现"能生成但兑换必失败"的 CDK。
func TestRedeemBlackDiamondLimitMatchesServer(t *testing.T) {
	if redeemMaxBlackDiamondDays != 3650 {
		t.Fatalf("redeemMaxBlackDiamondDays = %d, want 3650（服务端 GMActivateBlackDiamond 上限）", redeemMaxBlackDiamondDays)
	}
}
