package dnfbridge

import (
	"context"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 深渊派对史诗掉落率 = GM 全局倍率 × 角色身上的城镇增益药剂窗口。
//
// 服主 2026-10-03 的原话就是这条口径：
//
//	"在服务端看来 无非就是决定是否出史诗前 如果使用了药剂并且在持续时间内
//	 把决定出史诗的可能性翻一倍"
//
// 所以这里**只做一件事**：把 currentGMHellEpicDropRatePercent() 的结果按药剂倍率放大。
// 决定出史诗的地方仍然只有 dungeon_hell_party_drop.go 里那一次 gmAdjustHellEpicRarity
// 调用，本函数不新增任何掷骰、也不改稀有度权重。
//
// 为什么要在掉落热路径上读角色记录：药剂是"城镇使用、进图生效"的（PVF
// `[action usable place] [village] [seria room]`），而 runtimeDungeonState.Character
// 是进图那一刻的角色快照，正好包含刚落的增益窗口；掉落发生时再回读一次数据库既没必要，
// 也不会让同一次进图的前后掉落用上不同的值。
//
// 窗口是**角色级**的（存在 Character.Stats，不是账号）：谁用的药，就只对谁生效；
// 重登、换频道都不影响它 —— 换频道会新建会话并重跑一次"登录/回城"的恢复投影，
// 窗口本身一直躺在角色记录里。过期的窗口由 statbuff.ActiveEpicDropRate 自己判掉
// （它按 now 比较到期时间），所以"10 分钟到了"这件事不需要调用方再管。
func (s *Service) currentHellPartyEpicDropRatePercent(character dnfrepo.CharacterRecord) int {
	percent := s.currentGMHellEpicDropRatePercent()
	effect, active := statbuff.ActiveEpicDropRate(character, s.gameplayNow())
	if !active {
		return clampHellEpicDropRatePercent(percent)
	}
	multiplier := s.currentEpicDropRatePotionMultiplierPercent(int(effect.MultiplierPercent))
	return clampHellEpicDropRatePercent(scaleHellEpicDropRateByPotion(percent, multiplier))
}

// currentEpicDropRatePotionMultiplierPercent 返回本次实际使用的药剂倍数。
//
// 正常情况下就是 PVF 表里查出来的值（1136 ⇒ 200 = 2 倍）。GM 把"临时测试倍率"拧成
// 非 0 时改用它 —— 服主要肉眼确认"用了药剂史诗真的掉得更多"，2 倍是看不出来的。
//
// ⚠ 它**只替换倍数**：调用方仍然必须先通过 statbuff.ActiveEpicDropRate（窗口没过期），
// 所以这个旋钮测的仍然是"药剂到底有没有用"，不是"GM 能不能硬调掉落"。
// 读设置失败时退回 PVF 倍数（fail-safe：宁可少给加成，也不给一个来路不明的倍数）。
func (s *Service) currentEpicDropRatePotionMultiplierPercent(pvfMultiplierPercent int) int {
	settings, err := s.GMGameplaySettings(context.Background())
	if err != nil || settings.EpicDropRatePotionTestMultiplierPercent <= 0 {
		return pvfMultiplierPercent
	}
	return settings.EpicDropRatePotionTestMultiplierPercent
}

// scaleHellEpicDropRateByPotion 是纯函数版本：percent × multiplier / 100。
func scaleHellEpicDropRateByPotion(percent int, multiplierPercent int) int {
	if percent <= 0 || multiplierPercent <= 0 {
		return 0
	}
	return percent * multiplierPercent / 100
}

func clampHellEpicDropRatePercent(percent int) int {
	if percent > gmHellEpicDropRateMaximumPercent {
		return gmHellEpicDropRateMaximumPercent
	}
	if percent < gmHellEpicDropRateMinimumPercent {
		return gmHellEpicDropRateMinimumPercent
	}
	return percent
}
