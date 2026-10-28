package dnfbridge

import (
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
)

// currentWeddingRoomTownID 是婚房城镇号（PVF：town/coupleroom.twn）。
const currentWeddingRoomTownID = 101

// 本文件在工作区的等价实现说明
// ---------------------------------------------------------------------------
// 上游自己遍历 dnftown.MovableArea 矩形、靠 Params 认出"通往 101/0 的门"再算落点。
// 本工作区的 MovableArea 只有 X/Y/Width/Height，没有 Params，所以照搬不了。
// 工作区的既有做法（SeriaRoomReturn）是**在装载期**把门边安全落点推导好，
// 本文件沿用同一套路：town/wedding_room_return.go 在装载期算出
// Area.WeddingRoomReturn，运行时直接取用。
//
// 为什么必须落在赛丽亚房间(38/1)而不是城镇居民区(38/0)：婚房是从赛丽亚房间那扇
// 门进去的，原生就是回到赛丽亚房间；而且 WeddingRoomReturn 正是挂在 38/1 上
// （那扇 `49 224 34 52 101 0` 的门所在的地图）。
//
// 落点语义与上游一致：必须落在传送门触发矩形之外，否则客户端的重叠 latch 保持
// 武装，会把角色立刻送回婚房。townPortalReturnPoint 还额外要求落点位于
// [virtual movable area] 内，比上游更严。

// normalizeCurrentWeddingRoomExit 把"从婚房(101/0)走左门出来"的原生 op36
// 归一化为"回到赛丽亚房间(38/1)那扇婚房门旁边的安全落点"。
//
// 上游注释记录的 EXE 行为：0x3410D33 的 -101 分支用 0x223CC20 记住的入口房间，
// 或在该记忆为空时回退 Underfoot 2/5；两条路径都以 (300,200)、方向 5 调用
// 0x3406950。所以这里只认这个精确形状；其它传送/驿站选择继续走原有路线。
func (s *Service) normalizeCurrentWeddingRoomExit(
	session *gameSession, character dnfrepo.CharacterRecord, request currentTownSetUserAreaRequest,
) (currentTownSetUserAreaRequest, bool, error) {
	if character.Stats["town_id"] != currentWeddingRoomTownID || character.Stats["area_id"] != 0 ||
		!request.LooksLikeTownCrossTownPortalSelection(currentWeddingRoomTownID) ||
		request.PositionX != 300 || request.PositionY != 200 || request.Direction != 5 {
		return request, false, nil
	}
	// 场景快照对不上时**放行原始请求**，不报错。
	//
	// 教训（2026-10-04 真机）：这里早先返回 error，调用方 handleTownSetUserArea
	// 会直接中止整个移动请求 —— 一旦装载期没能推导出落点（例如地图只有 `(r)`
	// 修订版、原图缺失），玩家就会被**永久困在婚房里出不去**。
	// 把玩家困住远比落点不精确糟糕：客户端自己请求的就是赛丽亚房间的
	// (300,200)，那本身就是一个合法的安全点。
	if snapshot := session.townPositionSnapshot; snapshot.CharacterID != 0 &&
		(snapshot.CharacterID != session.selectedCharacterID || snapshot.TownID != currentWeddingRoomTownID || snapshot.AreaID != 0) {
		return request, false, nil
	}
	area, found := s.townArea(newCharacterInitialTownID, newCharacterInitialAreaID)
	if !found {
		return request, false, nil
	}
	point, ok := currentWeddingRoomExitLandingPoint(area)
	if !ok {
		return request, false, nil
	}
	request.TownID = newCharacterInitialTownID
	request.AreaID = byte(area.ID)
	request.PositionX = int16(point.X)
	request.PositionY = int16(point.Y)
	request.Direction = newCharacterInitialDirection
	return request, true, nil
}

// currentWeddingRoomExitLandingPoint 挑一个"门外的安全落点"。
//
// 优先级：装载期推导出的 WeddingRoomReturn（婚房门旁，原生口径）→
// 同区域的 SeriaRoomReturn → 该地图自己的 [gate] 锚点。
// 三者都没有时返回 false，调用方会放行客户端的原始请求（宁可落点一般，
// 也绝不把玩家困在婚房里）。
func currentWeddingRoomExitLandingPoint(area dnftown.Area) (dnftown.Gate, bool) {
	if area.ID < 0 || area.ID > int64(^byte(0)) {
		return dnftown.Gate{}, false
	}
	for _, candidate := range []*dnftown.Gate{area.WeddingRoomReturn, area.SeriaRoomReturn, area.Gate} {
		if candidate == nil {
			continue
		}
		if candidate.X < 0 || candidate.X > int64(^uint16(0)) ||
			candidate.Y < 0 || candidate.Y > int64(^uint16(0)) {
			continue
		}
		return *candidate, true
	}
	return dnftown.Gate{}, false
}

// currentWeddingPublicReturnOrigin 只修复"被婚房污染过的回城点"。
//
// 注意它与上面那个函数取的是**不同**的锚点，别合并：
//   - normalizeCurrentWeddingRoomExit 要送玩家回赛丽亚房间(38/1)，所以用 38/1 的
//     WeddingRoomReturn（婚房门旁）。
//   - 本函数服务的是 op1425"回到上个城镇"。上游从当前城镇那扇"指向赛丽亚房间的
//     零尺寸反向传送门"取公共到达锚点，也就是**城镇居民区那一侧**(38/0)，所以用
//     townSeriaRoomReturnArea(38) 的 SeriaRoomReturn。用错一侧会把玩家塞进赛丽亚
//     房间而不是他要回的城镇。
func (s *Service) currentWeddingPublicReturnOrigin(characterID uint16) (currentTownPositionSnapshot, bool) {
	if s == nil || characterID == 0 {
		return currentTownPositionSnapshot{}, false
	}
	area, found := s.townSeriaRoomReturnArea(newCharacterInitialTownID)
	if !found || area.SeriaRoomReturn == nil ||
		area.ID < 0 || area.ID > int64(^byte(0)) ||
		area.SeriaRoomReturn.X < 0 || area.SeriaRoomReturn.X > int64(^uint16(0)) ||
		area.SeriaRoomReturn.Y < 0 || area.SeriaRoomReturn.Y > int64(^uint16(0)) {
		return currentTownPositionSnapshot{}, false
	}
	return currentTownPositionSnapshot{
		CharacterID:   characterID,
		TownID:        byte(newCharacterInitialTownID),
		AreaID:        byte(area.ID),
		PositionX:     uint16(area.SeriaRoomReturn.X),
		PositionY:     uint16(area.SeriaRoomReturn.Y),
		MovementCode:  newCharacterInitialDirection,
		PositionValid: true,
	}, true
}
