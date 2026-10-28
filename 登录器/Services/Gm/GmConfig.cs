using System;
using System.IO;
using System.Text.Json;

namespace DNFLogin.Gm
{
    /// <summary>GM 工具连接配置（对应 Go 版 settings / instance.json）</summary>
    public sealed class GmSettings
    {
        public string Host { get; set; } = "127.0.0.1";
        public int Port { get; set; } = 3306;
        public string Database { get; set; } = "dnf";
        public string User { get; set; } = "root";
        public string Password { get; set; } = "";
        public string PvfPath { get; set; } = "";

        /// <summary>数据库模式：portable=MySQL / sqlite（来自 instance.json database.mode）</summary>
        public string Mode { get; set; } = "portable";

        /// <summary>SQLite 模式数据库文件路径（instance.json database.path，相对服务端 runtime 目录）</summary>
        public string DatabasePath { get; set; } = "data/dnf90.db";

        /// <summary>DNF90Server 实例配置（server.adminListen / adminToken）</summary>
        public string InstancePath { get; set; } = @"D:\DNF\runtime\config\instance.json";

        /// <summary>角色名称搜索列：0=character_id，1=name</summary>
        public int CharacterSearchBy { get; set; } = 1;

        public string MySqlConnString() => $"server={Host};port={Port};database={Database};uid={User};pwd={Password};charset=utf8;";

        public GmSettings Clone() => (GmSettings)MemberwiseClone();
    }

    /// <summary>GM 工具配置持久化：AppData\DNFLogin\gmconfig.json</summary>
    public static class GmConfig
    {
        private static readonly string ConfigDir = Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "DNFLogin");
        private static readonly string ConfigFile = Path.Combine(ConfigDir, "gmconfig.json");

        private static GmSettings? _current;

        public static GmSettings Current => _current ??= Load();

        public static GmSettings Load()
        {
            try
            {
                if (File.Exists(ConfigFile))
                {
                    var json = File.ReadAllText(ConfigFile);
                    var loaded = JsonSerializer.Deserialize<GmSettings>(json);
                    if (loaded is not null)
                    {
                        _current = loaded;
                        return loaded;
                    }
                }
            }
            catch
            {
                // 配置损坏时回退默认值
            }
            _current = new GmSettings();
            return _current;
        }

        public static void Save(GmSettings settings)
        {
            _current = settings;
            Directory.CreateDirectory(ConfigDir);
            var json = JsonSerializer.Serialize(settings, new JsonSerializerOptions { WriteIndented = true });
            File.WriteAllText(ConfigFile, json);
        }
    }
}
