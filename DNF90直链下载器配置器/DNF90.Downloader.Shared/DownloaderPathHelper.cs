namespace DNF90.Downloader.Shared;

public static class DownloaderPathHelper
{
    public const string DefaultDownloaderName = "DNF直链下载器";
    public const string DefaultFolderName = "地下城与勇士安装文件";
    public const string DefaultDisk = "D";

    public static string NormalizeDisk(string? disk)
    {
        var value = string.IsNullOrWhiteSpace(disk) ? DefaultDisk : disk.Trim();
        value = value.TrimEnd(':').Trim();
        if (value.Length != 1 || !char.IsAsciiLetter(value[0]))
        {
            throw new ArgumentException("默认下载磁盘必须是单个英文字母，例如 D。", nameof(disk));
        }
        return value.ToUpperInvariant();
    }

    public static string NormalizeFolderName(string? folderName)
    {
        var value = string.IsNullOrWhiteSpace(folderName) ? DefaultFolderName : folderName.Trim();
        if (value is "." or ".." ||
            value.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0 ||
            value.IndexOfAny(new[] { '\\', '/' }) >= 0 ||
            value.EndsWith('.'))
        {
            throw new ArgumentException("默认下载文件夹名称包含非法字符。", nameof(folderName));
        }
        return value;
    }

    public static string DefaultDownloadDirectory(string? disk, string? folderName)
    {
        var normalizedDisk = NormalizeDisk(disk);
        var normalizedFolder = NormalizeFolderName(folderName);
        return Path.Combine(normalizedDisk + ":\\", normalizedFolder);
    }

    public static string NormalizeSelectedDirectory(string selectedDirectory, string? folderName)
    {
        if (string.IsNullOrWhiteSpace(selectedDirectory))
        {
            throw new ArgumentException("下载目录不能为空。", nameof(selectedDirectory));
        }

        var full = Path.GetFullPath(selectedDirectory.Trim());
        var root = Path.GetPathRoot(full)
            ?? throw new ArgumentException("下载目录必须是绝对路径。", nameof(selectedDirectory));
        var normalizedFull = full.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        var normalizedRoot = root.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        return string.Equals(normalizedFull, normalizedRoot, StringComparison.OrdinalIgnoreCase)
            ? Path.Combine(full, NormalizeFolderName(folderName))
            : full;
    }

    public static string NormalizeDownloaderName(string? name)
    {
        var value = string.IsNullOrWhiteSpace(name) ? DefaultDownloaderName : name.Trim();
        if (value.EndsWith(".exe", StringComparison.OrdinalIgnoreCase))
        {
            value = value[..^4].Trim();
        }
        if (string.IsNullOrWhiteSpace(value) ||
            value.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0 ||
            value is "." or "..")
        {
            throw new ArgumentException("下载器名称包含非法文件名字符。", nameof(name));
        }
        return value;
    }
}
