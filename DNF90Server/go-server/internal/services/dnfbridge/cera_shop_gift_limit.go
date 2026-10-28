package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 每日赠送点券额度。
//
// 文案与数值都来自客户端自己的字符串表：**70107**（= op136 结果码 0xA3）
// 「每个账号每天最多可以赠送10万点券的礼物」。所以这是**账号维度、按天**的额度。
//
// 现场 2026-09-27 17:46：同一个账号连送 5 个 39900 的礼包（合计 199500 点券）没有任何
// 拦截 —— 这条规则当时根本没实现（只是记在"已知未做"里）。现在补上：
//
//	op136：今天额度已用完 → 直接回 0xA3，客户端弹那句话（连收件人都不用查）
//	op64 ：这一单会让今天的累计**超过** 10 万 → 回 11「超出赠送金额上限」，
//	       并且在**扣钱之前**整笔拒绝（额度记账和扣款在同一次结账事务里）
const currentCeraShopGiftDailyCeraLimit uint32 = 100000

// currentCeraShopGiftDailyPolicy 是"赠送点券额度"这条计数器的策略。
//
// 直接复用 purchaselimit 的计数原语：它本来就是"账号/角色维度 + 周期窗口 + 落在
// 账号 Metadata（或角色 Stats）"的现成实现，跨天归零靠窗口键完成，不需要我们自己
// 判断日期、也不用新增表或列。
//
// ItemID 填 1 只是占位（Policy.Valid 不接受 0）；KeyTag 把这条计数放进独立命名空间，
// 绝不会和 PVF 道具限购 / 商城商品限购撞键。
const currentCeraShopGiftDailyKeyTag = "gift_cera:"

var errCurrentCeraShopGiftDailyLimit = errors.New("dnf cera shop gift daily cera allowance is exhausted")

// currentCeraShopGiftFailureCodeProbePath 是"赠送失败码"的临时试码文件。
//
// 为什么要有它：客户端到底用哪张表渲染 op64 的失败码，现在有两种互相矛盾的证据 ——
//
//	实测：回 code 11，客户端画的是字符串 258「错误 : 点券不足」 ⇒ 表像 `247 + code`；
//	字符串表：21980 =「超出赠送金额上限」正好等于 `21969 + 11` ⇒ 表像 `21969 + code`。
//
// 两张表都真实存在（21969 那段确实是商城文案，但反汇编显示它只被一个 `arg2 == 13` 的
// 充值/商城页对话框引用，不是按 code 索引的购买回执表）。代码里翻不出结论，就**用真机试**：
// 把想试的码写成一行十进制放进这个文件，服务端下一次发赠送失败包时就用它 ——
// 不用重新编译、也不用重启。
//
// 文件不存在 / 内容非法 ⇒ 用代码里映射出来的正常码。定位完就把这个开关删掉。
//
// 两个路径都试：服务端的 cwd 是 `runtime`（启动命令是 `-config configs/dnfbridge.toml`），
// 但换一种启动方式 cwd 可能就是包根目录了。
var currentCeraShopGiftFailureCodeProbePaths = []string{
	`configs\dnf\gift_failure_code.txt`,
	`runtime\configs\dnf\gift_failure_code.txt`,
}

// currentCeraShopGiftFailureCodeProbe 读取临时指定的失败码。
//
// 文件允许写注释（以 # 开头）和空行 —— 取**第一个**能解析成 0..255 的数字行。
func currentCeraShopGiftFailureCodeProbe() (byte, bool) {
	for _, path := range currentCeraShopGiftFailureCodeProbePaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			value, err := strconv.ParseUint(line, 10, 8)
			if err != nil {
				continue
			}
			return byte(value), true
		}
	}
	return 0, false
}

func currentCeraShopGiftDailyPolicy() purchaselimit.Policy {
	return purchaselimit.Policy{
		ItemID:  1,
		Scope:   purchaselimit.ScopeAccount,
		Period:  purchaselimit.PeriodDaily,
		Limit:   currentCeraShopGiftDailyCeraLimit,
		Section: "cera_shop_gift_daily_cera",
		KeyTag:  currentCeraShopGiftDailyKeyTag,
	}
}

// currentCeraShopGiftDailyRemaining 返回这个账号今天还能赠送多少点券（只读）。
func currentCeraShopGiftDailyRemaining(account *dnfrepo.AccountRecord, now time.Time) (int64, bool) {
	if account == nil {
		return 0, false
	}
	state := purchaselimit.ReadAccount(account.Metadata, currentCeraShopGiftDailyPolicy(), now)
	return int64(state.Remaining), true
}

// consumeCurrentCeraShopGiftDailyCera 在结账事务里扣减今天的赠送额度。
//
// 超额返回 errCurrentCeraShopGiftDailyLimit；调用方把它整笔回滚，所以**一分钱都不会扣**。
// 记账必须和扣款在同一个事务里：分开写会出现"额度扣了但钱没扣"或反过来。
func consumeCurrentCeraShopGiftDailyCera(account *dnfrepo.AccountRecord, cera int64, now time.Time) error {
	if account == nil {
		return errCurrentCeraShopGiftSenderMissing
	}
	if cera <= 0 {
		// 免费赠送（0 点券）不占额度，也不该因为"额度为 0"被拦。
		return nil
	}
	if cera > math.MaxUint32 {
		return fmt.Errorf("%w: amount %d is out of range", errCurrentCeraShopGiftDailyLimit, cera)
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string, 2)
	}
	if err := purchaselimit.ConsumeAccount(account.Metadata, currentCeraShopGiftDailyPolicy(), uint32(cera), now); err != nil {
		return fmt.Errorf("%w: %v", errCurrentCeraShopGiftDailyLimit, err)
	}
	return nil
}
