using System;
using System.Collections;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Media.Imaging;
using DfoGmTool;
using DfoGmTool.Services;
// 别名：规避 DNFLogin.Gm 命名空间内已有 GmConfig 等类型的遮蔽
using S4GmConfig = DfoGmTool.GmConfig;
using S4Env = DfoGmTool.Services.GmRuntimeEnvironment;
using S4GmService = DfoGmTool.Services.GmService;
using S4PvfIndex = DfoGmTool.Services.PvfIndexService;
using S4ItemIcon = DfoGmTool.Services.ItemIconResult;

namespace DNFLogin.Gm
{
    /// <summary>
    /// S4 系列（A12/A21）内嵌 GM 管理台：源码级直连 SQLite（inventory.db）+ PVF 索引，
    /// 复用 Web GM 的 GmRuntimeEnvironment/GmService 业务层，无需启动 5050 Web 服务。
    /// </summary>
    public partial class S4GmPageControl : UserControl
    {
        private S4Env? _env;

        public S4GmPageControl()
        {
            InitializeComponent();
            Loaded += (_, _) => ReloadEnvironment();
        }

        // ---------- 环境 ----------

        private void ReloadButton_Click(object sender, RoutedEventArgs e) => ReloadEnvironment();

        public void ReloadEnvironment()
        {
            try
            {
                SetStatus("加载中...", "#FFFFB74D");
                var root = AppConfig.Settings.ServerRootPath;
                if (string.IsNullOrWhiteSpace(root) || !Directory.Exists(root))
                {
                    SetStatus("未找到服务端目录", "#FFFF6B6B");
                    DbPathText.Text = "数据库: -";
                    PvfPathText.Text = "PVF: -";
                    Log("请先在「游戏配置」中选择 A12/A21 服务端根目录（包含 DfoServer.exe 的目录）");
                    return;
                }

                var db = Path.Combine(root, "Data", "inventory.db");
                var pvf = Path.Combine(root, "Data", "Pvf", "Script.pvf");
                if (!S4GmConfig.TryCreate(db, pvf, out var config, out var error))
                {
                    SetStatus("环境创建失败", "#FFFF6B6B");
                    Log("环境创建失败: " + error);
                    return;
                }

                _env = new S4Env(config);

                var st = _env.GetStatus();
                DbPathText.Text = "数据库: " + (st.Database ?? "-");
                PvfPathText.Text = "PVF: " + (st.Pvf ?? "-");
                if (st.Ready)
                {
                    SetStatus("已就绪", "#FF7BD389");
                    Log("GM 环境已就绪（SQLite 直连，PVF 索引" + (st.IndexReady ? "可用" : "构建中") + "）");
                }
                else
                {
                    SetStatus(st.Error ?? "未就绪", "#FFFFB74D");
                    Log("环境状态: " + (st.Error ?? "未知"));
                }

                // 物品分类下拉（就绪后自动加载；索引未就绪时返回 ready=false，点击"刷新分类"可重试）
                LoadItemCategories();
            }
            catch (Exception ex)
            {
                SetStatus("初始化异常", "#FFFF6B6B");
                Log("初始化异常: " + ex.Message);
            }
        }

        private void SetStatus(string text, string colorHex)
        {
            StatusText.Text = text;
            try { StatusText.Foreground = new SolidColorBrush((Color)ColorConverter.ConvertFromString(colorHex)); }
            catch { StatusText.Foreground = Brushes.White; }
        }

        private void Log(string msg)
        {
            LogText.Text = msg;
        }

        // ---------- 执行辅助 ----------

        /// <summary>执行 GM 操作；返回 null 表示成功，否则返回错误信息。data 为业务数据（List 或 null）。</summary>
        private string? Run(Func<S4GmService, S4PvfIndex, object> op, out object? data)
        {
            data = null;
            if (_env == null) return "GM 环境未初始化";
            try
            {
                var r = _env.Execute(op);
                if (r == null) return null;
                var t = r.GetType();
                var sp = t.GetProperty("success");
                if (sp == null) { data = r; return null; } // 非包装结果，直接当数据
                var ok = sp.GetValue(r) is bool b && b;
                data = t.GetProperty("data")?.GetValue(r)
                       ?? t.GetProperty("results")?.GetValue(r)
                       ?? t.GetProperty("items")?.GetValue(r)
                       ?? t.GetProperty("quests")?.GetValue(r);
                var err = t.GetProperty("error")?.GetValue(r) as string;
                return ok ? null : (err ?? "操作失败");
            }
            catch (Exception ex)
            {
                return "异常: " + ex.Message;
            }
        }

        private static IEnumerable ToList(object? data) => data as IEnumerable ?? Array.Empty<object>();

        private static int GetInt(TextBox box, int fallback)
            => int.TryParse(box?.Text?.Trim(), out var v) ? v : fallback;

        private static long GetLong(TextBox box, long fallback)
            => long.TryParse(box?.Text?.Trim(), out var v) ? v : fallback;

        private static object? GetProp(object? o, params string[] names)
        {
            if (o == null) return null;
            var t = o.GetType();
            foreach (var n in names)
            {
                var p = t.GetProperty(n);
                if (p != null) return p.GetValue(o);
            }
            return null;
        }

        private static int GetPropInt(object? o, int fallback, params string[] names)
            => GetProp(o, names) is IConvertible c ? c.ToInt32(null) : fallback;

        private void LogResult(string? err, string action)
        {
            if (err == null) Log(action + "成功");
            else Log(action + "失败: " + err);
        }

        // ---------- 列头美化 ----------

        private static void BeautifyColumn(object sender, DataGridAutoGeneratingColumnEventArgs e)
        {
            if (e.PropertyName.Length > 0)
                e.Column.Header = char.ToUpper(e.PropertyName[0]) + e.PropertyName.Substring(1);
        }

        private void AccountGrid_AutoGeneratingColumn(object sender, DataGridAutoGeneratingColumnEventArgs e) => BeautifyColumn(sender, e);
        private void CharacterGrid_AutoGeneratingColumn(object sender, DataGridAutoGeneratingColumnEventArgs e) => BeautifyColumn(sender, e);
        private void ItemGrid_AutoGeneratingColumn(object sender, DataGridAutoGeneratingColumnEventArgs e) => BeautifyColumn(sender, e);
        private void QuestGrid_AutoGeneratingColumn(object sender, DataGridAutoGeneratingColumnEventArgs e) => BeautifyColumn(sender, e);
        private void MailGrid_AutoGeneratingColumn(object sender, DataGridAutoGeneratingColumnEventArgs e) => BeautifyColumn(sender, e);

        // ---------- 账号管理 ----------

        private void RefreshAccounts_Click(object sender, RoutedEventArgs e)
        {
            var err = Run((gm, _) => gm.ListAccounts(), out var data);
            if (err != null) { Log("刷新账号失败: " + err); return; }
            AccountGrid.ItemsSource = ToList(data);
            Log("账号列表已刷新");
        }

        private void AccountGrid_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (AccountGrid.SelectedItem == null) return;
            var id = GetPropInt(AccountGrid.SelectedItem, -1, "AccountId", "accountId");
            if (id > 0) AccountIdBox.Text = id.ToString();
        }

        private void SetAccountCera_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var val = GetLong(AccountCeraBox, -1);
            if (aid <= 0 || val < 0) { Log("请填写有效的账号ID与CERA值"); return; }
            var err = Run((gm, _) => gm.AdjustAccountCurrency(aid, "cera", 0, val), out _);
            LogResult(err, "设置账号CERA");
        }

        private void SetAccountToken_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var val = GetLong(AccountTokenBox, -1);
            if (aid <= 0 || val < 0) { Log("请填写有效的账号ID与TokenCERA值"); return; }
            var err = Run((gm, _) => gm.AdjustAccountCurrency(aid, "token", 0, val), out _);
            LogResult(err, "设置账号TokenCERA");
        }

        private void SetAccountLucky_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var val = GetLong(AccountLuckyBox, -1);
            if (aid <= 0 || val < 0) { Log("请填写有效的账号ID与幸运星值"); return; }
            var err = Run((gm, _) => gm.AdjustAccountCurrency(aid, "luckystar", 0, val), out _);
            LogResult(err, "设置账号幸运星");
        }

        private void SetAccountHonor_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var val = GetInt(AccountHonorBox, -1);
            if (aid <= 0 || val < 0) { Log("请填写有效的账号ID与荣誉等级"); return; }
            var err = Run((gm, _) => gm.SetAccountHonorLevel(aid, val), out _);
            LogResult(err, "设置荣誉等级");
        }

        private void MaxAccountCapsule_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            if (aid <= 0) { Log("请填写有效的账号ID"); return; }
            var err = Run((gm, _) => gm.MaxGrowthCapsuleExp(aid), out _);
            LogResult(err, "成长胶囊拉满");
        }

        private void ClearAccountCargo_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            if (aid <= 0) { Log("请填写有效的账号ID"); return; }
            var err = Run((gm, _) => gm.ClearAccountCargo(aid), out _);
            LogResult(err, "清空账号仓库");
        }

        private void AdjustCube_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var itemId = GetInt(CubeItemIdBox, -1);
            var amount = GetInt(CubeAmountBox, int.MinValue);
            if (aid <= 0 || itemId <= 0 || amount == int.MinValue) { Log("请填写有效的账号ID/晶块ItemID/数量"); return; }
            var err = Run((gm, _) => gm.AdjustCubeFragment(aid, itemId, amount), out _);
            LogResult(err, "调整晶块");
        }

        private void AdjustSoul_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var itemId = GetInt(SoulItemIdBox, -1);
            var amount = GetInt(SoulAmountBox, int.MinValue);
            if (aid <= 0 || itemId <= 0 || amount == int.MinValue) { Log("请填写有效的账号ID/灵魂ItemID/数量"); return; }
            var err = Run((gm, _) => gm.AdjustSoulWarehouse(aid, itemId, amount), out _);
            LogResult(err, "调整灵魂");
        }

        // ---------- 角色管理 ----------

        private void RefreshCharacters_Click(object sender, RoutedEventArgs e)
        {
            var aid = GetInt(AccountIdBox, -1);
            var err = Run((gm, _) => gm.ListCharacters(aid), out var data);
            if (err != null) { Log("刷新角色失败: " + err); return; }
            CharacterGrid.ItemsSource = ToList(data);
            Log(aid > 0 ? $"角色列表已刷新（账号 {aid}）" : "角色列表已刷新（全部账号）");
        }

        private void CharacterGrid_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (CharacterGrid.SelectedItem == null) return;
            var id = GetPropInt(CharacterGrid.SelectedItem, -1, "characterId", "CharacterId");
            if (id > 0)
            {
                ItemTargetCharBox.Text = id.ToString();
                QuestCharBox.Text = id.ToString();
                MailCharBox.Text = id.ToString();
            }
        }

        private void SetCharLevel_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var level = GetInt(CharLevelBox, -1);
            if (cid <= 0 || level < 0) { Log("请填写有效的角色ID与等级"); return; }
            var err = Run((gm, _) => gm.SetLevel(cid, level), out _);
            LogResult(err, "设置等级");
        }

        private void SetCharGold_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var val = GetInt(CharGoldBox, int.MinValue);
            if (cid <= 0 || val == int.MinValue) { Log("请填写有效的角色ID与金币值"); return; }
            var err = Run((gm, _) => gm.SetWalletValue(cid, "gold", val), out _);
            LogResult(err, "设置金币");
        }

        private void SetCharCera_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var val = GetInt(CharCeraBox, int.MinValue);
            if (cid <= 0 || val == int.MinValue) { Log("请填写有效的角色ID与CERA值"); return; }
            var err = Run((gm, _) => gm.SetWalletValue(cid, "cera", val), out _);
            LogResult(err, "设置角色CERA");
        }

        private void AdjustCharSpTp_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var sp = GetInt(CharSpBox, int.MinValue);
            var tp = GetInt(CharTpBox, int.MinValue);
            if (cid <= 0 || sp == int.MinValue || tp == int.MinValue) { Log("请填写有效的角色ID/SP/TP（可为0）"); return; }
            var err = Run((gm, _) => gm.AdjustSpTp(cid, sp, tp), out _);
            LogResult(err, "调整SP/TP");
        }

        private void RenameChar_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var name = CharNameBox.Text?.Trim();
            if (cid <= 0 || string.IsNullOrWhiteSpace(name)) { Log("请填写有效的角色ID与新名字"); return; }
            var err = Run((gm, _) => gm.RenameCharacter(cid, name), out _);
            LogResult(err, "改名");
        }

        private void GetGrowOptions_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, _) => gm.GetGrowOptions(cid), out var data);
            if (err != null) { Log("获取成长选项失败: " + err); return; }
            var text = DumpData(data);
            Log("成长选项: " + (text.Length > 500 ? text.Substring(0, 500) + "..." : text));
        }

        private void GetExpertJob_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, _) => gm.GetExpertJob(cid), out var data);
            if (err != null) { Log("获取专家职业失败: " + err); return; }
            Log("专家职业: " + DumpData(data));
        }

        private void ListCharItems_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.ListItems(cid, pvf), out var data);
            if (err != null) { Log("查看物品失败: " + err); return; }
            ItemGrid.ItemsSource = ToList(data);
            ItemDetailText.Text = $"角色 {cid} 物品列表（{(data as IEnumerable ?? Array.Empty<object>()).Cast<object>().Count()} 项）";
            Log($"已加载角色 {cid} 的物品列表");
        }

        private void SetGrowType_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var first = GetInt(GrowFirstBox, -1);
            var second = GetInt(GrowSecondBox, -1);
            if (cid <= 0 || first < 0 || second < 0) { Log("请填写有效的角色ID与成长转职/觉醒（可为0）"); return; }
            var err = Run((gm, _) => gm.SetGrowType(cid, first, second), out _);
            LogResult(err, $"设置成长类型 {first}/{second}");
        }

        private void SetExpertJob_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var type = GetInt(ExpertTypeBox, -1);
            if (cid <= 0 || type < 0) { Log("请填写有效的角色ID与专家职业类型"); return; }
            int? level = null;
            long? exp = null;
            if (int.TryParse(ExpertLevelBox?.Text?.Trim(), out var lv)) level = lv;
            if (long.TryParse(ExpertExpBox?.Text?.Trim(), out var ex)) exp = ex;
            var err = Run((gm, _) => gm.SetExpertJob(cid, type, level, exp), out _);
            LogResult(err, $"设置专家职业 {type} (Lv{level?.ToString() ?? "-"}/Exp{exp?.ToString() ?? "-"})");
        }

        private void MaxExpertJob_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var type = GetInt(ExpertTypeBox, -1);
            if (cid <= 0 || type < 0) { Log("请填写有效的角色ID与专家职业类型"); return; }
            var err = Run((gm, _) => gm.MaxExpertJob(cid, type), out _);
            LogResult(err, $"专家职业 {type} 拉满");
        }

        private void RemoveItem_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var templateId = GetInt(ItemRemoveTemplateBox, -1);
            var count = GetInt(ItemRemoveCountBox, 1);
            if (cid <= 0 || templateId <= 0) { Log("请填写有效的角色ID与物品模板ID"); return; }
            if (count <= 0) count = 1;
            var err = Run((gm, _) => gm.RemoveItem(cid, templateId, count), out _);
            LogResult(err, $"删除物品 {templateId} x{count}（角色 {cid}）");
        }

        // ---------- 物品索引 ----------

        private sealed class CategoryOption
        {
            public string Display { get; set; } = "";
            public string? Kind { get; set; }
            public string? Tag { get; set; }
            public string? Segment { get; set; }
            public override string ToString() => Display;
        }

        private bool _loadingCategories;

        private void ItemSearchBox_KeyDown(object sender, KeyEventArgs e)
        {
            if (e.Key == Key.Enter) SearchItems_Click(sender, e);
        }

        private void LoadItemCategories_Click(object sender, RoutedEventArgs e) => LoadItemCategories();

        /// <summary>加载物品分类（装备按部位标签、堆叠物按背包入格分类），失败时仅记录日志不阻塞搜索</summary>
        private void LoadItemCategories()
        {
            if (_env == null) return;
            var err = Run((gm, pvf) => pvf.GetItemCategories(), out var data);
            if (err != null)
            {
                Log("加载物品分类失败: " + err);
                return;
            }
            if (data == null) return;

            _loadingCategories = true;
            try
            {
                var selected = ItemCategoryCombo.SelectedItem as CategoryOption;
                ItemCategoryCombo.Items.Clear();
                ItemCategoryCombo.Items.Add(new CategoryOption { Display = "全部分类" });

                var equipment = GetProp(data, "equipment") as IEnumerable;
                if (equipment != null)
                {
                    foreach (var c in equipment)
                    {
                        var tag = GetProp(c, "tag")?.ToString();
                        var count = GetPropInt(c, 0, "count");
                        if (string.IsNullOrEmpty(tag)) continue;
                        ItemCategoryCombo.Items.Add(new CategoryOption
                        {
                            Display = $"装备 / {tag}（{count}）",
                            Kind = "equipment",
                            Tag = tag
                        });
                    }
                }

                var stackable = GetProp(data, "stackable") as IEnumerable;
                if (stackable != null)
                {
                    foreach (var c in stackable)
                    {
                        var segment = GetProp(c, "segment")?.ToString();
                        var count = GetPropInt(c, 0, "count");
                        if (string.IsNullOrEmpty(segment)) continue;
                        ItemCategoryCombo.Items.Add(new CategoryOption
                        {
                            Display = $"消耗品 / {segment}（{count}）",
                            Kind = "stackable",
                            Segment = segment
                        });
                    }
                }

                // 恢复原选中分类（若仍存在）
                var match = selected != null
                    ? ItemCategoryCombo.Items.Cast<CategoryOption>().FirstOrDefault(o => o.Display == selected.Display)
                    : null;
                ItemCategoryCombo.SelectedItem = match ?? ItemCategoryCombo.Items[0];
            }
            finally
            {
                _loadingCategories = false;
            }
        }

        private void ItemCategoryCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (_loadingCategories || !IsLoaded) return;
            SearchItems_Click(sender, e);
        }

        private void SearchItems_Click(object sender, RoutedEventArgs e)
        {
            var query = ItemSearchBox.Text?.Trim() ?? "";
            var cat = ItemCategoryCombo.SelectedItem as CategoryOption;
            var err = Run((gm, pvf) => pvf.SearchItems(query,
                    cat?.Kind, cat?.Tag, cat?.Segment, null, 0, 0, 0, 60, 0, null),
                out var data);
            if (err != null) { Log("搜索物品失败: " + err); return; }
            ItemGrid.ItemsSource = ToList(data);
            ItemDetailText.Text = $"物品搜索「{query}」" + (cat != null && cat.Display != "全部分类" ? " / 分类 " + cat.Display : "") + "（最多60条）";
            Log($"物品搜索完成: {(data as IEnumerable ?? Array.Empty<object>()).Cast<object>().Count()} 条");
        }

        private void ItemGrid_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (ItemGrid.SelectedItem == null) return;
            var itemId = GetPropInt(ItemGrid.SelectedItem, -1, "itemId", "ItemId", "id", "templateId");
            if (itemId <= 0) return;
            PreviewItemById(itemId);
        }

        private void PreviewItem_Click(object sender, RoutedEventArgs e)
        {
            var itemId = GetPropInt(ItemGrid.SelectedItem ?? new { }, -1, "itemId", "ItemId", "id", "templateId");
            if (itemId <= 0) { Log("请先在物品列表中选择一项"); return; }
            PreviewItemById(itemId);
        }

        private void PreviewItemById(int itemId)
        {
            var err = Run((gm, pvf) => pvf.GetItemPreview(itemId), out var data);
            if (err != null) { Log("预览失败: " + err); return; }
            var name = GetProp(data, "name")?.ToString();
            ItemDetailText.Text = name != null ? $"[{itemId}] {name}\n" + DumpData(data) : DumpData(data);
            if (_env != null)
            {
                var icon = _env.TryGetItemIcon(itemId);
                if (icon.Png != null)
                {
                    using var ms = new MemoryStream(icon.Png);
                    var bmp = new BitmapImage();
                    bmp.BeginInit();
                    bmp.CacheOption = BitmapCacheOption.OnLoad;
                    bmp.StreamSource = ms;
                    bmp.EndInit();
                    bmp.Freeze();
                    ItemIconImage.Source = bmp;
                }
                else
                {
                    ItemIconImage.Source = null;
                }
            }
            Log($"已预览物品 {itemId}");
        }

        private void GiveItem_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(ItemTargetCharBox, -1);
            var count = GetInt(ItemCountBox, 1);
            var itemId = GetPropInt(ItemGrid.SelectedItem ?? new { }, -1, "itemId", "ItemId", "id", "templateId");
            if (cid <= 0 || itemId <= 0) { Log("请选择物品并填写有效角色ID"); return; }
            if (count <= 0) count = 1;
            var err = Run((gm, pvf) => gm.GiveItem(cid, itemId, count, pvf), out var data);
            LogResult(err, $"发放物品 {itemId} x{count} → 角色 {cid}");
        }

        // ---------- 任务管理 ----------

        private void ListQuests_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.ListQuests(cid, pvf), out var data);
            if (err != null) { Log("列出任务失败: " + err); return; }
            QuestGrid.ItemsSource = ToList(data);
            Log($"角色 {cid} 当前任务已列出");
        }

        private void SearchQuests_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            var query = QuestSearchBox.Text?.Trim() ?? "";
            if (cid <= 0 || string.IsNullOrWhiteSpace(query)) { Log("请填写有效的角色ID与搜索关键词"); return; }
            var err = Run((gm, pvf) => gm.SearchQuests(cid, query, 30, pvf), out var data);
            if (err != null) { Log("搜索任务失败: " + err); return; }
            QuestGrid.ItemsSource = ToList(data);
            Log($"任务搜索「{query}」完成");
        }

        private void MainQuestOverview_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.MainQuestOverview(cid, pvf), out var data);
            if (err != null) { Log("主线概览失败: " + err); return; }
            QuestGrid.ItemsSource = ToList(data);
            Log($"角色 {cid} 主线概览已加载");
        }

        private void AchievementOverview_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.AchievementOverview(cid, pvf), out var data);
            if (err != null) { Log("成就概览失败: " + err); return; }
            QuestGrid.ItemsSource = ToList(data);
            Log($"角色 {cid} 成就概览已加载");
        }

        private void ListClearedQuests_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.ListClearedQuests(cid, pvf), out var data);
            if (err != null) { Log("加载已完成任务失败: " + err); return; }
            QuestGrid.ItemsSource = ToList(data);
            Log($"角色 {cid} 已完成任务列表已加载");
        }

        private void QuestGrid_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            // 无特殊处理；选中项用于完成/清除/批量
        }

        private static List<int> SelectedQuestIds(DataGrid grid)
        {
            var ids = new List<int>();
            foreach (var item in grid.SelectedItems)
            {
                var id = GetPropInt(item, -1, "questId", "QuestId");
                if (id > 0) ids.Add(id);
            }
            return ids;
        }

        private void CompleteSelectedQuest_Click(object sender, RoutedEventArgs e)
        {
            var ids = SelectedQuestIds(QuestGrid);
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0 || ids.Count == 0) { Log("请选择任务并填写有效角色ID"); return; }
            if (ids.Count == 1)
            {
                var err = Run((gm, pvf) => gm.CompleteQuestChain(cid, ids[0], pvf), out _);
                LogResult(err, $"完成任务 {ids[0]}");
            }
            else
            {
                var err = Run((gm, pvf) => gm.CompleteQuestBatch(cid, ids), out _);
                LogResult(err, $"批量完成 {ids.Count} 个任务");
            }
        }

        private void UnclearSelectedQuest_Click(object sender, RoutedEventArgs e)
        {
            var ids = SelectedQuestIds(QuestGrid);
            var cid = GetInt(QuestCharBox, -1);
            if (cid <= 0 || ids.Count == 0) { Log("请选择任务并填写有效角色ID"); return; }
            foreach (var id in ids)
            {
                var err = Run((gm, pvf) => gm.UnclearQuest(cid, id), out _);
                if (err != null) Log($"清除任务 {id} 失败: {err}");
            }
            Log($"已清除 {ids.Count} 个任务");
        }

        private void BatchCompleteQuests_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            var ids = SelectedQuestIds(QuestGrid);
            if (cid <= 0 || ids.Count == 0) { Log("请选择任务并填写有效角色ID"); return; }
            var err = Run((gm, pvf) => gm.CompleteQuestBatch(cid, ids), out _);
            LogResult(err, $"批量完成 {ids.Count} 个任务");
        }

        /// <summary>标记任务完成（MarkQuestReady：将任务推进到可提交状态）</summary>
        private void MarkReadyQuest_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            var ids = SelectedQuestIds(QuestGrid);
            if (cid <= 0 || ids.Count == 0) { Log("请选择任务并填写有效角色ID"); return; }
            var ok = 0;
            foreach (var id in ids)
            {
                var err = Run((gm, pvf) => gm.MarkQuestReady(cid, id), out _);
                if (err != null) Log($"标记完成 {id} 失败: {err}");
                else ok++;
            }
            Log($"已标记 {ok}/{ids.Count} 个任务为可完成状态");
        }

        /// <summary>强制完成任务（ForceCompleteQuest：无条件完成，含未满足前置条件）</summary>
        private void ForceCompleteQuest_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(QuestCharBox, -1);
            var ids = SelectedQuestIds(QuestGrid);
            if (cid <= 0 || ids.Count == 0) { Log("请选择任务并填写有效角色ID"); return; }
            var ok = 0;
            foreach (var id in ids)
            {
                var err = Run((gm, pvf) => gm.ForceCompleteQuest(cid, id), out _);
                if (err != null) Log($"强制完成 {id} 失败: {err}");
                else ok++;
            }
            Log($"已强制完成 {ok}/{ids.Count} 个任务");
        }

        // ---------- 邮件管理 ----------

        private void RefreshMail_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(MailCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.ListMailbox(cid, pvf), out var data);
            if (err != null) { Log("刷新邮件失败: " + err); return; }
            MailGrid.ItemsSource = ToList(data);
            Log($"角色 {cid} 邮件已刷新");
        }

        private void DeleteMail_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(MailCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var msgId = GetPropInt(MailGrid.SelectedItem ?? new { }, -1, "messageId", "MessageId", "id");
            if (msgId <= 0) { Log("请先在邮件列表中选择一项"); return; }
            var err = Run((gm, pvf) => gm.DeleteMailboxMessage(cid, msgId), out _);
            LogResult(err, $"删除邮件 {msgId}");
        }

        private void ClearMail_Click(object sender, RoutedEventArgs e)
        {
            var cid = GetInt(MailCharBox, -1);
            if (cid <= 0) { Log("请填写有效的角色ID"); return; }
            var err = Run((gm, pvf) => gm.ClearMailbox(cid), out _);
            LogResult(err, "清空邮箱");
        }

        // ---------- 数据展示辅助 ----------

        private static string DumpData(object? data)
        {
            if (data == null) return "(空)";
            if (data is string s) return s;
            if (data is IEnumerable en && data is not string)
            {
                var parts = new List<string>();
                foreach (var item in en)
                {
                    if (parts.Count >= 5) { parts.Add("..."); break; }
                    parts.Add(DumpData(item));
                }
                return "[" + string.Join(", ", parts) + "]";
            }
            var t = data.GetType();
            var props = t.GetProperties(BindingFlags.Public | BindingFlags.Instance)
                .Where(p => p.GetIndexParameters().Length == 0)
                .Select(p => $"{p.Name}={p.GetValue(data)?.ToString() ?? "null"}")
                .ToArray();
            if (props.Length == 0) return data.ToString() ?? "";
            return string.Join(" | ", props.Take(12));
        }
    }
}
