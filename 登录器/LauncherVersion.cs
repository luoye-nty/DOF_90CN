namespace DNFLogin
{
    /// <summary>
    /// 登录器版本号 —— 三端联动功能「登录器版本校验」的登录器侧唯一事实源。
    ///
    /// ★★★ 每次发布新登录器，必须同步改这里的 <see cref="Current"/> ★★★
    ///
    /// 这个值会：
    ///   1. 在登录器「设置 → 关于」页显示成「YH登录器V{Current}」
    ///      （SettingsControl 已改成绑定本常量，改这一处即可，不要再去 XAML 里手改文案）；
    ///   2. 在**启动自检阶段**与**玩家点「登录」时**各上报一次给运维工具
    ///      （GET /launcher/v1/version-check，见 Services.LauncherVersionGuard）；
    ///   3. 由运维工具拿去与它内置的 builtinLauncherVersion 做**完全一致**的比对。
    ///
    /// ── 版本号一共存在于这几处，发布时请一起改（顺序无所谓，但必须全改到）──────────
    ///   1) 登录器（DNFLogin.build.json） launcherVersion
    ///   2) 运维工具              launcher_version.go → builtinLauncherVersion
    ///      → 生效方式：重新编译运维工具并**重启认证服务**（常量是编译期绑定的）。
    ///   3) 登录器「设置 → 关于」页 —— **不用单独改**，已绑定本常量。
    ///   4) 管理工具              —— **不用改**，它从运维工具读 required_version 展示。
    ///
    /// ── 比对规则（两端必须同源，否则全服都进不去）────────────────────────────
    /// 字符串**完全一致**才算通过：
    ///   · 大小写敏感；
    ///   · **不带 "v" 前缀**（写 "1.5.5"，不要写 "v1.5.5"）；
    ///   · 不带首尾空白。
    /// 比对失败 → 登录器停在更新窗口强制更新，**更新完成前禁止进入登录界面**。
    /// 之所以不用"版本高低"比较：需求明确要求完全一致，防止有人私自换包。
    ///
    /// ── 改版本号时容易踩的坑 ────────────────────────────────────────────────
    ///   · 只改了登录器没改运维工具 → 新登录器一上线就被判定"版本不符"，
    ///     于是它又会去下载自己，形成"更新完还是提示更新"的死循环。
    ///   · 只改了运维工具没改登录器 → 所有老玩家全部卡在更新界面（直到新包发出去）。
    ///   所以：**先把新登录器包传上直链，再改运维工具并重启认证服务**，最后才发登录器。
    /// </summary>
    public static class LauncherVersion
    {
        /// <summary>
        /// 当前登录器版本号，来自构建时嵌入的 DNFLogin.build.json。
        /// 与运维工具 launcher_version.go 的 builtinLauncherVersion 逐字符一致。
        /// 例："1.5.5"（不带 v 前缀）。
        /// </summary>
        public static string Current => BuildParameters.LauncherVersion;

        /// <summary>
        /// 「设置 → 关于」页显示的完整版本文案。
        /// 所有需要显示版本号的地方都用它，避免文案漂移。
        /// </summary>
        public static string DisplayText => "落叶DNF登录器V" + Current;
    }
}
