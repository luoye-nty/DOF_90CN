package dnfbridge

import (
	"context"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// grantCurrentWeddingMainProduct 是上游 grantCurrentCeraShopMainProduct 在工作区的等价物。
//
// 两份 fork 的发放入口签名不同，必须适配：
//
//   - 上游：grantCurrentCeraShopProduct(inventory, definition, count)
//   - 工作区：grantCurrentCeraShopProduct(inventory, definition, count, mainSlotCount)
//
// 工作区把 mainSlotCount 改成必填是有意为之（见 cera_shop_grant.go 的说明）：
// 它决定 limitCurrentMainInventoryDefinitionSlots 是否被调用，从而决定新物品能不能
// 落进"未解锁"的背包扩展行。漏传的后果不是报错，而是物品静默落进客户端不渲染的
// 格子——玩家看起来就是奖励消失了。
//
// 所以这里必须把该角色已购买的背包扩展行数取出来一并透传，不能为了少改几行而
// 走 3 参数的旧语义。取不到时 currentInventoryPlacementMainSlotCount 返回 0
// （等价于默认 4 行），宁可对已扩展角色少放宽几行，也不让物品越界。
//
// 上游该函数不接受 context，婚礼侧的调用点之一（weddingGiftAllocator 返回的闭包）
// 也确实拿不到调用方的 ctx，所以这里沿用服务端写路径的固定超时，保持签名与上游
// 一致，把差异收敛在这一个文件里。
func (s *Service) grantCurrentWeddingMainProduct(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
) ([]uint16, error) {
	if definition.InventoryListType != dnfrepo.MainInventoryListType {
		return nil, fmt.Errorf("%w: main-list grant received list %d", errCurrentCeraShopProductUnavailable, definition.InventoryListType)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	mainSlotCount := s.currentInventoryPlacementMainSlotCount(ctx, inventory, "")
	return grantCurrentCeraShopProduct(inventory, definition, count, mainSlotCount)
}
