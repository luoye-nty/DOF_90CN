using System.IO;
using System.Reflection;

namespace DNF90.Setup.Builder;

internal static class EmbeddedInstallerRuntime
{
    private const string ResourceName = "DNF90.Setup.Builder.Runtime.Installer.exe";
    private static readonly object Sync = new();
    private static string? _temporaryDirectory;

    public static string Extract()
    {
        lock (Sync)
        {
            if (_temporaryDirectory is not null)
            {
                var existing = Path.Combine(_temporaryDirectory, "落叶DNF安装程序.exe");
                if (File.Exists(existing))
                {
                    return existing;
                }
            }

            var assembly = Assembly.GetExecutingAssembly();
            using var resource = assembly.GetManifestResourceStream(ResourceName);
            if (resource is null)
            {
                var adjacent = Path.Combine(AppContext.BaseDirectory, "落叶DNF安装程序.exe");
                if (File.Exists(adjacent))
                {
                    return adjacent;
                }
                throw new FileNotFoundException(
                    "配置器内没有找到独立安装器模板。请使用 Build-InstallerSuite.ps1 重新构建完整工具。");
            }

            var directory = Path.Combine(
                Path.GetTempPath(),
                "DNF90SetupBuilder",
                Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(directory);
            var path = Path.Combine(directory, "落叶DNF安装程序.exe");
            using (var output = new FileStream(path, FileMode.CreateNew, FileAccess.Write, FileShare.None))
            {
                resource.CopyTo(output);
            }
            _temporaryDirectory = directory;
            return path;
        }
    }

    public static void Cleanup()
    {
        lock (Sync)
        {
            if (_temporaryDirectory is null)
            {
                return;
            }
            try
            {
                Directory.Delete(_temporaryDirectory, recursive: true);
            }
            catch
            {
                // 程序退出时清理失败不影响生成结果。
            }
            _temporaryDirectory = null;
        }
    }
}
