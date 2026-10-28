package dnfbridge

import "longheng.io/server/internal/modules/dnf/adventuregroup"

// 冒险团商店每个页签的"当前窗口进度"。
//
// 现场取证（2026-09-23）：商店点数 8 字节条目 = `{数量, 当前窗口进度}`，
// 而且条目**按位置**对应页签（不是按字段匹配）——
//
//   - 数量（第 0 个 dword）决定页签头部的 "X / 250 个"；
//   - 进度（第 2 个 dword）决定那条"获得1个结晶"进度条的百分比，
//     分母是 PVF 的 `[exp to get shop point]`（1500 万）；
//     成长胶囊那一格的分母是 `[capsule max exp]`。
//
// 证据：往第 0 条（骁勇结晶）的第 2 个 dword 写 1,500,000，商店里那条进度条
// 正好显示 10%；同一轮往报文 raw+24 / raw+32 写的探针值完全不显示，
// 说明进度既不在报文头部也不在别处，就在这个字段里。
//
// 以前这里写的是"分类号"（0/1/2/3/4），于是骁勇结晶那一格恒为 0 → 恒 0.00%。
func currentAdventureInfoShopPointProgress(
	index int,
	projection adventuregroup.Projection,
) uint32 {
	switch index {
	case int(adventuregroup.ShopPointBrave):
		// 距下一个骁勇结晶还差多少经验（PVF 1500 万一个，余数即进度）。
		return clampCurrentAdventureProgress(projection.Runtime.BraveExperience)
	case currentAdventureInfoCapsuleProgressEntry:
		// 成长胶囊页签（4 个页签里的第 4 个）：当前这一格的进度。
		// 胶囊可以攒 10 格，所以必须取"累计值 mod 单格"，否则攒过 100%
		// 之后客户端会算出 100% 以上、看起来卡住不动。
		gauge := projection.CapsuleGauge
		if gauge == 0 {
			return 0
		}
		return clampCurrentAdventureProgress(projection.Runtime.GrowthExperience % gauge)
	default:
		// 荣耀结晶（远征队奖励）与纯粹结晶（帝国竞技场）没有经验窗口。
		return 0
	}
}

// currentAdventureInfoCapsuleClaimCount 返回成长胶囊页签"可获得的经验值"那一栏
// 要发的值 = 现在可以领取几次（每次 = 10% 档 = 1,000,000 点经验值）。
//
// 取证（2026-09-23）：
//   - 客户端解析 sub_C7DAF0 时把报文 raw+7416 存进 manager+0x310，
//     0x1DE1F30 就是 `mov eax,[ecx+0x310]; ret`；
//   - 0x13F0FA0 拿到这个值后 `for (i = 0; i < count; i++)` 逐个拼字符串建列表
//     —— 它是**次数**，不是经验值总量。第一版当成经验值发（1,100 万）会让
//     客户端建上千万个字符串而卡死，所以这里只用 adventuregroup 算出来的、
//     上限为"10 格 × 每格 10 次 = 100"的次数；
//   - 同一个 manager 上的 0x308 是"领取请求已发出"标志，0xC79730 发 op1491 时
//     **不带任何字段**，和服务端的空 body 校验一致；
//   - 页签上那句说明文字自己写着"累积10%以上时可以领取经验值"、
//     "经验值按照10% : 1,000,000点的比例换算"、"角色Lv : Lv.50 ~ 86"。
func currentAdventureInfoCapsuleClaimCount(projection adventuregroup.Projection) uint32 {
	return projection.CapsuleClaimCount
}

// currentAdventureInfoCapsuleProgressEntry 是成长胶囊页签在商店点数数组里的
// 位置：数组 5 条 = 3 个结晶分类 + 成长胶囊 + 1 条备用，页签顺序与 XUI 一致。
const currentAdventureInfoCapsuleProgressEntry = 3

func clampCurrentAdventureProgress(value uint64) uint32 {
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}
