using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using System.Windows.Media.Animation;

namespace DNFLogin.Gm
{
    /// <summary>GM 工具主面板（对应 Go 版 GmPage：角色/属性/转职/物品/邮件钱包/设置）</summary>
    public partial class GmPageControl : UserControl
    {
        private readonly GmSettings _settings;
        private readonly GmDatabaseService _db = new();
        private readonly GmCharacterService _svc;
        private readonly GmItemEngine _engine;
        private readonly DoubleAnimation _loadingRotateAnimation = new(0, 360, TimeSpan.FromMilliseconds(1200))
        {
            RepeatBehavior = RepeatBehavior.Forever,
        };

        private CharacterView? _selectedRole;
        private DirectItemSearchResult? _itemResult;
        private DirectItemSearchView? _selectedItem;
        private GmPvfCatalog? _pvfCatalog;
        private int _itemPage = 1;
        private string _itemKeyword = "";
        private string _itemCategory = "all";
        private bool _busy;

        private const int ItemPageSize = 200;

        public GmPageControl()
        {
            InitializeComponent();
            _settings = GmConfig.Load().Clone();
            _svc = new GmCharacterService(_db);
            _engine = new GmItemEngine(_db);
            // 联动：若登录器已完成服务端初始化，自动同步 GM 连接信息（失败则沿用既有配置）
            SyncFromLoginConfig();
            LoadSettingsTab();
            BuildJobCombos();
            UpdateRoleState();
            Status("就绪");

            // 兜底：页面加载时若服务端已在运行，自动连接数据库（失败静默，懒连接下次查询重试）
            Loaded += async (_, _) =>
            {
                if (DNFLogin.Services.ServerManager.ServerRunning)
                {
                    await AutoConnectAsync();
                }
            };
        }

        /// <summary>
        /// 从登录器服务端配置（AppConfig.ServerRootPath + instance.json）同步 GM 连接信息。
        /// 联动字段：Host / Port / Database / User / Password / PvfPath / InstancePath；
        /// 未检测到有效服务端配置时返回 false，且不覆盖现有配置。
        /// </summary>
        public bool SyncFromLoginConfig()
        {
            var root = AppConfig.Settings.ServerRootPath?.Trim() ?? "";
            if (string.IsNullOrWhiteSpace(root) || !Directory.Exists(root))
            {
                return false;
            }

            var info = DNFLogin.Services.ServerManager.LoadInfo(root);
            if (info is null)
            {
                return false;
            }

            if (!string.IsNullOrWhiteSpace(info.DatabaseHost))
            {
                _settings.Host = info.DatabaseHost;
            }
            if (info.DatabasePort > 0)
            {
                _settings.Port = info.DatabasePort;
            }
            if (!string.IsNullOrWhiteSpace(info.DatabaseName))
            {
                _settings.Database = info.DatabaseName;
            }
            if (!string.IsNullOrWhiteSpace(info.DatabaseUser))
            {
                _settings.User = info.DatabaseUser;
            }
            _settings.Password = info.DatabasePassword ?? _settings.Password;

            // 数据库模式与 SQLite 路径同步（instance.json database.mode / database.path）
            if (!string.IsNullOrWhiteSpace(info.DatabaseMode))
            {
                _settings.Mode = info.DatabaseMode;
            }
            if (!string.IsNullOrWhiteSpace(info.DatabasePath))
            {
                _settings.DatabasePath = info.DatabasePath;
            }

            // instance.json 的 game.pvfPath 为相对 runtime 的路径，拼接为绝对路径
            if (!string.IsNullOrWhiteSpace(info.PvfPath))
            {
                _settings.PvfPath = Path.IsPathRooted(info.PvfPath)
                    ? info.PvfPath
                    : Path.Combine(root, "runtime", info.PvfPath);
            }

            _settings.InstancePath = Path.Combine(root, "runtime", "config", "instance.json");

            GmConfig.Save(_settings);
            LoadSettingsTab();
            SettingsStatus.Text = "已自动同步服务端配置";
            Status("已自动同步服务端配置");
            return true;
        }

        private void Status(string message)
        {
            StatusBar.Text = message;
        }

        /// <summary>
        /// 自动连接数据库（服务端启动就绪后调用）。成功刷新状态栏；失败不弹阻塞框，
        /// 静默记录，由 GmDatabaseService.EnsureConnectedAsync 懒连接机制在下次查询时自动重试。
        /// </summary>
        public async Task AutoConnectAsync()
        {
            try
            {
                SaveSettingsTab();
                GmConfig.Save(_settings);
                await _db.ConnectAsync(_settings).ConfigureAwait(true);
                SettingsStatus.Text = "数据库已自动连接";
                Status("数据库已自动连接");
            }
            catch (Exception)
            {
                SettingsStatus.Text = "数据库未连接（查询时将自动重试）";
                Status("数据库未连接（查询时将自动重试）");
            }
        }

        /// <summary>
        /// 服务端启动后后台预加载 PVF 索引（Task.Run 不阻塞启动流程），
        /// 加载期间显示中央转圈层并回报进度，完成/失败后隐藏。
        /// </summary>
        public async Task PreloadPvfAsync()
        {
            try
            {
                SaveSettingsTab();
                GmConfig.Save(_settings);
                if (string.IsNullOrWhiteSpace(_settings.PvfPath))
                {
                    return;
                }
                ShowLoading("正在加载物品索引...");
                var progress = new Progress<double>(p =>
                {
                    LoadingText.Text = $"正在解析物品索引 {p:0}%";
                });
                _pvfCatalog = await GmPvfCatalog.LoadAsync(_settings.PvfPath, progress, CancellationToken.None).ConfigureAwait(true);
                HideLoading();
                SettingsStatus.Text = $"PVF 索引已就绪（{_pvfCatalog.TotalItemCount:N0} 件物品）";
                Status("PVF 索引已就绪（后台预加载）");
            }
            catch (Exception)
            {
                HideLoading();
                SettingsStatus.Text = "PVF 预加载失败，请检查 PVF 文件是否完整或路径是否正确";
                Status("PVF 预加载失败，请检查 PVF 文件是否完整或路径是否正确");
            }
        }

        private void ShowLoading(string text)
        {
            LoadingText.Text = text;
            LoadingOverlay.Visibility = Visibility.Visible;
            LoadingRotate.BeginAnimation(RotateTransform.AngleProperty, _loadingRotateAnimation);
        }

        private void HideLoading()
        {
            LoadingOverlay.Visibility = Visibility.Collapsed;
            LoadingRotate.BeginAnimation(RotateTransform.AngleProperty, null);
        }

        private void SetBusy(bool busy)
        {
            _busy = busy;
            IsEnabled = !busy;
        }

        // ── 设置 Tab ───────────────────────────────────────────

        private void LoadSettingsTab()
        {
            SetHost.Text = _settings.Host;
            SetPort.Text = _settings.Port.ToString(CultureInfo.InvariantCulture);
            SetDatabase.Text = _settings.Database;
            SetUser.Text = _settings.User;
            SetPassword.Password = _settings.Password;
            SetPvfPath.Text = _settings.PvfPath;
            SetInstancePath.Text = _settings.InstancePath;
        }

        private void SaveSettingsTab()
        {
            _settings.Host = SetHost.Text.Trim();
            if (int.TryParse(SetPort.Text.Trim(), out var port) && port > 0 && port < 65536)
            {
                _settings.Port = port;
            }
            _settings.Database = SetDatabase.Text.Trim();
            _settings.User = SetUser.Text.Trim();
            _settings.Password = SetPassword.Password;
            _settings.PvfPath = SetPvfPath.Text.Trim();
            _settings.InstancePath = SetInstancePath.Text.Trim();
        }

        private void SaveConfigBtn_Click(object sender, RoutedEventArgs e)
        {
            SaveSettingsTab();
            GmConfig.Save(_settings);
            SettingsStatus.Text = "配置已保存";
            Status("配置已保存");
        }

        private void TestConnBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                SaveSettingsTab();
                GmConfig.Save(_settings);
                await _db.ConnectAsync(_settings).ConfigureAwait(true);
                SettingsStatus.Text = "数据库连接成功";
                Status("数据库连接成功");
            }, err => SettingsStatus.Text = "连接失败：" + err);
        }

        private void LoadPvfBtn_Click(object sender, RoutedEventArgs e)
        {
            if (string.IsNullOrWhiteSpace(SetPvfPath.Text.Trim()))
            {
                SettingsStatus.Text = "请先填写 PVF 路径";
                return;
            }
            _ = PreloadPvfAsync();
        }

        // ── 角色管理 Tab ───────────────────────────────────────

        private void QueryBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var queryType = (QueryTypeCombo.SelectedIndex == 0) ? "account" : "character";
                var query = QueryInput.Text.Trim();
                if (query.Length == 0)
                {
                    CharList.ItemsSource = await _svc.ListAllCharactersDirectAsync().ConfigureAwait(true);
                    Status($"已列出全部角色 {CharList.Items.Count} 个");
                    return;
                }
                var rows = await _svc.ListCharactersAsync(_settings, queryType, query).ConfigureAwait(true);
                CharList.ItemsSource = rows;
                Status($"查询完成，共 {rows.Count} 个角色");
            }, err => Status("查询失败：" + err));
        }

        private void CharList_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            _selectedRole = CharList.SelectedItem as CharacterView;
            UpdateRoleState();
        }

        private void UpdateRoleState()
        {
            if (_selectedRole is null)
            {
                SelectedCharInfo.Text = "未选择角色";
                SelectedCharExtra.Text = "";
                PropTarget.Text = "未选择角色";
                CurrentJobInfo.Text = "未选择角色";
                ItemRoleInfo.Text = "未选择角色";
                MailRoleInfo.Text = "未选择角色";
                WalletRoleInfo.Text = "未选择角色";
                return;
            }
            var role = _selectedRole;
            SelectedCharInfo.Text = $"角色：{role.Name}（{role.CharacterID}）  等级：{role.Level}";
            SelectedCharExtra.Text = $"账号：{role.AccountID}  金币：{role.Gold:N0}  点券：{role.Cera:N0}";
            PropTarget.Text = $"目标角色：{role.Name}（{role.CharacterID}）";
            CurrentJobInfo.Text = $"目标角色：{role.Name}（{role.CharacterID}）";
            ItemRoleInfo.Text = role.Name;
            MailRoleInfo.Text = role.Name;
            WalletRoleInfo.Text = role.Name;
        }

        // ── 属性编辑 Tab ───────────────────────────────────────

        private void LoadStatsBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                var stats = await _svc.QueryStatsAsync(role.CharacterID).ConfigureAwait(true);
                StatLine1.Text = $"力量：{stats.Strength}  智力：{stats.Intelligence}  体力：{stats.Vitality}  精神：{stats.Spirit}";
                StatLine2.Text = $"HP：{stats.HPMax:N0}  MP：{stats.MPMax:N0}";
                StatLine3.Text = $"物攻：{stats.PhysicalAttack}  物防：{stats.PhysicalDefense}  魔攻：{stats.MagicalAttack}  魔防：{stats.MagicalDefense}  独立：{stats.IndependentAtk}";
                StatLine4.Text = $"火抗：{stats.FireRes}  水抗：{stats.WaterRes}  暗抗：{stats.DarkRes}  光抗：{stats.LightRes}";
                var abnormal = new List<string>();
                for (var i = 0; i < 18; i++)
                {
                    if (stats.AbnormalResistance[i] != 0)
                    {
                        abnormal.Add($"{AbnormalLabels.Labels[i]}:{stats.AbnormalResistance[i]}");
                    }
                }
                StatLine5.Text = "异常抗性：" + (abnormal.Count == 0 ? "无" : string.Join(" ", abnormal));
                StatLine6.Text = $"移动速度：{stats.MoveSpeed}  攻速：{stats.AttackSpeed}  施放：{stats.CastSpeed}  硬直：{stats.HitRecovery}";
                StatLine7.Text = $"跳跃：{stats.JumpPower}  负重：{stats.Weight:N0}  负重等级：{stats.StatLevel}";
                StatLine8.Text = $"HP回复：{stats.HPRegenSpeed}  MP回复：{stats.MPRegenSpeed}  背包上限：{stats.InventoryLimit:N0}";
                StatLine9.Text = "";
                StatLine10.Text = "";
                StatLine11.Text = "";
                StatLine12.Text = "";
                StatLine13.Text = "";
                StatLine14.Text = "";

                var sptp = await _svc.QuerySPTPAsync(_settings, role.CharacterID).ConfigureAwait(true);
                SPInput.Text = sptp.RemainingSP.ToString(CultureInfo.InvariantCulture);
                TPInput.Text = sptp.RemainingTP.ToString(CultureInfo.InvariantCulture);
                LevelInput.Text = sptp.Level.ToString(CultureInfo.InvariantCulture);
                Status("属性读取完成");
            }, err => Status("读取属性失败：" + err));
        }

        private void RefreshSPTPBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                var sptp = await _svc.QuerySPTPAsync(_settings, role.CharacterID).ConfigureAwait(true);
                SPInput.Text = sptp.RemainingSP.ToString(CultureInfo.InvariantCulture);
                TPInput.Text = sptp.RemainingTP.ToString(CultureInfo.InvariantCulture);
                LevelInput.Text = sptp.Level.ToString(CultureInfo.InvariantCulture);
                Status($"SP/TP 已刷新（总SP {sptp.TotalSP} / 总TP {sptp.TotalTP}）");
            }, err => Status("刷新 SP/TP 失败：" + err));
        }

        private void ApplySPTPBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                int? sp = null;
                int? tp = null;
                int? level = null;
                if (int.TryParse(SPInput.Text.Trim(), out var spVal))
                {
                    sp = spVal;
                }
                if (int.TryParse(TPInput.Text.Trim(), out var tpVal))
                {
                    tp = tpVal;
                }
                if (int.TryParse(LevelInput.Text.Trim(), out var lvVal))
                {
                    level = lvVal;
                }
                if (sp.HasValue || tp.HasValue)
                {
                    await _svc.UpdateSPTPAsync(_settings, role.CharacterID, sp, tp).ConfigureAwait(true);
                }
                if (level.HasValue)
                {
                    await _svc.UpdateLevelAsync(_settings, role.CharacterID, level.Value).ConfigureAwait(true);
                }
                if (sp is null && tp is null && level is null)
                {
                    throw new InvalidOperationException("请输入 SP/TP/等级 至少一项");
                }
                Status("SP/TP/等级 已更新");
            }, err => Status("更新失败：" + err));
        }

        private void UnlockDungeonsBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                var count = await _svc.UnlockDungeonsAsync(_settings, role.CharacterID).ConfigureAwait(true);
                Status($"副本全解锁完成，共处理 {count} 个副本");
            }, err => Status("副本解锁失败：" + err));
        }

        private void UnlockSlotsBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                var (before, after) = await _svc.UnlockEquipSlotsAsync(_settings, role.CharacterID).ConfigureAwait(true);
                Status($"装备槽解锁完成：{before} → {after}");
            }, err => Status("装备槽解锁失败：" + err));
        }

        // ── 转职觉醒 Tab ───────────────────────────────────────

        private void BuildJobCombos()
        {
            JobCombo.ItemsSource = GmClassData.ClassTree;
            JobCombo.DisplayMemberPath = nameof(ClassDef.JobName);
            if (GmClassData.ClassTree.Count > 0)
            {
                JobCombo.SelectedIndex = 0;
            }
        }

        private void JobCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (JobCombo.SelectedItem is not ClassDef job)
            {
                return;
            }
            GrowCombo.ItemsSource = job.GrowTypes;
            GrowCombo.DisplayMemberPath = nameof(GrowTypeDef.Name);
            if (job.GrowTypes.Count > 0)
            {
                GrowCombo.SelectedIndex = 0;
            }
            else
            {
                GrowCombo.ItemsSource = null;
                AwakeCombo.ItemsSource = null;
            }
        }

        private void GrowCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            if (GrowCombo.SelectedItem is not GrowTypeDef grow)
            {
                AwakeCombo.ItemsSource = null;
                return;
            }
            var awakenings = new List<string> { "无" };
            awakenings.AddRange(grow.Awakenings);
            AwakeCombo.ItemsSource = awakenings;
            AwakeCombo.SelectedIndex = 0;
        }

        private void ApplyJobBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                if (JobCombo.SelectedItem is not ClassDef job)
                {
                    throw new InvalidOperationException("请选择职业");
                }
                var firstGrow = 0;
                var secondGrow = 0;
                var growName = "";
                if (GrowCombo.SelectedItem is GrowTypeDef grow)
                {
                    firstGrow = grow.GrowType;
                    growName = grow.Name;
                    var awakeIndex = AwakeCombo.SelectedIndex;
                    if (awakeIndex > 0)
                    {
                        secondGrow = awakeIndex; // 1=一觉, 2=二觉
                    }
                }
                await _svc.UpdateJobAsync(_settings, role.CharacterID, job.JobID, firstGrow, secondGrow).ConfigureAwait(true);
                JobPreview.Text = $"已应用：{job.JobName} / {(firstGrow > 0 ? growName : "无转职")} / {(secondGrow > 0 ? "觉醒 Lv" + secondGrow : "无觉醒")}";
                Status($"转职应用完成：{job.JobName}");
            }, err => Status("转职失败：" + err));
        }

        // ── 物品发放 Tab ───────────────────────────────────────

        private void ItemSearchBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                _itemKeyword = ItemSearchInput.Text.Trim();
                _itemCategory = ItemCategoryToKey(ItemCategoryCombo.SelectedIndex);
                _itemPage = 1;
                await LoadItemPageAsync().ConfigureAwait(true);
            }, err => Status("物品搜索失败：" + err));
        }

        private void ItemPrevPageBtn_Click(object sender, RoutedEventArgs e)
        {
            if (_itemPage <= 1)
            {
                return;
            }
            _itemPage--;
            _ = RunAsync(async () => await LoadItemPageAsync().ConfigureAwait(true), err => Status("翻页失败：" + err));
        }

        private void ItemNextPageBtn_Click(object sender, RoutedEventArgs e)
        {
            if (_itemResult is null || _itemPage >= _itemResult.PageCount)
            {
                return;
            }
            _itemPage++;
            _ = RunAsync(async () => await LoadItemPageAsync().ConfigureAwait(true), err => Status("翻页失败：" + err));
        }

        private async Task LoadItemPageAsync()
        {
            if (_pvfCatalog is null)
            {
                ShowLoading("正在加载物品索引...");
                try
                {
                    _pvfCatalog = await GmPvfCatalog.LoadAsync(_settings.PvfPath, null, CancellationToken.None).ConfigureAwait(true);
                }
                finally
                {
                    HideLoading();
                }
            }
            var catalog = _pvfCatalog;
            _itemResult = await Task.Run(() => catalog.Search(_itemKeyword, _itemCategory, _itemPage, ItemPageSize)).ConfigureAwait(true);
            ItemList.ItemsSource = _itemResult.Items;
            ItemPageInfo.Text = $"{_itemResult.Page}/{_itemResult.PageCount}（共 {_itemResult.Total}）";
            _selectedItem = null;
            ItemResult.Text = "";
        }

        private static string ItemCategoryToKey(int index)
        {
            return index switch
            {
                1 => "消耗品",
                2 => "装备",
                3 => "材料",
                4 => "任务",
                5 => "活动",
                _ => "all",
            };
        }

        private void ItemList_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            _selectedItem = ItemList.SelectedItem as DirectItemSearchView;
            if (_selectedItem is not null)
            {
                ItemResult.Text = $"[{_selectedItem.ItemID}] {_selectedItem.Name}（{_selectedItem.Category}）去向：{_selectedItem.Destination}";
            }
        }

        private void ItemPreviewBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var (role, item, amount) = ParseItemGrantInput();
                var plan = await _engine.ProcessAsync(_settings, role.Name, item.ItemID, amount, 0, 0, 0, false).ConfigureAwait(true);
                ItemResult.Text = $"预览：现有 {plan.Existing:N0} → 发放后 {plan.After:N0}，空槽 {plan.SlotsFree} 个，占用槽位：{string.Join(",", plan.Slots.Select(s => s.ToString(CultureInfo.InvariantCulture)))}";
                Status("物品预览完成");
            }, err => Status("预览失败：" + err));
        }

        private void ItemGrantBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var (role, item, amount) = ParseItemGrantInput();
                var reinforce = 0;
                var amplify = 0;
                var amplifyType = AmplifyTypeCombo.SelectedIndex; // 0=无
                int.TryParse(ItemReinforceInput.Text.Trim(), out reinforce);
                int.TryParse(ItemAmplifyInput.Text.Trim(), out amplify);
                var plan = await _engine.ProcessAsync(_settings, role.Name, item.ItemID, amount, reinforce, amplify, amplifyType, true).ConfigureAwait(true);
                ItemResult.Text = $"已发放 {item.Name} x{amount} 给 {role.Name}（现有 {plan.Existing:N0} → 共 {plan.After:N0}）";
                Status("物品发放完成");
            }, err => Status("发放失败：" + err));
        }

        private (CharacterView Role, DirectItemSearchView Item, uint Amount) ParseItemGrantInput()
        {
            var role = RequireRole();
            if (_selectedItem is null)
            {
                throw new InvalidOperationException("请先在列表中选中要发放的物品");
            }
            if (!uint.TryParse(ItemAmountInput.Text.Trim(), out var amount) || amount == 0)
            {
                throw new InvalidOperationException("数量无效");
            }
            return (role, _selectedItem, amount);
        }

        // ── 邮件与钱包 Tab ─────────────────────────────────────

        private void MailSendBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                if (!long.TryParse(MailGoldInput.Text.Trim(), out var gold))
                {
                    gold = 0;
                }
                uint itemId = 0;
                uint itemCount = 0;
                if (!string.IsNullOrWhiteSpace(MailItemIdInput.Text.Trim()))
                {
                    if (!uint.TryParse(MailItemIdInput.Text.Trim(), out itemId))
                    {
                        throw new InvalidOperationException("物品 ID 无效");
                    }
                }
                if (!string.IsNullOrWhiteSpace(MailItemCountInput.Text.Trim()))
                {
                    if (!uint.TryParse(MailItemCountInput.Text.Trim(), out itemCount))
                    {
                        throw new InvalidOperationException("物品数量无效");
                    }
                }
                var result = await _svc.SendMailAsync(_settings, role.Name, MailTitleInput.Text, MailBodyInput.Text,
                    gold, itemId, itemCount, MailBindCheck.IsChecked == true).ConfigureAwait(true);
                MailResult.Text = result.Message;
                Status("邮件发送完成");
            }, err => MailResult.Text = "发送失败：" + err);
        }

        private void WalletQueryBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                var result = await _svc.QueryWalletAsync(_settings, role.Name).ConfigureAwait(true);
                WalletResultText.Text = $"角色：{result.Character.Name}  金币：{result.Gold:N0}  点券：{result.Cera:N0}（{result.Mode}）";
                Status("钱包查询完成");
            }, err => WalletResultText.Text = "查询失败：" + err);
        }

        private void WalletGoldBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                if (!long.TryParse(WalletGoldInput.Text.Trim(), out var amount) || amount <= 0)
                {
                    throw new InvalidOperationException("金币数量无效");
                }
                var result = await _svc.UpdateWalletNativeAsync(_settings, role.Name, "gold", amount).ConfigureAwait(true);
                WalletResultText.Text = result.Message;
                Status("金币邮件已发送");
            }, err => WalletResultText.Text = "发送失败：" + err);
        }

        private void WalletCeraBtn_Click(object sender, RoutedEventArgs e)
        {
            _ = RunAsync(async () =>
            {
                var role = RequireRole();
                if (!long.TryParse(WalletCeraInput.Text.Trim(), out var amount) || amount <= 0)
                {
                    throw new InvalidOperationException("点券数量无效");
                }
                var result = await _svc.UpdateWalletNativeAsync(_settings, role.Name, "cera", amount).ConfigureAwait(true);
                WalletResultText.Text = result.Message;
                Status("点券已发放");
            }, err => WalletResultText.Text = "发放失败：" + err);
        }

        // ── 通用 ───────────────────────────────────────────────

        private CharacterView RequireRole()
        {
            if (_selectedRole is null)
            {
                throw new InvalidOperationException("请先在角色管理中查询并选择角色");
            }
            return _selectedRole;
        }

        private async Task RunAsync(Func<Task> action, Action<string> onError)
        {
            if (_busy)
            {
                return;
            }
            SetBusy(true);
            try
            {
                await action().ConfigureAwait(true);
            }
            catch (Exception ex)
            {
                onError?.Invoke(ex.Message);
            }
            finally
            {
                SetBusy(false);
            }
        }
    }
}
