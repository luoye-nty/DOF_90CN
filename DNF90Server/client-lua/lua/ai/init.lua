-- Loaded into each game-owned monster AI Lua 5.1 state after the original
-- PVF script has finished loading.  Stock table-driven monster AI calls
-- begin_pattern; FSM AI calls Proc and event callbacks.  Both families are
-- routed through the same persistent 90CNLua takeover plan.

local bridge = rawget(_G, "dnf90_ai")
if type(bridge) ~= "table" or type(bridge._get_plan) ~= "function" then
    return
end

local wrapped_entries = rawget(_G, "__dnf90_ai_wrapped_entries")
if type(wrapped_entries) ~= "table" then
    wrapped_entries = {}
    _G.__dnf90_ai_wrapped_entries = wrapped_entries
end

local function integer(value, fallback)
    local number = tonumber(value)
    if not number or number ~= number then
        return fallback
    end
    if number >= 0 then
        return math.floor(number)
    end
    return math.ceil(number)
end

local function has_field(field_mask, field)
    return math.floor(field_mask / field) % 2 == 1
end

local function write_plan(monster_id, action, move_method,
        destination_select, destination_param1, destination_param2,
        field_mask)
    if has_field(field_mask, 1) then
        MyInfo.nextAction = integer(action, -1)
    end
    if has_field(field_mask, 2) then
        MyInfo.nextMoveMethod = integer(move_method, -1)
    end
    if has_field(field_mask, 4) then
        MyInfo.nextDestinationSelect = integer(destination_select, -1)
    end
    if has_field(field_mask, 8) then
        MyInfo.nextDestinationParam1 = integer(destination_param1, -1)
    end
    if has_field(field_mask, 16) then
        MyInfo.nextDestinationParam2 = integer(destination_param2, -1)
    end
    _G.__dnf90_ai_monster_id = integer(monster_id, 0)
    -- Compatibility alias retained for 1.15.x diagnostics.
    _G.__dnf90_ai_object_key = _G.__dnf90_ai_monster_id
end

local retreat_actions = {
    [7] = true,  -- Botticelli Hide
    [16] = true, -- Botticelli Teleport
}

local function target_is_near()
    if type(Patrol) ~= "table" or
            type(Patrol.isTargetAttackArea) ~= "function" then
        return nil
    end
    local ok, result = pcall(
        Patrol.isTargetAttackArea, Patrol, 80, 20, 20, 20)
    if not ok then
        return nil
    end
    return result == true
end

local function skill_is_ready(action)
    if type(Battle) ~= "table" or
            type(Battle.isSkillCooltime) ~= "function" then
        return false
    end
    local ok, in_cooltime = pcall(
        Battle.isSkillCooltime, Battle, action)
    return ok and in_cooltime ~= true
end

local function rewrite_retreat_action()
    local original_action = integer(MyInfo.nextAction, -1)
    if retreat_actions[original_action] ~= true then
        return
    end
    local near = target_is_near()
    if near == nil then
        return
    end
    local candidates = near and
        {2, 6, 15, 0} or {6, 15, 2, 0}
    local smart_action = -1
    for _, candidate in ipairs(candidates) do
        if skill_is_ready(candidate) then
            smart_action = candidate
            break
        end
    end
    MyInfo.nextAction = smart_action
    if type(bridge._report_smart_action) == "function" then
        pcall(bridge._report_smart_action,
            original_action,
            smart_action < 0 and 65535 or smart_action,
            near and 1 or 0)
    end
end

local function make_takeover_entry(original)
    return function(...)
        local takeover, monster_id, action, move_method, destination_select,
            destination_param1, destination_param2, mode, field_mask =
                bridge._get_plan()
        if takeover ~= true or type(MyInfo) ~= "table" then
            return original(...)
        end
        mode = integer(mode, 0)
        field_mask = integer(field_mask, 31)
        if field_mask < 1 or field_mask > 31 then
            field_mask = 31
        end
        if mode == 2 then
            -- Smart aggression keeps every stock decision except the exact
            -- retreat actions proven by this Boss's PVF.  Those are replaced
            -- using the live target distance and real skill cooldown checks.
            original(...)
            rewrite_retreat_action()
            write_plan(monster_id, action, move_method, destination_select,
                destination_param1, destination_param2, field_mask)
            return
        end
        if mode == 1 then
            -- Overlay mode keeps the stock action/skill/cooldown decision,
            -- then replaces only explicitly selected next* fields.  This is
            -- used by persistent aggro chase so the Boss still attacks.
            original(...)
            write_plan(monster_id, action, move_method, destination_select,
                destination_param1, destination_param2, field_mask)
            return
        end
        -- The client reads these stock PVF fields after the entry returns.
        -- Suppressing the original callback prevents it from overwriting the
        -- persistent plan during takeover.
        write_plan(monster_id, action, move_method, destination_select,
            destination_param1, destination_param2, field_mask)
    end
end

-- begin_pattern is the current monster/PVF decision entry.  Proc and the
-- explicit callbacks cover the older FSM family without wrapping unrelated
-- globals from required support libraries.
local takeover_entries = {
    "begin_pattern",
    "Proc",
    "ProcDamage_Down",
    "OnEvent",
    "OnActionEnd",
    "OnChangeState",
    "OnActionSkillEnd",
    "OnUseSkill",
    "OnAttack",
    "OnDamage",
    "OnMyDie",
    "OnChangeTargetHitable",
    "OnWarningArea",
    "OnTargetUseSkill",
    "OnResetComboDamage",
    "OnActiveStatus",
    "OnUseCubeSkill",
    "OnStartBattle",
}

for _, name in ipairs(takeover_entries) do
    local original = rawget(_G, name)
    if type(original) == "function" and wrapped_entries[name] ~= true then
        _G["__dnf90_ai_original_" .. name] = original
        _G[name] = make_takeover_entry(original)
        wrapped_entries[name] = true
    end
end

if next(wrapped_entries) ~= nil then
    _G.__dnf90_ai_entry_wrapped = wrapped_entries
else
    _G.__dnf90_ai_entry_wrapped = nil
end
-- Compatibility marker retained for 1.15.x diagnostics.
_G.__dnf90_ai_proc_wrapped = wrapped_entries.Proc == true
