package main

// ============================================================================
//  双开（多开）开关 —— 三端联动功能「后台可开启关闭双开」的核心
// ============================================================================
//
// 需求来源：玩家/运营反馈「双开增加开关 —— 现在双开默认打开，建议增加后台可开启关闭」。
// 即：登录器界面上的「双开」勾选框（LoginWindow.xaml 的 DualCheckBox）现在对所有玩家
// 开放，运营希望能在后台随时把它关掉、也能随时改回来。
//
// ── 数据链路（照抄「注册码开关 registration-info」那条）────────────────────
//
//	internal/config.Config.MultiOpenEnabled（本文件读写，落盘 config.json）
//	  → moPublicState()（认证服务注入：main.go svc.SetMultiOpen）
//	  → GET /launcher/v1/multi-open（免 ticket，**永远 200**；internal/server/multi_open.go）
//	  → 登录器 Services/RemoteAuthService.GetMultiOpenAsync → 静态 bool? MultiOpenEnabled
//	  → LoginWindow.ApplyMultiOpenGate()（勾选框/按钮置灰 + 四道硬闸门）
//
// 实时推送（照抄「更新维护」那条已经在生产跑通的路，见 internal/server/multi_open.go）：
//
//	moApply() 落盘成功后 → svc.MultiOpenChanged()（Broadcast）
//	  → 所有挂起的长轮询 GET /launcher/v1/multi-open/wait 立刻返回
//	  → 登录器 LoginWindow.MultiOpenLoopAsync → ApplyMultiOpenStateAsync（置灰 + Toast）
//
// 所以"运营在后台一关，已经开着的登录器马上变灰"，不需要玩家重开登录器。
// 管理侧：管理工具「服务器选项 → 双开开关」
//	  → GET/POST /manage/v1/multi-open（internal/manage/handlers_multi_open.go）
//	  → moStatus() / moApply()（本文件）
//
// ── ★ 三态语义：只有明确收到 enabled:false 才关闭 ★ ───────────────────────
//
// 这是本功能最容易出事的地方。配置字段 MultiOpenEnabled 是 `*bool`：
//
//	nil    = 后台从未设置过（老 config.json）→ 对外接口**不下发** enabled 字段
//	         → 登录器拿到 null → **保持允许双开**（= 改造前的现有行为）。
//	&true  = 明确允许双开。
//	&false = 明确关闭双开 —— **只有这一种情况**登录器才会关掉双开。
//
// 降级口径（务必保持）：
//   - 老运维工具（没有 /launcher/v1/multi-open 端点）→ 登录器 404 → 拉取失败 → 保持允许；
//   - 网络异常 / 超时 / 认证服务没起来 → 同上，保持允许；
//   - 老配置（config.json 里根本没有 multi_open_enabled 键）→ 下发空 data → 保持允许。
//
// 换句话说：**"关"必须来自运营的一次明确点击**，绝不能来自某个零值 / 拉取失败。
// 否则"老运维工具 + 新登录器"或者"刚升级完还没设置"就会让全服双开瞬间消失。
//
// ── 关闭时玩家侧会发生什么（绝不杀进程）──────────────────────────────────
//
// 关双开只是"以后不许再开"：已经跑着的副账号**不会被踢下线**。
// 玩家侧看到的是"勾选框与副账号登录按钮**立刻**变灰 + 一条提示"（实时推送，见上），
// 而已经在玩的那个副账号一点都不受影响。
// 这一点与「更新维护」是**故意不同**的 —— 维护会 GameProcessManager.KillAll()
// 立刻结束游戏进程（见 登录器 LoginWindow.xaml.cs 的维护分支），双开开关**绝不**照抄那一步。
//
// 实现细节：
//   - 对外端点（给登录器）：internal/server/multi_open.go
//   - 管理端点（给管理工具）：internal/manage/handlers_multi_open.go
//   - 配置落盘字段：internal/config/config.go → MultiOpenEnabled

import (
	"context"
	"fmt"

	"dnf90ops/internal/server"
)

// moStat 双开开关配置快照（与 paid 版 manage.MultiOpenStatus 字段一一对应，JSON tag 必须一致）。
type moStat struct {
	// Enabled 玩家侧生效值：true = 允许双开，false = 已关闭。
	//
	// 注意：**从未设置过**（配置里是 nil）时这里报 true —— 因为玩家侧的实际行为就是
	// "允许"，管理工具的勾选框必须与玩家侧一致，不能让运营看到"没勾"却还有人在双开。
	Enabled bool `json:"enabled"`
	// Configured 后台是否**显式设置过**（false = 老配置 / 从未保存过，玩家侧按"允许"处理）。
	// 管理工具用它写状态行文案（「尚未在后台设置过，当前按默认允许」），不影响玩家侧判定。
	Configured bool `json:"configured"`
}

// moPublicState 返回双开开关实时快照（认证服务注入用；线程安全）。
//
// 与 maintPublicState / lvPublicState 一样在同一把配置锁里一次读完。
// **未设置过（nil）时返回空快照** —— 端点据此不下发 enabled 字段，登录器保持允许双开。
func (a *App) moPublicState() server.MultiOpenState {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil || a.cfg.MultiOpenEnabled == nil {
		return server.MultiOpenState{}
	}
	enabled := *a.cfg.MultiOpenEnabled
	return server.MultiOpenState{Enabled: &enabled}
}

// moStatus 返回给管理工具展示的配置快照（同一把锁里读，与玩家侧同源）。
func (a *App) moStatus() moStat {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil || a.cfg.MultiOpenEnabled == nil {
		// 没有配置 / 从未设置过：玩家侧实际是"允许"，这里必须报同一个值。
		return moStat{Enabled: true, Configured: false}
	}
	return moStat{Enabled: *a.cfg.MultiOpenEnabled, Configured: true}
}

// moApply 应用并持久化双开开关（以本次入参为准，落盘失败整体回滚）。
//
// 落盘后显示"已设置过"（Configured=true），此后运维工具重启、认证服务重启都保持本次取值
// —— 这正是 `*bool` 而不是 `bool` 的原因（omitempty 对指针不省略非 nil，显式 false 能存住）。
//
// 生效时机（两段，缺一不可）：
//   - **立刻**：广播唤醒所有挂起的长轮询（/launcher/v1/multi-open/wait），
//     已经开着的登录器**马上**置灰/恢复 —— 这是"不用让玩家重开登录器"的实现方式；
//   - 兜底：登录器启动自检拉一次短端点 + 点副账号登录前复查一次（老登录器只认这两条）。
//
// 副作用：**没有任何进程操作**。关闭双开不会结束任何已在运行的副账号游戏进程
// （这一点与 maintApply 是**故意不同**的 —— 维护会 KillAll 把游戏断掉，双开开关绝不照抄那一步），
// 只是"以后不许再开"。
func (a *App) moApply(_ context.Context, enabled bool) (moStat, error) {
	if a == nil || a.cfg == nil {
		return moStat{}, fmt.Errorf("配置尚未加载，无法应用双开开关")
	}
	value := enabled

	a.psCfgMu.Lock()
	old := a.cfg.MultiOpenEnabled
	// 用 cfg.Mutate 而不是裸 Save：MultiOpenEnabled 是**指针**字段，与 Save 内部的
	// json.Marshal 并发时属于"成组替换的指针字段"（见 config.Mutate 的说明）；
	// Mutate 全程持有 saveMu，apply 改内存、落盘失败时 rollback 恢复内存。
	// 外面再套一层 psCfgMu：保证与 moPublicState / moStatus 的读取互斥，不会读到半截状态。
	saveErr := a.cfg.Mutate(
		func() { a.cfg.MultiOpenEnabled = &value },
		func() { a.cfg.MultiOpenEnabled = old },
	)
	a.psCfgMu.Unlock()
	if saveErr != nil {
		return a.moStatus(), fmt.Errorf("配置持久化失败: %w", saveErr)
	}

	// 广播给所有等待中的登录器（长轮询 /launcher/v1/multi-open/wait）。
	// 必须在**落盘成功之后**：写盘失败时状态已整体回滚，此时广播会让登录器按一个
	// 并未生效的值置灰/恢复（运营看到开关弹回去，玩家侧却已经变过了）。
	waiting := 0
	if a.svc != nil {
		a.svc.MultiOpenChanged()
		waiting = a.svc.MultiOpenWaiting()
	}

	a.uiSafe(func() {
		if enabled {
			a.logf("双开开关：已允许玩家双开（登录器「双开」勾选框可用，已唤醒等待中的登录器 %d 个）", waiting)
		} else {
			a.logf("双开开关：已禁止新双开 —— 登录器「双开」勾选框与副账号登录按钮**立刻**置灰"+
				"（已唤醒等待中的登录器 %d 个）；"+
				"**已在运行的副账号不会被踢下线**", waiting)
		}
	})
	return a.moStatus(), nil
}
