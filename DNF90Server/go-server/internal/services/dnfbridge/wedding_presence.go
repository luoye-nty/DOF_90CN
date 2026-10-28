package dnfbridge

import (
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

// 婚姻场景的实例作用域
// ---------------------------------------------------------------------------
// 上游把这两段判定内联写在 publishCurrentTownPresence 里，本工作区的形状不同：
// publishCurrentTownPresence 的 instanceKey 由调用方决定，函数内部只做
// "public / private-room" 二选一。为了让婚姻场景在两种形状下语义一致，这里把
// 判定收敛成一个解析器，由 publishCurrentTownPresence 在分类之前调用。
//
// 两个婚姻场景都是"按夫妻对分实例的公共场景"，不是"不可见的私密场景"：
//   - 教堂默认房（城镇 39，区域 8..38）：已预约（wedding_state 2/3）的夫妻都能进，
//     实例键是 pairID；weddingCeremonyPeers 正是用同一个键去查同屏宾客。
//   - 婚房（城镇 101）：仪式完成（wedding_state 4）后可用，实例键同为 pairID。
//
// 返回值语义：
//   - scoped=false：这不是婚姻场景，调用方继续走原有分类逻辑；
//   - scoped=true, ok=true：是婚姻场景且有权进入，instanceKey 已给出；
//   - scoped=true, ok=false：是婚姻场景但该角色无权进入（未预约/未结婚/频道不符）。
//     调用方必须放弃发布，否则会把无权角色放进别人的夫妻实例。

// weddingChurchTownID 是教堂所在的城镇号，weddingChurchAreaMin/Max 是默认房区间。
const (
	weddingChurchTownID  = 39
	weddingChurchAreaMin = 8
	weddingChurchAreaMax = 38
)

// currentWeddingTownInstanceKey 解析婚姻场景实例键，见上方说明。
func (s *Service) currentWeddingTownInstanceKey(
	character dnfrepo.CharacterRecord, townID, areaID byte, channelID uint16,
) (uint32, bool, bool) {
	switch {
	case townID == weddingChurchTownID && areaID >= weddingChurchAreaMin && areaID <= weddingChurchAreaMax:
		key, admitted := weddingTownInstance(character, areaID, channelID)
		return key, true, admitted
	case townID == currentWeddingRoomTownID:
		state, err := wedding.Read(character)
		if err != nil || state.Status != 4 {
			return 0, true, false
		}
		return state.PairID, true, true
	default:
		return 0, false, true
	}
}
