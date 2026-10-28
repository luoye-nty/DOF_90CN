# 90CNLua

`90CNLua.dll` is a separate Release/Win32 plugin for the pinned current
`NoPack.exe`. It reuses the Lua 5.1.5 runtime statically linked into the game;
it does not ship a second Lua runtime.

The plugin calls the verified `luaL_newstate` and `luaL_openlibs` entrypoints
from the game's embedded runtime and owns one persistent Lua state on a
dedicated worker thread. For monster control it signature-gates both the
current EXE's common AI decision runner and its inner native-Lua factory,
accepting only controllers with the exact current-image AI vtable. Every newly
created game-owned AI Lua state
receives the small external `lua\ai\init.lua` wrapper after the stock script
and `AIBridge` are ready. Current table-driven PVF AI enters through
`begin_pattern`; older FSM AI enters through `Proc` and explicit event
callbacks. The wrapper delegates to every original decision entry until a
script explicitly installs a takeover plan. Overlay plans run the stock Boss
decision first and then replace only selected `MyInfo.next*` fields, preserving
the original attacks, skills, cooldowns, and phase logic. Ordinary table-driven
C++ AI does not create a native Lua state, but it reaches the same common
decision runner and is now assigned the same bounded opaque handle. Lua can
therefore overlay its action, movement, or destination-selection return value
without exposing or storing a native object address.

## Current-EXE native entrypoint map

These addresses are RVAs relative to the pinned `NoPack.exe`; each executable
entrypoint is checked against its current-image prologue before installation or
use.

| Purpose | Current-EXE evidence |
| --- | --- |
| Plugin-owned Lua 5.1.5 state | `luaL_newstate` `0x00002A40`; `luaL_openlibs` `0x00001530` |
| Native monster Lua state | inner factory `0x00EE7070`; result `lua_State*` at `+0x04` |
| Native AI ownership | controller constructor `0x0221E0D0`; vtable `0x041CE00C`; shared AI owner at controller `+0x08`; combat object at owner `+0x58` |
| Universal AI decision | synchronous runner `0x0222ACC0`; result kind at controller `+0x88`; kind `1` is destination selection, kind `3` is movement, remaining kinds are action |
| Monster combat sampling / Boss mutation | require the exact current-image mode-3 stat-block getter at vtable `+1608`, current-HP getter at `+1052`, zero-argument max-HP getter at `+1056`, and HP-state getter at `+1644`; current HP is written through the EXE's proved 64-bit encoded-value writer, while attack/move speed, physical/magical attack/defense, and 104-byte per-skill cooldown rows remain encoded in the combat object |
| Native Lua AI globals | table/PVF entry `begin_pattern`; FSM entries `Proc`, `ProcDamage_Down`, `OnEvent`; lifecycle `Init`, `Enter`, `Exit` |
| Mixed-format AI dispatch | outer loader `0x0221F150`; deliberately not treated as a Lua-state factory |
| Current-character motion | manager pointer `0x04DB2798`; `__thiscall` dispatcher `0x015F8DD0` |
| Actor resolution | current `0x0229A050`; by object key `0x02299A30`; scene index `0x0229FF00`; context `0x013B6AA0` |
| Mood / animation effect | store `0x02290680`; visual refresh `0x02294670`; primary ANI effect `0x02E463C0`; alternate effect `0x02E460B0` |

The public Lua surface reaches these through copied commands and snapshots in
`90CN.dll`; it does not publish any RVA or process address to scripts.
The `monster_id` returned by `get_monsters()` is likewise an opaque scalar
allocated by the plugin. It deliberately is not accepted by actor APIs such as
`get_actor()` or `actor_apply_mood()`.

The bundled Lua worker detects a live Boss crossing from above 50% HP to 50%
or below once per room. `90CN.dll` binds that opaque handle to exactly one
current type-3 op29 actor and asks the server for a one-shot 10% decision. An
approval is applied on the same monster's next native AI tick: attack and move
speed are doubled, physical and magical attack are tripled, both defenses are
doubled, active and future skill cooldowns are halved, and current HP is
restored to the unchanged maximum. The write is transactional at the client
boundary: failed validation or any partial native write rolls back, and the
visible mutation notice is emitted only after the native write succeeds.

Visible notices and game-control requests cross fixed-capacity queues in
`90CN.dll`. Motion, mood effects, and actor snapshots execute only on the DNF
main-window thread. The worker never calls an actor, renderer, or game UI
entrypoint directly, and no Lua API exposes a native address.

Build:

```text
msbuild 90CNLua.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32
```

Put `90CNLua.dll` beside `90CN.dll`, put the persistent starter script at
`lua\init.lua`, and put the native AI wrapper at `lua\ai\init.lua` beside the
DLL. `90CN.dll` loads the plugin when it is present.
Set `DNF_LUA_PLUGIN=0` to disable loading. Set `DNF_LUA_SCRIPT` to an absolute
path, or a path relative to the plugin directory, to select another script.

Saving the script triggers a reload on the worker's next one-second poll. A
host may also call the exported `Request90CNLuaReload`; it only schedules work
and never executes Lua on the caller's thread. `lua\ai\init.lua` is read when
a new native AI controller is created, so edits apply to subsequently created
monsters (normally the next room load), not an already running decision wrapper.

## 原生游戏控制

| 接口 | 说明 |
| --- | --- |
| `dnf90.get_monsters()` | 返回最近 5 秒实际执行过决策的所有游戏原生 AI 怪物（Lua AI 与表驱动 C++ AI）及不透明 `monster_id`。同一怪物的多个控制器合并，并返回控制器/Lua 状态数量、决策次数、覆盖次数、原始/最终结果和最近一次智能改招等诊断信息。 |
| `dnf90.takeover_monster(monster_id)` | Lua AI 停止原决策/事件入口；表驱动 C++ AI 仍计算原生决策以维持内部状态，再覆盖返回字段。原生生命周期不会被截断。 |
| `dnf90.set_monster_plan(monster_id, plan)` | 设置 `action`、`move_method`、`destination_select` 和两个目的地参数。Lua AI 支持全部 `MyInfo.next*` 字段；表驱动 C++ AI 可靠支持前三种返回值。默认 `mode="replace"`；`overlay`/`smart` 只覆盖明确字段。 |
| `dnf90.set_monster_aggression(monster_id[, options])` | 启用智能持续追击：保留原生移动、转身、攻击、技能、冷却和阶段逻辑，持续使用 Boss 自身的 `CHASE(0)`；当 Botticelli 原生 AI 选择 `Hide(7)` 或 `Teleport(16)` 时，按距离与技能冷却改选近战攻击。持续到释放、换房或离开副本。 |
| `dnf90.set_all_monster_aggression_enabled(enabled[, options])` | 打开或关闭全怪物智能追击；随附脚本默认打开并每 250 毫秒发现新控制器。默认只覆盖 `destination_select=CHASE(0)`，不强制攻击或技能编号。 |
| `dnf90.get_all_monster_aggression_state()` | 返回全局开关、当前可见/已管理数量、扫描次数、累计应用/失败次数和选项副本。 |
| `dnf90.release_monster(monster_id)` | 删除一条接管计划，下一次决策入口恢复原始 AI。 |
| `dnf90.release_all_monsters()` | 删除全部计划；换房、离开副本和回城也会自动清理。 |
| `dnf90.get_actors()` | 请求 UI 线程刷新并返回最近完成的当前角色/场景槽快照。首次请求可能返回 `nil`。 |
| `dnf90.get_actor(object_key)` | 请求指定对象并返回最近完成的坐标快照；异步首帧可能返回 `nil`。 |
| `dnf90.self_play_motion(motion_id)` | 将当前角色动作命令加入 UI 线程队列；当前只接受 EXE 明确识别的 `3055`、`3056`、`3057`。 |
| `dnf90.actor_apply_mood(object_key, mood_id)` | 复用客户端原生 mood 保存、视觉刷新和前后景 ANI 创建链；键 `0` 表示当前角色。 |

示例：

```lua
for _, monster in ipairs(dnf90.get_monsters()) do
    dnf90.set_monster_aggression(monster.monster_id)
end

-- 随附脚本默认已经开启；此调用可调整或重新启用全怪物策略。
dnf90.set_all_monster_aggression_enabled(true, {
    destination_select = 0,
})

dnf90.self_play_motion(3055)
dnf90.actor_apply_mood(0, 9)
```

`set_monster_aggression` 追逐怪物原生 AI 已选定的当前目标，不会声称直接修改
尚未证实的原生仇恨数值表。当前 Boss 的 PVF 脚本证实了目的地和移动常量，而
`KeepingWithTargetAgro` 辅助函数本身没有实现，因此接口没有暴露猜测字段。

对非 Lua 的表驱动 AI，公共决策函数只返回当前种类对应的单个整数，所以通用层
可以可靠覆盖动作、移动或目的地选择；两个目的地参数仍仅在原生 Lua `MyInfo`
路径开放。在当前 EXE 证明通用参数返回约定之前，不会对普通怪物写猜测内存。

本版没有开放技能调用和任意 `.ani` 路径。技能入口的其余参数与通用 ANI
对象的销毁责任仍需现场调用证据；在此之前不会用猜测参数冒充可用接口。

## Lua 接口

| 接口 | 中文说明 |
| --- | --- |
| `dnf90.log(text)` | 把 UTF-8 文本写入调试输出。 |
| `dnf90.notify(text)` | 把文本加入固定容量队列，由 DNF 主窗口线程显示游戏内提示。 |
| `dnf90.version()` | 返回插件接口版本字符串。 |
| `dnf90.reload()` | 登记热重载请求，在专用 Lua 工作线程的下一次轮询中执行。 |
| `dnf90.is_ready()` | 返回 `true`，表示基础接口已注册。 |
| `dnf90.set_interval(interval_ms, callback)` | 创建 250 毫秒到 24 小时的重复定时器，并返回定时器编号。 |
| `dnf90.cancel_timer(timer_id)` | 取消尚未结束的定时器，返回是否找到该编号。 |
| `dnf90.get_client_info()` | 返回进程、Lua 工作线程、游戏基址、兼容配置和插件版本。 |
| `dnf90.get_character_stats()` | 返回当前 EXE 已接受的本地角色基础属性快照；尚未就绪时返回 `nil`。 |
| `dnf90.get_equipment_snapshot()` | 返回当前 EXE 已接受的完整穿戴创建列表深拷贝，包含槽位、物品、强化和增幅字段。 |
| `dnf90.get_damage_affixes()` | 返回服务端按当前 PVF、完整穿戴状态和角色记录汇总的白字/黄字/爆伤、追加项、所有攻击力、职业、等级和三攻。 |
| `dnf90.calculate_equipment_score(equipment, affixes, base_score, three_attack_score)` | 使用自定义 V6 规则计算 PVF 装备品级分、0～32 全槽打造分、伤害乘区和并入三攻的所有攻击力加成。 |
| `dnf90.calculate_combat_power(stats, equipment, affixes)` | 合并基础属性分、全槽打造分和 PVF 词条倍率；装备或词条未就绪时只算基础分。 |
| `dnf90.get_combat_power_rank(score)` | 按 `0/3100/8100/23000/52000/65000/130000` 阈值返回段位。 |
| `dnf90.get_combat_power()` | 返回当前已计算的分数、段位、公式版本和属性快照版本。 |
| `dnf90.set_combat_panel_enabled(enabled)` | 启用或关闭个人信息窗口右侧的战斗力侧栏。 |
| `dnf90.set_debug_panel_enabled(enabled)` | 启用或关闭左上角常驻的本地只读调试面板。 |
| `dnf90.get_debug_panel_state()` | 返回由已接受角色快照、装备快照和副本生命周期事件组成的调试状态副本。 |
| `dnf90.set_dungeon_tracker_enabled(enabled)` | 打开或关闭内置副本计时器提示；关闭后仍维护可查询数据。 |
| `dnf90.get_dungeon_run()` | 返回当前或最近一次副本的计时、房间数和当前房间快照。 |
| `dnf90.on(event_name, callback)` | 持续监听事件，返回可取消的监听器编号。 |
| `dnf90.once(event_name, callback)` | 注册只执行一次的事件监听器。 |
| `dnf90.off(listener_id)` | 取消监听器，返回是否找到该编号。 |
| `dnf90.on_event(event)` | 每条受支持事件都会先调用的通用客户端事件回调。 |
| `dnf90.on_ui_ready(event)` | DNF 主窗口、窗口过程和提示队列就绪后调用一次。 |
| `dnf90.on_ui_closed(event)` | DNF 主窗口进入关闭生命周期时尽力调用一次。 |
| `dnf90.on_enter_town(event)` | 当前 EXE 接受一次最终城镇切换并确认当前角色后调用。 |
| `dnf90.on_enter_dungeon(event)` | 当前 EXE 完成副本信息、首张地图和当前角色激活后调用一次。 |
| `dnf90.on_dungeon_room_changed(event)` | 副本活动态接受一个与上次不同的完整房间 op29 后调用。 |
| `dnf90.on_leave_dungeon(event)` | 副本活动态接受最终回城 op24 后，在 `enter_town` 前调用。 |
| `dnf90.on_reload()` | 首次加载或热重载成功后，由 DLL 在 Lua 工作线程调用。 |

所有定时器回调、重载回调和脚本加载都由同一个专用线程串行执行，不会并发进入
Lua。定时器首次执行发生在指定间隔之后；回调返回 `false` 会自动停止，回调抛错
也会停止并输出诊断。热重载会清除旧脚本留下的定时器，避免旧函数继续运行。

`90CN.dll` 把已确认的主窗口和场景切换生命周期写入独立的 32 项固定队列，
`90CNLua.dll` 每 250 毫秒在 Lua 专用线程取出事件。每条事件先调用
`dnf90.on_event(event)`，再调用对应的 `dnf90.on_ui_ready(event)` 或
`dnf90.on_ui_closed(event)`、`dnf90.on_enter_town(event)`、
`dnf90.on_enter_dungeon(event)`、`dnf90.on_dungeon_room_changed(event)`、
`dnf90.on_leave_dungeon(event)`。注册式监听器位于通用回调和专用回调之间，并按
`dnf90.on`/`dnf90.once` 的注册顺序执行；一个回调抛错不会阻止其他监听器或专用
回调。所有 `event` 表都包含
`name`、`sequence`、`process_id`、`ui_thread_id` 和 `context_flags`。副本上下文有效时还包含
`dungeon_id`、`room_x`、`room_y`、`room_layer_flag`、`map_id` 和布尔值
`is_boss_room`；换房事件另含 `previous_room_x`、`previous_room_y`、
`previous_room_layer_flag` 和 `previous_map_id`。`ui_closed` 使用最早可靠的
主窗口关闭边界并尽力交付；进程立即终止时不能保证 Lua 回调执行完毕。
`enter_town` 只在当前 EXE 接受一条 typed class0/op24 最终城镇切换、两个原生提前返回
门控均未拒绝，并且最终选中对象与当前角色一致时入队；登录进城、城镇移动和副本返回
城镇都会产生各自的事件。首版仍不猜测角色名、频道、地图或场景字段。
`enter_dungeon` 只在同一 DNF UI 线程依次接受 typed class0/op28 副本信息、首个
结构完整且初始房间标志为 1 的 op29，以及精确的四字节 op3 当前角色激活状态后
入队。角色对象键还必须解析为当前角色。后续换房只发送 op29，不会重复产生事件；
结算返回城镇仍产生 `enter_town`。副本编号和 BOSS 坐标来自受校验的 op28；房间坐标、
分层标志和地图编号来自完整 op29。`is_boss_room` 只比较当前房间与同一次 op28 的 BOSS
坐标，不硬编码地图编号，也不从房间顺序猜测。
进入活动态后，每个后续结构完整的 op29 都会按房间坐标、分层标志和地图标识与上次已接受房间
比较；不同才产生 `dungeon_room_changed`，相同重包被抑制，回到此前离开的旧房间仍是
一次真实换房。活动态最终接受 op24 时先产生 `leave_dungeon`，再产生现有
`enter_town`；尚未完成进图时取消选图不会产生离开副本事件。`leave_dungeon` 保留清理前
最后一个已确认的副本和房间上下文。事件结构采用追加字段，20 字节旧前缀与当前 60 字节
结构双向兼容；新 Lua 遇到旧传输 DLL 时只收到通用字段，不会补造场景值。

`dnf90.on` 和 `dnf90.once` 当前只接受 `ui_ready`、`ui_closed`、`enter_town`、
`enter_dungeon`、`dungeon_room_changed`、`leave_dungeon`。`once` 在调用回调
前先取消自身；回调中新注册的监听器从下一条事件开始执行。热重载会清除旧脚本的
全部监听器并重新执行新脚本中的注册代码，同时保持监听器编号单调递增，避免旧编号
误取消新监听器。

`notify` 接受转换后最多 511 个 UTF-16 字符且不含内嵌 NUL 的文本。它返回 `true`
只表示消息已进入 32 项固定队列；队列未就绪、已满或参数无效时返回 `false`。
`90CN.dll` 的现有窗口过程会在 DNF UI 线程取出消息，并绘制由 DLL 自己拥有、
鼠标可穿透且会自动隐藏的游戏窗口从属提示。Lua 工作线程从不直接执行 USER32/GDI 或
游戏 UI；返回 `true` 只证明已经安全入队。

战斗力侧栏使用同一条线程安全边界。`90CN.dll` 只在当前 EXE 原生 class0/op2
mode1 或 mode3 处理完成后，复制本地角色 92 字节状态块中的 HP、MP、四维和速度；
同一完整包的穿戴创建列表通过有界变长解析器复制为最多 33 项的普通数据快照，任何
越界长度、重复槽位、未知槽位或不完整行都会整包拒绝，不把半条装备交给 Lua。
`90CNLua.dll` 通过 `dnf90.get_character_stats()` 和
`dnf90.get_equipment_snapshot()` 取得副本，在 Lua 工作线程执行
`dnf90.get_damage_affixes()` 取得服务端 PVF 投影，并在 Lua 工作线程执行
`dnf90.calculate_combat_power(stats, equipment, affixes)`，再把结果复制回主 DLL。主 DLL每 100 毫秒在
DNF UI 线程调用当前 EXE 的 `sub_2FD4B40(sceneRoot, 0xD9)`，所以个人信息窗口打开时
自动显示侧栏，关闭时自动隐藏；`0xC9` 是装备栏，不再作为个人面板判断。侧栏从原生
个人面板根控件读取实时位置，拖动个人面板后会跟随。Lua 不调用 USER32/GDI，也不读取
活动游戏对象。

随附公式 V6 的基础属性分为 `四维总和 × 8 + (最大HP + 最大MP) ÷ 20 + 物攻 + 魔攻 + 独立`。装备分额外使用服务端按当前 PVF 投影的 `最低等级×20 + 品级×5 + 稀有度×100`，打造分覆盖当前
EXE 的完整穿戴槽 `0～32`，包括时装、光环、宠物、红/蓝/绿宠物装备、名称装饰和勋章：
每个已穿槽 200 分、强化/增幅等级平方乘 10，增幅类型为 `1～4` 时再加增幅基础值乘 20。
服务端另从当前 `Script.pvf` 汇总白字、黄字、爆伤、黄追、爆追和所有攻击力，并从角色
`.chr` 及所有穿戴槽解析职业、等级和三攻。普通黄字与普通爆伤按互斥类别取最高，白字及
追加类别累计；黄追并入黄字倍率，爆追并入爆伤倍率，所有攻击力只按三攻加成计算，不再
作为独立可见词条。词条分为 `基础属性分 × (白字/黄字/爆伤总倍率 - 1)`。技能攻击力、无视防御和本命防具系数没有足够当前客户端
证据，暂不猜权重。这是本服可审计的 TGP 风格自定义分数，不冒充官方未公开公式。保存 `lua\init.lua` 会强制使用当前
属性与装备快照热重算，无需重启；
更换 DLL 或首次部署仍需在客户端完全退出后进行。

离线回归可运行 `py -3 tests\combat_power_v4_test.py` 和
`py -3 tests\native_ai_bridge_test.py`；前者验证 0～32 全槽、宠物槽、
强化与增幅分项、PVF 品级分和词条合并、真实职业/等级/三攻、深拷贝、无装备快照兼容路径，以及提交给 UI 的 V6 总分。

随附的 `lua\init.lua` 默认启用战斗力侧栏和副本计时器，左上角本地调试面板默认隐藏：进图提示开始，按去重后的真实房间事件计数，
首次进入 BOSS 坐标时显示当前用时，离开副本时显示总用时和经过房间数。它不创建周期
演示定时器，也不在城镇或热重载时弹框。调用
`dnf90.set_dungeon_tracker_enabled(false)` 可关闭这些提示，跟踪数据仍能通过
`dnf90.get_dungeon_run()` 查询；热重载会保留正在进行的纯数据状态并重新注册当前脚本监听器。

`get_client_info()` 首版只公开无需猜测游戏内存即可可靠取得的元数据，不包含角色、
频道、背包或场景信息。`dnf90` 表中以下划线开头的成员是启动脚本和 DLL 之间的内部
协议，不应由业务脚本调用或覆盖。

Diagnostics keep the `OutputDebugString` `[90CNLua]` stream and also copy
bounded lines through `90CN.dll` into its existing timestamped
`90CN_trace.log`; the plugin does not create a separate log file.

This build is tied to the current executable compatibility profile. It checks
the proved embedded Lua 5.1.5 prologues before creating its owned state and
refuses an unknown or unrestored process. Adding a Lua file to PVF does not
make it run automatically; this loader executes only the configured external
script.
