package dnfbridge

import (
	"context"
	"time"
)

// warmOptionalPVFCatalogs 在监听端口打开前，把「否则会拖到玩家第一个请求里才建」的
// PVF 目录先建好。
//
// 背景（2026-09-26 本机测试服实测）
//
//	服务重启后第一次进城镇：op143 ENUM_CMDPACKET_CHANGE_TUTORIAL_FLAG = 1589ms
//	同一客户端 30 秒后第二次进城：           同一 op143            =  491ms
//
// 同一份 packet_log.txt 显示差额来自三个目录被懒加载在那一次进城请求里：
// cera 商店目录（前后空档 308ms）、消费时间目录、称号簿映射。
// 也就是说 ~1100ms 是「解析 + 建索引」的账，而且它发生在玩家自己的会话事件循环上
// ——那 1.59 秒里该会话的事件循环完全堵死，客户端收不到任何东西。
//
// 参照官方端 df_game_r：它对每个子系统都有一个**显式命名的加载入口**
// （importCashShopItemList / importAttendanceScript / importTitleBookEtcScript …），
// 由 main -> Init(int,char**) -> ScriptManager::LoadScript() 在启动期统一跑完，
// 而不是留一个懒单例等玩家去触发。
//
// 为什么不加进 pvfPreloads 表
//
// 那张表（见 Start）把 loader 返回的 error 当作**致命错误**：会取消 ctx、关闭
// packet log 并拒绝启动。把这三个**可选功能**加进去，等于让「PVF 里没有商店数据」
// 从「商城不可用」升级成「整台服务器起不来」——那是比卡顿严重得多的回归。
//
// 所以这里只做**尽力而为**的预热，并且比懒路径更保守：
//   - 返回 error  → 只记一条 WARN，不中断启动；
//   - panic       → 由 warmOptionalCatalog 兜住，只记一条 WARN，不中断启动。
//
// 三个懒访问器保持原语义（首次使用时重试，或返回同一个已经记下的错误），
// 改动前后它们的对外行为逐字节等价。
//
// 调用位置的两个硬约束
//
//  1. 必须在 pvfPreloads 之后。currentCeraShopCatalog 与消费时间目录都要读
//     s.initialEquipmentArchive，而它由该表第 1 项 preloadInitialEquipmentIndex 打开。
//     提前调用会让 LoadArchive 的失败被永久缓存进 initialEquipmentLoadErr
//     （见 initialEquipmentArchiveLocked），那才会真的修 A 坏 B。
//  2. 必须在 FreeOSMemory 边界之前。那条既有注释写明该边界就是用来回收启动期这些
//     「短命 PVF 文档索引」的，预热属于同一类工作，不该落在边界之外。
func (s *Service) warmOptionalPVFCatalogs(ctx context.Context) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return
	}

	// cera 商店目录。成功时它自己会打 dnf-cera-shop-pvf-catalog-loaded。
	// 失败时不做补偿：错误已被 currentCeraShopCatalog 记进 ceraShopCatalogLoadErr，
	// 与玩家首次打开商城时失败的结果完全相同——只是提前到了启动期。
	s.warmOptionalCatalog("cera shop catalog", func() {
		if _, err := s.currentCeraShopCatalog(); err != nil {
			s.logWarn("dnfbridge cera shop catalog warmup failed", "error", err)
		}
	})

	// 商城限购扫描：把 cera 商店目录**自带的那一份**道具解析器的缓存填满。
	//
	// 背景（2026-09-26 卡顿栈采样器抓到，conn=game-000364，本地 13:36:15.678）：
	//
	//	readDungeonDropText
	//	 → (*pvfDungeonDropCatalog).ResolveItem
	//	  → (*pvfCeraShopCatalog).BuyRestrictPolicies
	//	   → (*Service).currentCeraShopSoldOutEntries
	//	    → (*Service).sendCurrentCeraShopBuyRestrictProjection
	//
	// 这一段跑在会话事件循环上，实占 445ms，是「重启后第一次进城」窗口里最大的一笔。
	//
	// 为什么既有的全量道具预热盖不住它：newPVFCeraShopCatalog 在 cera_shop_catalog.go:78
	// **自己**调了一次 newPVFDungeonDropCatalog，于是进程里存在**两份互相独立的
	// itemRefs / itemCache**——服务级那份见 currentPVFItemCatalog，商城那份挂在
	// pvfCeraShopCatalog.items。preloadDungeonDropItemDefinitions 只填服务级那份，
	// 所以商城这条路径照旧逐条冷读 PVF 文本 + dnfpvf.Parse，一字不减。
	//
	// 这里把同一段扫描在启动期**空跑一遍**（空角色/账号数据，结果直接丢弃），
	// 目的只是让那三万余个商品各自的道具定义落进私有 catalog 的 itemCache。
	// 它只读不写：currentCeraShopSoldOutEntries 不碰任何持久化状态，
	// ResolveItem 的副作用也只有它自己的缓存。
	//
	// 代价 ≈ 该扫描本身的耗时（本机实测 445ms 启动时间），换来的是重启后第一个
	// 进城的玩家不再承担它（同一段在低配 VPS 上实测 2.4 秒）。
	s.warmOptionalCatalog("cera shop buy restrict sweep", func() {
		catalog, err := s.currentCeraShopCatalog()
		if err != nil || catalog == nil {
			// 目录本身没建起来：上一步已经记过 WARN，这里不重复报。
			return
		}
		started := time.Now()
		entries := s.currentCeraShopSoldOutEntries(catalog, nil, nil, s.currentPurchaseLimitTime())
		// 这一行是**验收依据**：没有它，本次改动在日志上完全不可见，
		// 只能靠"第一次进城少了 445ms"间接推断。elapsed_ms 应当与
		// 玩家侧原本承担的那一段同量级（本机 ≈445ms）。
		// entries 用空角色/账号数据跑，正常应为 0——非 0 说明有人拿
		// "已买满"判定依赖了空 map 的零值，那是个需要单独看的信号。
		s.logPacketEvent("dnf-cera-shop-buy-restrict-sweep-preloaded",
			"products", len(catalog.products),
			"entries_with_empty_stats", len(entries),
			"elapsed_ms", time.Since(started).Milliseconds())
	})

	// 消费时间目录，三态返回 (state, available, err)：
	//   available == false 表示「运行时 PVF 归档尚未打开」，是**延后**而不是失败，
	//   调用方会 defer 到下一次、且不会缓存任何东西。预热阶段本不该出现它
	//   （pvfPreloads 已打开归档），真出现也只记一条。
	//
	// 刻意传 runCtx 而**不**像懒路径那样给超时 ctx：LoadAttendancePVFCatalog 一旦
	// 因 ctx 取消而返回错误，该错误会被永久写进 spendTimeCatalogLoadErr，
	// 等于把一个本可重试的失败变成永久失败。runCtx 只在服务关闭时取消，
	// 不会引入这种风险。
	s.warmOptionalCatalog("spend time catalog", func() {
		if _, available, err := s.currentSpendTimeRuntimeCatalogIfAvailable(ctx); err != nil {
			s.logWarn("dnfbridge spend time catalog warmup failed", "error", err)
		} else if !available {
			s.logWarn("dnfbridge spend time catalog warmup deferred",
				"reason", "runtime_pvf_archive_not_loaded")
		}
	})

	// 称号簿映射：无 error 返回。失败时返回 nil 且**不缓存**（titleBookMappingCache
	// 只在成功解析后才赋值），下一次调用会重试，因此这里不需要也不应该做任何补偿。
	s.warmOptionalCatalog("title book mapping", func() {
		s.currentTitleBookMapping()
	})
}

// warmOptionalCatalog 跑一次预热，把 panic 降级成一条 WARN。
//
// 为什么必须自己 recover：这三个访问器在正常服务里只在会话事件循环内被调用，
// 而事件循环自带 recover（见 eventloop 的 recordProcessed 收 panicked 参数）——
// 也就是说解析器即使 panic，今天最坏结果也只是毁掉那一个请求。Start 里没有这层
// 保护，直接调用会把同一个 panic 升级成「服务器起不来」：那是比卡顿严重得多的回归。
//
// 兜住之后最坏情况退化成「这一次预热被跳过」，懒访问器会在玩家首次使用时重试，
// 与改动前的行为一致。
func (s *Service) warmOptionalCatalog(name string, run func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logWarn("dnfbridge optional catalog warmup panicked",
				"catalog", name, "panic", recovered)
		}
	}()
	run()
}
