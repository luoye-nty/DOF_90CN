using System.IO.Compression;
using System.Security.Cryptography;
using System.Text.Json;

namespace DNF90.Setup.Shared;

public static class GameInstaller
{
    public static async Task InstallAsync(
        SetupPayload payload,
        string installDirectory,
        IProgress<InstallProgress>? progress,
        CancellationToken cancellationToken = default)
    {
        var root = Path.GetFullPath(installDirectory);
        Directory.CreateDirectory(root);

        var totalBytes = Math.Max(1, payload.Manifest.TotalUncompressedBytes);
        long installedBytes = 0;
        var lastPercent = -1;

        foreach (var package in payload.Packages)
        {
            cancellationToken.ThrowIfCancellationRequested();
            progress?.Report(new InstallProgress(
                "正在校验 " + package.DisplayName,
                package.FileName,
                installedBytes * 100.0 / totalBytes));

            await using var packageStream = payload.OpenPackageStream(package);
            var hash = Convert.ToHexString(
                await SHA256.HashDataAsync(packageStream, cancellationToken).ConfigureAwait(false));
            if (!string.Equals(hash, package.Sha256, StringComparison.OrdinalIgnoreCase))
            {
                throw new InvalidDataException("安装分卷校验失败：" + package.FileName);
            }
            packageStream.Position = 0;

            using var archive = new ZipArchive(packageStream, ZipArchiveMode.Read, leaveOpen: true);
            foreach (var entry in archive.Entries)
            {
                cancellationToken.ThrowIfCancellationRequested();
                if (string.IsNullOrWhiteSpace(entry.FullName) ||
                    entry.FullName.EndsWith('/') || entry.FullName.EndsWith('\\'))
                {
                    continue;
                }

                var entryName = entry.FullName.Replace('\\', '/');
                var destinationPath = SafePath.CombineArchivePath(root, entryName);
                if (destinationPath is null)
                {
                    throw new InvalidDataException("安装包包含非法路径：" + entryName);
                }

                var parent = Path.GetDirectoryName(destinationPath);
                if (!string.IsNullOrEmpty(parent))
                {
                    Directory.CreateDirectory(parent);
                }

                await using var input = entry.Open();
                await using var output = new FileStream(
                    destinationPath, FileMode.Create, FileAccess.Write, FileShare.None, 1024 * 1024,
                    FileOptions.Asynchronous | FileOptions.SequentialScan);
                var buffer = new byte[1024 * 1024];
                int read;
                while ((read = await input.ReadAsync(buffer.AsMemory(), cancellationToken)
                           .ConfigureAwait(false)) > 0)
                {
                    await output.WriteAsync(buffer.AsMemory(0, read), cancellationToken).ConfigureAwait(false);
                    installedBytes += read;
                    var percent = (int)Math.Floor(installedBytes * 100.0 / totalBytes);
                    if (percent != lastPercent)
                    {
                        lastPercent = percent;
                        progress?.Report(new InstallProgress(
                            "正在安装 " + package.DisplayName,
                            entryName,
                            Math.Min(99, percent)));
                    }
                }

                try
                {
                    var lastWrite = entry.LastWriteTime;
                    if (lastWrite.Year is >= 1980 and <= 2107)
                    {
                        File.SetLastWriteTime(destinationPath, lastWrite.LocalDateTime);
                    }
                }
                catch
                {
                    // 文件时间写入失败不影响文件内容。
                }
            }
        }

        var installManifestPath = Path.Combine(root, ".dnf90-install-manifest.json");
        var manifestJson = JsonSerializer.Serialize(
            payload.Manifest,
            new JsonSerializerOptions(JsonSerializerDefaults.Web) { WriteIndented = true });
        await File.WriteAllTextAsync(installManifestPath, manifestJson, cancellationToken).ConfigureAwait(false);

        progress?.Report(new InstallProgress("安装完成", root, 100));
    }
}
