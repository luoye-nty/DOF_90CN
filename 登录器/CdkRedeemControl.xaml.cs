using System;
using System.Collections.Generic;
using System.Text;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using DNFLogin.Services;

namespace DNFLogin
{
    /// <summary>
    /// 90 外网版 CDK 兑换页：输入 20 位兑换码 → 选择账号/角色 → 系统邮件发放奖励。
    /// 成功时输出区只显示 CDK 名称；失败显示明确原因。
    ///
    /// 两条取票路径：
    ///   ① 已登录会话：LoginWindow 切页时 PushSessions 注入（主/副槽位已登录 90 会话）；
    ///   ② 手动登录：本页直接「账号 + 密码」登录认证服务换 ticket —— 用于"人没进游戏/服务器
    ///      没启动"的场景（例如玩家在别的机器上兑换、或服主给玩家补发）。
    ///      刻意要求密码，不允许"只填账号就列出角色"，否则任何人都能拿别人账号名
    ///      枚举角色列表（账号名是可猜的，密码不是）。
    /// </summary>
    public partial class CdkRedeemControl : UserControl
    {
        /// <summary>CDK 页会话快照（由 LoginWindow 从 _remoteSessions 构建并注入）</summary>
        public sealed class CdkSessionInfo
        {
            public int Slot { get; }
            public string Ticket { get; }
            public string AccountId { get; }
            public string Username { get; }

            /// <summary>是否是本页「手动登录」换来的 ticket（不启动游戏、不占在线名额）</summary>
            public bool Manual { get; }

            public CdkSessionInfo(int slot, string ticket, string accountId, string username, bool manual = false)
            {
                Slot = slot;
                Ticket = ticket ?? string.Empty;
                AccountId = accountId ?? string.Empty;
                Username = username ?? string.Empty;
                Manual = manual;
            }

            public string DisplayName => Manual
                ? Username + "（手动登录）"
                : Username + (Slot == GameProcessManager.MainSlot
                    ? "（主）"
                    : Slot == GameProcessManager.SecondarySlot
                        ? "（副）"
                        // 8 开扩容后还有 3~8 号槽位：按槽位号显示，不再一律叫"副"。
                        : $"（{Slot + 1} 号窗口）");

            public override string ToString() => DisplayName;
        }

        /// <summary>手动登录会话的伪槽位（永不与真实主/副槽位相同）</summary>
        private const int ManualSlot = -1;

        /// <summary>兑换码字符集/长度：唯一出处见 <see cref="CdkCodeExtractor"/>（避免与批量解析各写一份漂移）。</summary>
        private const string CodeCharset = CdkCodeExtractor.CodeCharset;
        private const int CodeUnitLength = CdkCodeExtractor.CodeLength;

        /// <summary>码框里"当前已识别"的 20 位码（不带横线）。空串表示还没识别到。</summary>
        private string _currentCode = string.Empty;

        private static readonly SolidColorBrush SuccessBrush = new(Color.FromRgb(0x52, 0xC4, 0x1A));
        private static readonly SolidColorBrush ErrorBrush = new(Color.FromRgb(0xFF, 0x70, 0x43));
        private static readonly SolidColorBrush DefaultBrush = new(Color.FromRgb(0xE6, 0xE6, 0xE6));

        private readonly List<CdkSessionInfo> _sessions = new();
        private CdkSessionInfo? _manualSession;
        private bool _suppressCodeFormat;
        private bool _loadingRoles;
        private bool _redeeming;
        private bool _manualLoggingIn;

        private CdkSessionInfo? SelectedSession => AccountComboBox.SelectedItem as CdkSessionInfo;
        private CdkCharacter? SelectedCharacter => CharacterComboBox.SelectedItem as CdkCharacter;

        public CdkRedeemControl()
        {
            InitializeComponent();
            OutputTextBox.Foreground = DefaultBrush;
            // 粘贴单独拦一道：拿**完整剪贴板文本**抠码，而不是让 WPF 先把整段塞进码框
            // 再指望过滤器把中文滤掉（旧实现正是这样把码截断/错位成 20 位的）。
            // 范式与同库 RescueSetupWindow.cs 的 CodeBox_OnPaste 一致。
            DataObject.AddPastingHandler(CodeTextBox, CodeTextBox_OnPaste);
            UpdateCodeStatus(string.Empty);
        }

        /// <summary>登录窗口切换至本页时注入当前已登录的 90 会话快照（变化时重建账号下拉并自动拉角色）</summary>
        public void PushSessions(IReadOnlyList<CdkSessionInfo> sessions)
        {
            var changed = sessions is null
                ? _sessions.Count != 0
                : !SameSessionList(_sessions, sessions);
            if (!changed)
            {
                return;
            }

            _sessions.Clear();
            if (sessions is not null)
            {
                foreach (var s in sessions)
                {
                    _sessions.Add(s);
                }
            }

            // 手动登录会话不随主窗口会话刷新而丢失（否则切一次页就得重新输密码）。
            RebuildAccountSelection();
        }

        /// <summary>账号下拉的完整候选集：已登录会话 + 手动登录会话（如有）</summary>
        private List<CdkSessionInfo> BuildSessionChoices()
        {
            var list = new List<CdkSessionInfo>(_sessions);
            if (_manualSession is not null &&
                !list.Exists(s => string.Equals(s.Ticket, _manualSession.Ticket, StringComparison.Ordinal)))
            {
                list.Add(_manualSession);
            }

            return list;
        }

        private static bool SameSessionList(IReadOnlyList<CdkSessionInfo> a, IReadOnlyList<CdkSessionInfo> b)
        {
            if (a.Count != b.Count)
            {
                return false;
            }

            for (var i = 0; i < a.Count; i++)
            {
                var x = a[i];
                var y = b[i];
                if (x.Slot != y.Slot ||
                    !string.Equals(x.Ticket, y.Ticket, StringComparison.Ordinal) ||
                    !string.Equals(x.AccountId, y.AccountId, StringComparison.Ordinal) ||
                    !string.Equals(x.Username, y.Username, StringComparison.Ordinal))
                {
                    return false;
                }
            }

            return true;
        }

        private void RebuildAccountSelection()
        {
            CharacterComboBox.ItemsSource = null;
            SetOutput(string.Empty, true);

            var choices = BuildSessionChoices();
            if (choices.Count == 0)
            {
                AccountComboBox.ItemsSource = null;
                SessionStatusText.Text = "未检测到已登录的账号：可以在下方用「账号 + 密码」手动登录后兑换（不会启动游戏）";
                SessionStatusText.Foreground = DefaultBrush;
                RedeemButton.IsEnabled = false;
                if (RefreshCharactersButton is not null)
                {
                    RefreshCharactersButton.IsEnabled = false;
                }

                if (ManualLoginButton is not null)
                {
                    ManualLoginButton.IsEnabled = !_manualLoggingIn;
                }

                return;
            }

            SessionStatusText.Text = "已选账号：将按选中账号拉取游戏角色，奖励通过游戏内邮件发放";
            SessionStatusText.Foreground = DefaultBrush;
            AccountComboBox.ItemsSource = null;
            AccountComboBox.ItemsSource = choices;

            // 默认选中主槽（槽位 0）会话；无主槽则选第一个
            AccountComboBox.SelectedItem = null;
            foreach (var s in choices)
            {
                if (!s.Manual && s.Slot == GameProcessManager.MainSlot)
                {
                    AccountComboBox.SelectedItem = s;
                    break;
                }
            }

            if (AccountComboBox.SelectedItem is null && choices.Count > 0)
            {
                AccountComboBox.SelectedItem = choices[0];
            }

            RefreshCharactersButton.IsEnabled = true;
            if (ManualLoginButton is not null)
            {
                ManualLoginButton.IsEnabled = !_manualLoggingIn;
            }

            UpdateRedeemState();
        }

        /// <summary>
        /// 手动登录：账号 + 密码 → /launcher/v1/login 换 ticket → 拉该账号角色列表。
        /// 不启动游戏、不写入 _remoteSessions，因此不影响在线人数/心跳/踢人轮询。
        /// </summary>
        private async void ManualLoginButton_Click(object sender, RoutedEventArgs e)
        {
            if (_manualLoggingIn)
            {
                return;
            }

            var account = ManualAccountTextBox.Text?.Trim() ?? string.Empty;
            var password = ManualPasswordBox.Password ?? string.Empty;
            if (string.IsNullOrWhiteSpace(account) || string.IsNullOrWhiteSpace(password))
            {
                SetOutput("手动登录需要同时填写账号与密码（只填账号不能查看该账号的角色列表）", false);
                return;
            }

            _manualLoggingIn = true;
            ManualLoginButton.IsEnabled = false;
            ManualLoginButton.Content = "登录中…";
            try
            {
                var (ok, err, ticket, accountId, _) =
                    await RemoteAuthService.LoginForTicketAsync(AppConfig.Settings, account, password)
                        .ConfigureAwait(true);
                if (!ok || string.IsNullOrWhiteSpace(ticket))
                {
                    SetOutput(string.IsNullOrWhiteSpace(err) ? "登录失败，请检查账号密码" : err, false);
                    SessionStatusText.Text = "手动登录失败：兑换需要先通过账号密码验证";
                    SessionStatusText.Foreground = ErrorBrush;
                    return;
                }

                _manualSession = new CdkSessionInfo(ManualSlot, ticket, accountId, account, manual: true);
                // 密码不落内存、不写配置：登录成功后立刻清空输入框。
                ManualPasswordBox.Password = string.Empty;
                RebuildAccountSelection();
                // 自动选中刚登录的账号并拉角色（RebuildAccountSelection 默认选主槽，这里改选手动会话）。
                AccountComboBox.SelectedItem = _manualSession;
                await LoadCharactersAsync();
            }
            finally
            {
                _manualLoggingIn = false;
                ManualLoginButton.Content = "登录并获取角色";
                ManualLoginButton.IsEnabled = true;
                UpdateRedeemState();
            }
        }

        private async void AccountComboBox_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            CharacterComboBox.ItemsSource = null;
            if (SelectedSession is not null)
            {
                await LoadCharactersAsync();
            }
            else
            {
                UpdateRedeemState();
            }
        }

        private async void RefreshCharactersButton_Click(object sender, RoutedEventArgs e)
        {
            await LoadCharactersAsync();
        }

        private async Task LoadCharactersAsync()
        {
            if (_loadingRoles)
            {
                return;
            }

            var session = SelectedSession;
            if (session is null)
            {
                SetOutput("请先选择账号：已登录会话，或用下方「账号 + 密码」手动登录", false);
                return;
            }

            var (host, port) = ResolveEndpoint();
            _loadingRoles = true;
            RefreshCharactersButton.IsEnabled = false;
            CharacterComboBox.ItemsSource = null;
            SetOutput("正在获取角色列表…", true);

            try
            {
                var (ok, err, characters) = await RemoteAuthService.GetCharactersAsync(
                    AppConfig.Settings, host, port, session.Ticket).ConfigureAwait(true);

                if (!ok)
                {
                    SessionStatusText.Text = "角色列表获取失败";
                    SessionStatusText.Foreground = ErrorBrush;
                    SetOutput(err, false);
                    // 手动登录的 ticket 失效（过期/被清理）：丢掉它并提示重新登录，
                    // 否则玩家会一直对着一个永远失败的账号点"刷新角色"。
                    if (session.Manual && LooksLikeTicketExpired(err))
                    {
                        _manualSession = null;
                        RebuildAccountSelection();
                    }

                    return;
                }

                if (characters.Count == 0)
                {
                    SessionStatusText.Text = "当前账号下暂无角色，请先在游戏内创建角色";
                    SessionStatusText.Foreground = ErrorBrush;
                    SetOutput("当前账号下暂无角色，请先在游戏内创建角色后再兑换", false);
                    return;
                }

                SessionStatusText.Text = $"账号 {session.Username} 共有 {characters.Count} 个角色";
                SessionStatusText.Foreground = DefaultBrush;
                CharacterComboBox.ItemsSource = characters;
                CharacterComboBox.SelectedItem = characters[0];
                // 文案复位（2026-10-15 反馈）：拉取成功必须把上面那句"正在获取角色列表…"覆盖掉，
                // 否则加载完成后输出区一直停在加载态，玩家以为卡住了（此前只有失败/无角色两条
                // 分支会改写文案，成功这条漏了）。
                SetOutput($"已获取到 {characters.Count} 个角色：填好 20 位兑换码后点「兑 换」即可发放。", true);
            }
            finally
            {
                _loadingRoles = false;
                RefreshCharactersButton.IsEnabled = BuildSessionChoices().Count > 0;
                UpdateRedeemState();
            }
        }

        /// <summary>错误文案是否属于"登录凭证失效"（用于清掉手动登录会话并提示重新登录）</summary>
        private static bool LooksLikeTicketExpired(string error)
        {
            if (string.IsNullOrWhiteSpace(error))
            {
                return false;
            }

            return error.Contains("登录已过期", StringComparison.Ordinal) ||
                   error.Contains("重新登录", StringComparison.Ordinal) ||
                   error.Contains("凭证", StringComparison.Ordinal);
        }

        private async void RedeemButton_Click(object sender, RoutedEventArgs e)
        {
            if (_redeeming || _batchRunning)
            {
                return;
            }

            var session = SelectedSession;
            var character = SelectedCharacter;

            // 码从"当前已识别"取（extractor 抠出来的，已去横线、全大写）。
            // 旧实现直接对码框文本 Replace("-","")，只要框里混进中文就必然长度不符。
            var code = _currentCode;
            if (session is null)
            {
                SetOutput("请先选择账号：已登录会话，或用下方「账号 + 密码」手动登录", false);
                return;
            }

            if (character is null)
            {
                SetOutput("请先加载并选择要投放奖励的角色", false);
                return;
            }

            if (code.Length != CodeUnitLength)
            {
                // 这句以前是死代码（按钮在长度不符时已经变灰，永远点不到），
                // 现在按钮放行由 UpdateRedeemState 决定，但玩家一旦点进来必须看到原因。
                SetOutput("没能从码框内容里识别出 20 位兑换码（格式：XXXX-XXXX-XXXX-XXXX-XXXX）。\n" +
                          "可以整段粘贴发卡网给的内容（含「卡号：」「密码：」等标签），会自动提取。", false);
                UpdateCodeStatus(_currentCode, "没识别到 20 位兑换码：请整段粘贴发卡网内容，或手输 20 位码");
                return;
            }

            var (host, port) = ResolveEndpoint();
            _redeeming = true;
            RedeemButton.IsEnabled = false;
            RedeemButton.Content = "兑换中…";

            try
            {
                var (ok, err, name, message) = await RemoteAuthService.RedeemCdkAsync(
                    AppConfig.Settings, host, port, session.Ticket, code, character.CharacterId).ConfigureAwait(true);

                if (ok)
                {
                    // 成功：显示 CDK 名称 + 后端发放明细（点券余额 / 黑钻到期 / 物品件数）。
                    // 老后端没有 message 时只显示名称，行为与改造前一致。
                    var text = string.IsNullOrWhiteSpace(message) ? name : message;
                    if (!string.IsNullOrWhiteSpace(name) &&
                        !text.Contains(name, StringComparison.Ordinal))
                    {
                        text = name + "\r\n" + text;
                    }

                    SetOutput(text, true);
                }
                else
                {
                    SetOutput(err, false);
                }
            }
            finally
            {
                _redeeming = false;
                RedeemButton.Content = "兑 换";
                UpdateRedeemState();
            }
        }

        private void CharacterComboBox_SelectionChanged(object sender, SelectionChangedEventArgs e)
        {
            UpdateRedeemState();
        }

        /// <summary>
        /// 码框文本变化：**extractor 优先、过滤兜底**。
        ///
        /// 旧实现是"取前 20 个合法字符"，于是发卡网的
        /// <c>卡号：XXXX-… 密码：1234</c> 会把中文丢掉、把订单号/密码里的数字当成码开头，
        /// 凑够 20 位却是错的（服务端 404），玩家只看到"兑换不了"。
        ///
        /// 现在：整段文本能抠出唯一 20 位码就用它（中文标签/全角/换行全交给 extractor）；
        /// 抠不出来（例如玩家正在一个字一个字手输）才退回"过滤 + 每 4 位加横线"的输入体验，
        /// 并且**一定**在码框下方说明当前状态，不再出现"按钮变灰但零提示"。
        /// </summary>
        private void CodeTextBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            if (_suppressCodeFormat)
            {
                return;
            }

            var tb = CodeTextBox;
            if (tb is null)
            {
                return;
            }

            var raw = tb.Text ?? string.Empty;

            // ① extractor 优先：整段粘贴（含中文标签）能唯一确定一个码时直接采用。
            if (CdkCodeExtractor.TryExtract(raw, out var extracted) &&
                CdkBatchParser.IsValidCode(extracted))
            {
                var canonical = CdkBatchParser.FormatCode(extracted);
                if (!string.Equals(canonical, raw, StringComparison.Ordinal))
                {
                    _suppressCodeFormat = true;
                    tb.Text = canonical;
                    _suppressCodeFormat = false;
                    tb.CaretIndex = canonical.Length;
                }

                _currentCode = extracted;
                UpdateCodeStatus(extracted);
                UpdateRedeemState();
                return;
            }

            // ② 兜底：只要字符集里的字符，够 20 位就边输边格式化（手输体验）。
            var caret = tb.CaretIndex;
            var unitsBefore = 0;
            for (var i = 0; i < caret && i < raw.Length; i++)
            {
                if (CodeCharset.IndexOf(char.ToUpperInvariant(CdkCodeExtractor.ToHalfWidth(raw[i]))) >= 0)
                {
                    unitsBefore++;
                }
            }

            var code = new StringBuilder();
            foreach (var ch in raw)
            {
                if (code.Length >= CodeUnitLength)
                {
                    break;
                }

                var up = char.ToUpperInvariant(CdkCodeExtractor.ToHalfWidth(ch));
                if (CodeCharset.IndexOf(up) >= 0)
                {
                    code.Append(up);
                }
            }

            _currentCode = code.Length == CodeUnitLength ? code.ToString() : string.Empty;
            var formatted = FormatCode(code.ToString());
            if (!string.Equals(formatted, raw, StringComparison.Ordinal))
            {
                _suppressCodeFormat = true;
                tb.Text = formatted;
                _suppressCodeFormat = false;
                tb.CaretIndex = MapUnitToIndex(formatted, Math.Min(unitsBefore, code.Length));
            }

            UpdateCodeStatus(_currentCode);
            UpdateRedeemState();
        }

        /// <summary>
        /// 粘贴拦截：拿**完整剪贴板文本**跑 extractor。
        ///
        /// 成功 → 直接把格式化后的码写进码框（此时 MaxLength 已放宽到 512，不会再被截断），
        /// 玩家看到的就是"粘贴即识别"；失败 → 原样贴进去并给一句明确提示，
        /// 而不是静默把标签里的数字当成码。
        /// </summary>
        private void CodeTextBox_OnPaste(object sender, DataObjectPastingEventArgs e)
        {
            if (!e.DataObject.GetDataPresent(typeof(string)))
            {
                e.CancelCommand();
                UpdateCodeStatus(string.Empty, "剪贴板里没有文本内容，请复制发卡网给的整段内容后再粘贴");
                return;
            }

            var text = e.DataObject.GetData(typeof(string)) as string ?? string.Empty;
            if (!CdkCodeExtractor.TryExtract(text, out var code) || !CdkBatchParser.IsValidCode(code))
            {
                // 让 WPF 按默认行为粘贴（玩家能看到自己贴进来的原文），只是明确告诉他没识别到。
                UpdateCodeStatus(string.Empty, "未识别到 20 位兑换码：请整段粘贴发卡网给的内容（含 卡号：/密码： 等标签）");
                return;
            }

            e.CancelCommand(); // 用抠出来的码替换这次粘贴
            var canonical = CdkBatchParser.FormatCode(code);
            var current = code;

            // 场景：码框里已有前 19 位，玩家补粘最后一位 → 拼起来再抠一次，
            // 否则会把已输入的内容整段顶掉。
            if (!string.IsNullOrEmpty(CodeTextBox.Text))
            {
                var old = CodeTextBox.Text;
                var caret = Math.Max(0, Math.Min(CodeTextBox.CaretIndex, old.Length));
                var selection = Math.Max(0, Math.Min(CodeTextBox.SelectionLength, old.Length - caret));
                var merged = old.Remove(caret, selection).Insert(caret, canonical);
                if (CdkCodeExtractor.TryExtract(merged, out var mergedCode) &&
                    CdkBatchParser.IsValidCode(mergedCode))
                {
                    current = mergedCode;
                    canonical = CdkBatchParser.FormatCode(mergedCode);
                }
            }

            _suppressCodeFormat = true;
            CodeTextBox.Text = canonical;
            CodeTextBox.CaretIndex = canonical.Length;
            _suppressCodeFormat = false;

            _currentCode = current;
            UpdateCodeStatus(current);
            UpdateRedeemState();
        }

        /// <summary>码框下方的实时状态：识别到就回显标准形状，没识别到就说清怎么改。</summary>
        private void UpdateCodeStatus(string code, string? overrideText = null)
        {
            if (CodeStatusText is null)
            {
                return;
            }

            if (!string.IsNullOrEmpty(overrideText))
            {
                CodeStatusText.Text = overrideText;
                CodeStatusText.Foreground = ErrorBrush;
                return;
            }

            var raw = (CodeTextBox?.Text ?? string.Empty).Trim();
            if (CdkBatchParser.IsValidCode(code))
            {
                CodeStatusText.Text = "已识别：" + CdkBatchParser.FormatCode(code);
                CodeStatusText.Foreground = SuccessBrush;
                return;
            }

            if (raw.Length == 0)
            {
                CodeStatusText.Text = "支持整段粘贴，粘贴后自动提取 20 位兑换码";
                CodeStatusText.Foreground = DefaultBrush;
                return;
            }

            // 只差几位（正在手输）时别喊"未识别"，免得每敲一个键都报一次错。
            var typed = 0;
            foreach (var ch in raw)
            {
                if (CodeCharset.IndexOf(char.ToUpperInvariant(CdkCodeExtractor.ToHalfWidth(ch))) >= 0)
                {
                    typed++;
                }
            }

            if (typed > 0 && typed < CodeUnitLength)
            {
                CodeStatusText.Text = $"已输入 {Math.Min(typed, CodeUnitLength)}/{CodeUnitLength} 位";
                CodeStatusText.Foreground = DefaultBrush;
                return;
            }

            CodeStatusText.Text = "未识别到 20 位兑换码（可整段粘贴发卡网内容）";
            CodeStatusText.Foreground = ErrorBrush;
        }

        private void UpdateRedeemState()
        {
            if (RedeemButton is null)
            {
                return;
            }

            var code = _currentCode;
            var ready = SelectedSession is not null &&
                        SelectedCharacter is not null &&
                        code.Length == CodeUnitLength &&
                        !_loadingRoles &&
                        !_redeeming &&
                        // 批量进行中也禁掉单码按钮：否则码框里留着码时能并发提交，
                        // 两条链路互相覆盖输出区的进度/结果文案。
                        !_batchRunning;
            RedeemButton.IsEnabled = ready;

            // 批量兑换：只要求"账号 + 角色"，不要求填兑换码（码在 txt 里）。
            if (BatchRedeemButton is not null)
            {
                BatchRedeemButton.IsEnabled = SelectedSession is not null &&
                                              SelectedCharacter is not null &&
                                              !_loadingRoles &&
                                              !_redeeming &&
                                              !_batchRunning;
            }
            if (BatchCancelButton is not null)
            {
                BatchCancelButton.Visibility = _batchRunning ? Visibility.Visible : Visibility.Collapsed;
            }
        }

        // ── 批量兑换（导入 txt：一行一个 20 位码） ──────────────────────────

        /// <summary>单次批量上限（与服务端限频一起决定：500 个串行大约几分钟）。</summary>
        private const int BatchMaxCodes = 500;

        /// <summary>每个码之间的间隔：给服务端留出兑换限频的余量，也免得把日志刷爆。</summary>
        private const int BatchDelayMs = 150;

        private bool _batchRunning;
        private System.Threading.CancellationTokenSource? _batchCts;

        private void BatchCancelButton_Click(object sender, RoutedEventArgs e)
        {
            _batchCts?.Cancel();
            if (BatchCancelButton is not null)
            {
                BatchCancelButton.IsEnabled = false;
            }
            SetOutput("已请求取消，正在收尾（当前这一个码兑换完就停）…", false);
        }

        private async void BatchRedeemButton_Click(object sender, RoutedEventArgs e)
        {
            if (_batchRunning || _redeeming)
            {
                return;
            }

            var session = SelectedSession;
            var character = SelectedCharacter;
            if (session is null)
            {
                SetOutput("请先选择账号：已登录会话，或用下方「账号 + 密码」手动登录", false);
                return;
            }
            if (character is null)
            {
                SetOutput("请先加载并选择要投放奖励的角色（整批都用这个角色）", false);
                return;
            }

            var dlg = new Microsoft.Win32.OpenFileDialog
            {
                Title = "选择兑换码 txt（一行一个 20 位码）",
                Filter = "文本文件 (*.txt)|*.txt|所有文件 (*.*)|*.*",
                CheckFileExists = true,
            };
            if (dlg.ShowDialog() != true)
            {
                return;
            }

            List<string> lines;
            try
            {
                lines = new List<string>(System.IO.File.ReadAllLines(dlg.FileName, Encoding.UTF8));
            }
            catch (Exception ex)
            {
                SetOutput("读取文件失败：" + ex.Message, false);
                return;
            }

            var parsed = CdkBatchParser.Parse(lines, BatchMaxCodes);
            if (parsed.Codes.Count == 0)
            {
                SetOutput($"文件里没有可用的兑换码（无效行 {parsed.InvalidLines}，重复 {parsed.DuplicateCodes}）。\n" +
                          "要求：每行一个 20 位兑换码，只含大写字母与数字 2-9（不含 0/O/1/I），可带横线；\n" +
                          "也支持发卡网那种「卡号：XXXX-XXXX-XXXX-XXXX-XXXX 密码：1234」的整段内容（自动抠码）。", false);
                return;
            }

            var confirmText = $"将从文件读到的兑换码逐个兑换：\n\n" +
                              $"· 待兑换：{parsed.Codes.Count} 个\n" +
                              $"· 账号：{session.DisplayName}\n" +
                              $"· 角色：{character.Name}（{character.CharacterId}）\n" +
                              (parsed.InvalidLines > 0 ? $"· 无效行（已跳过）：{parsed.InvalidLines}\n" : "") +
                              (parsed.DuplicateCodes > 0 ? $"· 重复码（只兑换一次）：{parsed.DuplicateCodes}\n" : "") +
                              (parsed.OverLimitCodes > 0 ? $"· 超出上限未处理：{parsed.OverLimitCodes}\n" : "") +
                              "\n是否开始？";
            if (MessageBox.Show(confirmText, "批量兑换", MessageBoxButton.OKCancel, MessageBoxImage.Question) != MessageBoxResult.OK)
            {
                return;
            }

            _batchRunning = true;
            _batchCts = new System.Threading.CancellationTokenSource();
            var token = _batchCts.Token;
            var (host, port) = ResolveEndpoint();

            var succeeded = new List<string>();
            var failures = new List<string>();
            var canceled = false;

            if (BatchProgressBar is not null)
            {
                BatchProgressBar.Visibility = Visibility.Visible;
                BatchProgressBar.Value = 0;
                BatchProgressBar.Maximum = parsed.Codes.Count;
            }
            if (BatchCancelButton is not null)
            {
                BatchCancelButton.IsEnabled = true;
            }
            UpdateRedeemState();

            try
            {
                for (var i = 0; i < parsed.Codes.Count; i++)
                {
                    if (token.IsCancellationRequested)
                    {
                        canceled = true;
                        break;
                    }

                    var code = parsed.Codes[i];
                    var display = CdkBatchParser.FormatCode(code);
                    SetOutput($"兑换中… {i + 1}/{parsed.Codes.Count}：{display}", false);

                    try
                    {
                        var (ok, err, name, _) = await RemoteAuthService.RedeemCdkAsync(
                            AppConfig.Settings, host, port, session.Ticket, code, character.CharacterId).ConfigureAwait(true);
                        if (ok)
                        {
                            succeeded.Add(string.IsNullOrWhiteSpace(name) ? display : $"{display}（{name}）");
                        }
                        else
                        {
                            failures.Add($"{display}：{err}");
                        }
                    }
                    catch (Exception ex)
                    {
                        failures.Add($"{display}：{ex.Message}");
                    }

                    if (BatchProgressBar is not null)
                    {
                        BatchProgressBar.Value = i + 1;
                    }

                    if (i + 1 < parsed.Codes.Count)
                    {
                        try
                        {
                            await Task.Delay(BatchDelayMs, token).ConfigureAwait(true);
                        }
                        catch (TaskCanceledException)
                        {
                            canceled = true;
                            break;
                        }
                    }
                }
            }
            finally
            {
                _batchRunning = false;
                _batchCts?.Dispose();
                _batchCts = null;
                if (BatchProgressBar is not null)
                {
                    BatchProgressBar.Visibility = Visibility.Collapsed;
                }
                UpdateRedeemState();
            }

            // 汇总：成功 N / 失败 M（逐个列出原因）+ 可选写出结果文件。
            var summary = new StringBuilder();
            summary.AppendLine($"批量兑换完成{(canceled ? "（已取消，剩余未处理）" : "")}");
            summary.AppendLine($"成功 {succeeded.Count} / 失败 {failures.Count} / 共 {parsed.Codes.Count}");
            if (parsed.InvalidLines > 0 || parsed.DuplicateCodes > 0 || parsed.OverLimitCodes > 0)
            {
                summary.AppendLine($"跳过：无效行 {parsed.InvalidLines}、重复码 {parsed.DuplicateCodes}、超上限 {parsed.OverLimitCodes}");
            }
            if (failures.Count > 0)
            {
                summary.AppendLine();
                summary.AppendLine("失败明细：");
                foreach (var failure in failures)
                {
                    summary.AppendLine("· " + failure);
                }
            }

            var resultPath = TryWriteBatchResult(dlg.FileName, summary.ToString(), succeeded, failures);
            if (!string.IsNullOrEmpty(resultPath))
            {
                summary.AppendLine();
                summary.AppendLine("结果已保存到：" + resultPath);
            }

            SetOutput(summary.ToString().TrimEnd(), failures.Count == 0);
        }

        /// <summary>把批量结果写到 txt（放在源文件旁边）；失败不影响主流程，只返回空串。</summary>
        private static string TryWriteBatchResult(string sourcePath, string summary, List<string> succeeded, List<string> failures)
        {
            try
            {
                var directory = System.IO.Path.GetDirectoryName(sourcePath);
                if (string.IsNullOrWhiteSpace(directory))
                {
                    return "";
                }
                var name = $"cdk_result_{DateTime.Now:yyyyMMdd_HHmmss}.txt";
                var path = System.IO.Path.Combine(directory, name);
                var builder = new StringBuilder();
                builder.AppendLine(summary);
                builder.AppendLine();
                builder.AppendLine("[成功]");
                foreach (var item in succeeded)
                {
                    builder.AppendLine(item);
                }
                builder.AppendLine();
                builder.AppendLine("[失败/跳过]");
                foreach (var item in failures)
                {
                    builder.AppendLine(item);
                }
                System.IO.File.WriteAllText(path, builder.ToString(), new UTF8Encoding(true));
                return path;
            }
            catch
            {
                return "";
            }
        }

        private static string FormatCode(string code) => CdkBatchParser.FormatCode(code);

        private static int MapUnitToIndex(string formatted, int units)
        {
            var idx = 0;
            var u = 0;
            while (idx < formatted.Length && u < units)
            {
                if (formatted[idx] != '-')
                {
                    u++;
                }

                idx++;
            }

            return idx;
        }

        /// <summary>解析 90 外网 API 地址：优先 RemoteIp，回退 Host；端口默认 18112（对齐 LoginForTicketAsync）</summary>
        private static (string Host, int Port) ResolveEndpoint()
        {
            var settings = AppConfig.Settings;
            var host = !string.IsNullOrWhiteSpace(settings.RemoteIp)
                ? settings.RemoteIp.Trim()
                : settings.Host.Trim();
            var port = int.TryParse(settings.RemotePort, out var p) && p > 0 ? p : 18112;
            return (host, port);
        }

        private void SetOutput(string text, bool success)
        {
            if (OutputTextBox is null)
            {
                return;
            }

            OutputTextBox.Text = text ?? string.Empty;
            OutputTextBox.Foreground = success ? SuccessBrush : (text.Length == 0 ? DefaultBrush : ErrorBrush);
        }
    }
}
