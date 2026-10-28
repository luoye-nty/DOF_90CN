package dnfbridge

// 副本里"技能消耗的无色小晶块"埋点。
//
// 需求背景：无色小晶块（item 3037）是账号共享材料，只有部分技能会消耗它；
// "技能放了很多但一个无色都没掉"是判断外挂的重要依据，所以要在副本记录里如实计数。
//
// 口径（已与需求确认）：
//   - **只统计技能消耗**：客户端释放这类技能时发的是 class-1/op18 DELETE_ITEM，
//     这里解析该请求、累加 item_id=3037 的数量；
//   - **晶体契约不计入**：地下城里每 30 秒自动扣的那 1 个无色走的是
//     dungeon_crystal_contract.go 的 ConsumeSelected，根本不经过 op18，自然落不到这里；
//   - 只在**真实副本运行中**计数（调用点在练习场/修炼道场的提前返回之后），
//     城镇里挪仓库/丢弃无色不会误计 —— 副本内也没有仓库操作。
//
// 安全边界：这是**纯观测**，绝不改动回包、背包或任何玩法状态；
// 解析失败就当作没看见（埋点不能把主流程带崩）。

import (
	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
)

// currentColorlessCubeItemID 无色小晶块的 item id（账号晶体仓固定格 0:358 的那个材料）。
const currentColorlessCubeItemID int32 = 3037

// noteCurrentColorlessSpendFromCommand 累计本次副本运行里技能消耗的无色数量。
//
// 为什么遍历全部 entry 而不是只取第一条：既有药水配对逻辑只关心 Entries[0]，
// 但技能扣无色可能一次报文带多条（不同格子/批量），只取第一条会漏计 —— 计数必须全算。
func (s *Service) noteCurrentColorlessSpendFromCommand(session *gameSession, body []byte) {
	if s == nil || session == nil || len(body) == 0 {
		return
	}
	parsed, err := dnfinventory.DecodeDeleteRequest(body)
	if err != nil {
		return
	}

	spent := int64(0)
	for _, entry := range parsed.Entries {
		if entry.ItemID != currentColorlessCubeItemID {
			continue
		}
		count := int64(entry.DeleteCount)
		if count <= 0 {
			// 单条格式（非扩展）不带 DeleteCount：按"删了 1 个"记，绝不当成 0 漏掉。
			count = 1
		}
		spent += count
	}
	if spent == 0 {
		return
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime != nil {
		session.dungeon.runtime.colorlessSpent += spent
	}
	session.dungeon.mu.Unlock()
}
