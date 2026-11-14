using System.IO;
using System.Text;
using DNF90.Setup.Shared;

namespace DNF90.Setup.Builder;

internal sealed class InstallerBuilderSettings
{
    public string ClientPath { get; set; } = "";
    public string LauncherPath { get; set; } = "";
    public string OutputPath { get; set; } = "";
    public string Version { get; set; } = "";
    public string ImagePackTargetMb { get; set; } = "";
    public string SetupName { get; set; } = "";
    public string PackagePrefix { get; set; } = "";
    public string InstallFolderName { get; set; } = "";
    public ReleaseContent ReleaseContent { get; set; } = ReleaseContent.Full;
}

internal static class InstallerBuilderSettingsStore
{
    private const string FileName = "落叶DNF安装包配置器.ini";

    public static string SettingsPath => Path.Combine(AppContext.BaseDirectory, FileName);

    public static InstallerBuilderSettings? Load()
    {
        try
        {
            if (!File.Exists(SettingsPath))
            {
                return null;
            }

            var values = ReadIni(SettingsPath);
            var settings = new InstallerBuilderSettings
            {
                ClientPath = Get(values, nameof(InstallerBuilderSettings.ClientPath)),
                LauncherPath = Get(values, nameof(InstallerBuilderSettings.LauncherPath)),
                OutputPath = Get(values, nameof(InstallerBuilderSettings.OutputPath)),
                Version = Get(values, nameof(InstallerBuilderSettings.Version)),
                ImagePackTargetMb = Get(values, nameof(InstallerBuilderSettings.ImagePackTargetMb)),
                SetupName = Get(values, nameof(InstallerBuilderSettings.SetupName)),
                PackagePrefix = Get(values, nameof(InstallerBuilderSettings.PackagePrefix)),
                InstallFolderName = Get(values, nameof(InstallerBuilderSettings.InstallFolderName)),
            };
            if (Enum.TryParse<ReleaseContent>(
                    Get(values, nameof(InstallerBuilderSettings.ReleaseContent)),
                    ignoreCase: true,
                    out var releaseContent))
            {
                settings.ReleaseContent = releaseContent;
            }
            return settings;
        }
        catch
        {
            return null;
        }
    }

    public static void Save(InstallerBuilderSettings settings)
    {
        var content = new StringBuilder()
            .AppendLine("[Builder]")
            .AppendLine("ClientPath=" + Escape(settings.ClientPath))
            .AppendLine("LauncherPath=" + Escape(settings.LauncherPath))
            .AppendLine("OutputPath=" + Escape(settings.OutputPath))
            .AppendLine("Version=" + Escape(settings.Version))
            .AppendLine("ImagePackTargetMb=" + Escape(settings.ImagePackTargetMb))
            .AppendLine("SetupName=" + Escape(settings.SetupName))
            .AppendLine("PackagePrefix=" + Escape(settings.PackagePrefix))
            .AppendLine("InstallFolderName=" + Escape(settings.InstallFolderName))
            .AppendLine("ReleaseContent=" + settings.ReleaseContent)
            .ToString();

        var tempPath = SettingsPath + ".tmp";
        File.WriteAllText(tempPath, content, new UTF8Encoding(encoderShouldEmitUTF8Identifier: false));
        File.Move(tempPath, SettingsPath, overwrite: true);
    }

    private static Dictionary<string, string> ReadIni(string path)
    {
        var values = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var rawLine in File.ReadAllLines(path, Encoding.UTF8))
        {
            var line = rawLine.Trim();
            if (line.Length == 0 || line.StartsWith('[') || line.StartsWith('#') || line.StartsWith(';'))
            {
                continue;
            }
            var separator = line.IndexOf('=');
            if (separator <= 0)
            {
                continue;
            }
            values[line[..separator].Trim()] = Unescape(line[(separator + 1)..].Trim());
        }
        return values;
    }

    private static string Get(IReadOnlyDictionary<string, string> values, string key)
        => values.TryGetValue(key, out var value) ? value : "";

    private static string Escape(string value)
        => (value ?? "").Replace("\\", "\\\\").Replace("\r", "\\r").Replace("\n", "\\n");

    private static string Unescape(string value)
    {
        var result = new StringBuilder(value.Length);
        for (var i = 0; i < value.Length; i++)
        {
            if (value[i] == '\\' && i + 1 < value.Length)
            {
                var next = value[++i];
                if (next is 'r' or 'n' or '\\')
                {
                    result.Append(next switch
                    {
                        'r' => '\r',
                        'n' => '\n',
                        _ => '\\',
                    });
                }
                else
                {
                    result.Append('\\').Append(next);
                }
            }
            else
            {
                result.Append(value[i]);
            }
        }
        return result.ToString();
    }
}
