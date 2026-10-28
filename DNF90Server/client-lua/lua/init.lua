-- 本脚本由 90CNLua.dll 使用当前 EXE 内置的 Lua 5.1.5 运行时加载。
-- DLL 创建并独占一个 Lua 状态；所有脚本、重载回调和定时器都在同一工作线程执行。
-- 请使用 UTF-8 编码保存；保存文件后会在下一次轮询时自动热重载。

-- 接口名称：_G.__90cn_lua_load_count
-- 中文说明：记录脚本累计加载次数，热重载不会把原来的计数清零。
-- 数据类型：整数。
_G.__90cn_lua_load_count = (_G.__90cn_lua_load_count or 0) + 1

local dnf90 = _G.dnf90
if type(dnf90) ~= "table" or not dnf90.is_ready() then
    error("90CNLua 原生接口尚未就绪")
end

-- 原生接口：dnf90.log(text)
-- 中文说明：把 UTF-8 文本写入调试输出，不会创建游戏目录日志。
-- 参数：text，可转换为字符串的值。
-- 返回值：无。

-- 原生接口：dnf90.notify(text)
-- 中文说明：把 UTF-8 文本提交到固定容量队列，由 DNF 主窗口线程显示游戏内提示。
-- 参数：text，不含内嵌 NUL、转换后不超过 511 个 UTF-16 字符的文本。
-- 返回值：成功入队返回 true；接口未就绪、参数无效或队列已满时返回 false。
-- 线程约束：本函数不会从 Lua 工作线程直接调用游戏 UI。

-- 原生接口：dnf90.version()
-- 中文说明：查询当前 90CNLua 插件接口版本。
-- 参数：无。
-- 返回值：版本字符串，例如“90CNLua/1.2.0”。

-- 原生接口：dnf90.reload()
-- 中文说明：登记一次热重载请求，在专用 Lua 工作线程的下一次轮询中执行。
-- 参数：无。
-- 返回值：无。

-- 原生接口：dnf90.is_ready()
-- 中文说明：检查 DLL 基础接口是否已经注册到当前 Lua 虚拟机。
-- 参数：无。
-- 返回值：注册成功时返回 true。

-- 以下五个下划线开头的函数供本启动脚本内部使用，不属于公开脚本接口。
local native_clock_ms = dnf90._clock_ms
local native_client_info = dnf90._client_info
local native_character_stats = dnf90._character_stats
local native_equipment_snapshot = dnf90._equipment_snapshot
local native_damage_affix_snapshot = dnf90._damage_affix_snapshot
local native_update_combat_panel = dnf90._update_combat_panel
local native_update_debug_panel = dnf90._update_debug_panel
local native_queue_game_command = dnf90._queue_game_command
local native_request_boss_mutation = dnf90._request_boss_mutation
local native_poll_boss_mutation_decision =
    dnf90._poll_boss_mutation_decision
local native_apply_boss_mutation = dnf90._apply_boss_mutation
local native_actor_snapshot = dnf90._actor_snapshot
local native_ai_controllers = dnf90._ai_controllers
local native_ai_set_plan = dnf90._ai_set_plan
local native_ai_release = dnf90._ai_release
local native_ai_release_all = dnf90._ai_release_all

-- 战斗力 / 副本计时提示 / 调试浮层：整体停用（2026-09-26）。
-- 服务端不需要战斗力，客户端也就不再算、不再推；DLL 侧对应的入口也已用
-- kCombatPowerEnabled / kDebugPanelEnabled 关死。
-- 保留：怪物 AI 接管、boss 变异、客户端提示、表情、事件系统。
local kCombatPowerPanelEnabled = false
local kDungeonTrackerNoticeEnabled = false
local kDebugPanelEnabled = false

-- 每次重载都丢弃旧回调，避免旧脚本函数继续运行；定时器编号保持单调递增。
local previous_timer_state = rawget(_G, "__dnf90_timer_state")
local next_timer_id = 0
if type(previous_timer_state) == "table" then
    next_timer_id = tonumber(previous_timer_state.next_id) or 0
end
local timer_state = {
    next_id = next_timer_id,
    timers = {},
}
_G.__dnf90_timer_state = timer_state

local function clock_ms()
    local value = tonumber(native_clock_ms())
    if not value then
        error("无法读取 90CNLua 单调时钟")
    end
    return value
end

-- 接口名称：dnf90.set_interval(interval_ms, callback)
-- 中文说明：创建重复定时器；首次执行发生在 interval_ms 毫秒之后。
-- 参数：interval_ms 为 250～86400000 毫秒；callback 为回调函数。
-- 回调参数：callback(timer_id)，timer_id 是当前定时器编号。
-- 回调返回值：返回 false 会自动取消当前定时器，其他返回值表示继续运行。
-- 返回值：成功时返回大于 0 的定时器编号；参数错误时抛出 Lua 错误。
function dnf90.set_interval(interval_ms, callback)
    local interval = tonumber(interval_ms)
    if not interval or interval ~= interval or interval < 250 or interval > 86400000 then
        error("interval_ms 必须是 250～86400000 之间的毫秒数", 2)
    end
    if type(callback) ~= "function" then
        error("callback 必须是 Lua 函数", 2)
    end

    interval = math.floor(interval)
    local timer_id = timer_state.next_id + 1
    if timer_id > 2147483647 then
        timer_id = 1
    end
    while timer_state.timers[timer_id] do
        timer_id = timer_id + 1
    end
    timer_state.next_id = timer_id
    timer_state.timers[timer_id] = {
        interval_ms = interval,
        next_at_ms = clock_ms() + interval,
        callback = callback,
    }
    return timer_id
end

-- 接口名称：dnf90.cancel_timer(timer_id)
-- 中文说明：取消由 set_interval 创建、且尚未结束的定时器。
-- 参数：timer_id，set_interval 返回的定时器编号。
-- 返回值：成功取消返回 true；编号不存在时返回 false。
function dnf90.cancel_timer(timer_id)
    local id = tonumber(timer_id)
    if not id or not timer_state.timers[id] then
        return false
    end
    timer_state.timers[id] = nil
    return true
end

-- 接口名称：dnf90.get_client_info()
-- 中文说明：读取不依赖游戏角色内存的可靠客户端元数据。
-- 参数：无。
-- 返回值：包含 process_id、worker_thread_id、game_base、profile、
--         plugin_version 和 timer_resolution_ms 的新表。
function dnf90.get_client_info()
    local process_id, worker_thread_id, game_base, profile, plugin_version =
        native_client_info()
    return {
        process_id = tonumber(process_id),
        worker_thread_id = tonumber(worker_thread_id),
        game_base = game_base,
        profile = profile,
        plugin_version = plugin_version,
        timer_resolution_ms = 250,
    }
end

-- 内部接口：dnf90._dispatch_timers()
-- 中文说明：由 DLL 工作线程每 250 毫秒调用一次；脚本不要主动调用或覆盖。
local function checked_integer(value, name, minimum, maximum)
    local number = tonumber(value)
    if not number or number ~= number or number ~= math.floor(number) or
        number < minimum or number > maximum then
        error(name .. " 必须是 " .. minimum .. " 到 " .. maximum .. " 之间的整数", 3)
    end
    return number
end

-- 将本地角色切换到客户端原生动作。命令只入队，实际调用发生在 DNF UI 线程。
function dnf90.self_play_motion(motion_id)
    local motion = checked_integer(motion_id, "motion_id", 3055, 3057)
    return native_queue_game_command(1, 0, motion) == true
end

-- 对指定 object_key 应用客户端原生 mood/ANI 特效；object_key=0 表示当前角色。
function dnf90.actor_apply_mood(object_key, mood_id)
    local key = checked_integer(object_key, "object_key", 0, 65535)
    local mood = checked_integer(mood_id, "mood_id", 0, 32767)
    return native_queue_game_command(2, key, mood) == true
end

local function convert_actor_snapshot(available, native_snapshot)
    if available ~= true or type(native_snapshot) ~= "table" then
        return nil
    end
    local count = tonumber(native_snapshot.actor_count)
    if not count or count < 0 or count > 16 or
        type(native_snapshot.actors) ~= "table" then
        return nil
    end
    local actors = {}
    for index = 1, count do
        local row = native_snapshot.actors[tostring(index)]
        if type(row) ~= "table" then
            return nil
        end
        actors[index] = {
            object_key = tonumber(row.object_key),
            flags = tonumber(row.flags),
            x = tonumber(row.x),
            y = tonumber(row.y),
            z = tonumber(row.z),
        }
    end
    return {
        generation = tonumber(native_snapshot.generation),
        ui_thread_id = tonumber(native_snapshot.ui_thread_id),
        captured_tick = tonumber(native_snapshot.captured_tick),
        actor_count = count,
        actors = actors,
    }
end

-- 请求一次 UI 线程刷新并返回最近完成的角色快照。首次调用可能返回 nil；
-- 下一次 250ms 定时轮询中即可取得已完成的 generation。
function dnf90.get_actors()
    return convert_actor_snapshot(native_actor_snapshot())
end

function dnf90.get_actor(object_key)
    local key = checked_integer(object_key, "object_key", 1, 65535)
    local snapshot = convert_actor_snapshot(native_actor_snapshot(key))
    if not snapshot then
        return nil
    end
    for _, actor in ipairs(snapshot.actors) do
        if actor.object_key == key then
            return actor
        end
    end
    return nil
end

-- 返回最近五秒内实际执行过决策的游戏原生 AI 怪物，既包括 Lua AI，
-- 也包括表驱动的 C++ AI。同一怪物的多个控制器合并为一项。
-- monster_id 是 DLL 分配的不透明句柄，不是角色对象键或原生地址。
function dnf90.get_monsters()
    local native_snapshot = native_ai_controllers()
    if type(native_snapshot) ~= "table" then
        return {}
    end
    local count = tonumber(native_snapshot.count) or 0
    local monsters = {}
    for index = 1, count do
        local row = native_snapshot.items[tostring(index)]
        if type(row) == "table" then
            local monster_id = tonumber(row.monster_id or row.object_key)
            local takeover = tonumber(row.takeover) == 1
            local plan_mode = nil
            if takeover then
                local native_mode = tonumber(row.plan_mode) or 0
                plan_mode = native_mode == 2 and "smart" or
                    (native_mode == 1 and "overlay" or "replace")
            end
            local last_smart_action =
                tonumber(row.last_smart_action) or 0
            if last_smart_action == 65535 then
                last_smart_action = -1
            end
            monsters[#monsters + 1] = {
                monster_id = monster_id,
                -- 兼容 1.15.0；这是不透明的怪物 AI 句柄，不是角色对象键。
                object_key = monster_id,
                age_ms = tonumber(row.age_ms),
                takeover = takeover,
                plan_mode = plan_mode,
                field_mask = tonumber(row.field_mask) or 0,
                smart_rewrite_count =
                    tonumber(row.smart_rewrite_count) or 0,
                last_original_action =
                    tonumber(row.last_original_action) or 0,
                last_smart_action = last_smart_action,
                last_smart_near =
                    tonumber(row.last_smart_near) == 1,
                smart_rewrite_age_ms =
                    tonumber(row.smart_rewrite_age_ms),
                controller_count = tonumber(row.controller_count) or 0,
                state_count = tonumber(row.state_count) or 0,
                native_lua = tonumber(row.native_lua) == 1,
                universal_control =
                    tonumber(row.universal_control) == 1,
                decision_count = tonumber(row.decision_count) or 0,
                decision_override_count =
                    tonumber(row.decision_override_count) or 0,
                last_decision_kind =
                    tonumber(row.last_decision_kind) or 0,
                last_native_decision =
                    tonumber(row.last_native_decision),
                last_applied_decision =
                    tonumber(row.last_applied_decision),
                decision_age_ms = tonumber(row.decision_age_ms),
                monster_index = tonumber(row.monster_index),
                hp_percent_tenths =
                    tonumber(row.hp_percent_tenths),
                hp_percent =
                    (tonumber(row.hp_percent_tenths) or 0) / 10,
                hp_snapshot_valid =
                    tonumber(row.hp_snapshot_valid) == 1,
                half_health_crossed =
                    tonumber(row.half_health_crossed) == 1,
                mutated = tonumber(row.mutated) == 1,
                entry_mask = tonumber(row.entry_mask) or 0,
                has_begin_pattern = tonumber(row.has_begin_pattern) == 1,
                has_proc = tonumber(row.has_proc) == 1,
                has_proc_damage_down =
                    tonumber(row.has_proc_damage_down) == 1,
                has_on_event = tonumber(row.has_on_event) == 1,
                has_lifecycle = tonumber(row.has_lifecycle) == 1,
            }
        end
    end
    return monsters
end

-- Lua AI 的 replace 模式停止原决策/事件入口并写入 MyInfo.next*；
-- 表驱动 C++ AI 为保持内部状态完整，仍先计算原生决策，再覆盖所选返回字段。
-- Init、Enter、Exit 生命周期入口始终由客户端原生逻辑负责。
function dnf90.set_monster_plan(monster_id, plan)
    local key = checked_integer(monster_id, "monster_id", 1, 65535)
    if type(plan) ~= "table" then
        error("plan 必须是表", 2)
    end
    local mode_name = plan.mode or "replace"
    if mode_name ~= "replace" and mode_name ~= "overlay" and
            mode_name ~= "smart" then
        error("plan.mode 必须是 replace、overlay 或 smart", 2)
    end
    local mode = mode_name == "smart" and 2 or
        (mode_name == "overlay" and 1 or 0)
    local field_mask = 0
    local function field(name, minimum, maximum, bit)
        local value = plan[name]
        if mode ~= 0 and value == nil then
            return -1
        end
        field_mask = field_mask + bit
        return checked_integer(value == nil and -1 or value,
            "plan." .. name, minimum, maximum)
    end
    local action = field("action", -1, 65535, 1)
    local move_method = field("move_method", -1, 2, 2)
    local destination_select = field("destination_select", -1, 100, 4)
    local destination_param1 = field(
        "destination_param1", -2147483648, 2147483647, 8)
    local destination_param2 = field(
        "destination_param2", -2147483648, 2147483647, 16)
    if field_mask == 0 then
        error("overlay 计划至少要指定一个 next* 字段", 2)
    end
    return native_ai_set_plan(key, action, move_method,
        destination_select, destination_param1, destination_param2,
        mode, field_mask) == true
end

function dnf90.takeover_monster(monster_id)
    return dnf90.set_monster_plan(monster_id, {})
end

-- 保留怪物原生攻击/技能决策，但在每次 AI 入口返回后持续写入
-- 已现场证实的 CHASE(0)。该接口持续追逐原生当前目标，
-- 不会伪造或直接修改尚未证实的客户端仇恨数值表。
function dnf90.set_monster_aggression(monster_id, options)
    if options == nil then
        options = {}
    elseif type(options) ~= "table" then
        error("options 必须是表", 2)
    end
    local plan = {
        mode = "smart",
        destination_select = options.destination_select == nil and
            0 or options.destination_select,
    }
    if options.move_method ~= nil then
        plan.move_method = options.move_method
    end
    if options.action ~= nil then
        plan.action = options.action
    end
    if options.destination_param1 ~= nil then
        plan.destination_param1 = options.destination_param1
    end
    if options.destination_param2 ~= nil then
        plan.destination_param2 = options.destination_param2
    end
    return dnf90.set_monster_plan(monster_id, plan)
end

function dnf90.release_monster(monster_id)
    local key = checked_integer(monster_id, "monster_id", 1, 65535)
    return native_ai_release(key) == true
end

function dnf90.release_all_monsters()
    return native_ai_release_all() == true
end

local function normalize_all_monster_aggression_options(options)
    if options == nil then
        options = {}
    elseif type(options) ~= "table" then
        error("options 必须是表", 3)
    end
    local result = {
        destination_select = options.destination_select == nil and 0 or
            checked_integer(options.destination_select,
                "options.destination_select", -1, 100),
    }
    if options.move_method ~= nil then
        result.move_method = checked_integer(options.move_method,
            "options.move_method", -1, 2)
    end
    if options.action ~= nil then
        result.action = checked_integer(options.action,
            "options.action", -1, 65535)
    end
    if options.destination_param1 ~= nil then
        result.destination_param1 = checked_integer(
            options.destination_param1, "options.destination_param1",
            -2147483648, 2147483647)
    end
    if options.destination_param2 ~= nil then
        result.destination_param2 = checked_integer(
            options.destination_param2, "options.destination_param2",
            -2147483648, 2147483647)
    end
    return result
end

-- 热重载保留总开关和累计统计；定时器本身由本次脚本重新创建。
local previous_all_monster_aggression_state =
    rawget(_G, "__dnf90_all_monster_aggression_state")
local all_monster_aggression_state = {
    enabled = true,
    options = {destination_select = 0},
    managed = {},
    scan_count = 0,
    applied_total = 0,
    failure_total = 0,
    visible_count = 0,
    managed_count = 0,
    last_scan_ms = nil,
    timer_id = nil,
}
if type(previous_all_monster_aggression_state) == "table" then
    all_monster_aggression_state.enabled =
        previous_all_monster_aggression_state.enabled ~= false
    all_monster_aggression_state.options =
        normalize_all_monster_aggression_options(
            previous_all_monster_aggression_state.options)
    if type(previous_all_monster_aggression_state.managed) == "table" then
        all_monster_aggression_state.managed =
            previous_all_monster_aggression_state.managed
    end
    all_monster_aggression_state.scan_count =
        tonumber(previous_all_monster_aggression_state.scan_count) or 0
    all_monster_aggression_state.applied_total =
        tonumber(previous_all_monster_aggression_state.applied_total) or 0
    all_monster_aggression_state.failure_total =
        tonumber(previous_all_monster_aggression_state.failure_total) or 0
end
_G.__dnf90_all_monster_aggression_state = all_monster_aggression_state

local function apply_all_monster_aggression()
    local state = all_monster_aggression_state
    if not state.enabled then
        return true
    end
    local monsters = dnf90.get_monsters()
    local visible = {}
    local managed_count = 0
    state.scan_count = state.scan_count + 1
    state.visible_count = #monsters
    state.last_scan_ms = clock_ms()
    for _, monster in ipairs(monsters) do
        local monster_id = tonumber(monster.monster_id)
        if monster_id then
            visible[monster_id] = true
            if state.managed[monster_id] and monster.takeover then
                managed_count = managed_count + 1
            elseif not monster.takeover then
                if dnf90.set_monster_aggression(
                        monster_id, state.options) then
                    state.managed[monster_id] = true
                    managed_count = managed_count + 1
                    state.applied_total = state.applied_total + 1
                else
                    state.failure_total = state.failure_total + 1
                end
            end
        end
    end
    for monster_id in pairs(state.managed) do
        if not visible[monster_id] then
            state.managed[monster_id] = nil
        end
    end
    state.managed_count = managed_count
    return true
end

-- 默认启用全怪物智能追击。只覆盖已证实的 CHASE(0) 目的地选择；
-- 攻击、技能、冷却、阶段和未指定的移动字段继续使用每只怪物的原生 AI。
function dnf90.set_all_monster_aggression_enabled(enabled, options)
    if type(enabled) ~= "boolean" then
        error("enabled 必须是布尔值", 2)
    end
    if options ~= nil then
        all_monster_aggression_state.options =
            normalize_all_monster_aggression_options(options)
    end
    all_monster_aggression_state.enabled = enabled
    if enabled then
        return apply_all_monster_aggression()
    end
    local released = true
    for monster_id in pairs(all_monster_aggression_state.managed) do
        if not dnf90.release_monster(monster_id) then
            released = false
        end
    end
    all_monster_aggression_state.managed = {}
    all_monster_aggression_state.managed_count = 0
    return released
end

function dnf90.get_all_monster_aggression_state()
    local state = all_monster_aggression_state
    local options = {}
    for name, value in pairs(state.options) do
        options[name] = value
    end
    return {
        enabled = state.enabled,
        options = options,
        scan_count = state.scan_count,
        applied_total = state.applied_total,
        failure_total = state.failure_total,
        visible_count = state.visible_count,
        managed_count = state.managed_count,
        last_scan_ms = state.last_scan_ms,
        timer_id = state.timer_id,
    }
end

apply_all_monster_aggression()
all_monster_aggression_state.timer_id = dnf90.set_interval(250, function()
    return apply_all_monster_aggression()
end)

function dnf90._dispatch_timers()
    local now = clock_ms()
    local due_ids = {}
    for timer_id, timer in pairs(timer_state.timers) do
        if now >= timer.next_at_ms then
            due_ids[#due_ids + 1] = timer_id
        end
    end

    for _, timer_id in ipairs(due_ids) do
        local timer = timer_state.timers[timer_id]
        if timer then
            -- 先安排下次执行，避免耗时回调触发补偿式连跑。
            timer.next_at_ms = now + timer.interval_ms
            local ok, keep_running = pcall(timer.callback, timer_id)
            if timer_state.timers[timer_id] == timer then
                if not ok then
                    timer_state.timers[timer_id] = nil
                    dnf90.log("定时器 " .. timer_id .. " 出错并已停止：" .. tostring(keep_running))
                elseif keep_running == false then
                    timer_state.timers[timer_id] = nil
                end
            end
        end
    end
end

-- 战斗力面板只把“公式与状态”交给 Lua；窗口创建、原生个人面板检测和绘制都在主 DLL 的 UI 线程完成。
-- 这样保存本文件后可以热重载公式，又不会让 Lua 工作线程直接碰游戏窗口。
local previous_combat_power_state = rawget(_G, "__dnf90_combat_power_state")
local combat_power_state = {
    enabled = true,
    revision = 0,
    source_generation = nil,
    source_signature = nil,
    stats = nil,
    equipment = nil,
    affixes = nil,
    result = nil,
    submit_pending = false,
}
if type(previous_combat_power_state) == "table" then
    combat_power_state.enabled = previous_combat_power_state.enabled ~= false
    combat_power_state.revision =
        tonumber(previous_combat_power_state.revision) or 0
end
_G.__dnf90_combat_power_state = combat_power_state

local function bounded_nonnegative_integer(value)
    local number = tonumber(value) or 0
    if number ~= number or number <= 0 then
        return 0
    end
    if number > 2000000000 then
        return 2000000000
    end
    return math.floor(number)
end

-- 接口名称：dnf90.get_character_stats()
-- 中文说明：读取当前 EXE 已经成功接收并应用的本地角色基础属性快照。
-- 参数：无。
-- 返回值：尚未收到完整角色状态时返回 nil；成功时返回一个新的数值表。
-- 返回字段：generation、valid_flags、hp_max、mp_max、strength、vitality、
--           intelligence、spirit、move_speed、attack_speed、cast_speed。
-- 数据边界：这些四维是协议 92 字节基础状态，不冒充个人面板经过装备结算后的最终绿色数值。
function dnf90.get_character_stats()
    local available, generation, valid_flags, hp_max, mp_max,
        strength, vitality, intelligence, spirit,
        move_speed, attack_speed, cast_speed = native_character_stats()
    if available ~= true then
        return nil
    end

    local stats = {
        generation = tonumber(generation),
        valid_flags = tonumber(valid_flags),
        hp_max = tonumber(hp_max),
        mp_max = tonumber(mp_max),
        strength = tonumber(strength),
        vitality = tonumber(vitality),
        intelligence = tonumber(intelligence),
        spirit = tonumber(spirit),
        move_speed = tonumber(move_speed),
        attack_speed = tonumber(attack_speed),
        cast_speed = tonumber(cast_speed),
    }
    if not stats.generation or not stats.valid_flags or
        not stats.hp_max or not stats.mp_max or
        not stats.strength or not stats.vitality or
        not stats.intelligence or not stats.spirit then
        return nil
    end
    return stats
end

-- 接口名称：dnf90.get_equipment_snapshot()
-- 中文说明：读取当前 EXE 已经成功接收并应用的本地角色穿戴对象快照。
-- 参数：无。
-- 返回值：尚未收到完整 mode1/mode3 创建列表时返回 nil；成功时返回一个新的深拷贝表。
-- 返回字段：generation、source_stat_generation、valid_flags、item_count、items。
-- 每件装备字段：item_id、quality_seed、durability、actor_slot、upgrade_level、
--               amplify_type、amplify_value。
-- 数据边界：这里只呈现线协议已经携带的身份和强化/增幅状态；不按 item_id 猜稀有度、
--           套装、黄字、爆伤、附加伤害或 PVF 词条。
function dnf90.get_equipment_snapshot()
    local available, native_snapshot = native_equipment_snapshot()
    if available ~= true or type(native_snapshot) ~= "table" then
        return nil
    end

    local generation = tonumber(native_snapshot.generation)
    local source_stat_generation = tonumber(native_snapshot.source_stat_generation)
    local valid_flags = tonumber(native_snapshot.valid_flags)
    local item_count = tonumber(native_snapshot.item_count)
    if not generation or not source_stat_generation or not valid_flags or
        not item_count or item_count < 0 or item_count > 33 or
        type(native_snapshot.items) ~= "table" then
        return nil
    end

    local items = {}
    for index = 1, item_count do
        local native_item = native_snapshot.items[tostring(index)]
        if type(native_item) ~= "table" then
            return nil
        end
        local item = {
            item_id = tonumber(native_item.item_id),
            quality_seed = tonumber(native_item.quality_seed),
            durability = tonumber(native_item.durability),
            actor_slot = tonumber(native_item.actor_slot),
            upgrade_level = tonumber(native_item.upgrade_level),
            amplify_type = tonumber(native_item.amplify_type),
            amplify_value = tonumber(native_item.amplify_value),
        }
        if not item.item_id or item.item_id <= 0 or
            not item.actor_slot or item.actor_slot < 0 or item.actor_slot > 32 or
            not item.upgrade_level or not item.amplify_type or
            not item.amplify_value then
            return nil
        end
        items[index] = item
    end
    return {
        generation = generation,
        source_stat_generation = source_stat_generation,
        valid_flags = valid_flags,
        item_count = item_count,
        items = items,
    }
end

-- 接口名称：dnf90.get_damage_affixes()
-- 中文说明：读取服务端按当前 Script.pvf 和完整穿戴槽位 0～32 汇总的伤害词条。
-- 参数：无。
-- 返回值：尚未收到服务端投影时返回 nil；成功时返回全新的只读数值表。
-- 词条定义：white_damage=白字（附加伤害）；yellow_damage=黄字（对敌人伤害增加）；
--           critical_damage=爆伤（暴击伤害）；pvf_equipment_score=PVF 等级/品级/稀有度基础分。
--           黄追和爆追单独返回，绝不混入前三项显示。
function dnf90.get_damage_affixes()
    local available, native_snapshot = native_damage_affix_snapshot()
    if available ~= true or type(native_snapshot) ~= "table" then
        return nil
    end
    local snapshot = {
        generation = tonumber(native_snapshot.generation),
        valid_flags = tonumber(native_snapshot.valid_flags),
        version = tonumber(native_snapshot.version),
        white_damage_tenths = tonumber(native_snapshot.white_damage_tenths),
        yellow_damage_tenths = tonumber(native_snapshot.yellow_damage_tenths),
        critical_damage_tenths = tonumber(native_snapshot.critical_damage_tenths),
        yellow_additional_tenths = tonumber(native_snapshot.yellow_additional_tenths),
        critical_additional_tenths = tonumber(native_snapshot.critical_additional_tenths),
        all_attack_tenths = tonumber(native_snapshot.all_attack_tenths),
        equipped_item_count = tonumber(native_snapshot.equipped_item_count),
        active_set_count = tonumber(native_snapshot.active_set_count),
        job = tonumber(native_snapshot.job),
        grow_type = tonumber(native_snapshot.grow_type),
        level = tonumber(native_snapshot.level),
        physical_attack = tonumber(native_snapshot.physical_attack),
        magical_attack = tonumber(native_snapshot.magical_attack),
        independent_attack = tonumber(native_snapshot.independent_attack),
        pvf_equipment_score = tonumber(native_snapshot.pvf_equipment_score),
    }
    for name, value in pairs(snapshot) do
        if type(value) ~= "number" or value < 0 then
            return nil
        end
    end
    if type(native_snapshot.profession) ~= "string" or
        native_snapshot.profession == "" or snapshot.level < 1 then
        return nil
    end
    snapshot.profession = native_snapshot.profession
    snapshot.white_damage = snapshot.white_damage_tenths / 10
    snapshot.yellow_damage = snapshot.yellow_damage_tenths / 10
    snapshot.critical_damage = snapshot.critical_damage_tenths / 10
    snapshot.yellow_additional = snapshot.yellow_additional_tenths / 10
    snapshot.critical_additional = snapshot.critical_additional_tenths / 10
    snapshot.all_attack = snapshot.all_attack_tenths / 10
    return snapshot
end

-- 接口名称：dnf90.get_combat_power_rank(score)
-- 中文说明：按参考面板给出的七段阈值返回本服自定义段位名称。
-- 阈值：0、3100、8100、23000、52000、65000、130000。
-- 参数：score，非负战斗力整数。
-- 返回值：探索、开拓、无畏、征服、战绝、英杰或武炼。
function dnf90.get_combat_power_rank(score)
    local value = bounded_nonnegative_integer(score)
    if value < 3100 then return "探索" end
    if value < 8100 then return "开拓" end
    if value < 23000 then return "无畏" end
    if value < 52000 then return "征服" end
    if value < 65000 then return "战绝" end
    if value < 130000 then return "英杰" end
    return "武炼"
end

local combat_equipment_slots = {}
for slot = 0, 32 do
    combat_equipment_slots[slot] = true
end

-- 接口名称：dnf90.calculate_equipment_score(equipment, affixes, base_score)
-- 中文说明：用完整穿戴快照和服务端 PVF 词条计算自定义装备加成 V6，返回可审计分项。
-- 参数：equipment 为完整穿戴快照；affixes 为 PVF 词条；base_score 为已计算的基础分；three_attack_score 为三攻合计。
-- 槽位范围：当前 EXE 的穿戴槽 0～32 全部参与，包括时装、光环、宠物、宠物装备和勋章。
-- 词条公式：词条倍率=(1+白字)×(1+黄字+黄追)×(1+爆伤+爆追)；词条分=基础分×(词条倍率-1)。
--           黄追并入黄字、爆追并入爆伤；所有攻击力只计算三攻加成。
-- 基础装备分：PVF 等级/品级/稀有度分 + 每个已穿槽 200 分 + 强化/增幅等级平方×10 + 增幅基础值×20。
function dnf90.calculate_equipment_score(equipment, affixes, base_score, three_attack_score)
    if type(equipment) ~= "table" or type(equipment.items) ~= "table" then
        error("equipment 必须是装备快照表", 2)
    end
    if type(affixes) ~= "table" then
        error("affixes 必须是 PVF 词条快照表", 2)
    end
    local base = bounded_nonnegative_integer(base_score)
    local item_count = 0
    local occupancy_score = 0
    local upgrade_score = 0
    local amplification_score = 0
    for _, item in ipairs(equipment.items) do
        local slot = math.floor(tonumber(item.actor_slot) or -1)
        local item_id = bounded_nonnegative_integer(item.item_id)
        if combat_equipment_slots[slot] and item_id > 0 then
            item_count = item_count + 1
            occupancy_score = occupancy_score + 200
            local upgrade_level = math.floor(tonumber(item.upgrade_level) or 0)
            upgrade_level = math.max(0, math.min(31, upgrade_level))
            upgrade_score = upgrade_score + upgrade_level * upgrade_level * 10

            local amplify_type = math.floor(tonumber(item.amplify_type) or 0)
            if amplify_type >= 1 and amplify_type <= 4 then
                local amplify_value = math.floor(tonumber(item.amplify_value) or 0)
                amplify_value = math.max(0, math.min(1000, amplify_value))
                amplification_score = amplification_score + amplify_value * 20
            end
        end
    end
    local white = math.max(0, tonumber(affixes.white_damage) or 0) / 100
    local yellow = math.max(0,
        (tonumber(affixes.yellow_damage) or 0) +
        (tonumber(affixes.yellow_additional) or 0)) / 100
    local critical = math.max(0,
        (tonumber(affixes.critical_damage) or 0) +
        (tonumber(affixes.critical_additional) or 0)) / 100
    local all_attack = math.max(0,
        tonumber(affixes.all_attack) or 0) / 100
    -- “所有攻击力”属于三攻加成，不作为面板第 4/5 条词条展示。
    -- 黄追并入黄字，爆追并入爆伤，官方样式只显示白字、黄字、爆伤三行。
    local affix_multiplier = (1 + white) * (1 + yellow) * (1 + critical)
    local affix_score = bounded_nonnegative_integer(
        base * (affix_multiplier - 1))
    local three_attack_bonus = bounded_nonnegative_integer(
        bounded_nonnegative_integer(three_attack_score) * all_attack)
    local pvf_equipment_score = bounded_nonnegative_integer(
        affixes.pvf_equipment_score)
    return {
        score = bounded_nonnegative_integer(
            occupancy_score + upgrade_score + amplification_score +
            affix_score + three_attack_bonus + pvf_equipment_score),
        item_count = item_count,
        occupancy_score = occupancy_score,
        upgrade_score = upgrade_score,
        amplification_score = amplification_score,
        affix_score = affix_score,
        three_attack_bonus = three_attack_bonus,
        pvf_equipment_score = pvf_equipment_score,
        affix_multiplier = affix_multiplier,
        all_attack_multiplier = 1 + all_attack,
    }
end

-- 接口名称：dnf90.calculate_combat_power(stats, equipment, affixes)
-- 中文说明：计算自定义战斗力 V6；保存本文件后，当前角色会用新公式热重算。
-- 参数：stats 为角色基础属性；equipment 为完整穿戴快照；affixes 为服务端 PVF 词条快照。
-- 当前公式：基础属性分 = 四维总和 × 8 +（最大 HP + 最大 MP）÷ 20 + 物攻 + 魔攻 + 独立。
-- 总分 = 基础属性分 + calculate_equipment_score 返回的装备分；任一装备数据源未就绪时装备分为 nil。
-- 返回值：包含 total_score、base_attribute_score、equipment_score、equipment_breakdown、
--         rank 和 formula_version 的新表。
function dnf90.calculate_combat_power(stats, equipment, affixes)
    if type(stats) ~= "table" then
        error("stats 必须是角色属性表", 2)
    end
    local strength = math.max(0, tonumber(stats.strength) or 0)
    local vitality = math.max(0, tonumber(stats.vitality) or 0)
    local intelligence = math.max(0, tonumber(stats.intelligence) or 0)
    local spirit = math.max(0, tonumber(stats.spirit) or 0)
    local hp_max = math.max(0, tonumber(stats.hp_max) or 0)
    local mp_max = math.max(0, tonumber(stats.mp_max) or 0)
    local physical_attack = math.max(0,
        tonumber(affixes and affixes.physical_attack) or 0)
    local magical_attack = math.max(0,
        tonumber(affixes and affixes.magical_attack) or 0)
    local independent_attack = math.max(0,
        tonumber(affixes and affixes.independent_attack) or 0)
    local three_attack_score = bounded_nonnegative_integer(
        physical_attack + magical_attack + independent_attack)
    local base_score = bounded_nonnegative_integer(
        (strength + vitality + intelligence + spirit) * 8 +
        (hp_max + mp_max) / 20 + three_attack_score)
    local equipment_breakdown = nil
    local equipment_score = nil
    if equipment ~= nil and affixes ~= nil then
        equipment_breakdown = dnf90.calculate_equipment_score(
            equipment, affixes, base_score, three_attack_score)
        equipment_score = equipment_breakdown.score
    end
    local total_score = bounded_nonnegative_integer(
        base_score + (equipment_score or 0))
    return {
        total_score = total_score,
        base_attribute_score = base_score,
        equipment_score = equipment_score,
        equipment_breakdown = equipment_breakdown,
        rank = dnf90.get_combat_power_rank(total_score),
        affixes = affixes,
        three_attack_score = three_attack_score,
        formula_version = 6,
    }
end

local function submit_combat_power_panel()
    if not kCombatPowerPanelEnabled then return end
    local stats = combat_power_state.stats or {}
    local result = combat_power_state.result or {}
    combat_power_state.revision = combat_power_state.revision + 1
    if combat_power_state.revision > 2147483647 then
        combat_power_state.revision = 1
    end
    local equipment_valid = result.equipment_score ~= nil
    local affixes = combat_power_state.affixes or {}
    local affixes_valid = tonumber(affixes.generation) ~= nil
    local accepted = native_update_combat_panel(
        combat_power_state.enabled and 1 or 0,
        combat_power_state.revision,
        tonumber(combat_power_state.source_generation) or 0,
        combat_power_state.result and 1 or 0,
        bounded_nonnegative_integer(result.total_score),
        bounded_nonnegative_integer(result.base_attribute_score),
        equipment_valid and 1 or 0,
        bounded_nonnegative_integer(result.equipment_score),
        bounded_nonnegative_integer(stats.hp_max),
        bounded_nonnegative_integer(stats.mp_max),
        math.floor(tonumber(stats.strength) or 0),
        math.floor(tonumber(stats.vitality) or 0),
        math.floor(tonumber(stats.intelligence) or 0),
        math.floor(tonumber(stats.spirit) or 0),
        bounded_nonnegative_integer(result.formula_version),
        affixes_valid and 1 or 0,
        bounded_nonnegative_integer(affixes.generation),
        bounded_nonnegative_integer(affixes.white_damage_tenths),
        bounded_nonnegative_integer(affixes.yellow_damage_tenths),
        bounded_nonnegative_integer(affixes.critical_damage_tenths),
        bounded_nonnegative_integer(affixes.yellow_additional_tenths),
        bounded_nonnegative_integer(affixes.critical_additional_tenths),
        bounded_nonnegative_integer(affixes.all_attack_tenths),
        bounded_nonnegative_integer(affixes.equipped_item_count),
        bounded_nonnegative_integer(affixes.active_set_count),
        bounded_nonnegative_integer(affixes.job),
        bounded_nonnegative_integer(affixes.grow_type),
        bounded_nonnegative_integer(affixes.level),
        bounded_nonnegative_integer(affixes.physical_attack),
        bounded_nonnegative_integer(affixes.magical_attack),
        bounded_nonnegative_integer(affixes.independent_attack),
        type(affixes.profession) == "string" and affixes.profession or "--")
    combat_power_state.submit_pending = accepted ~= true
    return accepted == true
end

local function refresh_combat_power(force)
    local stats = dnf90.get_character_stats()
    if not stats then
        return false
    end
    local equipment = dnf90.get_equipment_snapshot()
    local affixes = dnf90.get_damage_affixes()
    local equipment_generation = equipment and equipment.generation or 0
    local affix_generation = affixes and affixes.generation or 0
    local source_signature = tostring(stats.generation) .. ":" ..
        tostring(equipment_generation) .. ":" .. tostring(affix_generation)
    if not force and source_signature == combat_power_state.source_signature then
        if combat_power_state.submit_pending then
            return submit_combat_power_panel()
        end
        return true
    end

    combat_power_state.source_generation = stats.generation
    combat_power_state.source_signature = source_signature
    combat_power_state.stats = stats
    combat_power_state.equipment = equipment
    combat_power_state.affixes = affixes
    combat_power_state.result = dnf90.calculate_combat_power(
        stats, equipment, affixes)
    return submit_combat_power_panel()
end

-- 接口名称：dnf90.set_combat_panel_enabled(enabled)
-- 中文说明：启用或关闭个人信息面板右侧的战斗力侧栏；默认启用。
-- 参数：enabled 必须是布尔值。
-- 返回值：主 DLL 已接收本次显示状态时返回 true，否则返回 false 并由定时器自动重试。
function dnf90.set_combat_panel_enabled(enabled)
    if type(enabled) ~= "boolean" then
        error("enabled 必须是布尔值", 2)
    end
    combat_power_state.enabled = enabled
    return submit_combat_power_panel()
end

-- 接口名称：dnf90.get_combat_power()
-- 中文说明：返回当前已计算的战斗力只读快照，便于其他 Lua 业务逻辑复用。
-- 参数：无。
-- 返回值：数据尚未就绪时返回 nil；否则返回包含分数、段位、公式版本和属性版本的新表。
function dnf90.get_combat_power()
    local result = combat_power_state.result
    if not result then return nil end
    return {
        total_score = result.total_score,
        base_attribute_score = result.base_attribute_score,
        equipment_score = result.equipment_score,
        equipment_breakdown = result.equipment_breakdown,
        equipment_generation = combat_power_state.equipment and
            combat_power_state.equipment.generation or nil,
        affix_generation = combat_power_state.affixes and
            combat_power_state.affixes.generation or nil,
        affixes = result.affixes,
        profession = combat_power_state.affixes and
            combat_power_state.affixes.profession or nil,
        level = combat_power_state.affixes and
            combat_power_state.affixes.level or nil,
        physical_attack = combat_power_state.affixes and
            combat_power_state.affixes.physical_attack or nil,
        magical_attack = combat_power_state.affixes and
            combat_power_state.affixes.magical_attack or nil,
        independent_attack = combat_power_state.affixes and
            combat_power_state.affixes.independent_attack or nil,
        rank = result.rank,
        formula_version = result.formula_version,
        source_generation = combat_power_state.source_generation,
        enabled = combat_power_state.enabled,
    }
end

-- 每 250 毫秒检查一次属性版本；没有变化时不重复计算或重绘。
-- 热重载时强制重算一次，所以修改公式后不需要重启客户端。
refresh_combat_power(true)
combat_power_state.timer_id = dnf90.set_interval(250, function()
    refresh_combat_power(false)
end)

-- 每次热重载都丢弃旧监听器，避免旧脚本函数继续运行；监听器编号保持单调递增。
local previous_event_state = rawget(_G, "__dnf90_event_state")
local next_listener_id = 0
if type(previous_event_state) == "table" then
    next_listener_id = tonumber(previous_event_state.next_id) or 0
end
local event_state = {
    next_id = next_listener_id,
    listeners = {},
    order = {
        ui_ready = {},
        ui_closed = {},
        enter_town = {},
        enter_dungeon = {},
        dungeon_room_changed = {},
        leave_dungeon = {},
    },
}
_G.__dnf90_event_state = event_state

local function add_event_listener(event_name, callback, run_once)
    if type(event_name) ~= "string" or not event_state.order[event_name] then
        error("event_name 必须是 ui_ready、ui_closed、enter_town、enter_dungeon、dungeon_room_changed 或 leave_dungeon", 3)
    end
    if type(callback) ~= "function" then
        error("callback 必须是 Lua 函数", 3)
    end

    local listener_id = event_state.next_id + 1
    if listener_id > 2147483647 then
        listener_id = 1
    end
    while event_state.listeners[listener_id] do
        listener_id = listener_id + 1
    end
    event_state.next_id = listener_id
    event_state.listeners[listener_id] = {
        event_name = event_name,
        callback = callback,
        run_once = run_once,
    }
    local event_order = event_state.order[event_name]
    event_order[#event_order + 1] = listener_id
    return listener_id
end

-- 接口名称：dnf90.on(event_name, callback)
-- 中文说明：按注册顺序持续监听一种客户端事件。
-- 参数：event_name 当前接受 "ui_ready"、"ui_closed"、"enter_town"、"enter_dungeon"、"dungeon_room_changed" 或 "leave_dungeon"；callback 接收 event 数据表。
-- 返回值：大于 0 的监听器编号，可交给 dnf90.off(listener_id) 取消。
function dnf90.on(event_name, callback)
    return add_event_listener(event_name, callback, false)
end

-- 接口名称：dnf90.once(event_name, callback)
-- 中文说明：注册只执行一次的客户端事件监听器；调用前先移除，避免回调重入时重复执行。
-- 参数和返回值：与 dnf90.on 相同。
function dnf90.once(event_name, callback)
    return add_event_listener(event_name, callback, true)
end

-- 接口名称：dnf90.off(listener_id)
-- 中文说明：取消尚未执行或仍在持续监听的事件监听器。
-- 参数：listener_id 为 dnf90.on 或 dnf90.once 返回的编号。
-- 返回值：成功取消返回 true；编号无效、已取消或单次监听器已执行时返回 false。
function dnf90.off(listener_id)
    local id = tonumber(listener_id)
    if not id or id ~= math.floor(id) or not event_state.listeners[id] then
        return false
    end
    event_state.listeners[id] = nil
    return true
end

-- 回调名称：dnf90.on_event(event)
-- 中文说明：每条受支持的客户端事件都会先调用此通用回调，然后再调用同名专用回调。
-- 参数：event 为只读约定的数据表，基础字段包含 name、sequence、process_id 和 ui_thread_id；
--       已确认的副本事件还包含 dungeon_id、room_x、room_y、room_layer_flag、map_id 和 is_boss_room。
-- 返回值：忽略；回调抛错会被隔离并写入调试输出，不会阻止对应的专用回调。
function dnf90.on_event(_event)
end

-- 回调名称：dnf90.on_ui_ready(event)
-- 中文说明：90CN.dll 已确认 DNF 主窗口、窗口过程和提示队列全部就绪后调用一次。
-- 参数：event.name 为 "ui_ready"，其余字段为事件编号、客户端进程和 DNF UI 线程编号。
-- 线程约束：本函数在 Lua 专用工作线程执行；需要显示文本时调用 dnf90.notify(text)。
function dnf90.on_ui_ready(event)
    dnf90.log(
        "客户端 UI 已就绪，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end

-- 回调名称：dnf90.on_ui_closed(event)
-- 中文说明：DNF 主窗口进入关闭生命周期时尽力调用一次；进程立即结束时不保证执行完毕。
-- 参数：event.name 为 "ui_closed"，其余字段与 on_ui_ready 相同。
-- 线程约束：本函数在 Lua 专用工作线程执行，不在 DNF 窗口线程中直接执行 Lua。
function dnf90.on_ui_closed(event)
    dnf90.log(
        "客户端 UI 正在关闭，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end

-- 回调名称：dnf90.on_enter_town(event)
-- 中文说明：当前 EXE 接受一次 class0/op24 最终城镇切换，并确认最终对象为当前角色后调用。
-- 参数：event.name 为 "enter_town"，其余字段与 on_ui_ready 相同；首版不猜测角色、地图或频道。
-- 线程约束：事件在 DNF UI 线程确认，但本函数只在 Lua 专用工作线程执行。
function dnf90.on_enter_town(event)
    dnf90.log(
        "角色已进入城镇，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end

-- 回调名称：dnf90.on_enter_dungeon(event)
-- 中文说明：当前 EXE 依次接受副本信息、首张地图，并用当前角色的激活状态完成进图后调用一次。
-- 参数：event.name 为 "enter_dungeon"，并包含已确认的副本编号、初始房间、地图编号和 BOSS 房标志。
-- 线程约束：事件在 DNF UI 线程确认，但本函数只在 Lua 专用工作线程执行；换房间不会重复调用。
function dnf90.on_enter_dungeon(event)
    dnf90.log(
        "角色已进入副本，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end

-- 回调名称：dnf90.on_dungeon_room_changed(event)
-- 中文说明：副本活动态接受一个与上次房间坐标、分层标志或地图标识不同的完整 op29 后调用；首次进图不调用。
-- 参数：除当前副本/房间字段外，还包含 previous_room_x、previous_room_y、previous_room_layer_flag 和 previous_map_id。
-- 线程约束：事件在 DNF UI 线程确认，但本函数只在 Lua 专用工作线程执行；相同房间重包会被去重。
function dnf90.on_dungeon_room_changed(event)
    dnf90.log(
        "角色已切换副本房间，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end

-- 回调名称：dnf90.on_leave_dungeon(event)
-- 中文说明：副本活动态接受最终回城 op24 后调用一次，并且总是在同一包的 enter_town 回调之前。
-- 参数：event.name 为 "leave_dungeon"，并保留离开前最后一个已确认的副本和房间上下文；中途取消选图不会误报。
-- 线程约束：事件在 DNF UI 线程确认，但本函数只在 Lua 专用工作线程执行。
function dnf90.on_leave_dungeon(event)
    dnf90.log(
        "角色已离开副本，事件=" .. event.sequence ..
        "，UI 线程=" .. event.ui_thread_id)
end


local function dispatch_client_callback(callback_name, event)
    local callback = dnf90[callback_name]
    if type(callback) ~= "function" then
        return
    end
    local ok, error_message = pcall(callback, event)
    if not ok then
        dnf90.log(callback_name .. " 回调出错：" .. tostring(error_message))
    end
end

local function dispatch_registered_listeners(event)
    local current_order = event_state.order[event.name]
    local compact_order = {}
    for _, listener_id in ipairs(current_order) do
        local listener = event_state.listeners[listener_id]
        if listener and listener.event_name == event.name then
            compact_order[#compact_order + 1] = listener_id
        end
    end
    event_state.order[event.name] = compact_order

    -- 使用快照，确保回调中新注册的监听器从下一条事件开始执行。
    local snapshot = {}
    for index, listener_id in ipairs(compact_order) do
        snapshot[index] = listener_id
    end
    for _, listener_id in ipairs(snapshot) do
        local listener = event_state.listeners[listener_id]
        if listener and listener.event_name == event.name then
            if listener.run_once then
                event_state.listeners[listener_id] = nil
            end
            local ok, error_message = pcall(listener.callback, event)
            if not ok then
                dnf90.log(
                    "事件监听器 " .. listener_id .. " 出错：" ..
                    tostring(error_message))
            end
        end
    end
end

local function has_event_context_flag(flags, flag)
    return flags % (flag * 2) >= flag
end

-- 内部接口：dnf90._dispatch_client_event(name, sequence, process_id, ui_thread_id, ...)
-- 中文说明：由 DLL 工作线程从固定容量原生队列取出事件后调用；业务脚本不要主动调用或覆盖。
function dnf90._dispatch_client_event(
    name, sequence, process_id, ui_thread_id,
    context_flags, dungeon_id,
    room_x, room_y, room_layer_flag, map_id,
    previous_room_x, previous_room_y,
    previous_room_layer_flag, previous_map_id)
    if name ~= "ui_ready" and name ~= "ui_closed" and
        name ~= "enter_town" and name ~= "enter_dungeon" and
        name ~= "dungeon_room_changed" and name ~= "leave_dungeon" then
        dnf90.log("忽略未知客户端事件：" .. tostring(name))
        return
    end
    local flags = tonumber(context_flags) or 0
    local event = {
        name = name,
        sequence = tonumber(sequence),
        process_id = tonumber(process_id),
        ui_thread_id = tonumber(ui_thread_id),
        context_flags = flags,
    }
    if has_event_context_flag(flags, 1) then
        event.dungeon_id = tonumber(dungeon_id)
    end
    if has_event_context_flag(flags, 2) then
        event.room_x = tonumber(room_x)
        event.room_y = tonumber(room_y)
        event.room_layer_flag = tonumber(room_layer_flag)
        event.map_id = tonumber(map_id)
        event.is_boss_room = has_event_context_flag(flags, 8)
    end
    if has_event_context_flag(flags, 4) then
        event.previous_room_x = tonumber(previous_room_x)
        event.previous_room_y = tonumber(previous_room_y)
        event.previous_room_layer_flag = tonumber(previous_room_layer_flag)
        event.previous_map_id = tonumber(previous_map_id)
    end
    dispatch_client_callback("on_event", event)
    dispatch_registered_listeners(event)
    dispatch_client_callback("on_" .. name, event)
end

-- 功能名称：Lua 副本计时器
-- 中文说明：进图开始计时，按去重后的房间事件统计经过次数，进入 BOSS 坐标时提示，离开时显示总结。
-- 热重载约束：只保留纯数据状态并重新注册监听器，旧 Lua 函数不会残留。
local previous_dungeon_tracker_state =
    rawget(_G, "__dnf90_dungeon_tracker_state")
local dungeon_tracker_state = {
    enabled = true,
    active = false,
    started_at_ms = nil,
    ended_at_ms = nil,
    dungeon_id = nil,
    room_count = 0,
    room_x = nil,
    room_y = nil,
    room_layer_flag = nil,
    map_id = nil,
    is_boss_room = false,
    boss_notified = false,
}
if type(previous_dungeon_tracker_state) == "table" then
    dungeon_tracker_state.enabled =
        previous_dungeon_tracker_state.enabled ~= false
    if tonumber(previous_dungeon_tracker_state.started_at_ms) then
        dungeon_tracker_state.active =
            previous_dungeon_tracker_state.active == true
        dungeon_tracker_state.started_at_ms =
            tonumber(previous_dungeon_tracker_state.started_at_ms)
        dungeon_tracker_state.ended_at_ms =
            tonumber(previous_dungeon_tracker_state.ended_at_ms)
        dungeon_tracker_state.dungeon_id =
            tonumber(previous_dungeon_tracker_state.dungeon_id)
        dungeon_tracker_state.room_count =
            tonumber(previous_dungeon_tracker_state.room_count) or 0
        dungeon_tracker_state.room_x =
            tonumber(previous_dungeon_tracker_state.room_x)
        dungeon_tracker_state.room_y =
            tonumber(previous_dungeon_tracker_state.room_y)
        dungeon_tracker_state.room_layer_flag =
            tonumber(previous_dungeon_tracker_state.room_layer_flag)
        dungeon_tracker_state.map_id =
            tonumber(previous_dungeon_tracker_state.map_id)
        dungeon_tracker_state.is_boss_room =
            previous_dungeon_tracker_state.is_boss_room == true
        dungeon_tracker_state.boss_notified =
            previous_dungeon_tracker_state.boss_notified == true
    end
end
_G.__dnf90_dungeon_tracker_state = dungeon_tracker_state

local function dungeon_elapsed_ms()
    if not dungeon_tracker_state.started_at_ms then
        return 0
    end
    local finish_at = dungeon_tracker_state.active and
        clock_ms() or dungeon_tracker_state.ended_at_ms
    if not finish_at or finish_at < dungeon_tracker_state.started_at_ms then
        return 0
    end
    return math.floor(finish_at - dungeon_tracker_state.started_at_ms)
end

local function format_dungeon_duration(elapsed_ms)
    local total_seconds = math.floor(math.max(0, elapsed_ms) / 1000)
    local hours = math.floor(total_seconds / 3600)
    local minutes = math.floor(total_seconds / 60) % 60
    local seconds = total_seconds % 60
    if hours > 0 then
        return string.format("%d:%02d:%02d", hours, minutes, seconds)
    end
    return string.format("%02d:%02d", minutes, seconds)
end

local function update_dungeon_tracker_context(event)
    if event.dungeon_id ~= nil then
        dungeon_tracker_state.dungeon_id = event.dungeon_id
    end
    if event.room_x ~= nil then
        dungeon_tracker_state.room_x = event.room_x
        dungeon_tracker_state.room_y = event.room_y
        dungeon_tracker_state.room_layer_flag = event.room_layer_flag
        dungeon_tracker_state.map_id = event.map_id
        dungeon_tracker_state.is_boss_room =
            event.is_boss_room == true
    end
end

local function notify_dungeon_tracker(text)
    if not kDungeonTrackerNoticeEnabled then return end
    if dungeon_tracker_state.enabled and not dnf90.notify(text) then
        dnf90.log("副本计时提示入队失败：" .. text)
    end
end

-- 接口名称：dnf90.set_dungeon_tracker_enabled(enabled)
-- 中文说明：启用或关闭副本计时器的游戏内提示；关闭后仍继续维护可查询的运行数据。
-- 参数：enabled 必须为布尔值。
-- 返回值：当前启用状态。
function dnf90.set_dungeon_tracker_enabled(enabled)
    if type(enabled) ~= "boolean" then
        error("enabled 必须是布尔值", 2)
    end
    dungeon_tracker_state.enabled = enabled
    return dungeon_tracker_state.enabled
end

-- 接口名称：dnf90.get_dungeon_run()
-- 中文说明：返回当前或最近一次副本的只读快照，业务脚本修改返回表不会影响内部状态。
-- 返回字段：active、enabled、elapsed_ms、dungeon_id、room_count、room_x、room_y、
--           room_layer_flag、map_id 和 is_boss_room。
function dnf90.get_dungeon_run()
    return {
        active = dungeon_tracker_state.active,
        enabled = dungeon_tracker_state.enabled,
        elapsed_ms = dungeon_elapsed_ms(),
        dungeon_id = dungeon_tracker_state.dungeon_id,
        room_count = dungeon_tracker_state.room_count,
        room_x = dungeon_tracker_state.room_x,
        room_y = dungeon_tracker_state.room_y,
        room_layer_flag = dungeon_tracker_state.room_layer_flag,
        map_id = dungeon_tracker_state.map_id,
        is_boss_room = dungeon_tracker_state.is_boss_room,
    }
end

local function track_enter_dungeon(event)
    dungeon_tracker_state.active = true
    dungeon_tracker_state.started_at_ms = clock_ms()
    dungeon_tracker_state.ended_at_ms = nil
    dungeon_tracker_state.dungeon_id = nil
    dungeon_tracker_state.room_count = 1
    dungeon_tracker_state.room_x = nil
    dungeon_tracker_state.room_y = nil
    dungeon_tracker_state.room_layer_flag = nil
    dungeon_tracker_state.map_id = nil
    dungeon_tracker_state.is_boss_room = false
    dungeon_tracker_state.boss_notified = false
    update_dungeon_tracker_context(event)

    local dungeon_text = event.dungeon_id and
        ("副本 #" .. event.dungeon_id) or "副本"
    if event.is_boss_room then
        dungeon_tracker_state.boss_notified = true
        notify_dungeon_tracker(dungeon_text .. " 计时开始｜已进入 BOSS 房")
    else
        notify_dungeon_tracker(dungeon_text .. " 计时开始｜房间 1")
    end
end

local function track_dungeon_room_changed(event)
    if not dungeon_tracker_state.active then
        return
    end
    dungeon_tracker_state.room_count =
        dungeon_tracker_state.room_count + 1
    update_dungeon_tracker_context(event)
    if event.is_boss_room and not dungeon_tracker_state.boss_notified then
        dungeon_tracker_state.boss_notified = true
        notify_dungeon_tracker(
            "已进入 BOSS 房｜用时 " ..
            format_dungeon_duration(dungeon_elapsed_ms()) ..
            "｜房间 " .. dungeon_tracker_state.room_count)
    end
end

local function track_leave_dungeon(event)
    if not dungeon_tracker_state.active then
        return
    end
    update_dungeon_tracker_context(event)
    dungeon_tracker_state.ended_at_ms = clock_ms()
    dungeon_tracker_state.active = false
    notify_dungeon_tracker(
        "副本结束｜用时 " ..
        format_dungeon_duration(dungeon_elapsed_ms()) ..
        "｜经过 " .. dungeon_tracker_state.room_count .. " 个房间")
end

dungeon_tracker_state.listener_ids = {
    dnf90.on("enter_dungeon", track_enter_dungeon),
    dnf90.on("dungeon_room_changed", track_dungeon_room_changed),
    dnf90.on("leave_dungeon", track_leave_dungeon),
}

-- Boss mutation is intentionally split across three owners: this Lua worker
-- detects the one-time half-HP crossing, 90CN.dll binds the request to the
-- exact current op29 actor, and the server owns the 10% lottery. The approved
-- mutation itself is scheduled onto that monster's next native AI tick.
local previous_boss_mutation_state =
    rawget(_G, "__dnf90_boss_mutation_state")
local boss_mutation_state = {
    attempts = type(previous_boss_mutation_state) == "table" and
        previous_boss_mutation_state.attempts or {},
}
_G.__dnf90_boss_mutation_state = boss_mutation_state

local function reset_boss_mutation_state()
    boss_mutation_state.attempts = {}
end

local function poll_boss_mutation_decisions()
    for _ = 1, 32 do
        local available, decision = native_poll_boss_mutation_decision()
        if available ~= true or type(decision) ~= "table" then
            break
        end
        local monster_id = tonumber(decision.monster_id)
        local attempt = monster_id and
            boss_mutation_state.attempts[monster_id] or nil
        if attempt then
            attempt.settled = true
            attempt.allowed = tonumber(decision.allowed) == 1
            attempt.reason = tonumber(decision.reason) or -1
            if attempt.allowed then
                attempt.queued =
                    native_apply_boss_mutation(monster_id) == true
                if attempt.queued then
                    dnf90.log(
                        "Boss变异已获服务端允许并排入原生AI线程：monster_id=" ..
                        monster_id .. "，monster_index=" ..
                        tostring(decision.monster_index))
                end
            end
        end
    end
end

local function detect_boss_mutation_crossings()
    poll_boss_mutation_decisions()
    for _, monster in ipairs(dnf90.get_monsters()) do
        local monster_id = tonumber(monster.monster_id)
        local attempt = monster_id and
            boss_mutation_state.attempts[monster_id] or nil
        if attempt and attempt.allowed and attempt.queued and
                monster.mutated == true and not attempt.notified then
            attempt.applied = true
            attempt.notified = true
            dnf90.notify("Boss发生变异！")
            dnf90.log(
                "Boss变异原生属性写入完成：monster_id=" .. monster_id ..
                "，monster_index=" .. tostring(monster.monster_index))
        end
        if monster_id and monster.hp_snapshot_valid == true and
                monster.half_health_crossed == true and
                monster.mutated ~= true and
                not boss_mutation_state.attempts[monster_id] then
            local accepted =
                native_request_boss_mutation(monster_id) == true
            boss_mutation_state.attempts[monster_id] = {
                requested = accepted,
                settled = false,
                allowed = false,
                queued = false,
                applied = false,
                notified = false,
            }
        end
    end
end

boss_mutation_state.listener_ids = {
    dnf90.on("enter_dungeon", reset_boss_mutation_state),
    dnf90.on("dungeon_room_changed", reset_boss_mutation_state),
    dnf90.on("leave_dungeon", reset_boss_mutation_state),
    dnf90.on("enter_town", reset_boss_mutation_state),
}
boss_mutation_state.timer_id = dnf90.set_interval(250, function()
    detect_boss_mutation_crossings()
end)

-- The debug panel is intentionally limited to data exposed by this Lua bridge.
-- It has no object addresses, memory writes, or packet construction path.
local previous_debug_panel_state = rawget(_G, "__dnf90_debug_panel_state")
local debug_panel_state = {
    -- Keep the intrusive read-only overlay hidden by default. The runtime
    -- debug mode stays active; callers may enable this visual aid explicitly.
    enabled = false,
    revision = 0,
    latest_event_sequence = 0,
    latest_event_type = 0,
    context = 0,
    source_signature = nil,
    submit_pending = false,
}
if type(previous_debug_panel_state) == "table" then
    debug_panel_state.revision =
        tonumber(previous_debug_panel_state.revision) or 0
    debug_panel_state.latest_event_sequence =
        tonumber(previous_debug_panel_state.latest_event_sequence) or 0
    debug_panel_state.latest_event_type =
        tonumber(previous_debug_panel_state.latest_event_type) or 0
    debug_panel_state.context =
        tonumber(previous_debug_panel_state.context) or 0
end
_G.__dnf90_debug_panel_state = debug_panel_state

local debug_event_types = {
    ui_ready = 1,
    enter_town = 2,
    enter_dungeon = 3,
    dungeon_room_changed = 4,
    leave_dungeon = 5,
}

local function debug_panel_integer(value)
    return bounded_nonnegative_integer(value)
end

local function debug_panel_signed_integer(value)
    local number = tonumber(value) or 0
    if number ~= number then return 0 end
    if number > 2000000 then return 2000000 end
    if number < -2000000 then return -2000000 end
    return math.floor(number)
end

local function build_debug_panel_values()
    local stats = dnf90.get_character_stats()
    local hp_max = stats and debug_panel_integer(stats.hp_max) or 0
    local mp_max = stats and debug_panel_integer(stats.mp_max) or 0
    local character_valid = hp_max > 0 and mp_max > 0
    local equipment = dnf90.get_equipment_snapshot()
    local combat = dnf90.get_combat_power()
    local run = dnf90.get_dungeon_run()
    local dungeon_valid = debug_panel_state.context == 2 and
        run.active == true and debug_panel_integer(run.dungeon_id) > 0 and
        run.room_x ~= nil and run.room_y ~= nil and
        run.room_layer_flag ~= nil and run.map_id ~= nil
    local combat_score = combat and debug_panel_integer(combat.total_score) or 0
    local combat_valid = combat ~= nil and combat.total_score ~= nil
    return {
        enabled = debug_panel_state.enabled and 1 or 0,
        character_valid = character_valid and 1 or 0,
        hp_max = hp_max,
        mp_max = mp_max,
        strength = stats and debug_panel_signed_integer(stats.strength) or 0,
        intelligence = stats and debug_panel_signed_integer(stats.intelligence) or 0,
        move_speed = stats and debug_panel_integer(stats.move_speed) or 0,
        attack_speed = stats and debug_panel_integer(stats.attack_speed) or 0,
        cast_speed = stats and debug_panel_integer(stats.cast_speed) or 0,
        equipped_item_count = equipment and
            debug_panel_integer(equipment.item_count) or 0,
        combat_valid = combat_valid and 1 or 0,
        combat_score = combat_score,
        event_valid = debug_panel_state.latest_event_type > 0 and
            debug_panel_state.latest_event_sequence > 0 and 1 or 0,
        event_sequence = debug_panel_integer(
            debug_panel_state.latest_event_sequence),
        event_type = debug_panel_integer(debug_panel_state.latest_event_type),
        context = debug_panel_integer(debug_panel_state.context),
        dungeon_valid = dungeon_valid and 1 or 0,
        dungeon_id = dungeon_valid and debug_panel_integer(run.dungeon_id) or 0,
        room_x = dungeon_valid and debug_panel_integer(run.room_x) or 0,
        room_y = dungeon_valid and debug_panel_integer(run.room_y) or 0,
        room_layer_flag = dungeon_valid and
            debug_panel_integer(run.room_layer_flag) or 0,
        map_id = dungeon_valid and debug_panel_integer(run.map_id) or 0,
        room_count = dungeon_valid and debug_panel_integer(run.room_count) or 0,
    }
end

local function debug_panel_signature(values)
    return table.concat({
        values.enabled, values.character_valid, values.hp_max, values.mp_max,
        values.strength, values.intelligence, values.move_speed,
        values.attack_speed, values.cast_speed, values.equipped_item_count,
        values.combat_valid, values.combat_score, values.event_valid,
        values.event_sequence, values.event_type, values.context,
        values.dungeon_valid, values.dungeon_id, values.room_x, values.room_y,
        values.room_layer_flag, values.map_id, values.room_count,
    }, ":")
end

local function submit_debug_panel(force)
    if not kDebugPanelEnabled then return false end
    if type(native_update_debug_panel) ~= "function" then
        return false
    end
    local values = build_debug_panel_values()
    local signature = debug_panel_signature(values)
    if not force and signature == debug_panel_state.source_signature and
        not debug_panel_state.submit_pending then
        return true
    end
    debug_panel_state.revision = debug_panel_state.revision + 1
    if debug_panel_state.revision > 2000000000 then
        debug_panel_state.revision = 1
    end
    local accepted = native_update_debug_panel(
        values.enabled, debug_panel_state.revision,
        values.character_valid, values.hp_max, values.mp_max,
        values.strength, values.intelligence, values.move_speed,
        values.attack_speed, values.cast_speed, values.equipped_item_count,
        values.combat_valid, values.combat_score, values.event_valid,
        values.event_sequence, values.event_type, values.context,
        values.dungeon_valid, values.dungeon_id, values.room_x, values.room_y,
        values.room_layer_flag, values.map_id, values.room_count)
    debug_panel_state.source_signature = signature
    debug_panel_state.submit_pending = accepted ~= true
    return accepted == true
end

function dnf90.set_debug_panel_enabled(enabled)
    if type(enabled) ~= "boolean" then
        error("enabled must be a boolean", 2)
    end
    debug_panel_state.enabled = enabled
    return submit_debug_panel(true)
end

function dnf90.get_debug_panel_state()
    local values = build_debug_panel_values()
    return {
        enabled = debug_panel_state.enabled,
        revision = debug_panel_state.revision,
        latest_event_sequence = values.event_sequence,
        latest_event_type = values.event_type,
        context = values.context,
        character_valid = values.character_valid == 1,
        hp_max = values.hp_max,
        mp_max = values.mp_max,
        strength = values.strength,
        intelligence = values.intelligence,
        move_speed = values.move_speed,
        attack_speed = values.attack_speed,
        cast_speed = values.cast_speed,
        equipped_item_count = values.equipped_item_count,
        combat_valid = values.combat_valid == 1,
        combat_score = values.combat_score,
        dungeon_valid = values.dungeon_valid == 1,
        dungeon_id = values.dungeon_id,
        room_x = values.room_x,
        room_y = values.room_y,
        room_layer_flag = values.room_layer_flag,
        map_id = values.map_id,
        room_count = values.room_count,
    }
end

local function update_debug_panel_event(event)
    local event_type = debug_event_types[event.name]
    if not event_type then return end
    debug_panel_state.latest_event_type = event_type
    debug_panel_state.latest_event_sequence = tonumber(event.sequence) or 0
    if event.name == "enter_town" or event.name == "leave_dungeon" then
        debug_panel_state.context = 1
    elseif event.name == "enter_dungeon" or
        event.name == "dungeon_room_changed" then
        debug_panel_state.context = 2
    end
    submit_debug_panel(true)
end

debug_panel_state.listener_ids = {
    dnf90.on("ui_ready", update_debug_panel_event),
    dnf90.on("enter_town", update_debug_panel_event),
    dnf90.on("enter_dungeon", update_debug_panel_event),
    dnf90.on("dungeon_room_changed", update_debug_panel_event),
    dnf90.on("leave_dungeon", update_debug_panel_event),
}
submit_debug_panel(true)
debug_panel_state.timer_id = dnf90.set_interval(250, function()
    submit_debug_panel(false)
end)

-- 回调名称：dnf90.on_reload()
-- 中文说明：每次脚本首次加载或热重载成功后，由 DLL 在同一 Lua 工作线程调用一次。
-- 参数：无；需要加载次数时读取 _G.__90cn_lua_load_count。
-- 返回值：忽略。用户可以替换此函数，但不要在回调内再次调用 dnf90.reload()。
function dnf90.on_reload()
    local info = dnf90.get_client_info()
    dnf90.log(
        "重载回调执行，次数=" .. _G.__90cn_lua_load_count ..
        "，进程=" .. info.process_id ..
        "，工作线程=" .. info.worker_thread_id)
end

dnf90.log(
    "init.lua 加载成功，累计次数=" .. _G.__90cn_lua_load_count ..
    "，插件版本=" .. dnf90.version())

-- 启动脚本不会创建演示定时器；默认启用上面的副本计时、BOSS 房和离开总结提示。
-- 可调用 dnf90.set_dungeon_tracker_enabled(false) 关闭提示，跟踪数据仍可通过 get_dungeon_run() 查询。

-- 接口名称：_G.on_90cn_lua_ready()
-- 中文说明：查询当前脚本已经成功加载的次数，保留用于兼容既有验证工具。
-- 参数：无。
-- 返回值：整数，表示首次加载和后续热重载的累计次数。
function _G.on_90cn_lua_ready()
    return _G.__90cn_lua_load_count
end
