using System.IO.Compression;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace DNF90.Setup.Shared;

public static class ClientPackager
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web)
    {
        WriteIndented = true,
    };

    private static readonly string[] RequiredRootFiles =
    {
        "Script.pvf",
        "90CN.dll",
    };

    public static ClientAnalysis Analyze(string clientRoot)
    {
        var root = Path.GetFullPath(clientRoot);
        if (!Directory.Exists(root))
        {
            throw new DirectoryNotFoundException("游戏客户端目录不存在：" + root);
        }

        var imagePackRoot = Path.Combine(root, "ImagePacks2");
        if (!Directory.Exists(imagePackRoot))
        {
            throw new InvalidOperationException("客户端根目录缺少 ImagePacks2 文件夹。");
        }

        foreach (var required in RequiredRootFiles)
        {
            if (!File.Exists(Path.Combine(root, required)))
            {
                throw new InvalidOperationException("客户端根目录缺少必需文件：" + required);
            }
        }

        var exclusions = new HashSet<string>(
            RequiredRootFiles.Append(".dnf90-install-manifest.json"),
            StringComparer.OrdinalIgnoreCase);

        var allFiles = Directory.EnumerateFiles(root, "*", SearchOption.AllDirectories)
            .Select(path => new SourceFileInfo(
                path,
                Path.GetRelativePath(root, path).Replace('\\', '/'),
                new FileInfo(path).Length))
            .ToList();

        var imagePrefix = "ImagePacks2/";
        var imageFiles = allFiles
            .Where(x => x.RelativePath.StartsWith(imagePrefix, StringComparison.OrdinalIgnoreCase))
            .OrderBy(x => x.RelativePath, StringComparer.OrdinalIgnoreCase)
            .ToList();

        var otherFiles = allFiles
            .Where(x => !x.RelativePath.StartsWith(imagePrefix, StringComparison.OrdinalIgnoreCase))
            .Where(x => !exclusions.Contains(x.RelativePath))
            .OrderBy(x => x.RelativePath, StringComparer.OrdinalIgnoreCase)
            .ToList();

        return new ClientAnalysis
        {
            ClientRoot = root,
            ImagePackFiles = imageFiles,
            ScriptFiles = FindExact(allFiles, "Script.pvf"),
            Cn90Files = FindExact(allFiles, "90CN.dll"),
            OtherFiles = otherFiles,
        };
    }

    public static async Task<BuildResult> BuildAsync(
        ClientAnalysis analysis,
        BuildOptions options,
        IProgress<BuilderProgress>? progress,
        CancellationToken cancellationToken = default)
    {
        var outputDirectory = Path.GetFullPath(options.OutputDirectory);
        var clientRoot = Path.GetFullPath(options.ClientRoot);
        if (IsWithin(outputDirectory, clientRoot))
        {
            throw new InvalidOperationException("输出目录不能放在游戏客户端目录内部，否则后续会重复打包输出文件。");
        }

        if (string.IsNullOrWhiteSpace(options.Version) ||
            options.Version.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0)
        {
            throw new InvalidOperationException("安装包版本号为空或包含文件名非法字符。");
        }

        var setupBaseName = NormalizeSetupBaseName(options.SetupBaseName);
        var packagePrefix = NormalizePackagePrefix(options.PackagePrefix);
        var installFolderName = InstallPathHelper.NormalizeFolderName(
            options.InstallFolderName);
        var launcherRelativePath = ValidateLauncherPath(
            clientRoot,
            options.LauncherPath);

        if (options.ImagePackTargetBytes < 64L * 1024 * 1024)
        {
            throw new InvalidOperationException("ImagePacks2 单个分包目标不能小于 64 MB。");
        }

        if (!File.Exists(options.InstallerTemplatePath))
        {
            throw new FileNotFoundException("找不到独立安装器模板。请先构建安装器，再构建配置器。",
                options.InstallerTemplatePath);
        }

        if (SetupFooter.TryRead(options.InstallerTemplatePath) is not null)
        {
            throw new InvalidOperationException("所选安装器模板本身已经带有安装负载，请使用原始安装器模板。");
        }

        Directory.CreateDirectory(outputDirectory);
        var workDirectory = Path.Combine(outputDirectory, ".dnf90-package-work-" + Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(workDirectory);

        try
        {
            var plans = BuildPackagePlans(
                analysis,
                options.ImagePackTargetBytes,
                packagePrefix,
                options.Version.Trim(),
                options.ReleaseContent);
            var manifest = new SetupManifest
            {
                ProductName = "落叶DNF",
                PackagePrefix = packagePrefix,
                ReleaseContent = options.ReleaseContent,
                Version = options.Version.Trim(),
                SourceFolderName = new DirectoryInfo(clientRoot).Name,
                CreatedAt = DateTimeOffset.Now,
                TotalUncompressedBytes = plans.Sum(plan => plan.Files.Sum(file => file.Length)),
                InstallFolderName = installFolderName,
                LauncherRelativePath = launcherRelativePath,
            };

            long processedBytes = 0;
            var totalSourceBytes = Math.Max(1, manifest.TotalUncompressedBytes);

            var packageResults = new SetupPackageInfo[plans.Count];
            var maxParallelism = Math.Clamp(Environment.ProcessorCount / 2, 1, 4);
            await Parallel.ForEachAsync(
                Enumerable.Range(0, plans.Count),
                new ParallelOptions
                {
                    MaxDegreeOfParallelism = maxParallelism,
                    CancellationToken = cancellationToken,
                },
                async (index, token) =>
                {
                    var plan = plans[index];
                    progress?.Report(new BuilderProgress(
                        "正在压缩 " + plan.DisplayName,
                        $"{plan.Files.Count} 个文件",
                        Volatile.Read(ref processedBytes) * 90.0 / totalSourceBytes));

                    packageResults[index] = await CreatePackageAsync(
                        workDirectory,
                        plan,
                        p =>
                        {
                            var completed = Interlocked.Add(ref processedBytes, p.DeltaBytes);
                            progress?.Report(new BuilderProgress(
                                "正在压缩 " + plan.DisplayName,
                                p.RelativePath,
                                completed * 90.0 / totalSourceBytes));
                        },
                        token).ConfigureAwait(false);
                }).ConfigureAwait(false);
            manifest.Packages.AddRange(packageResults);

            manifest.TotalCompressedBytes = manifest.Packages.Sum(x => x.CompressedBytes);
            progress?.Report(new BuilderProgress(
                "正在生成分卷安装器",
                "正在写入安装清单…",
                92));

            var setupPath = Path.Combine(outputDirectory, setupBaseName + ".exe");
            await CreateExternalSetupAsync(
                options.InstallerTemplatePath,
                setupPath,
                workDirectory,
                manifest,
                cancellationToken).ConfigureAwait(false);

            progress?.Report(new BuilderProgress("生成完成", setupPath, 100));
            return new BuildResult(
                setupPath,
                outputDirectory,
                manifest);
        }
        finally
        {
            TryDeleteDirectory(workDirectory);
        }
    }

    private static List<SourceFileInfo> FindExact(IEnumerable<SourceFileInfo> files, string relativePath)
    {
        return files
            .Where(x => string.Equals(x.RelativePath, relativePath, StringComparison.OrdinalIgnoreCase))
            .ToList();
    }

    private static List<PackagePlan> BuildPackagePlans(
        ClientAnalysis analysis,
        long imagePackTargetBytes,
        string packagePrefix,
        string version,
        ReleaseContent releaseContent)
    {
        var remainingFiles = analysis.Cn90Files
            .Concat(analysis.OtherFiles)
            .OrderBy(x => x.RelativePath, StringComparer.OrdinalIgnoreCase)
            .ToList();
        var plans = new List<PackagePlan>();

        if (releaseContent is ReleaseContent.Full or ReleaseContent.PvfOnly)
        {
            plans.Add(new PackagePlan(
                "script-pvf",
                "Script.pvf",
                PackageName(packagePrefix, version, 1),
                analysis.ScriptFiles,
                CompressionLevel.Fastest));
        }

        if (releaseContent is ReleaseContent.Full or ReleaseContent.RemainingOnly)
        {
            plans.Add(new PackagePlan(
                "remaining-files",
                "登录器与剩余配件",
                PackageName(packagePrefix, version, 2),
                remainingFiles,
                CompressionLevel.Fastest));
        }

        if (releaseContent is ReleaseContent.Full or ReleaseContent.ImagePacksOnly)
        {
            var imageGroups = SplitImagePackFiles(analysis.ImagePackFiles, imagePackTargetBytes);
            for (var i = 0; i < imageGroups.Count; i++)
            {
                var number = i + 3;
                plans.Add(new PackagePlan(
                    $"imagepacks2-part-{number:000}",
                    $"ImagePacks2 分卷 {i + 1:000}",
                    PackageName(packagePrefix, version, number),
                    imageGroups[i],
                    CompressionLevel.Fastest));
            }
        }

        return plans;
    }

    private static string PackageName(string prefix, string version, int index)
        => $"{prefix}_{version}_{index:000}.zip";

    private static List<List<SourceFileInfo>> SplitImagePackFiles(
        IReadOnlyList<SourceFileInfo> files,
        long targetBytes)
    {
        var groups = new List<List<SourceFileInfo>>();
        var current = new List<SourceFileInfo>();
        long currentBytes = 0;

        foreach (var file in files)
        {
            if (current.Count > 0 && currentBytes + file.Length > targetBytes)
            {
                groups.Add(current);
                current = new List<SourceFileInfo>();
                currentBytes = 0;
            }

            current.Add(file);
            currentBytes += file.Length;
        }

        if (current.Count > 0)
        {
            groups.Add(current);
        }
        return groups;
    }

    private sealed record PackagePlan(
        string Id,
        string DisplayName,
        string FileName,
        IReadOnlyList<SourceFileInfo> Files,
        CompressionLevel CompressionLevel);

    private sealed record CopyProgress(long DeltaBytes, string RelativePath);

    private static async Task<SetupPackageInfo> CreatePackageAsync(
        string workDirectory,
        PackagePlan plan,
        Action<CopyProgress> reportProgress,
        CancellationToken cancellationToken)
    {
        var localPath = Path.Combine(workDirectory, plan.FileName);
        // ZIP 写入是同步 I/O + CPU 压缩。必须放到后台线程，
        // 否则会堵塞 WPF Dispatcher，界面会一直停在「正在准备安装器模板」。
        await Task.Run(() =>
        {
            using var output = new FileStream(
                localPath, FileMode.Create, FileAccess.Write, FileShare.None, 1024 * 1024,
                FileOptions.SequentialScan);
            using var archive = new ZipArchive(output, ZipArchiveMode.Create, leaveOpen: true);
            foreach (var file in plan.Files)
            {
                cancellationToken.ThrowIfCancellationRequested();
                var entry = archive.CreateEntry(file.RelativePath, plan.CompressionLevel);
                var lastWrite = File.GetLastWriteTime(file.FullPath);
                if (lastWrite.Year is >= 1980 and <= 2107)
                {
                    entry.LastWriteTime = lastWrite;
                }

                using var input = new FileStream(
                    file.FullPath, FileMode.Open, FileAccess.Read, FileShare.ReadWrite, 1024 * 1024,
                    FileOptions.SequentialScan);
                using var entryStream = entry.Open();
                input.CopyTo(entryStream, 1024 * 1024);
                reportProgress(new CopyProgress(file.Length, file.RelativePath));
            }
        }, cancellationToken).ConfigureAwait(false);

        await using var packageStream = new FileStream(
            localPath, FileMode.Open, FileAccess.Read, FileShare.Read, 1024 * 1024,
            FileOptions.Asynchronous | FileOptions.SequentialScan);
        var hash = Convert.ToHexString(await SHA256.HashDataAsync(packageStream, cancellationToken)
            .ConfigureAwait(false));

        return new SetupPackageInfo
        {
            Id = plan.Id,
            DisplayName = plan.DisplayName,
            FileName = plan.FileName,
            FileCount = plan.Files.Count,
            UncompressedBytes = plan.Files.Sum(x => x.Length),
            CompressedBytes = new FileInfo(localPath).Length,
            Sha256 = hash,
            LocalPath = localPath,
        };
    }

    private static async Task CreateExternalSetupAsync(
        string templatePath,
        string setupPath,
        string workDirectory,
        SetupManifest manifest,
        CancellationToken cancellationToken)
    {
        var packagesDirectory = Path.GetDirectoryName(setupPath)
            ?? throw new InvalidOperationException("无法确定安装器所在目录。");
        Directory.CreateDirectory(packagesDirectory);

        foreach (var package in manifest.Packages)
        {
            cancellationToken.ThrowIfCancellationRequested();
            // 临时包目录和最终输出目录都在同一块磁盘：移动到最终位置不会
            // 再次复制十几 GB 的 NPK，能显著缩短分卷发布的总耗时。
            File.Move(
                package.LocalPath,
                Path.Combine(packagesDirectory, package.FileName),
                overwrite: true);
        }

        var tempSetup = Path.Combine(workDirectory, "external-setup.tmp");
        File.Copy(templatePath, tempSetup, overwrite: true);
        var manifestBytes = JsonSerializer.SerializeToUtf8Bytes(manifest, JsonOptions);
        var payloadOffset = new FileInfo(tempSetup).Length;
        await using (var output = new FileStream(
                         tempSetup, FileMode.Append, FileAccess.Write, FileShare.None,
                         1024 * 1024, FileOptions.Asynchronous | FileOptions.SequentialScan))
        using (var writer = new BinaryWriter(output, Encoding.UTF8, leaveOpen: true))
        {
            writer.Write((long)manifestBytes.Length);
            writer.Write(manifestBytes);
            writer.Flush();
            var payloadLength = output.Length - payloadOffset;
            SetupFooter.Append(output, SetupSourceMode.ExternalPackages, payloadOffset, payloadLength);
            await output.FlushAsync(cancellationToken).ConfigureAwait(false);
        }

        MoveOverwrite(tempSetup, setupPath);
    }

    private static void MoveOverwrite(string source, string destination)
    {
        try
        {
            File.Move(source, destination, overwrite: true);
        }
        catch (IOException ex)
        {
            throw new IOException("无法写入安装器文件，请关闭正在运行的旧安装器后重试：" + destination, ex);
        }
    }

    private static string NormalizeSetupBaseName(string raw)
    {
        var name = (raw ?? "").Trim();
        if (name.EndsWith(".exe", StringComparison.OrdinalIgnoreCase))
        {
            name = name[..^4].Trim();
        }
        if (string.IsNullOrWhiteSpace(name) || name.IndexOfAny(Path.GetInvalidFileNameChars()) >= 0)
        {
            throw new InvalidOperationException("安装器名称不能为空，也不能包含路径或文件名非法字符。");
        }
        return name;
    }

    private static string ValidateLauncherPath(string clientRoot, string rawPath)
    {
        if (string.IsNullOrWhiteSpace(rawPath))
        {
            throw new InvalidOperationException("请先选择客户端登录器文件。");
        }

        var fullPath = Path.IsPathFullyQualified(rawPath)
            ? Path.GetFullPath(rawPath)
            : Path.GetFullPath(Path.Combine(clientRoot, rawPath));
        if (!File.Exists(fullPath))
        {
            throw new FileNotFoundException("所选择的登录器文件不存在。", fullPath);
        }
        if (!string.Equals(Path.GetExtension(fullPath), ".exe", StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException("登录器必须是 EXE 文件。");
        }
        if (!IsWithin(fullPath, clientRoot))
        {
            throw new InvalidOperationException("登录器文件必须位于所选客户端目录内。");
        }

        return Path.GetRelativePath(clientRoot, fullPath).Replace('\\', '/');
    }

    private static string NormalizePackagePrefix(string raw)
    {
        var prefix = (raw ?? "").Trim().TrimEnd('_');
        if (string.IsNullOrWhiteSpace(prefix) ||
            prefix.Any(ch => !(char.IsAsciiLetterOrDigit(ch) || ch is '_' or '-')))
        {
            throw new InvalidOperationException("压缩包前缀只能包含英文字母、数字、下划线或短横线。");
        }
        return prefix;
    }

    private static bool IsWithin(string candidate, string parent)
    {
        var full = Path.GetFullPath(candidate).TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        var root = Path.GetFullPath(parent).TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar);
        return string.Equals(full, root, StringComparison.OrdinalIgnoreCase) ||
               full.StartsWith(root + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase);
    }

    private static void TryDeleteDirectory(string path)
    {
        try
        {
            if (Directory.Exists(path))
            {
                Directory.Delete(path, recursive: true);
            }
        }
        catch
        {
            // 临时目录清理失败不应覆盖真正的构建结果或错误。
        }
    }
}
