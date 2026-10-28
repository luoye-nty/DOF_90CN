using System;
using System.IO;
using System.Text.Json;

namespace DNFLogin.Services.ClientYh
{
    /// <summary>
    /// 【已去授权】原"外网版 client.yh 启动自检编排"（M4，仅 EXTERNAL 发行版经
    /// App.OnStartup 调用）。
    ///
    /// 原逻辑（已删除）：同目录/隐蔽目录找 client.yh → 上报验证服务器换取票据与
    /// 明文载荷 → 校验失败即弹窗拒绝启动；再缓存短时票据供连接服主时申请 nonce。
    ///
    /// 现在：整个授权链已移除——不读 client.yh、不连验证服务器、不校验签名与绑定 IP，
    /// 恒放行。**连接哪个服由 AppConfig 的配置决定**（设置 → 服务器配置），与授权无关。
    ///
    /// 保留类与方法外壳的原因：EXTERNAL 编译开关与 App.OnStartup 的调用结构都还在，
    /// 保留外壳后"要不要恢复授权"只是本类内部实现的事，不必重新梳理启动流程。
    /// </summary>
    internal static class ClientYhGuard
    {
        /// <summary>启动自检总入口。去授权后恒返回 true（放行），不做任何校验。</summary>
        public static bool RunStartupCheck()
        {
            TryWriteE2eOk();
            App.LogStartup("启动检查已跳过（去授权版本）");
            return true;
        }

        /// <summary>E2E 通过分支落盘：仅当设置了 DNFLOGIN_E2E 时写入 ok=true（作者自动化验收用）</summary>
        private static void TryWriteE2eOk()
        {
            var e2eOut = Environment.GetEnvironmentVariable("DNFLOGIN_E2E");
            if (string.IsNullOrWhiteSpace(e2eOut))
            {
                return;
            }

            try
            {
                File.WriteAllText(e2eOut, JsonSerializer.Serialize(new
                {
                    ok = true,
                    code = "OK",
                    target = AppConfig.EditionProfileId,
                    time = DateTime.UtcNow.ToString("O")
                }, new JsonSerializerOptions { WriteIndented = true }));
            }
            catch
            {
                // E2E 落盘失败不影响正常启动
            }
        }
    }
}
