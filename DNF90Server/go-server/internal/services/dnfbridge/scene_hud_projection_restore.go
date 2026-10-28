package dnfbridge

import (
	"context"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

// restoreCurrentSceneHudProjections 重放"挂在 HUD 上、随客户端场景重建被清空"的持久投影。
//
// 背景（2026-10-08 现场）：客户端的 buff 栏、冷却表都是**进程内内存**，任何一次重建场景
// （登录选角、回城、**区域切换**）都会先把它清空，服务端必须重放。此前只有两条路径补过：
//
//	initial_town_route_packets.go —— 登录选角路线（含 guild state projection）
//	initial_town_route_packets.go —— 副本回城路线（exp / 公会 buff / 属性药水 / 冷却）
//
// 而**区域切换**（客户端 op36 → op22/op23/op356/op24，例如进赛利亚房间、回广场、店铺区）
// 同样重建场景，却什么都没补。实测时序（conn game-000125）：
//
//	04:02:43.083  op24  回城路线 —— 紧跟 op487 重发公会 buff 167/169 ✓
//	04:02:45.625  op24  区域切换 —— 没有任何 buff 重发 ✗
//	→ 公会属性（限时 +60 四维）就这样消失，小退重上才回来（登录只有一个 op24）。
//
// 所以凡是"我们自己发的场景切换"都要在 op24 之后调用本函数。调用方必须已经提交了场景
// 切换、并且客户端此刻已经有可用的 actor/scene（与 writeCurrentGuildBuffProjection 的
// 前置条件一致）。
func (s *Service) restoreCurrentSceneHudProjections(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if err := s.sendCurrentExperienceConsumableBuffProjection(session, source); err != nil {
		return fmt.Errorf("restore experience consumable HUD: %w", err)
	}
	// 公会 BUFF 挂在 HUD 而不是 actor 上，场景重建即被清空。两个公会属性
	// （限时 167 / 永久 169）都在这里重放，重放顺序由 writeCurrentGuildBuffProjection
	// 保证"先全部移除、再按登录顺序全部加回"。
	if err := s.sendCurrentGuildBuffProjection(session, source); err != nil {
		return fmt.Errorf("restore guild buff HUD: %w", err)
	}
	if err := s.sendCurrentStatChangePotionBuffProjection(session, source); err != nil {
		return fmt.Errorf("restore stat change potion HUD: %w", err)
	}
	if err := s.sendCurrentItemCooltimeProjection(session, source); err != nil {
		return fmt.Errorf("restore item cooltime list: %w", err)
	}
	// 装备实例的原始字节（勋章守护珠 raw socket 字、称号 raw 随机附魔）不在 mode1/mode3
	// 的创建行里，场景重建后必须单独补，否则勋章宝珠图标与属性一起消失。
	s.restoreCurrentSceneRawEquipmentRows(session, source)
	return nil
}

// restoreCurrentSceneWeddingRoomState 重放婚房状态（op1011 家具与摆放 + op1008 房间状态，含情侣树）。
//
// 客户端的"婚房属性"显示在结婚戒指上，**数据源就是这条状态**：服务器侧没有任何婚房属性
// 聚合，decoration/coupleroomiteminfo.etc（家具）与 decoration/coupletree.etc（情侣树）
// 里也只有装饰、生长与奖励字段，属性是客户端按这份房间数据自己算的。
//
// 它此前只在**真正踏进婚房**那一刻发过一次（town_move.go 与 publishCurrentTownPresence
// 里 townID==婚房 的分支），于是：
//  1. 每次上线不去婚房 → 戒指不显示婚房属性；
//  2. 进图（场景重建）→ 客户端那份数据被清掉且再无来源 → 既不显示也不生效；
//  3. 只有"小退 + 再进婚房"才能重新拿到。
//
// 已婚角色在任何场景都需要它，所以在每次"场景完成/重建"的边界补发一次（失败只记日志）。
func (s *Service) restoreCurrentSceneWeddingRoomState(session *gameSession, characterID uint16, source string) {
	if s == nil || session == nil || characterID == 0 {
		return
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	c, err := s.weddingCharacter(ctx, identity)
	if err != nil {
		return
	}
	state, stateErr := wedding.Read(c)
	if stateErr != nil || state.Status != 4 {
		return // 未婚角色没有婚房状态可发
	}
	if err := s.sendWeddingRoomState(session, c); err != nil {
		s.logGameEvent(session, "game-wedding-room-scene-restore-failed",
			"source", source, "char_id", characterID, "error", err)
		return
	}
	s.logGameEvent(session, "game-wedding-room-scene-restore",
		"source", source, "char_id", characterID,
		"msg_ids", []uint16{1011, 1008},
		"reason", "marriage_ring_room_attribute_has_no_other_source")
}

var _ = dnfrepo.CharacterRecord{}
