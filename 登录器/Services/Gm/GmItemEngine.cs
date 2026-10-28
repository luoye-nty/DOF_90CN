using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Security.Cryptography;
using System.Threading.Tasks;

namespace DNFLogin.Gm
{
    /// <summary>
    /// 物品发放引擎（移植 Go 版 direct_items.go）。
    /// 支持预览（preview）与事务发放（commit），兼容账号共享槽（晶体/灵魂仓库）。
    /// </summary>
    public sealed class GmItemEngine
    {
        private readonly GmDatabaseService _db;

        public GmItemEngine(GmDatabaseService db)
        {
            _db = db;
        }

        // ── 入口 ─────────────────────────────────────────────────

        public async Task<DirectItemPlanView> ProcessAsync(GmSettings settings, string roleName, uint itemID,
            uint amount, int reinforceLevel, int amplifyLevel, int amplifyType, bool grant)
        {
            if (string.IsNullOrWhiteSpace(roleName) || itemID == 0 || amount == 0 || amount > GmPvfCatalog.DirectItemMaxAmount)
            {
                throw new InvalidOperationException("角色、物品 ID 和数量无效；数量上限为 " + GmPvfCatalog.DirectItemMaxAmount);
            }
            var catalog = GmPvfCatalog.Load(settings.PvfPath);
            var definition = catalog.Resolve(itemID);
            definition = DefinitionForGrant(definition, DateTime.UtcNow);
            definition = DefinitionWithUpgrade(definition, reinforceLevel, amplifyLevel, amplifyType);
            GmPvfCatalog.ValidateDirectItemDefinition(definition);

            DirectItemPlan plan;
            if (!grant)
            {
                var character = await LoadCharacterAsync(roleName, false);
                var charItems = await LoadCharacterItemsAsync(character.CharacterID, false);
                var accountItems = await LoadAccountItemsAsync(character.AccountID, false);
                plan = PlanDirectItem(character, definition, amount, charItems, accountItems);
            }
            else
            {
                plan = await CommitDirectItemAsync(roleName, definition, amount);
            }
            return ToPlanView(plan);
        }

        // ── 事务发放 ─────────────────────────────────────────────

        private async Task<DirectItemPlan> CommitDirectItemAsync(string roleName, DirectItemDefinition definition, uint amount)
        {
            await _db.BeginAsync();
            try
            {
                var character = await LoadCharacterAsync(roleName, true);
                await _db.ExecAsync(
                    $"INSERT IGNORE INTO dnf_inventories (character_id, updated_at) VALUES ({GmDatabaseService.Esc(character.CharacterID)}, CURRENT_TIMESTAMP(6))");
                await _db.QueryAsync(
                    $"SELECT character_id FROM dnf_inventories WHERE character_id = {GmDatabaseService.Esc(character.CharacterID)} FOR UPDATE");
                await _db.ExecAsync(
                    $"INSERT IGNORE INTO dnf_account_inventories (account_id, updated_at) VALUES ({GmDatabaseService.Esc(character.AccountID)}, CURRENT_TIMESTAMP(6))");
                await _db.QueryAsync(
                    $"SELECT account_id FROM dnf_account_inventories WHERE account_id = {GmDatabaseService.Esc(character.AccountID)} FOR UPDATE");

                var charItems = await LoadCharacterItemsAsync(character.CharacterID, true);
                var accountItems = await LoadAccountItemsAsync(character.AccountID, true);
                var plan = PlanDirectItem(character, definition, amount, charItems, accountItems);

                await SaveCharacterItemsAsync(character.CharacterID, plan.CharacterDB, plan.CharTouched);
                await SaveAccountItemsAsync(character.AccountID, plan.AccountDB, plan.AcctTouched);
                await _db.ExecAsync(
                    $"UPDATE dnf_inventories SET updated_at = CURRENT_TIMESTAMP(6) WHERE character_id = {GmDatabaseService.Esc(character.CharacterID)}");
                if (plan.AcctTouched.Count > 0)
                {
                    await _db.ExecAsync(
                        $"UPDATE dnf_account_inventories SET updated_at = CURRENT_TIMESTAMP(6) WHERE account_id = {GmDatabaseService.Esc(character.AccountID)}");
                }
                await _db.CommitAsync();
                return plan;
            }
            catch
            {
                await _db.RollbackAsync();
                throw;
            }
        }

        // ── 角色与背包读取 ───────────────────────────────────────

        private async Task<CharacterView> LoadCharacterAsync(string roleName, bool lockRow)
        {
            string query =
                $"SELECT c.character_id, c.account_id, c.name, c.level, c.gold, " +
                $"COALESCE(CAST(m.entry_value AS SIGNED), 0) " +
                $"FROM dnf_characters AS c " +
                $"LEFT JOIN dnf_account_metadata AS m ON m.account_id = c.account_id AND m.entry_key = 'account_cera' " +
                $"WHERE c.name = {GmDatabaseService.Esc(roleName)} AND c.delete_flag = 0 LIMIT 1";
            if (lockRow)
            {
                query += " FOR UPDATE";
            }
            var rows = await _db.QueryAsync(query);
            if (rows.Count == 0)
            {
                throw new InvalidOperationException($"角色不存在: {roleName}");
            }
            var row = rows[0];
            return new CharacterView
            {
                CharacterID = GmDatabaseService.Str(row, "character_id"),
                AccountID = GmDatabaseService.Str(row, "account_id"),
                Name = GmDatabaseService.Str(row, "name"),
                Level = GmDatabaseService.Int(row, "level"),
                Gold = GmDatabaseService.Lng(row, "gold"),
                Cera = GmDatabaseService.Lng(row, "entry_value"),
            };
        }

        private async Task<Dictionary<string, DirectStoredItem>> LoadCharacterItemsAsync(string characterID, bool lockRow)
        {
            string query =
                $"SELECT entry_key, item_id, item_count, bind_flag, expire_at, raw_entry " +
                $"FROM dnf_inventory_items WHERE character_id = {GmDatabaseService.Esc(characterID)} " +
                $"AND collection_name = 'slots' ORDER BY entry_key";
            if (lockRow)
            {
                query += " FOR UPDATE";
            }
            var items = await LoadItemsAsync(query, characterID);
            await LoadExtrasAsync(
                $"SELECT entry_key, extra_key, extra_value FROM dnf_inventory_item_extra " +
                $"WHERE character_id = {GmDatabaseService.Esc(characterID)} AND collection_name = 'slots' ORDER BY entry_key, extra_key",
                items);
            return items;
        }

        private async Task<Dictionary<string, DirectStoredItem>> LoadAccountItemsAsync(string accountID, bool lockRow)
        {
            string query =
                $"SELECT entry_key, item_id, item_count, bind_flag, expire_at, raw_entry " +
                $"FROM dnf_account_inventory_items WHERE account_id = {GmDatabaseService.Esc(accountID)} ORDER BY entry_key";
            if (lockRow)
            {
                query += " FOR UPDATE";
            }
            var items = await LoadItemsAsync(query, accountID);
            await LoadExtrasAsync(
                $"SELECT entry_key, extra_key, extra_value FROM dnf_account_inventory_item_extra " +
                $"WHERE account_id = {GmDatabaseService.Esc(accountID)} ORDER BY entry_key, extra_key",
                items);
            return items;
        }

        private async Task<Dictionary<string, DirectStoredItem>> LoadItemsAsync(string query, string ownerID)
        {
            var rows = await _db.QueryBinaryAsync(query);
            var items = new Dictionary<string, DirectStoredItem>(rows.Count);
            foreach (var row in rows)
            {
                var item = new DirectStoredItem
                {
                    Key = GmDatabaseService.Str(row, "entry_key"),
                    ItemID = GmDatabaseService.Lng(row, "item_id"),
                    Count = GmDatabaseService.Lng(row, "item_count"),
                    Bind = GmDatabaseService.Str(row, "bind_flag") != "0" && GmDatabaseService.Str(row, "bind_flag") != "",
                    ExpireAt = GmDatabaseService.NullDateTime(row, "expire_at") ?? default,
                    Raw = GmDatabaseService.Raw(row, "raw_entry") ?? Array.Empty<byte>(),
                    Extra = new Dictionary<string, string>(),
                };
                items[item.Key] = item;
            }
            return items;
        }

        private async Task LoadExtrasAsync(string query, Dictionary<string, DirectStoredItem> items)
        {
            var rows = await _db.QueryAsync(query);
            foreach (var row in rows)
            {
                string entryKey = GmDatabaseService.Str(row, "entry_key");
                string extraKey = GmDatabaseService.Str(row, "extra_key");
                string extraValue = GmDatabaseService.Str(row, "extra_value");
                if (!items.TryGetValue(entryKey, out var item))
                {
                    continue;
                }
                item.Extra[extraKey] = extraValue;
            }
        }

        // ── 规划 ─────────────────────────────────────────────────

        public static DirectItemPlan PlanDirectItem(CharacterView character, DirectItemDefinition definition, uint amount,
            Dictionary<string, DirectStoredItem> charItems, Dictionary<string, DirectStoredItem> accountItems)
        {
            var plan = new DirectItemPlan
            {
                Character = character,
                Definition = definition,
                Amount = amount,
                Route = definition.Route,
                CharacterDB = CloneDirectItems(charItems),
                AccountDB = CloneDirectItems(accountItems),
                CharTouched = new HashSet<string>(),
                AcctTouched = new HashSet<string>(),
            };
            plan.Existing = DirectItemTotal(plan.CharacterDB, plan.AccountDB, definition.ItemID);
            plan.SlotsFree = DirectFreeSlots(plan.CharacterDB, definition.ListType, definition.SlotStart, definition.SlotEnd);

            uint remaining = amount;
            if (GmPvfCatalog.IsAccountSharedSlot(definition.ItemID))
            {
                short sharedSlot = GmPvfCatalog.AccountSharedSlot(definition.ItemID);
                plan.Route = "账号晶体/灵魂仓库";
                string key = DirectInventoryKey(0, sharedSlot);
                bool occupied = plan.AccountDB.TryGetValue(key, out var stack);
                if (stack is null || stack.ItemID == definition.ItemID)
                {
                    long space = GmPvfCatalog.DirectItemMaxAmount;
                    if (definition.StackLimit > 0)
                    {
                        space = definition.StackLimit;
                    }
                    space -= stack?.Count ?? 0;
                    if (space > 0)
                    {
                        long added = remaining;
                        if (added > space)
                        {
                            added = space;
                        }
                        if (!occupied)
                        {
                            stack = NewDirectStoredItem(key, definition, added);
                        }
                        else
                        {
                            stack!.Count += added;
                        }
                        stack.Raw = DirectItemRaw(stack, definition, 0, sharedSlot);
                        plan.AccountDB[key] = stack;
                        plan.AcctTouched.Add(key);
                        plan.Touched.Add(sharedSlot);
                        remaining -= (uint)added;
                    }
                }
                if (remaining > 0)
                {
                    plan.Route = "账号仓库，超出部分进入角色背包";
                }
            }

            if (remaining > 0)
            {
                GrantDirectCharacterItems(plan, definition, remaining);
            }
            plan.Touched = UniqueDirectSlots(plan.Touched);
            plan.After = DirectItemTotal(plan.CharacterDB, plan.AccountDB, definition.ItemID);
            return plan;
        }

        private static void GrantDirectCharacterItems(DirectItemPlan plan, DirectItemDefinition definition, uint amount)
        {
            long remaining = amount;
            var ranges = new List<(short Start, short End)>();
            if (definition.Kind == DirectItemKind.Stackable && definition.ListType == 0 && GmPvfCatalog.PrefersQuickSlots(definition.StackableType))
            {
                ranges.Add((3, 8));
            }
            ranges.Add((definition.SlotStart, definition.SlotEnd));

            if (definition.Kind == DirectItemKind.Stackable)
            {
                foreach (var slotRange in ranges)
                {
                    for (short slot = slotRange.Start; slot <= slotRange.End && remaining > 0; slot++)
                    {
                        string key = DirectInventoryKey(definition.ListType, slot);
                        if (!plan.CharacterDB.TryGetValue(key, out var stack) ||
                            stack.ItemID != definition.ItemID || stack.Bind || !ExpirationMatches(stack, definition))
                        {
                            continue;
                        }
                        long limit = GmPvfCatalog.DirectItemMaxAmount;
                        if (definition.StackLimit > 0)
                        {
                            limit = definition.StackLimit;
                        }
                        long space = limit - stack.Count;
                        if (space <= 0)
                        {
                            continue;
                        }
                        long added = remaining;
                        if (added > space)
                        {
                            added = space;
                        }
                        stack.Count += added;
                        stack.Raw = DirectItemRaw(stack, definition, definition.ListType, slot);
                        plan.CharacterDB[key] = stack;
                        plan.CharTouched.Add(key);
                        plan.Touched.Add(slot);
                        remaining -= added;
                    }
                }
            }

            while (remaining > 0)
            {
                long chunk = 1;
                if (definition.Kind == DirectItemKind.Stackable)
                {
                    chunk = remaining;
                    if (definition.StackLimit > 0 && chunk > definition.StackLimit)
                    {
                        chunk = definition.StackLimit;
                    }
                }
                var (slot, ok) = DirectFirstEmpty(plan.CharacterDB, definition.ListType, ranges);
                if (!ok)
                {
                    throw new InvalidOperationException($"背包空间不足：物品 {definition.ItemID} 需要更多槽位");
                }
                string key = DirectInventoryKey(definition.ListType, slot);
                var stack = NewDirectStoredItem(key, definition, chunk);
                if (definition.Kind == DirectItemKind.Equipment && definition.ListType == 0)
                {
                    stack.Extra["quality_seed"] = NewDirectQualitySeed().ToString(CultureInfo.InvariantCulture);
                }
                stack.Raw = DirectItemRaw(stack, definition, definition.ListType, slot);
                plan.CharacterDB[key] = stack;
                plan.CharTouched.Add(key);
                plan.Touched.Add(slot);
                remaining -= chunk;
            }
        }

        // ── 存储 ─────────────────────────────────────────────────

        private async Task SaveCharacterItemsAsync(string characterID, Dictionary<string, DirectStoredItem> items, HashSet<string> touched)
        {
            foreach (var key in SortedKeys(touched))
            {
                var item = items[key];
                await _db.ExecAsync(
                    $"INSERT INTO dnf_inventory_items " +
                    $"(character_id, collection_name, entry_key, item_id, item_count, bind_flag, expire_at, raw_entry) " +
                    $"VALUES ({GmDatabaseService.Esc(characterID)}, 'slots', {GmDatabaseService.Esc(key)}, " +
                    $"{GmDatabaseService.Num(item.ItemID)}, {GmDatabaseService.Num(item.Count)}, " +
                    $"{GmDatabaseService.Num(item.Bind ? 1 : 0)}, {NullableTime(item.ExpireAt)}, {GmDatabaseService.Hex(item.Raw)}) " +
                    $"ON DUPLICATE KEY UPDATE item_id=VALUES(item_id), item_count=VALUES(item_count), " +
                    $"bind_flag=VALUES(bind_flag), expire_at=VALUES(expire_at), raw_entry=VALUES(raw_entry)");
                await _db.ExecAsync(
                    $"DELETE FROM dnf_inventory_item_extra WHERE character_id={GmDatabaseService.Esc(characterID)} " +
                    $"AND collection_name='slots' AND entry_key={GmDatabaseService.Esc(key)}");
                foreach (var extraKey in SortedStringMapKeys(item.Extra))
                {
                    await _db.ExecAsync(
                        $"INSERT INTO dnf_inventory_item_extra (character_id, collection_name, entry_key, extra_key, extra_value) " +
                        $"VALUES ({GmDatabaseService.Esc(characterID)}, 'slots', {GmDatabaseService.Esc(key)}, " +
                        $"{GmDatabaseService.Esc(extraKey)}, {GmDatabaseService.Esc(item.Extra[extraKey])})");
                }
            }
        }

        private async Task SaveAccountItemsAsync(string accountID, Dictionary<string, DirectStoredItem> items, HashSet<string> touched)
        {
            foreach (var key in SortedKeys(touched))
            {
                var item = items[key];
                await _db.ExecAsync(
                    $"INSERT INTO dnf_account_inventory_items (account_id, entry_key, item_id, item_count, bind_flag, expire_at, raw_entry) " +
                    $"VALUES ({GmDatabaseService.Esc(accountID)}, {GmDatabaseService.Esc(key)}, " +
                    $"{GmDatabaseService.Num(item.ItemID)}, {GmDatabaseService.Num(item.Count)}, " +
                    $"{GmDatabaseService.Num(item.Bind ? 1 : 0)}, {NullableTime(item.ExpireAt)}, {GmDatabaseService.Hex(item.Raw)}) " +
                    $"ON DUPLICATE KEY UPDATE item_id=VALUES(item_id), item_count=VALUES(item_count), " +
                    $"bind_flag=VALUES(bind_flag), expire_at=VALUES(expire_at), raw_entry=VALUES(raw_entry)");
                await _db.ExecAsync(
                    $"DELETE FROM dnf_account_inventory_item_extra WHERE account_id={GmDatabaseService.Esc(accountID)} " +
                    $"AND entry_key={GmDatabaseService.Esc(key)}");
                foreach (var extraKey in SortedStringMapKeys(item.Extra))
                {
                    await _db.ExecAsync(
                        $"INSERT INTO dnf_account_inventory_item_extra (account_id, entry_key, extra_key, extra_value) " +
                        $"VALUES ({GmDatabaseService.Esc(accountID)}, {GmDatabaseService.Esc(key)}, " +
                        $"{GmDatabaseService.Esc(extraKey)}, {GmDatabaseService.Esc(item.Extra[extraKey])})");
                }
            }
        }

        // ── 定义增强与校验 ───────────────────────────────────────

        public static DirectItemDefinition DefinitionWithUpgrade(DirectItemDefinition definition, int reinforceLevel, int amplifyLevel, int amplifyType)
        {
            if (reinforceLevel < 0 || reinforceLevel > 31)
            {
                throw new InvalidOperationException("强化等级必须在 0 到 31 之间");
            }
            if (amplifyLevel < 0 || amplifyLevel > 31)
            {
                throw new InvalidOperationException("增幅等级必须在 0 到 31 之间");
            }
            if (amplifyType < 0 || amplifyType > 4)
            {
                throw new InvalidOperationException("增幅属性无效");
            }
            if (!definition.SupportsUpgrade)
            {
                return definition;
            }
            if (reinforceLevel > 0 && (amplifyLevel > 0 || amplifyType > 0))
            {
                throw new InvalidOperationException("同一件装备不能同时设置强化和增幅");
            }
            if (amplifyLevel > 0 && amplifyType == 0)
            {
                throw new InvalidOperationException("设置增幅等级时必须选择增幅属性");
            }
            definition.ReinforceLevel = (byte)reinforceLevel;
            definition.AmplifyLevel = (byte)amplifyLevel;
            definition.AmplifyType = (byte)amplifyType;
            if (amplifyType > 0)
            {
                // 当前客户端以非零增幅值区分已经净化、可继续增幅的装备。
                definition.AmplifyValue = 1;
            }
            return definition;
        }

        public static byte EquipmentUpgradeLevel(DirectItemDefinition definition) =>
            definition.AmplifyType > 0 ? definition.AmplifyLevel : definition.ReinforceLevel;

        public static DirectItemDefinition DefinitionForGrant(DirectItemDefinition definition, DateTime now)
        {
            if (definition.Kind != DirectItemKind.Stackable || definition.UsablePeriodDays <= 0)
            {
                return definition;
            }
            long seconds = definition.UsablePeriodDays * 86400L;
            long unixNow = (long)(now - DateTime.UnixEpoch).TotalSeconds;
            if (seconds <= 0 || unixNow > uint.MaxValue - seconds)
            {
                throw new InvalidOperationException($"物品 {definition.ItemID} 的使用期限超出客户端范围");
            }
            definition.ExpirationDate = now.AddDays(definition.UsablePeriodDays);
            return definition;
        }

        // ── raw 构造与工具 ───────────────────────────────────────

        public static DirectStoredItem NewDirectStoredItem(string key, DirectItemDefinition definition, long count)
        {
            var extra = new Dictionary<string, string>
            {
                ["source"] = "gmtool_direct_mysql",
                ["item_kind"] = definition.Kind == DirectItemKind.Equipment ? "equipment" : "stackable",
                ["pvf_path"] = definition.PVFPath,
            };
            if (definition.StackableType.Length > 0)
            {
                extra["stackable_type"] = definition.StackableType;
            }
            if (definition.EquipmentType.Length > 0)
            {
                extra["equipment_type"] = definition.EquipmentType;
            }
            if (definition.StackLimit > 0)
            {
                extra["stack_limit"] = definition.StackLimit.ToString(CultureInfo.InvariantCulture);
            }
            if (definition.Durability > 0)
            {
                extra["durability"] = definition.Durability.ToString(CultureInfo.InvariantCulture);
                extra["max_durability"] = definition.Durability.ToString(CultureInfo.InvariantCulture);
            }
            if (definition.SupportsUpgrade)
            {
                string level = EquipmentUpgradeLevel(definition).ToString(CultureInfo.InvariantCulture);
                extra["ext_data0"] = level;
                extra["packed_flag_byte"] = level;
                extra["reinforce"] = level;
                extra["upgrade_level"] = level;
                if (definition.AmplifyType > 0)
                {
                    string ampType = definition.AmplifyType.ToString(CultureInfo.InvariantCulture);
                    string ampValue = definition.AmplifyValue.ToString(CultureInfo.InvariantCulture);
                    foreach (var k in new[] { "amplify_type", "amplification_type", "byte_13", "value_13", "value_c" })
                    {
                        extra[k] = ampType;
                    }
                    foreach (var k in new[] { "amplify_value", "amplification_value", "marker_16", "marker16", "value_d" })
                    {
                        extra[k] = ampValue;
                    }
                }
            }
            if (definition.ListType == 1)
            {
                extra["item_kind"] = "avatar";
                extra["amount_or_count"] = "0";
            }

            var item = new DirectStoredItem
            {
                Key = key,
                ItemID = definition.ItemID,
                Count = count,
                Extra = extra,
            };
            if (definition.ExpirationDate != default)
            {
                item.ExpireAt = definition.ExpirationDate.ToUniversalTime();
                string expire = ((long)(item.ExpireAt - DateTime.UnixEpoch).TotalSeconds).ToString(CultureInfo.InvariantCulture);
                item.Extra["expire_time"] = expire;
                item.Extra["expire_unix"] = expire;
            }
            return item;
        }

        public static byte[] DirectItemRaw(DirectStoredItem item, DirectItemDefinition definition, byte listType, short slot)
        {
            byte[] raw;
            if (item.Raw.Length != GmPvfCatalog.DirectItemRawSize)
            {
                raw = new byte[GmPvfCatalog.DirectItemRawSize];
            }
            else
            {
                raw = (byte[])item.Raw.Clone();
            }
            WriteUInt16(raw, 0, (ushort)slot);
            WriteUInt32(raw, 2, item.ItemID);

            uint amount = (uint)item.Count;
            if (definition.Kind == DirectItemKind.Equipment && listType == 0)
            {
                if (item.Extra.TryGetValue("quality_seed", out var seedText) &&
                    uint.TryParse(seedText, NumberStyles.None, CultureInfo.InvariantCulture, out var seed) && seed > 0)
                {
                    amount = seed;
                }
            }
            WriteUInt32(raw, 6, amount);
            if (listType == 1)
            {
                WriteUInt32(raw, 6, 0);
            }
            if (definition.SupportsUpgrade)
            {
                raw[0x0A] = (byte)((raw[0x0A] & 0xE0) | (EquipmentUpgradeLevel(definition) & 0x1F));
                raw[0x13] = definition.AmplifyType;
                WriteUInt16(raw, 0x14, definition.AmplifyValue);
            }
            if (definition.Kind == DirectItemKind.Equipment)
            {
                WriteUInt16(raw, 0x0B, definition.Durability);
            }
            else if (item.ExpireAt != default)
            {
                WriteUInt16(raw, 0x0B, DirectUsePeriod(item.ExpireAt));
            }
            if (item.Bind)
            {
                raw[0x0D] = 1;
            }
            if (item.ExpireAt != default)
            {
                long unix = (long)(item.ExpireAt.ToUniversalTime() - DateTime.UnixEpoch).TotalSeconds;
                if (unix > 0 && unix <= uint.MaxValue)
                {
                    WriteUInt32(raw, GmPvfCatalog.DirectItemExpireOffset, (uint)unix);
                }
            }
            return raw;
        }

        public static ushort DirectUsePeriod(DateTime expire)
        {
            long seconds = (long)(expire - DateTime.UtcNow).TotalSeconds;
            if (seconds <= 0)
            {
                return 0;
            }
            if (seconds > ushort.MaxValue)
            {
                return ushort.MaxValue;
            }
            return (ushort)seconds;
        }

        public static uint NewDirectQualitySeed()
        {
            var raw = new byte[4];
            while (true)
            {
                RandomNumberGenerator.Fill(raw);
                uint value = BitConverter.ToUInt32(raw, 0);
                if (value != 0)
                {
                    return value % GmPvfCatalog.DirectItemQualityMax + 1;
                }
            }
        }

        // ── 集合工具（照抄 Go） ──────────────────────────────────

        public static Dictionary<string, DirectStoredItem> CloneDirectItems(Dictionary<string, DirectStoredItem> source)
        {
            var result = new Dictionary<string, DirectStoredItem>(source.Count);
            foreach (var kv in source)
            {
                var item = kv.Value;
                item.Raw = (byte[])item.Raw.Clone();
                item.Extra = new Dictionary<string, string>(item.Extra);
                result[kv.Key] = item;
            }
            return result;
        }

        public static long DirectItemTotal(Dictionary<string, DirectStoredItem> character, Dictionary<string, DirectStoredItem> account, uint itemID)
        {
            long total = 0;
            foreach (var collection in new[] { character, account })
            {
                foreach (var item in collection.Values)
                {
                    if (item.ItemID == itemID && item.Count > 0 && total <= long.MaxValue - item.Count)
                    {
                        total += item.Count;
                    }
                }
            }
            return total;
        }

        public static int DirectFreeSlots(Dictionary<string, DirectStoredItem> items, byte listType, short start, short end)
        {
            int free = 0;
            for (short slot = start; slot <= end; slot++)
            {
                if (!items.ContainsKey(DirectInventoryKey(listType, slot)))
                {
                    free++;
                }
            }
            return free;
        }

        public static (short Slot, bool Ok) DirectFirstEmpty(Dictionary<string, DirectStoredItem> items, byte listType, List<(short Start, short End)> ranges)
        {
            foreach (var slotRange in ranges)
            {
                for (short slot = slotRange.Start; slot <= slotRange.End; slot++)
                {
                    if (!items.ContainsKey(DirectInventoryKey(listType, slot)))
                    {
                        return (slot, true);
                    }
                }
            }
            return (0, false);
        }

        public static bool ExpirationMatches(DirectStoredItem item, DirectItemDefinition definition)
        {
            if (definition.ExpirationDate == default)
            {
                return item.ExpireAt == default;
            }
            return item.ExpireAt != default &&
                   (long)(item.ExpireAt.ToUniversalTime() - DateTime.UnixEpoch).TotalSeconds ==
                   (long)(definition.ExpirationDate.ToUniversalTime() - DateTime.UnixEpoch).TotalSeconds;
        }

        public static string DirectInventoryKey(byte listType, short slot) =>
            listType.ToString(CultureInfo.InvariantCulture) + ":" + slot.ToString(CultureInfo.InvariantCulture);

        public static List<short> UniqueDirectSlots(List<short> slots)
        {
            var seen = new HashSet<short>();
            var result = new List<short>(slots.Count);
            foreach (var slot in slots)
            {
                if (seen.Add(slot))
                {
                    result.Add(slot);
                }
            }
            result.Sort();
            return result;
        }

        public static string NullableTime(DateTime value) =>
            value == default ? "NULL" : GmDatabaseService.Utc(value);

        public static List<string> SortedKeys(HashSet<string> values)
        {
            var keys = values.ToList();
            keys.Sort(StringComparer.Ordinal);
            return keys;
        }

        public static List<string> SortedStringMapKeys(Dictionary<string, string> values)
        {
            var keys = values.Keys.ToList();
            keys.Sort(StringComparer.Ordinal);
            return keys;
        }

        // ── 响应视图 ─────────────────────────────────────────────

        public static DirectItemPlanView ToPlanView(DirectItemPlan plan)
        {
            return new DirectItemPlanView
            {
                Character = plan.Character,
                Definition = plan.Definition,
                Amount = plan.Amount,
                Route = plan.Route,
                Existing = plan.Existing,
                After = plan.After,
                SlotsFree = plan.SlotsFree,
                Slots = new List<short>(plan.Touched),
            };
        }

        private static void WriteUInt16(byte[] buffer, int offset, ushort value)
        {
            buffer[offset] = (byte)(value & 0xFF);
            buffer[offset + 1] = (byte)((value >> 8) & 0xFF);
        }

        private static void WriteUInt32(byte[] buffer, int offset, uint value)
        {
            buffer[offset] = (byte)(value & 0xFF);
            buffer[offset + 1] = (byte)((value >> 8) & 0xFF);
            buffer[offset + 2] = (byte)((value >> 16) & 0xFF);
            buffer[offset + 3] = (byte)((value >> 24) & 0xFF);
        }

        private static void WriteUInt32(byte[] buffer, int offset, long value) => WriteUInt32(buffer, offset, (uint)value);
    }
}
