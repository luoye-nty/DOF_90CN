using System;
using System.IO;
using System.Security.Cryptography;
using System.Text;

namespace DNFLogin.Services
{
    /// <summary>
    /// 登录器身份（Launcher Identity，LID）：本登录器持有的 ECDSA P-256 私钥，
    /// 用来给"我确实是这个登录器"签一份短票据。
    ///
    /// 定位要点：
    ///  · 服务端只存公钥（DNF90Control launcher-identity keygen 生成并写进 instance.json）；
    ///    私钥只在登录器这边，所以服务端包发给谁都不能自己造一份证明。
    ///  · 登录器只"出示"证明，从不"要求"证明，所以同一个登录器照样能连没有开
    ///    强绑定的第三方服务端（那种服务端根本不会发版本 2 的 LANA 挑战）。
    ///  · 没有配置私钥时一切照旧：只是无法连接开了 enforce 的服务端。
    ///
    /// 私钥读取顺序（前者优先）：
    ///   1) %APPDATA%\DNFLogin\launcher_identity.key  （换钥不用重编译，推荐分发时用这个）
    ///   2) 登录器 exe 同目录的 launcher_identity.key
    ///   3) 下面编译进程序集的常量（keygen 会打印可直接粘贴的内容）
    /// 文件内容可以是 PKCS#8 PEM，也可以是纯 base64 的 PKCS#8 DER。
    /// </summary>
    public static class LauncherIdentity
    {
        /// <summary>
        /// 编译进程序的私钥（base64 的 PKCS#8 DER）。
        ///
        /// 由 DNF90Control.exe launcher-identity keygen 生成；
        /// 留空表示这个构建没有身份，连接未强绑定的服务端不受影响。
        /// 发布前请不要把带私钥的源码或 PDB 一起发出去；本项目的 build.bat 会走
        /// ConfuserEx 混淆，保持这条流水线即可。
        /// </summary>
        private const string EmbeddedPrivateKeyBase64 = "";

        private const string KeyFileName = "launcher_identity.key";

        /// <summary>租约有效期：一次登录生命周期内足够，过期后必须重新签。</summary>
        private static readonly TimeSpan LeaseLifetime = TimeSpan.FromHours(12);

        private static readonly object Gate = new object();
        private static bool _loaded;
        private static ECDsa? _key;
        private static string _error = "";
        private static string _source = "";

        /// <summary>本构建是否带身份私钥。false 时所有签名单据返回 null，调用方按老流程走。</summary>
        public static bool Available
        {
            get
            {
                EnsureLoaded();
                return _key is not null;
            }
        }

        /// <summary>私钥来源（诊断用：embedded / 具体文件路径）。</summary>
        public static string Source
        {
            get
            {
                EnsureLoaded();
                return _source;
            }
        }

        /// <summary>最近一次加载失败原因（""=无错误）。</summary>
        public static string LastError
        {
            get
            {
                EnsureLoaded();
                return _error;
            }
        }

        /// <summary>
        /// 对规范语句签名，返回 base64(DER)。DER 是 .NET ECDsa.SignData 的默认编码，
        /// 也正是服务端 ecdsa.VerifyASN1 期望的形式，两端不需要额外转换。
        /// </summary>
        public static string? Sign(string statement)
        {
            EnsureLoaded();
            if (_key is null || string.IsNullOrEmpty(statement))
            {
                return null;
            }

            try
            {
                var payload = Encoding.UTF8.GetBytes(statement);
                var signature = _key.SignData(
                    payload,
                    HashAlgorithmName.SHA256,
                    DSASignatureFormat.Rfc3279DerSequence);
                return Convert.ToBase64String(signature);
            }
            catch (Exception ex)
            {
                _error = "签名失败：" + ex.Message;
                return null;
            }
        }

        /// <summary>
        /// 本地模式租约：把"这个 PID 的这个账号是本登录器启动的"绑到某一次服务端进程上。
        /// 语句格式必须与服务端 launcher_identity.go 的 LauncherIdentityLocalStatement 一致。
        /// </summary>
        public static string? BuildLocalLease(string serverId, int pid, string accountId)
        {
            if (string.IsNullOrWhiteSpace(serverId) || pid <= 0 || string.IsNullOrWhiteSpace(accountId))
            {
                return null;
            }

            long expiresAt = DateTimeOffset.UtcNow.Add(LeaseLifetime).ToUnixTimeSeconds();
            var statement = string.Join("|", new[]
            {
                "DNF90-LID-1",
                "local",
                serverId.Trim().ToLowerInvariant(),
                pid.ToString(System.Globalization.CultureInfo.InvariantCulture),
                EncodeField(accountId.Trim()),
                expiresAt.ToString(System.Globalization.CultureInfo.InvariantCulture)
            });

            var signature = Sign(statement);
            return signature is null ? null : statement + "|" + signature;
        }

        /// <summary>
        /// 远程模式租约：把"本次会话的票是本登录器拿到的"绑到票上。
        /// 语句格式必须与服务端 launcher_identity.go 的 LauncherIdentityRemoteStatement 一致。
        /// </summary>
        public static string? BuildRemoteLease(string subject)
        {
            if (string.IsNullOrWhiteSpace(subject))
            {
                return null;
            }

            long expiresAt = DateTimeOffset.UtcNow.Add(LeaseLifetime).ToUnixTimeSeconds();
            var statement = string.Join("|", new[]
            {
                "DNF90-LID-1",
                "remote",
                EncodeField(subject.Trim()),
                expiresAt.ToString(System.Globalization.CultureInfo.InvariantCulture)
            });

            var signature = Sign(statement);
            return signature is null ? null : statement + "|" + signature;
        }

        /// <summary>
        /// 变长字段用 base64url 编码，保证语句里的 "|" 只作分隔符使用——
        /// 账号名里带 "|" 时也能被服务端原样还原。
        /// </summary>
        private static string EncodeField(string value)
        {
            return Convert.ToBase64String(Encoding.UTF8.GetBytes(value))
                .TrimEnd('=')
                .Replace('+', '-')
                .Replace('/', '_');
        }

        private static void EnsureLoaded()
        {
            lock (Gate)
            {
                if (_loaded)
                {
                    return;
                }
                _loaded = true;
                LoadLocked();
                LogState();
            }
        }

        private static void LoadLocked()
        {
            foreach (var candidate in CandidateKeyPaths())
            {
                if (!File.Exists(candidate))
                {
                    continue;
                }
                if (TryImport(File.ReadAllText(candidate).Trim(), out var fromFile, out var fileError))
                {
                    _key = fromFile;
                    _source = candidate;
                    return;
                }
                // A file that exists but does not parse is a real
                // misconfiguration. Keep looking, but remember why, so the UI can
                // say something better than "no identity".
                _error = candidate + "：" + fileError;
            }

            if (string.IsNullOrWhiteSpace(EmbeddedPrivateKeyBase64))
            {
                return;
            }
            if (TryImport(EmbeddedPrivateKeyBase64, out var embedded, out var embeddedError))
            {
                _key = embedded;
                _source = "embedded";
                _error = "";
                return;
            }
            _error = "内置私钥无法解析：" + embeddedError;
        }

        /// <summary>
        /// 把身份加载结果写进启动日志。开了强绑定的服务端只会表现成"绑定失败"，
        /// 没有这一行的话，排查时会分不清是"没配密钥"还是"密钥不对"。
        /// </summary>
        private static void LogState()
        {
            try
            {
                App.LogStartup("launcher identity: " +
                    (_key is null ? "unavailable" : "ready") +
                    " source=" + (_source.Length > 0 ? _source : "(none)") +
                    (_error.Length > 0 ? " error=" + _error : ""));
            }
            catch
            {
                // 日志不可用不影响身份本身。
            }
        }

        private static string[] CandidateKeyPaths()
        {
            var paths = new System.Collections.Generic.List<string>(2);
            try
            {
                var appData = Path.Combine(
                    Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData),
                    "DNFLogin",
                    KeyFileName);
                paths.Add(appData);
            }
            catch
            {
                // 取不到 AppData 时跳过，不影响 exe 同目录与内置私钥两条路。
            }

            try
            {
                var exePath = Environment.ProcessPath;
                if (!string.IsNullOrWhiteSpace(exePath))
                {
                    var directory = Path.GetDirectoryName(exePath);
                    if (!string.IsNullOrWhiteSpace(directory))
                    {
                        paths.Add(Path.Combine(directory, KeyFileName));
                    }
                }
            }
            catch
            {
                // 同上：单一来源失败不阻断其余来源。
            }

            return paths.ToArray();
        }

        /// <summary>接受 PKCS#8 PEM 或纯 base64 的 PKCS#8 DER 两种写法。</summary>
        private static bool TryImport(string text, out ECDsa? key, out string error)
        {
            key = null;
            error = "";
            if (string.IsNullOrWhiteSpace(text))
            {
                error = "内容为空";
                return false;
            }

            try
            {
                var candidate = ECDsa.Create();
                if (text.Contains("-----BEGIN", StringComparison.Ordinal))
                {
                    candidate.ImportFromPem(text);
                }
                else
                {
                    candidate.ImportPkcs8PrivateKey(Convert.FromBase64String(CleanBase64(text)), out _);
                }

                if (candidate.KeySize != 256)
                {
                    candidate.Dispose();
                    error = "曲线不是 P-256（KeySize=" + candidate.KeySize + "）";
                    return false;
                }

                key = candidate;
                return true;
            }
            catch (Exception ex)
            {
                error = ex.Message;
                return false;
            }
        }

        private static string CleanBase64(string value)
        {
            var builder = new StringBuilder(value.Length);
            foreach (var character in value)
            {
                if (!char.IsWhiteSpace(character))
                {
                    builder.Append(character);
                }
            }
            return builder.ToString();
        }
    }
}
