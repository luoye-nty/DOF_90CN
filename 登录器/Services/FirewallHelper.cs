using System;
using System.Diagnostics;
using Microsoft.Win32;

namespace DNFLogin.Services
{
    /// <summary>
    /// 系统防火墙自检辅助：读取 HKLM\...\FirewallPolicy 下 DomainProfile / StandardProfile /
    /// PublicProfile 三个子键的 EnableFirewall (DWORD) 判断防火墙是否开启或未完全关闭；
    /// TurnOff / TurnOn 通过 netsh advfirewall 切换（登录器 app.manifest 为 requireAdministrator，
    /// 进程内可直接执行，无需提权弹窗）。
    /// </summary>
    public static class FirewallHelper
    {
        private const string FirewallPolicyKey =
            @"SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy";

        /// <summary>
        /// 防火墙开启或未完全关闭（域/专用/公用任一启用）返回 true；
        /// 注册表读取异常时容错返回 false，不阻塞启动自检。
        /// </summary>
        public static bool IsFirewallOn()
        {
            try
            {
                using var policy = Registry.LocalMachine.OpenSubKey(FirewallPolicyKey);
                if (policy is null)
                {
                    return false;
                }

                return IsProfileEnabled(policy, "DomainProfile") ||
                       IsProfileEnabled(policy, "StandardProfile") ||
                       IsProfileEnabled(policy, "PublicProfile");
            }
            catch
            {
                return false;
            }
        }

        private static bool IsProfileEnabled(RegistryKey policy, string profile)
        {
            try
            {
                using var key = policy.OpenSubKey(profile);
                if (key is null)
                {
                    return false;
                }

                var value = key.GetValue("EnableFirewall");
                return value is int raw ? raw == 1 : false;
            }
            catch
            {
                return false;
            }
        }

        /// <summary>关闭系统防火墙（netsh advfirewall set allprofiles state off），返回是否成功。</summary>
        public static bool TurnOff()
        {
            return RunNetsh("off");
        }

        /// <summary>开启系统防火墙（netsh advfirewall set allprofiles state on），备用恢复入口，返回是否成功。</summary>
        public static bool TurnOn()
        {
            return RunNetsh("on");
        }

        private static bool RunNetsh(string state)
        {
            try
            {
                var psi = new ProcessStartInfo("netsh", "advfirewall set allprofiles state " + state)
                {
                    UseShellExecute = false,
                    CreateNoWindow = true,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true
                };

                using var proc = Process.Start(psi);
                if (proc is null)
                {
                    return false;
                }

                proc.WaitForExit();
                return proc.ExitCode == 0;
            }
            catch
            {
                return false;
            }
        }
    }
}
