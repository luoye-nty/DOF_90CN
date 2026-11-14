using System.IO;
using System.Text;

namespace DNF90.DownloaderBuilder;

internal sealed class DownloaderBuilderSettings
{
    public string DownloaderName { get; set; } = "";
    public string Urls { get; set; } = "";
    public string Disk { get; set; } = "";
    public string FolderName { get; set; } = "";
    public string OutputPath { get; set; } = "";
}

internal static class DownloaderBuilderSettingsStore
{
    private const string FileName = "DNF直链下载器配置器.ini";

    public static string SettingsPath => Path.Combine(AppContext.BaseDirectory, FileName);

    public static DownloaderBuilderSettings? Load()
    {
        try
        {
            if (!File.Exists(SettingsPath))
            {
                return null;
            }

            var values = ReadIni(SettingsPath);
            return new DownloaderBuilderSettings
            {
                DownloaderName = Get(values, nameof(DownloaderBuilderSettings.DownloaderName)),
                Urls = Get(values, nameof(DownloaderBuilderSettings.Urls)),
                Disk = Get(values, nameof(DownloaderBuilderSettings.Disk)),
                FolderName = Get(values, nameof(DownloaderBuilderSettings.FolderName)),
                OutputPath = Get(values, nameof(DownloaderBuilderSettings.OutputPath)),
            };
        }
        catch
        {
            return null;
        }
    }

    public static void Save(DownloaderBuilderSettings settings)
    {
        var content = new StringBuilder()
            .AppendLine("[Builder]")
            .AppendLine("DownloaderName=" + Escape(settings.DownloaderName))
            .AppendLine("Urls=" + Escape(settings.Urls))
            .AppendLine("Disk=" + Escape(settings.Disk))
            .AppendLine("FolderName=" + Escape(settings.FolderName))
            .AppendLine("OutputPath=" + Escape(settings.OutputPath))
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
