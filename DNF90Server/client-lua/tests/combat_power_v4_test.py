from pathlib import Path

from lupa import LuaRuntime


SCRIPT = Path(__file__).resolve().parents[1] / "lua" / "init.lua"


def main() -> None:
    lua = LuaRuntime(unpack_returned_tuples=True)
    lua.execute(
        r'''
        __captured_panel = nil
        __clock_ms = 1000
        dnf90 = {}
        function dnf90.log(_) end
        function dnf90.notify(_) return true end
        function dnf90.version() return "90CNLua/1.12.0" end
        function dnf90.reload() end
        function dnf90.is_ready() return true end
        function dnf90._clock_ms() return tostring(__clock_ms) end
        function dnf90._client_info()
            return "1", "2", "0x04000000", "90cn-decode-bypass-v1", "90CNLua/1.12.0"
        end
        function dnf90._character_stats()
            return true, "7", "3", "54600", "31200", "438", "438", "361", "361", "0", "0", "0"
        end
        function dnf90._equipment_snapshot()
            return true, {
                generation = "9",
                source_stat_generation = "7",
                valid_flags = "1",
                item_count = "5",
                items = {
                    ["1"] = {item_id="101", quality_seed="1", durability="45", actor_slot="12", upgrade_level="13", amplify_type="3", amplify_value="7"},
                    ["2"] = {item_id="102", quality_seed="2", durability="0", actor_slot="13", upgrade_level="13", amplify_type="0", amplify_value="0"},
                    ["3"] = {item_id="103", quality_seed="3", durability="38", actor_slot="14", upgrade_level="13", amplify_type="3", amplify_value="7"},
                    ["4"] = {item_id="104", quality_seed="4", durability="0", actor_slot="9", upgrade_level="31", amplify_type="3", amplify_value="999"},
                    ["5"] = {item_id="105", quality_seed="5", durability="0", actor_slot="26", upgrade_level="31", amplify_type="3", amplify_value="999"},
                },
            }
        end
        function dnf90._damage_affix_snapshot()
            return true, {
                generation = "11",
                valid_flags = "1",
                version = "4",
                white_damage_tenths = "370",
                yellow_damage_tenths = "200",
                critical_damage_tenths = "200",
                yellow_additional_tenths = "160",
                critical_additional_tenths = "180",
                all_attack_tenths = "350",
                equipped_item_count = "5",
                active_set_count = "3",
                job = "11",
                grow_type = "33",
                level = "90",
                physical_attack = "10000",
                magical_attack = "8000",
                independent_attack = "2500",
                pvf_equipment_score = "38835",
                profession = "剑皇",
            }
        end
        function dnf90._update_combat_panel(...)
            __captured_panel = {...}
            return true
        end
        __queued_game_command = nil
        __ai_plan = nil
        function dnf90._queue_game_command(command_type, object_key, value)
            __queued_game_command = {
                command_type = command_type,
                object_key = object_key,
                value = value,
            }
            return true
        end
        function dnf90._actor_snapshot(requested_key)
            return true, {
                generation = "12",
                ui_thread_id = "34",
                captured_tick = "56",
                actor_count = "1",
                actors = {
                    ["1"] = {
                        object_key = tostring(requested_key or 77),
                        flags = "5", x = "100", y = "-20", z = "3",
                    },
                },
            }
        end
        function dnf90._ai_controllers()
            return {
                count = "1",
                items = {
                    ["1"] = {
                        object_key = "77", age_ms = "25", takeover = "0",
                        plan_mode = "0", field_mask = "0",
                        controller_count = "1", state_count = "0",
                        native_lua = "0", universal_control = "1",
                        decision_count = "4", decision_override_count = "2",
                        last_decision_kind = "1", last_native_decision = "3",
                        last_applied_decision = "0", decision_age_ms = "10",
                    },
                },
            }
        end
        function dnf90._ai_set_plan(
                key, action, move, destination, p1, p2, mode, field_mask)
            __ai_plan_count = (__ai_plan_count or 0) + 1
            __ai_plan = {
                key = key, action = action, move = move,
                destination = destination, p1 = p1, p2 = p2,
                mode = mode, field_mask = field_mask,
            }
            return true
        end
        function dnf90._ai_release(key)
            __ai_released = key
            return true
        end
        function dnf90._ai_release_all() return true end
        '''
    )
    lua.execute(SCRIPT.read_text(encoding="utf-8"))

    assert lua.eval("dnf90.self_play_motion(3055)") is True
    assert lua.eval("__queued_game_command.command_type") == 1
    assert lua.eval("__queued_game_command.value") == 3055
    assert lua.eval("dnf90.actor_apply_mood(77, 9)") is True
    assert lua.eval("__queued_game_command.command_type") == 2
    assert lua.eval("__queued_game_command.object_key") == 77

    actor = lua.eval("dnf90.get_actor(77)")
    assert actor["object_key"] == 77
    assert actor["x"] == 100
    assert actor["y"] == -20
    monsters = lua.eval("dnf90.get_monsters()")
    assert monsters[1]["monster_id"] == 77
    assert monsters[1]["object_key"] == 77
    assert monsters[1]["takeover"] is False
    assert monsters[1]["controller_count"] == 1
    assert monsters[1]["state_count"] == 0
    assert monsters[1]["native_lua"] is False
    assert monsters[1]["universal_control"] is True
    assert monsters[1]["decision_count"] == 4
    assert monsters[1]["decision_override_count"] == 2
    assert monsters[1]["last_native_decision"] == 3
    assert monsters[1]["last_applied_decision"] == 0

    # The shipped policy is enabled by default. It discovers table-driven
    # native controllers and overlays only CHASE(0), leaving stock attacks,
    # skills, cooldowns and phase logic untouched.
    all_ai = lua.eval("dnf90.get_all_monster_aggression_state()")
    assert all_ai["enabled"] is True
    assert all_ai["applied_total"] == 1
    assert all_ai["managed_count"] == 1
    assert lua.eval("__ai_plan.destination") == 0
    assert lua.eval("__ai_plan.mode") == 2
    assert lua.eval("__ai_plan.field_mask") == 4
    assert lua.eval("dnf90.set_all_monster_aggression_enabled(false)") is True
    assert lua.eval("__ai_released") == 77
    assert lua.eval("dnf90.get_all_monster_aggression_state().enabled") is False
    assert lua.eval("dnf90.set_all_monster_aggression_enabled(true)") is True
    assert lua.eval("dnf90.get_all_monster_aggression_state().managed_count") == 1
    lua.execute("__clock_ms = 1300; dnf90._dispatch_timers()")
    assert lua.eval("dnf90.get_all_monster_aggression_state().scan_count") >= 3
    assert lua.eval(
        "dnf90.set_monster_plan(77, {action=8, move_method=1, "
        "destination_select=38, destination_param1=100, destination_param2=-20})"
    ) is True
    assert lua.eval("__ai_plan.key") == 77
    assert lua.eval("__ai_plan.action") == 8
    assert lua.eval("__ai_plan.destination") == 38
    assert lua.eval("__ai_plan.mode") == 0
    assert lua.eval("__ai_plan.field_mask") == 31
    assert lua.eval("dnf90.set_monster_aggression(77)") is True
    assert lua.eval("__ai_plan.action") == -1
    assert lua.eval("__ai_plan.move") == -1
    assert lua.eval("__ai_plan.destination") == 0
    assert lua.eval("__ai_plan.p1") == -1
    assert lua.eval("__ai_plan.p2") == -1
    assert lua.eval("__ai_plan.mode") == 2
    assert lua.eval("__ai_plan.field_mask") == 4

    equipment = lua.eval("dnf90.get_equipment_snapshot()")
    assert equipment["item_count"] == 5
    assert equipment["items"][1]["actor_slot"] == 12
    equipment["items"][1]["item_id"] = 999
    assert lua.eval("dnf90.get_equipment_snapshot().items[1].item_id") == 101

    affixes = lua.eval("dnf90.get_damage_affixes()")
    assert affixes["white_damage"] == 37
    assert affixes["yellow_damage"] == 20
    assert affixes["critical_damage"] == 20
    assert affixes["yellow_additional"] == 16
    assert affixes["critical_additional"] == 18
    assert affixes["all_attack"] == 35
    assert affixes["profession"] == "剑皇"
    assert affixes["level"] == 90

    breakdown = lua.eval(
        "dnf90.calculate_equipment_score(dnf90.get_equipment_snapshot(), "
        "dnf90.get_damage_affixes(), 37574, 20500)"
    )
    assert breakdown["item_count"] == 5
    assert breakdown["occupancy_score"] == 1000
    assert breakdown["upgrade_score"] == 24290
    assert breakdown["amplification_score"] == 40240
    assert breakdown["affix_score"] == 59036
    assert breakdown["three_attack_bonus"] == 7174
    assert breakdown["pvf_equipment_score"] == 38835
    assert breakdown["score"] == 170575

    result = lua.eval(
        "dnf90.calculate_combat_power(dnf90.get_character_stats(), "
        "dnf90.get_equipment_snapshot(), dnf90.get_damage_affixes())"
    )
    assert result["base_attribute_score"] == 37574
    assert result["equipment_score"] == 170575
    assert result["total_score"] == 208149
    assert result["rank"] == "武炼"
    assert result["formula_version"] == 6

    rank_cases = (
        (3099, "探索"), (3100, "开拓"), (8099, "开拓"),
        (8100, "无畏"), (22999, "无畏"), (23000, "征服"),
        (51999, "征服"), (52000, "战绝"), (64999, "战绝"),
        (65000, "英杰"), (129999, "英杰"), (130000, "武炼"),
    )
    for score, want_rank in rank_cases:
        assert lua.eval(f"dnf90.get_combat_power_rank({score})") == want_rank

    base_only = lua.eval(
        "dnf90.calculate_combat_power(dnf90.get_character_stats(), nil, nil)"
    )
    assert base_only["total_score"] == 17074
    assert base_only["equipment_score"] is None

    panel = lua.globals().__captured_panel
    assert panel[5] == 208149
    assert panel[6] == 37574
    assert panel[7] == 1
    assert panel[8] == 170575
    assert panel[15] == 6
    assert panel[16] == 1
    assert panel[18] == 370
    assert panel[19] == 200
    assert panel[20] == 200
    assert panel[21] == 160
    assert panel[22] == 180
    assert panel[23] == 350
    assert panel[24] == 5
    assert panel[25] == 3
    assert panel[26] == 11
    assert panel[27] == 33
    assert panel[28] == 90
    assert panel[29] == 10000
    assert panel[30] == 8000
    assert panel[31] == 2500
    assert panel[32] == "剑皇"
    print("combat_power_v6_test: PASS")


if __name__ == "__main__":
    main()
