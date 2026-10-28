from pathlib import Path

from lupa import LuaRuntime


SCRIPT = Path(__file__).resolve().parents[1] / "lua" / "ai" / "init.lua"
PLUGIN_SOURCE = Path(__file__).resolve().parents[1] / "90CNLua.cpp"


def main() -> None:
    plugin_source = PLUGIN_SOURCE.read_text(encoding="utf-8")
    refresh_start = plugin_source.index("bool RefreshNativeMonsterCombatSnapshot")
    refresh_end = plugin_source.index("uintptr_t EncodedValueReaderRva", refresh_start)
    refresh_source = plugin_source[refresh_start:refresh_end]
    signature_gate = refresh_source.index(
        "g_gameBase + kNativeMonsterStatBlockGetterRva"
    )
    first_virtual_call = refresh_source.index("getIndex(combat)")
    assert signature_gate < first_virtual_call
    assert "vtable + kNativeMonsterStatBlockVtableOffset" in refresh_source
    assert "using NativeMonsterMaxHpGetter = __int64 (__thiscall*)(void*);" in plugin_source
    assert "const __int64 maxHP = getMaxHP(combat);" in refresh_source
    assert "getMaxHP(combat, 0, 1)" not in plugin_source
    assert "NativeMonsterHpSetter" not in plugin_source
    assert "kEncodedInt64WriteRva = 0x030DB870" in plugin_source
    assert "WriteNativeEncodedInt64Value(hpState, maxHP)" in plugin_source
    assert "g_gameBase + kNativeMonsterCurrentHpGetterRva" in refresh_source
    assert "g_gameBase + kNativeMonsterMaxHpGetterRva" in refresh_source

    lua = LuaRuntime(unpack_returned_tuples=True)
    lua.execute(
        r'''
        __original_proc_calls = 0
        __original_pattern_calls = 0
        __original_damage_calls = 0
        MyInfo = {}
        function Proc()
            __original_proc_calls = __original_proc_calls + 1
        end
        function begin_pattern()
            __original_pattern_calls = __original_pattern_calls + 1
            MyInfo.nextAction = __stock_action or 22
            MyInfo.nextMoveMethod = 0
            MyInfo.nextDestinationSelect = 3
            MyInfo.nextDestinationParam1 = 10
            MyInfo.nextDestinationParam2 = 20
        end
        function OnDamage()
            __original_damage_calls = __original_damage_calls + 1
        end
        dnf90_ai = {}
        __smart_report = nil
        function dnf90_ai._report_smart_action(original, replacement, near)
            __smart_report = {
                original = original, replacement = replacement, near = near,
            }
            return true
        end
        function dnf90_ai._get_plan()
            return false, "77", "-1", "-1", "-1", "-1", "-1", "0", "0"
        end
        '''
    )
    source = SCRIPT.read_text(encoding="utf-8")
    lua.execute(source)
    lua.execute("Proc()")
    lua.execute("begin_pattern()")
    lua.execute("OnDamage()")
    assert lua.eval("__original_proc_calls") == 1
    assert lua.eval("__original_pattern_calls") == 1
    assert lua.eval("__original_damage_calls") == 1

    lua.execute(
        r'''
        function dnf90_ai._get_plan()
            return true, "77", "8", "1", "38", "100", "-20", "0", "31"
        end
        begin_pattern()
        Proc()
        OnDamage()
        '''
    )
    assert lua.eval("__original_proc_calls") == 1
    assert lua.eval("__original_pattern_calls") == 1
    assert lua.eval("__original_damage_calls") == 1
    assert lua.eval("MyInfo.nextAction") == 8
    assert lua.eval("MyInfo.nextMoveMethod") == 1
    assert lua.eval("MyInfo.nextDestinationSelect") == 38
    assert lua.eval("MyInfo.nextDestinationParam1") == 100
    assert lua.eval("MyInfo.nextDestinationParam2") == -20
    assert lua.eval("__dnf90_ai_monster_id") == 77
    assert lua.eval("__dnf90_ai_object_key") == 77
    assert lua.eval("__dnf90_ai_wrapped_entries.begin_pattern") is True
    assert lua.eval("__dnf90_ai_wrapped_entries.Proc") is True
    assert lua.eval("__dnf90_ai_wrapped_entries.OnDamage") is True
    assert lua.eval("type(__dnf90_ai_entry_wrapped)") == "table"

    # Overlay keeps the stock action and parameters, while selected movement
    # fields persistently force a dash toward the stock current target.
    lua.execute(
        r'''
        function dnf90_ai._get_plan()
            return true, "77", "-1", "1", "0", "-1", "-1", "1", "6"
        end
        begin_pattern()
        '''
    )
    assert lua.eval("__original_pattern_calls") == 2
    assert lua.eval("MyInfo.nextAction") == 22
    assert lua.eval("MyInfo.nextMoveMethod") == 1
    assert lua.eval("MyInfo.nextDestinationSelect") == 0
    assert lua.eval("MyInfo.nextDestinationParam1") == 10
    assert lua.eval("MyInfo.nextDestinationParam2") == 20

    # Smart mode preserves stock decisions generally, but replaces the
    # Boss-proven Teleport/Hide retreat actions using live range/cooldown APIs.
    lua.execute(
        r'''
        __stock_action = 16
        Patrol = {}
        function Patrol:isTargetAttackArea(_, _, _, _) return true end
        Battle = {}
        function Battle:isSkillCooltime(_) return false end
        function dnf90_ai._get_plan()
            return true, "77", "-1", "-1", "0", "-1", "-1", "2", "4"
        end
        begin_pattern()
        '''
    )
    assert lua.eval("__original_pattern_calls") == 3
    assert lua.eval("MyInfo.nextAction") == 2
    assert lua.eval("MyInfo.nextMoveMethod") == 0
    assert lua.eval("MyInfo.nextDestinationSelect") == 0
    assert lua.eval("__smart_report.original") == 16
    assert lua.eval("__smart_report.replacement") == 2
    assert lua.eval("__smart_report.near") == 1

    # Reloading the bootstrap into the same native state is idempotent.
    lua.execute(
        r'''
        function dnf90_ai._get_plan()
            return true, "77", "8", "1", "38", "100", "-20", "0", "31"
        end
        '''
    )
    lua.execute(source)
    lua.execute("begin_pattern()")
    lua.execute("Proc()")
    assert lua.eval("__original_proc_calls") == 1
    assert lua.eval("__original_pattern_calls") == 3
    print("native_ai_bridge_test: PASS")


if __name__ == "__main__":
    main()
