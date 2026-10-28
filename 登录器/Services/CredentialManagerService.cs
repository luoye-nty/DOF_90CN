using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

namespace DNFLogin
{
    /// <summary>
    /// Windows 凭据管理器封装（P/Invoke CredWriteW / CredReadW / CredDeleteW / CredEnumerateW）。
    /// 条目名格式：DNFLogin_{账号}，对齐旧版 Go 登录器的 CredWriteW 用法。
    /// </summary>
    public static class CredentialManagerService
    {
        private const int CRED_TYPE_GENERIC = 1;
        private const uint CRED_PERSIST_LOCAL_MACHINE = 2;

        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
        private struct CREDENTIAL
        {
            public uint Flags;
            public uint Type;
            public IntPtr TargetName;
            public IntPtr Comment;
            public long LastWritten; // FILETIME（8 字节）
            public uint CredentialBlobSize;
            public IntPtr CredentialBlob;
            public uint Persist;
            public uint AttributeCount;
            public IntPtr Attributes;
            public IntPtr TargetAlias;
            public IntPtr UserName;
        }

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern bool CredWriteW(ref CREDENTIAL credential, uint flags);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern bool CredReadW(string target, uint type, uint reservedFlag, out IntPtr credentialPtr);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern bool CredDeleteW(string target, uint type, uint flags);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern bool CredEnumerateW(string? filter, uint flags, out uint count, out IntPtr credentialsPtr);

        [DllImport("advapi32.dll")]
        private static extern void CredFree(IntPtr buffer);

        /// <summary>条目名：DNFLogin_{账号}（90 版本沿用旧格式以兼容历史条目）</summary>
        public static string TargetNameFor(string account) => "DNFLogin_" + account;

        /// <summary>
        /// 版本化条目名：90 沿用 DNFLogin_{账号}（兼容旧凭据），
        /// S4 系列为 DNFLogin_{版本}_{账号}，实现记住密码按版本隔离。
        /// </summary>
        public static string TargetNameFor(string profileId, string account)
        {
            if (string.IsNullOrWhiteSpace(profileId) ||
                string.Equals(profileId, "90", StringComparison.OrdinalIgnoreCase))
            {
                return TargetNameFor(account);
            }

            return "DNFLogin_" + profileId + "_" + account;
        }

        /// <summary>保存账号密码到凭据管理器（按版本隔离）</summary>
        public static void Save(string profileId, string account, string password)
        {
            SaveCore(TargetNameFor(profileId, account), account, password);
        }

        /// <summary>保存账号密码到凭据管理器（旧格式，默认 90 兼容）</summary>
        public static void Save(string account, string password)
        {
            SaveCore(TargetNameFor(account), account, password);
        }

        private static void SaveCore(string target, string account, string password)
        {
            var passwordBytes = Encoding.Unicode.GetBytes(password);
            var targetPtr = Marshal.StringToCoTaskMemUni(target);
            var blobPtr = Marshal.AllocCoTaskMem(Math.Max(passwordBytes.Length, 1));
            if (passwordBytes.Length > 0)
            {
                Marshal.Copy(passwordBytes, 0, blobPtr, passwordBytes.Length);
            }

            var credential = new CREDENTIAL
            {
                Type = CRED_TYPE_GENERIC,
                TargetName = targetPtr,
                CredentialBlobSize = (uint)passwordBytes.Length,
                CredentialBlob = blobPtr,
                Persist = CRED_PERSIST_LOCAL_MACHINE,
                UserName = Marshal.StringToCoTaskMemUni(account)
            };

            try
            {
                CredWriteW(ref credential, 0);
            }
            finally
            {
                Marshal.FreeCoTaskMem(targetPtr);
                Marshal.FreeCoTaskMem(blobPtr);
                if (credential.UserName != IntPtr.Zero)
                {
                    Marshal.FreeCoTaskMem(credential.UserName);
                }
            }
        }

        /// <summary>按账号读取密码（按版本隔离）；不存在返回 false</summary>
        public static bool TryRead(string profileId, string account, out string password)
        {
            return TryReadCore(TargetNameFor(profileId, account), out password);
        }

        /// <summary>按账号读取密码（旧格式，默认 90 兼容）；不存在返回 false</summary>
        public static bool TryRead(string account, out string password)
        {
            return TryReadCore(TargetNameFor(account), out password);
        }

        private static bool TryReadCore(string target, out string password)
        {
            password = string.Empty;
            if (!CredReadW(target, CRED_TYPE_GENERIC, 0, out var ptr))
            {
                return false;
            }

            try
            {
                var cred = Marshal.PtrToStructure<CREDENTIAL>(ptr);
                if (cred.CredentialBlob == IntPtr.Zero || cred.CredentialBlobSize == 0)
                {
                    return false;
                }

                var bytes = new byte[cred.CredentialBlobSize];
                Marshal.Copy(cred.CredentialBlob, bytes, 0, bytes.Length);
                password = Encoding.Unicode.GetString(bytes);
                return true;
            }
            finally
            {
                CredFree(ptr);
            }
        }

        /// <summary>删除指定账号的凭据（按版本隔离）</summary>
        public static bool Delete(string profileId, string account)
        {
            return CredDeleteW(TargetNameFor(profileId, account), CRED_TYPE_GENERIC, 0);
        }

        /// <summary>删除指定账号的凭据（旧格式，默认 90 兼容）</summary>
        public static bool Delete(string account)
        {
            return CredDeleteW(TargetNameFor(account), CRED_TYPE_GENERIC, 0);
        }

        /// <summary>删除当前版本所有 DNFLogin_* 凭据</summary>
        public static void DeleteAll(string profileId)
        {
            foreach (var entry in EnumerateAll(profileId))
            {
                CredDeleteW(TargetNameFor(profileId, entry.Account), CRED_TYPE_GENERIC, 0);
            }
        }

        /// <summary>删除所有 DNFLogin_* 凭据（旧格式，默认 90 兼容）</summary>
        public static void DeleteAll()
        {
            foreach (var entry in EnumerateAll())
            {
                CredDeleteW(TargetNameFor(entry.Account), CRED_TYPE_GENERIC, 0);
            }
        }

        /// <summary>枚举到的凭据条目</summary>
        public sealed class CredentialEntry
        {
            public string Account { get; set; } = "";
            public string Password { get; set; } = "";
            public DateTime LastWritten { get; set; }
        }

        /// <summary>枚举当前版本所有凭据（按版本隔离；90 不含 S4 版本条目）</summary>
        public static List<CredentialEntry> EnumerateAll(string profileId)
        {
            var is90 = string.IsNullOrWhiteSpace(profileId) ||
                       string.Equals(profileId, "90", StringComparison.OrdinalIgnoreCase);
            var filter = is90 ? "DNFLogin_*" : "DNFLogin_" + profileId + "_*";
            return EnumerateCore(filter, is90 ? null : profileId);
        }

        /// <summary>枚举所有 DNFLogin_* 凭据（旧格式，默认 90 兼容）</summary>
        public static List<CredentialEntry> EnumerateAll()
        {
            return EnumerateCore("DNFLogin_*", null);
        }

        /// <summary>
        /// 枚举凭据。profileId 非空时用于从目标名中剥离 {profileId}_ 前缀还原账号；
        /// 90 版本兼容旧格式 DNFLogin_{账号}，且过滤掉 S4 版本的条目。
        /// </summary>
        private static List<CredentialEntry> EnumerateCore(string filter, string? profileId)
        {
            var result = new List<CredentialEntry>();
            if (!CredEnumerateW(filter, 0, out var count, out var ptr))
            {
                return result;
            }

            try
            {
                var items = new IntPtr[count];
                Marshal.Copy(ptr, items, 0, (int)count);
                foreach (var item in items)
                {
                    var cred = Marshal.PtrToStructure<CREDENTIAL>(item);
                    if (cred.Type != CRED_TYPE_GENERIC)
                    {
                        continue;
                    }

                    var target = Marshal.PtrToStringUni(cred.TargetName) ?? string.Empty;
                    const string prefix = "DNFLogin_";
                    if (!target.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
                    {
                        continue;
                    }

                    // 90 版本枚举 DNFLogin_* 时，跳过 S4 版本条目避免串号
                    if (profileId is null &&
                        (target.StartsWith("DNFLogin_S4A12_", StringComparison.OrdinalIgnoreCase) ||
                         target.StartsWith("DNFLogin_S4A21_", StringComparison.OrdinalIgnoreCase)))
                    {
                        continue;
                    }

                    var account = target.Substring(prefix.Length);
                    if (!string.IsNullOrWhiteSpace(profileId))
                    {
                        var verPrefix = profileId + "_";
                        if (!account.StartsWith(verPrefix, StringComparison.OrdinalIgnoreCase))
                        {
                            continue;
                        }

                        account = account.Substring(verPrefix.Length);
                    }

                    var password = string.Empty;
                    if (cred.CredentialBlob != IntPtr.Zero && cred.CredentialBlobSize > 0)
                    {
                        var bytes = new byte[cred.CredentialBlobSize];
                        Marshal.Copy(cred.CredentialBlob, bytes, 0, bytes.Length);
                        password = Encoding.Unicode.GetString(bytes);
                    }

                    result.Add(new CredentialEntry
                    {
                        Account = account,
                        Password = password,
                        LastWritten = DateTime.FromFileTime(cred.LastWritten)
                    });
                }
            }
            finally
            {
                CredFree(ptr);
            }

            return result;
        }
    }
}
