package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// preloadDungeonDropItemDefinitions 在监听端口打开前，把 itemRefs 索引里的全部道具
// 定义解析好并填入 itemCache。
//
// 背景（2026-09-26 实测：本机 + 203.0.113.13 对照，卡顿栈采样器抓到完整栈）
//
//	进城引导
//	 → sendCurrentSelectInventoryBootstrap        select_character_inventory_bootstrap.go:117
//	  → buildCurrentItemListBodyForSession         current_item_list_session.go:137
//	   → reconcileCurrentPVFItemExpirationsBestEffort        :358
//	    → 账号/角色资产事务包装器                  （MySQL 事务内）
//	     → projectCurrentPVFItemExpirations        item_expiration_owner.go:91
//	      → currentPVFDefinitionForItem            :19
//	       → pvfDungeonDropCatalog.ResolveItem     dungeon_drop_catalog.go:405
//	        → readDungeonDropText（读 PVF 文本）+ dnfpvf.Parse（解析文档）
//
// 该调用点对本角色的**每一个**背包 / 仓库 / 账号金库 / 装备栏条目各调一次
// ResolveItem。ResolveItem 本身有 itemCache，但**进程内首次为空**，于是全部落到冷路径。
// 单角色约 490 个不同道具，实测该段在本机占 419ms，在低配 VPS 上占到 2.4 秒
// （那边是 4 核 4 线程 / 4GB 内存且已超配，冷访问大量落在页面文件上）。
//
// 所以这里在启动期把 itemCache 一次性填满：之后那约 490 次调用全部变成 map 命中，
// 玩家侧的这段开销归零。
//
// 取向与官方端一致：df_game_r 由 main -> Init(int,char**) ->
// ARAD::ScriptManager::LoadScript() 在启动期把每个子系统脚本过一遍，出错就拒绝启动。
//
// 代价（刻意接受）：itemRefs 有 195,651 条（见启动事件
// dnf-pvf-item-catalog-loaded item_refs=...），全量解析约 0.85ms/条，即
// **启动多花约 2~3 分钟、常驻内存增加约 100MB**。启动慢可以接受，玩家侧卡顿不可以。
//
// 失败语义：**单个道具解析失败只记 WARN 并计数，不让服务起不来。**
// 理由与 warmOptionalPVFCatalogs 一致——新增一条启动失败路径，等于把
// 「某个道具文件写坏了」从「那个道具不可用」升级成「整台服务器起不来」，
// 那是比卡顿严重得多的回归。本函数只在「索引根本拿不到」时返回 error
// （那本来就是致命故障）。
func (s *Service) preloadDungeonDropItemDefinitions(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return fmt.Errorf("preload dungeon drop item definitions: %w", err)
	}
	if catalog == nil {
		return nil
	}
	ids := catalog.dungeonDropItemIDs()
	started := time.Now()
	resolved, unresolved, failed := 0, 0, 0
	for _, itemID := range ids {
		// 启动期 ctx 被取消说明服务正在关闭，早点退出，别拖住关闭流程。
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, resolveErr := catalog.ResolveItem(itemID); resolveErr != nil {
			if errors.Is(resolveErr, errDungeonDropItemUnresolved) {
				unresolved++
				continue
			}
			failed++
			// 只报前几条，避免一个坏 PVF 刷出二十万行日志。
			if failed <= 5 {
				s.logWarn("dnfbridge dungeon drop item definition unusable",
					"item_id", itemID, "error", resolveErr)
			}
			continue
		}
		resolved++
	}
	s.logPacketEvent("dnf-dungeon-drop-item-definitions-preloaded",
		"items", len(ids),
		"resolved", resolved,
		"unresolved", unresolved,
		"failed", failed,
		"elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

// dungeonDropItemIDs 返回 itemRefs 索引里全部道具 ID 的稳定快照（升序）。
//
// 只在锁内取一次键集合，真正的解析在锁外逐个进行——ResolveItem 自己会加锁，
// 若在这里持锁遍历两小时，会把启动期其他读该 catalog 的路径全堵死。
// 升序是为了让启动日志与内存布局可复现（map 遍历顺序随机会让排查困难）。
func (c *pvfDungeonDropCatalog) dungeonDropItemIDs() []uint32 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	ids := make([]uint32, 0, len(c.itemRefs))
	for itemID := range c.itemRefs {
		ids = append(ids, itemID)
	}
	c.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
