using System.Reflection;
using System.Text.Json;

namespace DNFLogin
{
    /// <summary>
    /// 构建期私有参数。数据来自项目根目录的 DNFLogin.build.json，
    /// 通过 EmbeddedResource 写入程序集；发布后不需要额外配置文件。
    /// </summary>
    public static class BuildParameters
    {
        private const string ResourceName = "DNFLogin.BuildParameters.json";
        private static readonly Lazy<LauncherBuildConfig> Value = new(Load, isThreadSafe: true);

        public static string ProfileId => Value.Value.ProfileId;
        public static string LauncherVersion => Value.Value.LauncherVersion;
        public static string LoginMode => Value.Value.LoginMode;
        public static string RemoteIp => Value.Value.RemoteIp;
        public static string RemotePort => Value.Value.RemotePort.ToString(System.Globalization.CultureInfo.InvariantCulture);
        public static int RemotePortNumber => Value.Value.RemotePort;

        private static LauncherBuildConfig Load()
        {
            using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName)
                ?? throw new InvalidOperationException("Missing embedded build configuration: " + ResourceName);
            var config = JsonSerializer.Deserialize<LauncherBuildConfig>(
                stream,
                new JsonSerializerOptions { PropertyNameCaseInsensitive = true })
                ?? throw new InvalidOperationException("Embedded build configuration is empty.");

            config.ProfileId = (config.ProfileId ?? "").Trim();
            config.LauncherVersion = (config.LauncherVersion ?? "").Trim();
            config.LoginMode = (config.LoginMode ?? "").Trim();
            config.RemoteIp = (config.RemoteIp ?? "").Trim();

            if (config.ProfileId == "")
            {
                throw new InvalidOperationException("DNFLogin.build.json: profileId is required.");
            }
            if (config.LauncherVersion == "" || config.LauncherVersion.StartsWith('v'))
            {
                throw new InvalidOperationException("DNFLogin.build.json: launcherVersion must be a non-empty version without a leading v.");
            }
            if (!string.Equals(config.LoginMode, "Remote", StringComparison.OrdinalIgnoreCase) &&
                !string.Equals(config.LoginMode, "Local", StringComparison.OrdinalIgnoreCase))
            {
                throw new InvalidOperationException("DNFLogin.build.json: loginMode must be Remote or Local.");
            }
            if (config.RemoteIp == "")
            {
                throw new InvalidOperationException("DNFLogin.build.json: remoteIp is required.");
            }
            if (config.RemotePort is < 1 or > 65535)
            {
                throw new InvalidOperationException("DNFLogin.build.json: remotePort must be in 1..65535.");
            }
            return config;
        }

        private sealed class LauncherBuildConfig
        {
            public string ProfileId { get; set; } = "90";
            public string LauncherVersion { get; set; } = "";
            public string LoginMode { get; set; } = "Remote";
            public string RemoteIp { get; set; } = "";
            public int RemotePort { get; set; } = 18112;
        }
    }
}
