using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using Microsoft.Win32;

namespace DNFLogin.Services
{
    /// <summary>
    /// 机器码（硬件指纹）——登录器上报给运维工具认证 API 的 machine_id。
    ///
    /// 背景（2026-09-24）：此前两处上报的都是 <c>Environment.MachineName</c>（计算机名），
    /// 玩家把电脑改个名就能绕过"机器码封禁"；而且计算机名可能含中文，
    /// 拼 LANA 认证帧时用 ASCII 编码会变成 '?'（同机不同值）。
    ///
    /// 现在改成真正的硬件指纹。组成（读 SMBIOS 原始表，纯 P/Invoke，不依赖 WMI、不加 NuGet 包）：
    ///   ① Type 1 System UUID
    ///   ② Type 2 主板序列号
    ///   ③ Type 1 系统序列号
    /// 硬件信息取不到时逐级降级：② 注册表 MachineGuid + 系统卷序列号 → ③ 计算机名（即改动前的行为）。
    /// 最终输出 <c>hw:&lt;32位hex&gt;</c>：定长 35 字符、纯 ASCII、不泄露原始序列号。
    ///
    /// 硬约束：游戏服 LANA 认证帧的 machine_id 上限 128 字节 ASCII
    /// （go-server internal/services/dnfbridge/launcher_auth.go：launcherAuthMaxMachineID = 128），
    /// 所以这里绝不能返回超长或非 ASCII 内容。降级到计算机名时也按 128 截断。
    ///
    /// 兼容：<see cref="ComputerName"/>（即旧的机器码）由调用方一并上报给运维工具，
    /// 服务端按"新硬件码优先、旧计算机名兼容"双匹配，历史机器封禁不会失效。
    /// </summary>
    public static class HardwareId
    {
        /// <summary>前缀，用来一眼区分"硬件码"与旧的计算机名；封禁值按字符串精确比对，前缀不影响匹配。</summary>
        private const string Prefix = "hw:";

        /// <summary>SHA256 取前 16 字节 → 32 hex：碰撞余量足够，且远低于 128 字节上限。</summary>
        private const int HashHexLength = 32;

        /// <summary>'RSMB' = 原始 SMBIOS 固件表（GetSystemFirmwareTable 的 Provider Signature）。</summary>
        private const uint RawSmbiosSignature = 0x52534D42;

        /// <summary>机器码：hw:&lt;32hex&gt;；硬件信息完全取不到时退回计算机名。</summary>
        public static string MachineCode { get; }

        /// <summary>计算机名（旧的机器码取值）。始终上报给服务端，用于兼容历史"按计算机名"的机器封禁。</summary>
        public static string ComputerName { get; }

        /// <summary>本次机器码的来源，便于排查："smbios" / "os-ids" / "computer-name"。</summary>
        public static string Source { get; }

        static HardwareId()
        {
            string computerName = SafeComputerName();
            ComputerName = computerName;

            string source = "computer-name";
            string code = computerName;
            try
            {
                string? hardware = FromSmbios();
                if (!string.IsNullOrEmpty(hardware))
                {
                    code = Hash(hardware);
                    source = "smbios";
                }
                else
                {
                    string? osIds = FromOsIdentifiers();
                    if (!string.IsNullOrEmpty(osIds))
                    {
                        code = Hash(osIds);
                        source = "os-ids";
                    }
                }
            }
            catch
            {
                // 任何异常都不能阻断登录：继续用下面的兜底值（与改动前行为一致）。
                code = computerName;
                source = "computer-name";
            }

            MachineCode = code ?? "";
            Source = source;
        }

        // ---------------- SMBIOS ----------------

        /// <summary>从 SMBIOS 原始表取 [System UUID, 主板序列号, 系统序列号]，全取不到返回 null。</summary>
        private static string? FromSmbios()
        {
            byte[] table = ReadRawSmbios();
            if (table.Length < 8) return null;

            string? systemUuid = null;
            string? boardSerial = null;
            string? systemSerial = null;

            // 前 8 字节是固件表头（'RSMB' + 长度），SMBIOS 结构体从 offset 8 开始。
            int offset = 8;
            while (offset + 4 <= table.Length)
            {
                byte type = table[offset];
                byte length = table[offset + 1];
                if (length < 4) break;
                if (offset + length > table.Length) break;
                if (type == 0x7F) break; // End-of-table

                if (type == 1 && length >= 0x08)
                {
                    // Type 1：0x07 = 序列号（字符串索引）；UUID 在 0x08，需要结构体长度 ≥ 0x18。
                    systemSerial = ReadString(table, offset, length, table[offset + 0x07]);
                    if (length >= 0x18) systemUuid = FormatUuid(table, offset + 0x08);
                }
                else if (type == 2 && length >= 0x08)
                {
                    // Type 2（主板）：0x07 = 序列号（字符串索引）。
                    boardSerial = ReadString(table, offset, length, table[offset + 0x07]);
                }

                int next = NextStructure(table, offset, length);
                if (next <= offset) break; // 防御：不前进就停，避免死循环
                offset = next;
            }

            var parts = new List<string>(3);
            AddPart(parts, systemUuid);
            AddPart(parts, boardSerial);
            AddPart(parts, systemSerial);
            return parts.Count == 0 ? null : string.Join("|", parts);
        }

        /// <summary>读原始 SMBIOS 固件表（GetSystemFirmwareTable 两次调用：先量长度再取内容）。</summary>
        private static byte[] ReadRawSmbios()
        {
            uint size = GetSystemFirmwareTable(RawSmbiosSignature, 0, IntPtr.Zero, 0);
            if (size == 0 || size > 1 << 20) return Array.Empty<byte>();

            IntPtr buffer = Marshal.AllocHGlobal((int)size);
            try
            {
                uint written = GetSystemFirmwareTable(RawSmbiosSignature, 0, buffer, size);
                if (written == 0) return Array.Empty<byte>();
                int take = (int)Math.Min(written, size);
                var raw = new byte[take];
                Marshal.Copy(buffer, raw, 0, take);
                return raw;
            }
            finally
            {
                Marshal.FreeHGlobal(buffer);
            }
        }

        /// <summary>按 SMBIOS 规则跳过字符串区，返回下一个结构体的起始偏移。</summary>
        private static int NextStructure(byte[] table, int offset, int length)
        {
            int pos = offset + length;
            // 字符串区：若干 NUL 结尾的字符串，遇到空字符串（连续两个 NUL）结束。
            while (pos + 1 < table.Length)
            {
                if (table[pos] == 0)
                {
                    if (table[pos + 1] == 0) return pos + 2;
                    pos++;
                    continue;
                }
                pos++;
            }
            return table.Length; // 尾部不完整：当作结束
        }

        /// <summary>按 1 起始的字符串索引取 SMBIOS 字符串；索引 0 或越界返回 null。</summary>
        private static string? ReadString(byte[] table, int offset, int length, byte index)
        {
            if (index == 0) return null;
            int pos = offset + length;
            int current = 1;
            while (pos < table.Length)
            {
                int end = pos;
                while (end < table.Length && table[end] != 0) end++;
                if (end >= table.Length) return null;
                if (current == index)
                {
                    string text = Encoding.ASCII.GetString(table, pos, end - pos).Trim();
                    return text.Length == 0 ? null : text;
                }
                if (end + 1 < table.Length && table[end + 1] == 0) return null; // 字符串区结束
                current++;
                pos = end + 1;
            }
            return null;
        }

        /// <summary>UUID 原始 16 字节转 hex（保留厂商字节序，不重排）；全 0 / 全 FF 视为未写入。</summary>
        private static string? FormatUuid(byte[] table, int offset)
        {
            if (offset + 16 > table.Length) return null;
            bool allZero = true, allOnes = true;
            for (int i = 0; i < 16; i++)
            {
                byte b = table[offset + i];
                if (b != 0x00) allZero = false;
                if (b != 0xFF) allOnes = false;
            }
            if (allZero || allOnes) return null;
            var sb = new StringBuilder(32);
            for (int i = 0; i < 16; i++) sb.Append(table[offset + i].ToString("x2"));
            return sb.ToString();
        }

        // ---------------- 降级：OS 侧标识 ----------------

        /// <summary>MachineGuid + 系统卷序列号（硬件表读不到时用；重装系统/格式化会变）。</summary>
        private static string? FromOsIdentifiers()
        {
            var parts = new List<string>(2);
            try
            {
                using RegistryKey? key = Registry.LocalMachine.OpenSubKey(@"SOFTWARE\Microsoft\Cryptography");
                if (key?.GetValue("MachineGuid") is string guid) AddPart(parts, guid);
            }
            catch
            {
                // 读不到就算了，继续下一项
            }
            try
            {
                string root = Path.GetPathRoot(Environment.SystemDirectory) ?? "C:\\";
                if (GetVolumeInformationW(root, null, 0, out uint serial, out _, out _, null, 0))
                {
                    AddPart(parts, serial.ToString("x8"));
                }
            }
            catch
            {
                // 同上
            }
            return parts.Count == 0 ? null : string.Join("|", parts);
        }

        // ---------------- 通用 ----------------

        /// <summary>过滤厂商占位值（"To be filled by O.E.M." 之类），避免所有机器算出同一个码。</summary>
        private static readonly HashSet<string> Placeholders = new(StringComparer.OrdinalIgnoreCase)
        {
            "", "0", "none", "n/a", "na", "null", "unknown", "not specified", "invalid",
            "to be filled by o.e.m.", "to be filled by oem", "filled by oem", "o.e.m.",
            "default string", "system serial number", "system product name", "system version",
            "base board serial number", "chassis serial number", "not applicable",
            "oem", "oem_serial", "x.x", "xxx", "00000000", "0000000000000000", "+1",
        };

        private static void AddPart(List<string> parts, string? value)
        {
            if (string.IsNullOrWhiteSpace(value)) return;
            string trimmed = value.Trim();
            if (Placeholders.Contains(trimmed)) return;
            parts.Add(trimmed.ToLowerInvariant());
        }

        /// <summary>归一化哈希：固定盐 + 固定顺序 → sha256 前 16 字节 → hw:&lt;32hex&gt;。</summary>
        private static string Hash(string parts)
        {
            byte[] digest = SHA256.HashData(Encoding.UTF8.GetBytes("dnf90-hwid-v1|" + parts));
            var sb = new StringBuilder(Prefix.Length + HashHexLength);
            sb.Append(Prefix);
            for (int i = 0; i < HashHexLength / 2; i++) sb.Append(digest[i].ToString("x2"));
            return sb.ToString();
        }

        private static string SafeComputerName()
        {
            try
            {
                string name = (Environment.MachineName ?? string.Empty).Trim();
                return name.Length > 128 ? name.Substring(0, 128) : name;
            }
            catch
            {
                return string.Empty;
            }
        }

        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern uint GetSystemFirmwareTable(uint firmwareTableProviderSignature, uint firmwareTableId, IntPtr firmwareTableBuffer, uint bufferSize);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool GetVolumeInformationW(
            string rootPathName,
            StringBuilder? volumeNameBuffer,
            uint volumeNameSize,
            out uint volumeSerialNumber,
            out uint maximumComponentLength,
            out uint fileSystemFlags,
            StringBuilder? fileSystemNameBuffer,
            uint fileSystemNameSize);
    }
}
