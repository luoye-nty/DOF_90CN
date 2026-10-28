using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;

namespace DNFLogin.Gm
{
    /// <summary>
    /// GM 工具角色业务服务（移植 Go 版 character.go / dungeons.go / equip_slots.go / class_tree.go / class_skills.go / native_admin.go）。
    /// 策略：优先调用 DNF90Server 原生 GM 接口（实时刷新），不可用时回退 MySQL 直连（物品发放由 GmItemEngine 处理）。
    /// </summary>
    public sealed class GmCharacterService
    {
        private readonly GmDatabaseService _db;

        public GmCharacterService(GmDatabaseService db)
        {
            _db = db;
        }

        public const int UnlockAllEquipSlotsMask = 0x13;

        // ── 角色列表 ───────────────────────────────────────────

        /// <summary>按账号或角色名查询角色列表（queryType: account | character）</summary>
        public async Task<List<CharacterView>> ListCharactersAsync(GmSettings settings, string queryType, string query,
            CancellationToken cancellationToken = default)
        {
            queryType = (queryType ?? "").Trim().ToLowerInvariant();
            query = (query ?? "").Trim();
            if (query.Length == 0 || (queryType != "account" && queryType != "character"))
            {
                throw new InvalidOperationException("请选择账号或角色名并输入完整查询内容");
            }
            var username = queryType == "account" ? query : "";
            var characterName = queryType == "character" ? query : "";
            try
            {
                var native = await GmNativeClient.QueryCharactersAsync(settings, username, characterName, cancellationToken).ConfigureAwait(false);
                return native.Select(ToCharacterView).ToList();
            }
            catch (Exception ex)
            {
                // 原生接口不可用时回退直连当前模式数据库，避免"查不到"
                try
                {
                    return await QueryDirectAsync(queryType, query).ConfigureAwait(false);
                }
                catch (Exception directEx)
                {
                    throw new InvalidOperationException($"原生 GM 接口不可用且直连查询失败（{directEx.Message}）：{ex.Message}", ex);
                }
            }
        }

        /// <summary>回退直连数据库查询（按账号或角色名，SQL 兼容 MySQL / SQLite）</summary>
        public async Task<List<CharacterView>> QueryDirectAsync(string queryType, string query)
        {
            if (queryType == "character")
            {
                var role = await FindCharacterDirectAsync(query).ConfigureAwait(false);
                return new List<CharacterView> { role };
            }
            // 账号：按登录账号（dnf_login_accounts.username）查其名下角色
            var sql =
                "SELECT c.character_id, c.account_id, c.name, c.level, c.gold, " +
                "COALESCE(CAST(m.entry_value AS " + (_db.IsSqlite ? "INTEGER" : "SIGNED") + "), 0) AS cera " +
                "FROM dnf_characters AS c " +
                "JOIN dnf_login_accounts AS la ON la.account_id = c.account_id " +
                "LEFT JOIN dnf_account_metadata AS m ON m.account_id = c.account_id AND m.entry_key = 'account_cera' " +
                "WHERE la.username = " + GmDatabaseService.Esc(query) + " AND c.delete_flag = 0 " +
                "ORDER BY c.account_id, c.slot, c.character_id";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            return rows.Select(ToCharacterView).ToList();
        }

        /// <summary>直连列出全部角色（SQL 兼容 MySQL / SQLite）</summary>
        public async Task<List<CharacterView>> ListAllCharactersDirectAsync()
        {
            string sql =
                "SELECT c.character_id, c.account_id, c.name, c.level, c.gold, " +
                "COALESCE(CAST(m.entry_value AS " + (_db.IsSqlite ? "INTEGER" : "SIGNED") + "), 0) AS cera " +
                "FROM dnf_characters AS c " +
                "LEFT JOIN dnf_account_metadata AS m ON m.account_id = c.account_id AND m.entry_key = 'account_cera' " +
                "WHERE c.delete_flag = 0 ORDER BY c.account_id, c.slot, c.character_id";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            return rows.Select(ToCharacterView).ToList();
        }

        /// <summary>按角色名直连查询（SQL 兼容 MySQL / SQLite）</summary>
        public async Task<CharacterView> FindCharacterDirectAsync(string roleName)
        {
            string sql =
                "SELECT c.character_id, c.account_id, c.name, c.level, c.gold, " +
                "COALESCE(CAST(m.entry_value AS " + (_db.IsSqlite ? "INTEGER" : "SIGNED") + "), 0) AS cera " +
                "FROM dnf_characters AS c " +
                "LEFT JOIN dnf_account_metadata AS m ON m.account_id = c.account_id AND m.entry_key = 'account_cera' " +
                "WHERE c.name = " + GmDatabaseService.Esc(roleName) + " AND c.delete_flag = 0 LIMIT 1";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在：{roleName}");
            }
            return ToCharacterView(rows[0]);
        }

        private static CharacterView ToCharacterView(NativeCharacterView row)
        {
            return new CharacterView
            {
                CharacterID = row.Character_id ?? "",
                AccountID = row.Account_id ?? "",
                Name = row.Name ?? "",
                Level = row.Level,
                Gold = row.Gold,
                Cera = row.Cera,
            };
        }

        private static CharacterView ToCharacterView(Dictionary<string, object?> row)
        {
            return new CharacterView
            {
                CharacterID = GmDatabaseService.Str(row, "character_id"),
                AccountID = GmDatabaseService.Str(row, "account_id"),
                Name = GmDatabaseService.Str(row, "name"),
                Level = GmDatabaseService.Int(row, "level"),
                Gold = GmDatabaseService.Lng(row, "gold"),
                Cera = GmDatabaseService.Lng(row, "cera"),
            };
        }

        // ── 角色属性 ───────────────────────────────────────────

        public async Task<StatBlock> QueryStatsAsync(string characterId)
        {
            string sql =
                "SELECT character_id, name, level, job, grow_type, " +
                "stat_hp_max, stat_mp_max, stat_strength, stat_intelligence, stat_vitality, stat_spirit, " +
                "stat_physical_attack, stat_physical_defense, stat_magical_attack, stat_magical_defense, stat_independent_attack, " +
                "stat_fire_resistance, stat_water_resistance, stat_dark_resistance, stat_light_resistance, " +
                "active_status_resistance_00, active_status_resistance_01, active_status_resistance_02, " +
                "active_status_resistance_03, active_status_resistance_04, active_status_resistance_05, " +
                "active_status_resistance_06, active_status_resistance_07, active_status_resistance_08, " +
                "active_status_resistance_09, active_status_resistance_10, active_status_resistance_11, " +
                "active_status_resistance_12, active_status_resistance_13, active_status_resistance_14, " +
                "active_status_resistance_15, active_status_resistance_16, active_status_resistance_17, " +
                "stat_inventory_limit, stat_hp_regen_speed, stat_mp_regen_speed, " +
                "stat_move_speed, stat_attack_speed, stat_cast_speed, " +
                "stat_hit_recovery, stat_jump_power, stat_weight, stat_level " +
                "FROM dnf_characters WHERE character_id = " + GmDatabaseService.Esc(characterId) + " AND delete_flag = 0 LIMIT 1";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
            var row = rows[0];
            var stats = new StatBlock
            {
                CharacterID = GmDatabaseService.Str(row, "character_id"),
                Name = GmDatabaseService.Str(row, "name"),
                Level = GmDatabaseService.Int(row, "level"),
                Job = GmDatabaseService.Str(row, "job"),
                GrowType = GmDatabaseService.Int(row, "grow_type"),
                HPMax = GmDatabaseService.Lng(row, "stat_hp_max"),
                MPMax = GmDatabaseService.Lng(row, "stat_mp_max"),
                Strength = GmDatabaseService.Int(row, "stat_strength"),
                Intelligence = GmDatabaseService.Int(row, "stat_intelligence"),
                Vitality = GmDatabaseService.Int(row, "stat_vitality"),
                Spirit = GmDatabaseService.Int(row, "stat_spirit"),
                PhysicalAttack = GmDatabaseService.Int(row, "stat_physical_attack"),
                PhysicalDefense = GmDatabaseService.Int(row, "stat_physical_defense"),
                MagicalAttack = GmDatabaseService.Int(row, "stat_magical_attack"),
                MagicalDefense = GmDatabaseService.Int(row, "stat_magical_defense"),
                IndependentAtk = GmDatabaseService.Int(row, "stat_independent_attack"),
                FireRes = GmDatabaseService.Int(row, "stat_fire_resistance"),
                WaterRes = GmDatabaseService.Int(row, "stat_water_resistance"),
                DarkRes = GmDatabaseService.Int(row, "stat_dark_resistance"),
                LightRes = GmDatabaseService.Int(row, "stat_light_resistance"),
                InventoryLimit = GmDatabaseService.Lng(row, "stat_inventory_limit"),
                HPRegenSpeed = GmDatabaseService.Int(row, "stat_hp_regen_speed"),
                MPRegenSpeed = GmDatabaseService.Int(row, "stat_mp_regen_speed"),
                MoveSpeed = GmDatabaseService.Lng(row, "stat_move_speed"),
                AttackSpeed = GmDatabaseService.Int(row, "stat_attack_speed"),
                CastSpeed = GmDatabaseService.Int(row, "stat_cast_speed"),
                HitRecovery = GmDatabaseService.Int(row, "stat_hit_recovery"),
                JumpPower = GmDatabaseService.Int(row, "stat_jump_power"),
                Weight = GmDatabaseService.Lng(row, "stat_weight"),
                StatLevel = GmDatabaseService.Int(row, "stat_level"),
            };
            for (var i = 0; i < 18; i++)
            {
                stats.AbnormalResistance[i] = GmDatabaseService.Int(row, "active_status_resistance_" + i.ToString("D2", CultureInfo.InvariantCulture));
            }
            return stats;
        }

        // ── SP/TP ─────────────────────────────────────────────

        public async Task<SptpBlock> QuerySPTPAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            try
            {
                var native = await GmNativeClient.QuerySPTPAsync(settings, characterId, cancellationToken).ConfigureAwait(false);
                return new SptpBlock
                {
                    CharacterID = native.Character_id ?? "",
                    Name = native.Name ?? "",
                    Level = native.Level,
                    TotalSP = native.Total_sp,
                    RemainingSP = native.Remaining_sp,
                    TotalTP = native.Total_tp,
                    RemainingTP = native.Remaining_tp,
                };
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            string sql =
                "SELECT c.character_id, c.name, c.level, " +
                "COALESCE(s.total_sp, 0), COALESCE(s.remaining_sp, 0), " +
                "COALESCE(s.total_tp, 0), COALESCE(s.remaining_tp, 0) " +
                "FROM dnf_characters c " +
                "LEFT JOIN dnf_skills s ON s.character_id = c.character_id " +
                "WHERE c.character_id = " + GmDatabaseService.Esc(characterId) + " AND c.delete_flag = 0 LIMIT 1";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
            var row = rows[0];
            return new SptpBlock
            {
                CharacterID = GmDatabaseService.Str(row, "character_id"),
                Name = GmDatabaseService.Str(row, "name"),
                Level = GmDatabaseService.Int(row, "level"),
                TotalSP = GmDatabaseService.Int(row, "total_sp"),
                RemainingSP = GmDatabaseService.Int(row, "remaining_sp"),
                TotalTP = GmDatabaseService.Int(row, "total_tp"),
                RemainingTP = GmDatabaseService.Int(row, "remaining_tp"),
            };
        }

        public async Task UpdateSPTPAsync(GmSettings settings, string characterId, int? remainingSp, int? remainingTp,
            CancellationToken cancellationToken = default)
        {
            if (!remainingSp.HasValue && !remainingTp.HasValue)
            {
                throw new InvalidOperationException("至少指定 remainingSp 或 remainingTp");
            }
            try
            {
                await GmNativeClient.UpdateSPTPAsync(settings, characterId, remainingSp, remainingTp, cancellationToken).ConfigureAwait(false);
                return;
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }

            // 读当前值保证一致性
            var curRows = await _db.QueryAsync(
                "SELECT COALESCE(total_sp,0) AS total_sp, COALESCE(total_tp,0) AS total_tp FROM dnf_skills WHERE character_id = "
                + GmDatabaseService.Esc(characterId)).ConfigureAwait(false);
            var rowExists = curRows.Count > 0;
            var curTotalSP = 0;
            var curTotalTP = 0;
            if (rowExists)
            {
                curTotalSP = GmDatabaseService.Int(curRows[0], "total_sp");
                curTotalTP = GmDatabaseService.Int(curRows[0], "total_tp");
            }

            var sets = new List<string>();
            var args = new List<object?>();
            if (remainingSp.HasValue)
            {
                if (rowExists && remainingSp.Value > curTotalSP)
                {
                    throw new InvalidOperationException($"当前SP不能超过最大SP（{curTotalSP}）");
                }
                sets.Add("remaining_sp = " + GmDatabaseService.Num(remainingSp.Value));
            }
            if (remainingTp.HasValue)
            {
                if (rowExists && remainingTp.Value > curTotalTP)
                {
                    throw new InvalidOperationException($"当前TP不能超过最大TP（{curTotalTP}）");
                }
                sets.Add("remaining_tp = " + GmDatabaseService.Num(remainingTp.Value));
            }
            if (sets.Count == 0)
            {
                return;
            }

            var updateSql = "UPDATE dnf_skills SET " + string.Join(", ", sets) + " WHERE character_id = " + GmDatabaseService.Esc(characterId);
            await _db.ExecAsync(updateSql).ConfigureAwait(false);

            // MySqlClient 不返回影响行数，直接探测记录是否存在
            var probe = await _db.QueryAsync(
                "SELECT 1 AS x FROM dnf_skills WHERE character_id = " + GmDatabaseService.Esc(characterId)).ConfigureAwait(false);
            if (probe.Count == 0)
            {
                var spVal = remainingSp ?? 0;
                var tpVal = remainingTp ?? 0;
                var spTotal = remainingSp ?? 0;
                var tpTotal = remainingTp ?? 0;
                await _db.ExecAsync(
                    "INSERT INTO dnf_skills (character_id, total_sp, remaining_sp, total_tp, remaining_tp) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", " + GmDatabaseService.Num(spTotal) + ", " + GmDatabaseService.Num(spVal)
                    + ", " + GmDatabaseService.Num(tpTotal) + ", " + GmDatabaseService.Num(tpVal) + ")").ConfigureAwait(false);
            }
        }

        // ── 等级 ──────────────────────────────────────────────

        public async Task UpdateLevelAsync(GmSettings settings, string characterId, int level,
            CancellationToken cancellationToken = default)
        {
            if (level < 1 || level > 90)
            {
                throw new InvalidOperationException("等级必须在 1-90 之间");
            }
            try
            {
                await GmNativeClient.SetLevelAsync(settings, characterId, level, cancellationToken).ConfigureAwait(false);
                return;
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            var sql = "UPDATE dnf_characters SET level = " + GmDatabaseService.Num(level)
                      + ", updated_at = CURRENT_TIMESTAMP(6) WHERE character_id = " + GmDatabaseService.Esc(characterId)
                      + " AND delete_flag = 0";
            var rows = await _db.QueryAsync(sql).ConfigureAwait(false);
            // 校验角色存在
            var probe = await _db.QueryAsync(
                "SELECT character_id FROM dnf_characters WHERE character_id = " + GmDatabaseService.Esc(characterId)
                + " AND delete_flag = 0 LIMIT 1").ConfigureAwait(false);
            if (probe.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
        }

        // ── 职业 ──────────────────────────────────────────────

        public async Task<JobInfo> QueryJobAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            try
            {
                var native = await GmNativeClient.QueryJobAsync(settings, characterId, cancellationToken).ConfigureAwait(false);
                var names = GmClassData.LookupClassName(native.Job, native.First_grow, native.Second_grow);
                return new JobInfo
                {
                    JobID = native.Job,
                    FirstGrow = native.First_grow,
                    SecondGrow = native.Second_grow,
                    JobName = names.JobName,
                    GrowTypeName = names.GrowName,
                    AwakeningName = names.AwakeName,
                };
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            var rows = await _db.QueryAsync(
                "SELECT CAST(job AS " + (_db.IsSqlite ? "INTEGER" : "UNSIGNED") + ") AS job, grow_type FROM dnf_characters WHERE character_id = "
                + GmDatabaseService.Esc(characterId) + " AND delete_flag = 0 LIMIT 1").ConfigureAwait(false);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
            var jobID = GmDatabaseService.Int(rows[0], "job");
            var growType = GmDatabaseService.Int(rows[0], "grow_type");
            var firstGrow = growType & 0xF;
            var secondGrow = (growType >> 4) & 0xF;
            var (jn, gn, an) = GmClassData.LookupClassName(jobID, firstGrow, secondGrow);
            return new JobInfo { JobID = jobID, FirstGrow = firstGrow, SecondGrow = secondGrow, JobName = jn, GrowTypeName = gn, AwakeningName = an };
        }

        public async Task UpdateJobAsync(GmSettings settings, string characterId, int job, int firstGrow, int secondGrow,
            CancellationToken cancellationToken = default)
        {
            try
            {
                await GmNativeClient.SetJobAsync(settings, characterId, job, firstGrow, secondGrow, cancellationToken).ConfigureAwait(false);
                return;
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            // grow_type 位布局: 高4位=觉醒(secondGrow), 低4位=转职(firstGrow)
            var growType = (secondGrow << 4) | (firstGrow & 0xF);
            var sql = "UPDATE dnf_characters SET job = " + GmDatabaseService.Esc(job.ToString(CultureInfo.InvariantCulture))
                      + ", grow_type = " + GmDatabaseService.Num(growType)
                      + ", updated_at = CURRENT_TIMESTAMP(6) WHERE character_id = " + GmDatabaseService.Esc(characterId)
                      + " AND delete_flag = 0";
            await _db.ExecAsync(sql).ConfigureAwait(false);
            var probe = await _db.QueryAsync(
                "SELECT character_id FROM dnf_characters WHERE character_id = " + GmDatabaseService.Esc(characterId)
                + " AND delete_flag = 0 LIMIT 1").ConfigureAwait(false);
            if (probe.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }

            // 同步任务状态，服务端以此识别觉醒
            if (secondGrow >= 1)
            {
                await _db.ExecAsync(
                    "INSERT INTO dnf_quest_states (character_id, quest_id, state_group, status, trigger_type, progress_value, reward_select_index, multiplier) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", 3054, 'states', 'completed', 0, 0, 0, 0)"
                    + " ON DUPLICATE KEY UPDATE status = VALUES(status)").ConfigureAwait(false);
            }
            if (secondGrow >= 2)
            {
                await _db.ExecAsync(
                    "INSERT INTO dnf_quest_states (character_id, quest_id, state_group, status, trigger_type, progress_value, reward_select_index, multiplier) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", 3157, 'states', 'completed', 0, 0, 0, 0)"
                    + " ON DUPLICATE KEY UPDATE status = VALUES(status)").ConfigureAwait(false);
            }

            // 重置 SP/TP 并清空技能表
            var resets = new[]
            {
                "UPDATE dnf_skills SET remaining_sp = total_sp, remaining_tp = total_tp WHERE character_id = " + GmDatabaseService.Esc(characterId),
                "UPDATE dnf_legacy_character_skill_points SET remaining_sp = total_sp, remaining_tp = total_tp WHERE character_id = " + GmDatabaseService.Esc(characterId),
                "DELETE FROM dnf_legacy_character_skills WHERE character_id = " + GmDatabaseService.Esc(characterId),
                "DELETE FROM dnf_skill_states WHERE character_id = " + GmDatabaseService.Esc(characterId),
                "DELETE FROM dnf_skill_cooldowns WHERE character_id = " + GmDatabaseService.Esc(characterId),
                "DELETE FROM dnf_skill_layouts WHERE character_id = " + GmDatabaseService.Esc(characterId),
            };
            foreach (var r in resets)
            {
                await _db.ExecAsync(r).ConfigureAwait(false);
            }

            await ApplyDefaultSkillsAsync(characterId, job, firstGrow, secondGrow).ConfigureAwait(false);
        }

        /// <summary>写入职业默认技能（dnf_legacy_character_skills + dnf_skill_states）</summary>
        private async Task ApplyDefaultSkillsAsync(string characterId, int job, int firstGrow, int secondGrow)
        {
            var jobKey = job.ToString(CultureInfo.InvariantCulture);
            if (!GmClassData.Skills.Classes.TryGetValue(jobKey, out var cls))
            {
                throw new InvalidOperationException($"未找到职业 {job} 的默认技能数据");
            }
            var allSkills = new List<SkillDef>(cls.BaseSkills);
            if (firstGrow > 0)
            {
                var gtKey = firstGrow.ToString(CultureInfo.InvariantCulture);
                if (cls.GrowTypes.TryGetValue(gtKey, out var gt))
                {
                    allSkills.AddRange(gt.Skills);
                    if (secondGrow >= 1 && gt.Awakenings.TryGetValue("1", out var aw1))
                    {
                        allSkills.AddRange(aw1);
                    }
                    if (secondGrow >= 2 && gt.Awakenings.TryGetValue("2", out var aw2))
                    {
                        allSkills.AddRange(aw2);
                    }
                }
            }
            if (allSkills.Count == 0)
            {
                return;
            }
            for (var i = 0; i < allSkills.Count; i++)
            {
                var skill = allSkills[i];
                await _db.ExecAsync(
                    "INSERT INTO dnf_legacy_character_skills (character_id, page_index, page_header, slot, skill_id, level, extra_values) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", 0, 0, " + GmDatabaseService.Num(i) + ", "
                    + GmDatabaseService.Num(skill.Id) + ", " + GmDatabaseService.Num(skill.Level) + ", '')").ConfigureAwait(false);
                await _db.ExecAsync(
                    "INSERT INTO dnf_skill_states (character_id, skill_id, skill_level, enabled) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", " + GmDatabaseService.Num(skill.Id) + ", "
                    + GmDatabaseService.Num(skill.Level) + ", 1) ON DUPLICATE KEY UPDATE skill_level = VALUES(skill_level), enabled = 1").ConfigureAwait(false);
            }
        }

        // ── 副本全解锁 ─────────────────────────────────────────

        public async Task<int> UnlockDungeonsAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            try
            {
                return await GmNativeClient.UnlockDungeonsAsync(settings, characterId, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            var probe = await _db.QueryAsync(
                "SELECT 1 AS x FROM dnf_characters WHERE character_id = " + GmDatabaseService.Esc(characterId)
                + " AND delete_flag = 0 LIMIT 1").ConfigureAwait(false);
            if (probe.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
            var maxDiffs = GmClassData.DungeonsMaxDifficulty;
            var count = 0;
            foreach (var pair in maxDiffs)
            {
                var maxState = pair.Value;
                if (maxState <= 0)
                {
                    continue; // 跳过无多难度系统的副本
                }
                await _db.ExecAsync(
                    "INSERT INTO dnf_dungeon_permissions (character_id, dungeon_id, clear_state, sort_order, updated_at) VALUES ("
                    + GmDatabaseService.Esc(characterId) + ", " + GmDatabaseService.Esc(pair.Key) + ", "
                    + GmDatabaseService.Num(maxState) + ", 0, CURRENT_TIMESTAMP(6))"
                    + " ON DUPLICATE KEY UPDATE clear_state = GREATEST(clear_state, VALUES(clear_state)), updated_at = CURRENT_TIMESTAMP(6)").ConfigureAwait(false);
                count++;
            }
            return count;
        }

        // ── 装备槽解锁 ─────────────────────────────────────────

        public async Task<(long Before, long After)> UnlockEquipSlotsAsync(GmSettings settings, string characterId,
            CancellationToken cancellationToken = default)
        {
            try
            {
                var result = await GmNativeClient.UnlockEquipSlotsAsync(settings, characterId, cancellationToken).ConfigureAwait(false);
                var slotBefore = 0L;
                var slotAfter = 0L;
                if (result.TryGetValue("before", out var b) && long.TryParse(Convert.ToString(b, CultureInfo.InvariantCulture), out var bv))
                {
                    slotBefore = bv;
                }
                if (result.TryGetValue("after", out var a) && long.TryParse(Convert.ToString(a, CultureInfo.InvariantCulture), out var av))
                {
                    slotAfter = av;
                }
                if (slotAfter == 0)
                {
                    slotAfter = slotBefore | UnlockAllEquipSlotsMask;
                }
                return (slotBefore, slotAfter);
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            var rows = await _db.QueryAsync(
                "SELECT ex_equip_slot_stat FROM dnf_characters WHERE character_id = " + GmDatabaseService.Esc(characterId)
                + " AND delete_flag = 0 LIMIT 1").ConfigureAwait(false);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {characterId}");
            }
            var before = GmDatabaseService.Lng(rows[0], "ex_equip_slot_stat");
            await _db.ExecAsync(
                "UPDATE dnf_characters SET ex_equip_slot_stat = ex_equip_slot_stat | " + GmDatabaseService.Num(UnlockAllEquipSlotsMask)
                + ", updated_at = UTC_TIMESTAMP(6) WHERE character_id = " + GmDatabaseService.Esc(characterId)).ConfigureAwait(false);
            return (before, before | UnlockAllEquipSlotsMask);
        }

        // ── 钱包 ──────────────────────────────────────────────

        public sealed class WalletResult
        {
            public CharacterView Character { get; set; } = new();
            public string Currency { get; set; } = "";
            public string Mode { get; set; } = "";
            public long Amount { get; set; }
            public long Before { get; set; }
            public long After { get; set; }
            public long Cera { get; set; }
            public long Gold { get; set; }
            public bool Online { get; set; }
            public bool Refreshed { get; set; }
            public string Delivery { get; set; } = "";
            public string Message { get; set; } = "";
            public bool Native { get; set; }
        }

        /// <summary>查询钱包（native 优先，MySQL 回退）</summary>
        public async Task<WalletResult> QueryWalletAsync(GmSettings settings, string roleName,
            CancellationToken cancellationToken = default)
        {
            try
            {
                var native = await GmNativeClient.WalletQueryAsync(settings, roleName, cancellationToken).ConfigureAwait(false);
                return new WalletResult
                {
                    Character = ToCharacterView(native.Character),
                    Currency = native.Currency ?? "",
                    Mode = native.Mode ?? "",
                    Before = native.Before,
                    After = native.After,
                    Cera = native.Cera,
                    Gold = native.Gold,
                    Online = native.Online,
                    Refreshed = native.Refreshed,
                    Native = true,
                };
            }
            catch (Exception ex)
            {
                if (string.Equals(GmNativeClient.InstanceDatabaseMode(settings), "sqlite", StringComparison.OrdinalIgnoreCase))
                {
                    throw new InvalidOperationException($"SQLite 模式必须连接 DNF90Server 原生 GM 接口: {ex.Message}", ex);
                }
                // fallback MySQL
            }
            var character = await FindCharacterDirectAsync(roleName).ConfigureAwait(false);
            return new WalletResult
            {
                Character = character,
                Currency = "cera",
                Mode = "query",
                Before = character.Cera,
                After = character.Cera,
                Cera = character.Cera,
                Gold = character.Gold,
                Native = false,
            };
        }

        /// <summary>
        /// 更新钱包资产（Go 版 handleWalletUpdate：原生邮件方式，无 MySQL 回退）。
        /// gold → 邮件附件；cera → 直接到账 + 通知邮件。
        /// </summary>
        public async Task<WalletResult> UpdateWalletNativeAsync(GmSettings settings, string roleName,
            string currency, long amount, CancellationToken cancellationToken = default)
        {
            currency = (currency ?? "").Trim().ToLowerInvariant();
            roleName = (roleName ?? "").Trim();
            if (roleName.Length == 0 || (currency != "cera" && currency != "gold") || amount <= 0
                || amount > GmNativeClient.MaxWalletBalance)
            {
                throw new InvalidOperationException("角色、邮件资产类型或数量无效");
            }
            var before = await GmNativeClient.WalletQueryAsync(settings, roleName, cancellationToken).ConfigureAwait(false);
            if (currency == "gold")
            {
                var mailRequest = new NativeMailRequest
                {
                    Character_name = roleName,
                    Title = "GM 金币邮件",
                    Body = $"系统发放金币 {amount}，请领取附件。",
                    Gold = amount,
                };
                var mailResult = new NativeMailResult();
                await GmNativeClient.SendMailAsync(settings, mailRequest, mailResult, cancellationToken).ConfigureAwait(false);
                var message = mailResult.Refreshed
                    ? "金币邮件已发送，在线邮箱已实时刷新；领取后到账"
                    : "金币邮件已发送，玩家领取邮件后到账";
                return new WalletResult
                {
                    Character = ToCharacterView(before.Character),
                    Currency = "gold",
                    Mode = "mail",
                    Amount = amount,
                    Before = before.Gold,
                    After = before.Gold,
                    Cera = before.Cera,
                    Gold = before.Gold,
                    Online = mailResult.Online,
                    Refreshed = mailResult.Refreshed,
                    Delivery = "claimable_mail",
                    Message = message,
                    Native = true,
                };
            }

            // cera：直接到账 + 通知邮件
            var credited = await GmNativeClient.WalletUpdateAsync(settings, roleName, "cera", "add", amount, cancellationToken).ConfigureAwait(false);
            var notice = new NativeMailRequest
            {
                Character_name = roleName,
                Title = "GM 点券到账通知",
                Body = $"系统已发放点券 {amount}，当前点券余额 {credited.Cera}。",
            };
            var noticeResult = new NativeMailResult();
            string? mailError = null;
            try
            {
                await GmNativeClient.SendMailAsync(settings, notice, noticeResult, cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                mailError = ex.Message;
            }
            var result = new WalletResult
            {
                Character = ToCharacterView(credited.Character),
                Currency = "cera",
                Mode = "mail",
                Amount = amount,
                Before = before.Cera,
                After = credited.Cera,
                Cera = credited.Cera,
                Gold = credited.Gold,
                Online = credited.Online || noticeResult.Online,
                Refreshed = credited.Refreshed && (mailError is null && (!noticeResult.Online || noticeResult.Refreshed)),
                Delivery = "credited_with_notice",
                Native = true,
            };
            if (mailError is not null)
            {
                result.Message = "点券已到账，但通知邮件发送失败；请勿重复操作";
                return result;
            }
            result.Message = noticeResult.Refreshed
                ? "点券已到账，通知邮件与在线邮箱均已实时刷新"
                : "点券已到账，通知邮件已发送";
            return result;
        }

        // ── 邮件 ──────────────────────────────────────────────

        public sealed class MailSendResult
        {
            public NativeMailResult Mail { get; set; } = new();
            public string Message { get; set; } = "";
        }

        public async Task<MailSendResult> SendMailAsync(GmSettings settings, string roleName, string title,
            string body, long gold, uint itemId, uint amount, bool bind, CancellationToken cancellationToken = default)
        {
            roleName = (roleName ?? "").Trim();
            title = (title ?? "").Trim();
            body = (body ?? "").Trim();
            if (roleName.Length == 0)
            {
                throw new InvalidOperationException("请选择邮件接收角色");
            }
            if (gold < 0 || gold > uint.MaxValue)
            {
                throw new InvalidOperationException("邮件金币超出客户端范围");
            }
            if ((itemId == 0) != (amount == 0))
            {
                throw new InvalidOperationException("附件物品 ID 与数量必须同时填写");
            }
            var request = new NativeMailRequest { Character_name = roleName, Title = title, Body = body, Gold = gold };
            if (itemId != 0)
            {
                request.Attachments.Add(new NativeMailAttachment { Item_id = itemId, Count = amount, Bind = bind });
            }
            if (title.Length == 0 && body.Length == 0 && gold == 0 && request.Attachments.Count == 0)
            {
                throw new InvalidOperationException("邮件内容、金币或附件至少填写一项");
            }
            var result = new NativeMailResult();
            var message = await GmNativeClient.SendMailAsync(settings, request, result, cancellationToken).ConfigureAwait(false);
            string final;
            if (result.Refreshed)
            {
                final = "邮件已提交，在线客户端邮箱已实时刷新";
            }
            else if (result.Online)
            {
                final = "邮件已提交；在线刷新未完成，下次打开邮箱时自动同步";
            }
            else
            {
                final = "邮件已提交；角色离线，上线后可直接领取";
            }
            return new MailSendResult { Mail = result, Message = final };
        }
    }
}
