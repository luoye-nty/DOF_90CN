package dnfbridge

import (
	"math"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 城镇增益药剂的"地下城通关经验 +N%"加成。
//
// 与别的加成（成长契约 +20%、黑钻、签到弹珠…）同一口径：先算出基础通关经验 gain，
// 再按窗口的百分比算出本次加成，最后由调用方累加进 totalGain。
//
// 为什么必须服务端算：通关经验是服务端发的，客户端算不出来 —— 所以这一族（赛丽亚的
// 幸运经验药剂 10008222，三档 50/100/200）与掉落率/强化成功率一样，要把窗口记在角色
// 记录里，到点由 statbuff.ActiveRate 自己判掉（过期即 0，绝不 fail-open）。
func (s *Service) currentStatChangePotionClearExperienceBonus(
	character dnfrepo.CharacterRecord,
	base uint32,
) uint32 {
	if s == nil || base == 0 {
		return 0
	}
	window, active := statbuff.ActiveRate(character, statbuff.KindDungeonClearExp, s.gameplayNow())
	if !active || window.Percent <= 0 {
		return 0
	}
	bonus := uint64(base) * uint64(window.Percent) / 100
	if bonus > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(bonus)
}
