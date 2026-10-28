using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using DNFLogin.Pvf;

namespace DNFLogin.Gm
{
    /// <summary>物品引用（kind + PVF 路径）</summary>
    public sealed class DirectItemReference
    {
        public DirectItemKind Kind { get; set; }
        public string Path { get; set; } = "";
    }

    /// <summary>
    /// PVF 物品目录（移植 Go 版 direct_items.go 的 directPVFCatalog）。
    /// 加载 stackable/equipment 两个 .lst，按需解析物品定义，构建搜索索引。
    /// </summary>
    public sealed class GmPvfCatalog
    {
        public const uint DirectItemRawSize = 0x77;
        public const int DirectItemExpireOffset = 0x38;
        public const uint DirectItemMaxAmount = int.MaxValue;
        public const uint DirectItemQualityMax = 999999997;
        public const string DirectStackableListPath = "stackable/stackable.lst";
        public const string DirectEquipmentListPath = "equipment/equipment.lst";

        private static readonly Dictionary<uint, short> DirectAccountSharedSlots = new()
        {
            [3033] = 354, [3034] = 355, [3035] = 356, [3036] = 357, [3037] = 358, [3262] = 359,
            [10100115] = 360, [10100116] = 361, [10099773] = 362, [10099774] = 363, [10099775] = 364, [10158124] = 365,
        };

        public static bool IsAccountSharedSlot(uint itemID) => DirectAccountSharedSlots.ContainsKey(itemID);

        public static short AccountSharedSlot(uint itemID) => DirectAccountSharedSlots[itemID];

        private readonly PvfArchive _archive;
        private readonly Dictionary<uint, DirectItemReference> _refs;
        private readonly Dictionary<uint, DirectItemDefinition> _definitions = new();
        private readonly object _lock = new();
        private List<DirectItemSearchView>? _searchItems;
        private Exception? _searchError;
        private int _processedCount;

        public PvfArchive Archive => _archive;

        /// <summary>物品引用总数（stackable + equipment 两个 .lst）</summary>
        public int TotalItemCount => _refs.Count;

        /// <summary>搜索索引构建时已解析物品数（供进度展示）</summary>
        public int ProcessedItemCount => _processedCount;

        private GmPvfCatalog(PvfArchive archive, Dictionary<uint, DirectItemReference> refs)
        {
            _archive = archive;
            _refs = refs;
        }

        private sealed class CatalogCacheEntry
        {
            public long Size;
            public long ModTimeTicks;
            public GmPvfCatalog Catalog = null!;
        }

        private static readonly object CatalogCacheLock = new();
        private static readonly Dictionary<string, CatalogCacheEntry> CatalogCache = new();

        /// <summary>加载（或复用缓存）PVF 物品目录</summary>
        public static GmPvfCatalog Load(string pvfPath)
        {
            pvfPath = pvfPath.Trim();
            if (pvfPath.Length == 0)
            {
                throw new InvalidOperationException("PVF 路径为空");
            }
            string absPath = Path.GetFullPath(pvfPath);
            var info = new FileInfo(absPath);
            if (!info.Exists)
            {
                throw new InvalidOperationException($"读取 PVF 文件: {absPath} 不存在");
            }

            lock (CatalogCacheLock)
            {
                if (CatalogCache.TryGetValue(absPath, out var cached) &&
                    cached.Size == info.Length && cached.ModTimeTicks == info.LastWriteTimeUtc.Ticks)
                {
                    return cached.Catalog;
                }

                PvfArchive archive;
                try
                {
                    archive = PvfArchive.Open(absPath);
                }
                catch (Exception ex)
                {
                    throw new InvalidOperationException($"解析 PVF 文件: {ex.Message}", ex);
                }

                var refs = new Dictionary<uint, DirectItemReference>();
                LoadItemList(archive, DirectStackableListPath, DirectItemKind.Stackable, refs);
                LoadItemList(archive, DirectEquipmentListPath, DirectItemKind.Equipment, refs);

                var catalog = new GmPvfCatalog(archive, refs);
                CatalogCache.Clear();
                CatalogCache[absPath] = new CatalogCacheEntry
                {
                    Size = info.Length,
                    ModTimeTicks = info.LastWriteTimeUtc.Ticks,
                    Catalog = catalog,
                };
                return catalog;
            }
        }

        /// <summary>
        /// 异步加载 PVF 目录并构建搜索索引（后台线程执行，UI 不卡死）。
        /// 构建索引期间按已解析物品数/总数通过 progress 回报进度 0~100；
        /// cancellationToken 取消时抛出 OperationCanceledException，缓存不被污染。
        /// </summary>
        public static async Task<GmPvfCatalog> LoadAsync(string pvfPath, IProgress<double>? progress = null, CancellationToken cancellationToken = default)
        {
            var catalog = await Task.Run(() => Load(pvfPath), cancellationToken).ConfigureAwait(false);
            await Task.Run(() => catalog.BuildSearchIndex(progress, cancellationToken), cancellationToken).ConfigureAwait(false);
            return catalog;
        }

        private static void LoadItemList(PvfArchive archive, string listPath, DirectItemKind kind, Dictionary<uint, DirectItemReference> refs)
        {
            string text;
            try
            {
                text = archive.ReadText(listPath);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException($"读取 PVF 列表 {listPath}: {ex.Message}", ex);
            }
            DnfDocument document;
            try
            {
                document = DnfDocument.Parse(listPath, text);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException($"解析 PVF 列表 {listPath}: {ex.Message}", ex);
            }
            foreach (var entry in DnfPvfIndex.ParseList(document))
            {
                if (entry.ID <= 0 || entry.ID > uint.MaxValue)
                {
                    continue;
                }
                uint itemID = (uint)entry.ID;
                if (refs.ContainsKey(itemID))
                {
                    continue;
                }
                refs[itemID] = new DirectItemReference { Kind = kind, Path = CleanDirectPvfPath(entry.Path) };
            }
        }

        /// <summary>解析物品 ID → 定义（带缓存）</summary>
        public DirectItemDefinition Resolve(uint itemID)
        {
            lock (_lock)
            {
                if (_definitions.TryGetValue(itemID, out var cached))
                {
                    return cached;
                }
                if (!_refs.TryGetValue(itemID, out var reference))
                {
                    throw new InvalidOperationException($"当前 PVF 中不存在物品 ID {itemID}");
                }
                var definition = ResolveCore(itemID, reference);
                _definitions[itemID] = definition;
                return definition;
            }
        }

        private DirectItemDefinition ResolveCore(uint itemID, DirectItemReference reference)
        {
            var (docPath, text) = ReadDirectItemText(reference.Kind, reference.Path);
            DnfDocument document;
            try
            {
                document = DnfDocument.Parse(docPath, text);
            }
            catch (Exception ex)
            {
                throw new InvalidOperationException($"解析物品 {itemID}: {ex.Message}", ex);
            }

            var definition = new DirectItemDefinition
            {
                ItemID = itemID,
                Kind = reference.Kind,
                PVFPath = docPath,
            };

            var name = document.Text("name");
            definition.Name = (name ?? "").Trim();
            if (definition.Name.Length == 0)
            {
                definition.Name = "未命名物品";
            }

            definition.AttachType = document.Text("attach type") ?? "";

            var expirationRaw = document.Text("expiration date") ?? "";
            if (expirationRaw.Trim().Length != 0)
            {
                if (!DateTime.TryParseExact(expirationRaw.Trim(), "yyyy-MM-dd HH:mm:ss",
                        CultureInfo.InvariantCulture, DateTimeStyles.None, out var parsed))
                {
                    throw new InvalidOperationException($"过期时间 \"{expirationRaw}\" 解析失败");
                }
                // PVF 内时间按东八区解释后转 UTC
                var cst = TimeZoneInfo.CreateCustomTimeZone("PVFCST", TimeSpan.FromHours(8), "PVFCST", "PVFCST");
                definition.ExpirationDate = TimeZoneInfo.ConvertTimeToUtc(parsed, cst);
            }

            if (reference.Kind == DirectItemKind.Equipment)
            {
                definition.EquipmentType = document.Text("equipment type") ?? "";
                ApplyEquipmentRoute(definition);
                foreach (var field in new[] { "durability", "maximum durability", "max durability" })
                {
                    var value = document.Int(field);
                    if (value is not null && value.Value > 0)
                    {
                        definition.Durability = (ushort)Math.Min(value.Value, ushort.MaxValue);
                        break;
                    }
                }
            }
            else
            {
                definition.StackableType = document.Text("stackable type") ?? "";
                ApplyStackableRoute(definition);
                var stackLimit = document.Int("stack limit");
                if (stackLimit is not null && stackLimit.Value > 0)
                {
                    definition.StackLimit = stackLimit.Value;
                }
                var usablePeriod = document.Int("usable period");
                if (usablePeriod is not null && usablePeriod.Value > 0)
                {
                    definition.UsablePeriodDays = usablePeriod.Value;
                }
            }
            return definition;
        }

        /// <summary>构建搜索索引（含缓存），失败返回上次异常</summary>
        public List<DirectItemSearchView> BuildSearchIndex()
            => BuildSearchIndex(null, CancellationToken.None);

        /// <summary>
        /// 构建搜索索引（含缓存），失败返回上次异常。
        /// 循环内按已解析物品数/总数通过 progress 回报进度 0~100；cancellationToken 取消时抛出 OperationCanceledException。
        /// </summary>
        public List<DirectItemSearchView> BuildSearchIndex(IProgress<double>? progress, CancellationToken cancellationToken)
        {
            lock (_lock)
            {
                if (_searchItems is not null)
                {
                    if (_searchError is not null)
                    {
                        throw _searchError;
                    }
                    return _searchItems;
                }
                var ids = _refs.Keys.OrderBy(x => x).ToList();
                int total = ids.Count;
                var items = new List<DirectItemSearchView>(total);
                Exception? firstError = null;
                _processedCount = 0;
                foreach (var itemID in ids)
                {
                    cancellationToken.ThrowIfCancellationRequested();
                    DirectItemDefinition definition;
                    try
                    {
                        definition = ResolveCore(itemID, _refs[itemID]);
                        ValidateDirectItemDefinition(definition);
                    }
                    catch (Exception ex)
                    {
                        firstError ??= ex;
                        continue;
                    }
                    string category = definition.Kind == DirectItemKind.Equipment ? "equipment" : "item";
                    items.Add(new DirectItemSearchView
                    {
                        ItemID = definition.ItemID,
                        Name = definition.Name,
                        Kind = definition.Kind == DirectItemKind.Equipment ? "equipment" : "stackable",
                        Category = category,
                        PVFPath = definition.PVFPath,
                        StackableType = definition.StackableType,
                        EquipmentType = definition.EquipmentType,
                        StackLimit = definition.StackLimit,
                        ListType = definition.ListType,
                        Destination = definition.Route,
                        SupportsUpgrade = definition.SupportsUpgrade,
                    });
                    _processedCount++;
                    if (progress is not null && (_processedCount % 10 == 0 || _processedCount == total))
                    {
                        progress.Report(100.0 * _processedCount / total);
                    }
                }
                _searchItems = items;
                _searchError = firstError;
                if (firstError is not null)
                {
                    throw firstError;
                }
                return items;
            }
        }

        /// <summary>搜索物品（关键词/分类/分页）</summary>
        public DirectItemSearchResult Search(string keyword, string category, int page, int pageSize)
        {
            var searchItems = BuildSearchIndex();
            keyword = (keyword ?? "").ToLowerInvariant().Trim();
            uint numericID = 0;
            bool numeric = keyword.Length > 0 && uint.TryParse(keyword, NumberStyles.None, CultureInfo.InvariantCulture, out numericID);

            var matches = new List<DirectItemSearchView>();
            foreach (var item in searchItems)
            {
                if (category != "all" && item.Category != category)
                {
                    continue;
                }
                if (numeric)
                {
                    if (item.ItemID != numericID)
                    {
                        continue;
                    }
                }
                else if (keyword.Length > 0 &&
                         !item.Name.ToLowerInvariant().Contains(keyword, StringComparison.Ordinal) &&
                         !item.PVFPath.ToLowerInvariant().Contains(keyword, StringComparison.Ordinal))
                {
                    continue;
                }
                matches.Add(item);
            }

            int pageCount = matches.Count > 0 ? (matches.Count + pageSize - 1) / pageSize : 0;
            int start = (page - 1) * pageSize;
            if (start > matches.Count)
            {
                start = matches.Count;
            }
            int end = start + pageSize;
            if (end > matches.Count)
            {
                end = matches.Count;
            }
            var items = matches.GetRange(start, end - start);
            return new DirectItemSearchResult
            {
                Items = items,
                Total = matches.Count,
                Page = page,
                PageSize = pageSize,
                PageCount = pageCount,
            };
        }

        private (string Path, string Text) ReadDirectItemText(DirectItemKind root, string listed)
        {
            listed = CleanDirectPvfPath(listed);
            var candidates = new List<string> { listed };
            if (!listed.StartsWith(root.ToString() + "/", StringComparison.OrdinalIgnoreCase))
            {
                candidates.Add(CleanDirectPvfPath(root + "/" + listed));
            }
            foreach (var candidate in candidates)
            {
                if (_archive.FindFile(candidate) is not null)
                {
                    return (candidate, _archive.ReadText(candidate));
                }
            }
            throw new InvalidOperationException($"PVF 路径不存在: {listed}");
        }

        // ── 路径与路由工具（照抄 direct_items.go） ───────────────

        public static string CleanDirectPvfPath(string value)
        {
            value = (value ?? "").Trim().Replace('\\', '/');
            if (value.StartsWith("./", StringComparison.Ordinal))
            {
                value = value.Substring(2);
            }
            // path.Clean("/"+value) 语义
            var parts = new List<string>();
            foreach (var seg in value.Split('/'))
            {
                if (seg.Length == 0 || seg == ".")
                {
                    continue;
                }
                if (seg == "..")
                {
                    if (parts.Count > 0)
                    {
                        parts.RemoveAt(parts.Count - 1);
                    }
                    continue;
                }
                parts.Add(seg);
            }
            return string.Join("/", parts);
        }

        public static string NormalizeEquipmentType(string value) =>
            (value ?? "").Trim().Trim('`').ToLowerInvariant();

        public static bool NormalEquipmentType(string value)
        {
            switch (NormalizeEquipmentType(value))
            {
                case "[weapon]":
                case "[title name]":
                case "[coat]":
                case "[shoulder]":
                case "[pants]":
                case "[shoes]":
                case "[waist]":
                case "[amulet]":
                case "[wrist]":
                case "[ring]":
                case "[support]":
                case "[magic stone]":
                case "[support weapon]":
                case "[earring]":
                case "[flag]":
                case "[name tag]":
                    return true;
                default:
                    return false;
            }
        }

        public static void ApplyEquipmentRoute(DirectItemDefinition definition)
        {
            definition.ListType = 0;
            definition.SlotStart = 9;
            definition.SlotEnd = 64;
            definition.Route = "角色装备背包";
            string typ = NormalizeEquipmentType(definition.EquipmentType);
            switch (typ)
            {
                case "[hat avatar]":
                case "[hair avatar]":
                case "[face avatar]":
                case "[coat avatar]":
                case "[pants avatar]":
                case "[shoes avatar]":
                case "[breast avatar]":
                case "[waist avatar]":
                case "[skin avatar]":
                case "[aurora avatar]":
                case "[weapon avatar]":
                    definition.ListType = 1;
                    definition.SlotStart = 0;
                    definition.SlotEnd = 500;
                    definition.Route = "角色时装背包";
                    break;
                case "[creature]":
                    definition.ListType = 7;
                    definition.SlotStart = 0;
                    definition.SlotEnd = 139;
                    definition.Route = "角色宠物背包";
                    break;
                case "[artifact red]":
                case "[artifact blue]":
                case "[artifact green]":
                    definition.ListType = 7;
                    definition.SlotStart = 140;
                    definition.SlotEnd = 188;
                    definition.Route = "角色宠物装备背包";
                    break;
                default:
                    definition.SupportsUpgrade = NormalEquipmentType(typ);
                    break;
            }
        }

        public static string NormalizeStackableType(string value) =>
            (value ?? "").Replace("`", "").Trim().ToLowerInvariant();

        public static void ApplyStackableRoute(DirectItemDefinition definition)
        {
            definition.ListType = 0;
            (definition.SlotStart, definition.SlotEnd) = StackableSlotRange(definition.StackableType);
            definition.Route = "角色道具背包";
            if (NormalizeStackableType(definition.StackableType) == "[feed]")
            {
                definition.ListType = 7;
                definition.SlotStart = 189;
                definition.SlotEnd = 238;
                definition.Route = "角色宠物消耗品背包";
            }
        }

        public static (short Start, short End) StackableSlotRange(string value)
        {
            string normalized = NormalizeStackableType(value);
            if (normalized.StartsWith("[material]", StringComparison.Ordinal) && normalized.EndsWith("4", StringComparison.Ordinal))
            {
                return (345, 359);
            }
            if (normalized.StartsWith("[material]", StringComparison.Ordinal))
            {
                return (121, 176);
            }
            if (normalized.StartsWith("[quest]", StringComparison.Ordinal))
            {
                return (177, 232);
            }
            if (normalized.StartsWith("[material expert job]", StringComparison.Ordinal))
            {
                return (233, 288);
            }
            if (normalized.StartsWith("[avatar emblem]", StringComparison.Ordinal))
            {
                return (289, 344);
            }
            return (65, 120);
        }

        public static bool PrefersQuickSlots(string value)
        {
            string normalized = NormalizeStackableType(value);
            return normalized.StartsWith("[waste]", StringComparison.Ordinal) ||
                   normalized.StartsWith("[material]", StringComparison.Ordinal);
        }

        public static void ValidateDirectItemDefinition(DirectItemDefinition definition)
        {
            if (definition.ItemID == 0 || definition.SlotStart < 0 || definition.SlotEnd < definition.SlotStart || definition.Route.Length == 0)
            {
                throw new InvalidOperationException("PVF 物品定义不完整");
            }
            if (definition.Kind != DirectItemKind.Stackable && definition.Kind != DirectItemKind.Equipment)
            {
                throw new InvalidOperationException($"物品 {definition.ItemID} 的 PVF 类型不受支持");
            }
        }
    }
}
