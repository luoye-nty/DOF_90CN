namespace DNF90.Setup.Shared;

public static class InstallPathHelper
{
    public const string DefaultInstallFolderName = "地下城与勇士";

    /// <summary>
    /// 用户选择磁盘/共享根目录时，自动改为根目录下的“地下城与勇士”；
    /// 选择普通文件夹时按用户选择原样使用。
    /// </summary>
    public static string NormalizeTarget(string selectedDirectory, string? installFolderName = null)
    {
        if (string.IsNullOrWhiteSpace(selectedDirectory))
        {
            throw new ArgumentException("安装目录不能为空。", nameof(selectedDirectory));
        }

        var full = Path.GetFullPath(selectedDirectory.Trim());
        var root = Path.GetPathRoot(full);
        if (string.IsNullOrWhiteSpace(root))
        {
            throw new ArgumentException("安装目录必须是绝对路径。", nameof(selectedDirectory));
        }

        var normalizedRoot = root.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        var normalizedFull = full.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        if (string.Equals(normalizedFull, normalizedRoot, StringComparison.OrdinalIgnoreCase))
        {
            return Path.Combine(full, NormalizeFolderName(installFolderName));
        }

        return full;
    }

    public static string NormalizeConfiguredDefault(string? installFolderName)
    {
        return Path.Combine(@"D:\", NormalizeFolderName(installFolderName));
    }

    public static string NormalizeFolderName(string? folderName)
    {
        var name = string.IsNullOrWhiteSpace(folderName)
            ? DefaultInstallFolderName
            : folderName.Trim();
        if (name is "." or ".." ||
            name.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0 ||
            name.IndexOfAny(new[] { '\\', '/' }) >= 0 ||
            name.EndsWith('.'))
        {
            throw new ArgumentException("默认安装文件夹名称不能包含路径、非法字符或以句点结尾。",
                nameof(folderName));
        }
        return name;
    }
}
