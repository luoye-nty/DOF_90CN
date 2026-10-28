package town

import (
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// weddingRoomTownID 是婚房城镇号（PVF：town/coupleroom.twn）。
const weddingRoomTownID int64 = 101

// weddingRoomPortalTrigger 找出本区域地图里"通往婚房(101/0)"的 [town movable
// area] 行。
//
// 复用 seriaRoomPortalTrigger 的两段式解析（扁平六整数形式 + 嵌套
// [movable area]/[area]/[move town] 形式），只把目标城镇换成 101。
//
// 存在的意义：从婚房走左门出来时，客户端会以 (300,200)/方向5 请求落到赛丽亚
// 房间；如果落点正好压在这扇门的触发矩形里，客户端重叠 latch 不解除，角色会被
// 立刻送回婚房。所以在装载期就把"门外最近的安全落点"算好（WeddingRoomReturn），
// 运行时直接用，避免在 bridge 侧复刻第二套未被验证的几何。
func weddingRoomPortalTrigger(doc *dnfpvf.Document) (townMovableArea, bool) {
	if doc == nil {
		return townMovableArea{}, false
	}
	if values, ok := sectionInts(doc, "town movable area"); ok {
		for offset := 0; offset+5 < len(values); offset += 6 {
			candidate := townMovableArea{
				x:          values[offset],
				y:          values[offset+1],
				width:      values[offset+2],
				height:     values[offset+3],
				targetTown: values[offset+4],
				targetArea: values[offset+5],
			}
			if candidate.width > 0 && candidate.height > 0 &&
				candidate.targetTown == weddingRoomTownID && candidate.targetArea == 0 {
				return candidate, true
			}
		}
	}

	insideMovableArea := false
	candidate := townMovableArea{}
	hasArea := false
	for idx, token := range doc.Tokens {
		if token.Kind != dnfpvf.TokenSection {
			continue
		}
		switch sectionName(token.Value) {
		case "movable area":
			insideMovableArea = true
			candidate = townMovableArea{}
			hasArea = false
		case "/movable area":
			insideMovableArea = false
			candidate = townMovableArea{}
			hasArea = false
		case "area":
			if !insideMovableArea {
				continue
			}
			values := followingInts(doc.Tokens, idx+1, 4)
			if len(values) == 4 && values[2] > 0 && values[3] > 0 {
				candidate.x = values[0]
				candidate.y = values[1]
				candidate.width = values[2]
				candidate.height = values[3]
				hasArea = true
			}
		case "move town":
			if !insideMovableArea || !hasArea {
				continue
			}
			values := followingInts(doc.Tokens, idx+1, 2)
			if len(values) == 2 && values[0] == weddingRoomTownID && values[1] == 0 {
				candidate.targetTown = values[0]
				candidate.targetArea = values[1]
				return candidate, true
			}
		}
	}
	return townMovableArea{}, false
}

// weddingRoomReturnPoint 复刻当前 EXE 对这道门的到达算法：保留传送门的垂直中心
// （原生 0x17CA8F0 的算法），并把角色放在门矩形**右边缘再往外半个门宽**处，也就是
// 朝房间内部那一侧。以 `[49,224,34,52 -> 101/0]` 为例得到 (100,250)。
//
// 与 townPortalReturnPoint 的分工：那个函数在四个方向上找"门外 + 可走"的最近点，
// 是通用兜底；婚房这道门有原生既定的落点，所以先用原生公式，只有当该点确实不在
// [virtual movable area] 内（或地图没声明该区域）时才退回通用兜底。这样既对齐
// 原生行为，又不会把角色放进墙里。
func weddingRoomReturnPoint(trigger townMovableArea, virtual []int64) (Gate, bool) {
	if trigger.width <= 0 || trigger.height <= 0 {
		return Gate{}, false
	}
	primary := Gate{
		X: trigger.x + trigger.width + trigger.width/2,
		Y: trigger.y + trigger.height/2,
	}
	if len(virtual) < 4 || pointInsideAnyMovableArea(primary, virtual) {
		return primary, true
	}
	return townPortalReturnPoint(trigger, virtual)
}
