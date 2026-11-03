using System.Diagnostics;
using System.Net;
using System.Net.Http;

namespace DNF90.Downloader.Shared;

public static class DirectDownloadEngine
{
    public static async Task<DownloadResult> DownloadAllAsync(
        DownloaderConfig config,
        string destinationDirectory,
        IProgress<DownloadProgress>? progress,
        CancellationToken cancellationToken = default)
    {
        var urls = config.DownloadUrls
            .Select(url => url.Trim())
            .Where(url => url.Length > 0)
            .ToList();
        if (urls.Count == 0)
        {
            throw new InvalidOperationException("下载器没有配置任何直链地址。");
        }

        Directory.CreateDirectory(destinationDirectory);
        var results = new List<DownloadItemResult>();
        using var handler = new SocketsHttpHandler
        {
            ConnectTimeout = TimeSpan.FromSeconds(30),
            AutomaticDecompression = DecompressionMethods.All,
            AllowAutoRedirect = true,
            MaxAutomaticRedirections = 10,
        };
        using var client = new HttpClient(handler)
        {
            Timeout = Timeout.InfiniteTimeSpan,
        };
        client.DefaultRequestHeaders.UserAgent.ParseAdd("DNF90DirectDownloader/1.0");

        for (var index = 0; index < urls.Count; index++)
        {
            cancellationToken.ThrowIfCancellationRequested();
            var url = urls[index];
            var uri = new Uri(url, UriKind.Absolute);
            progress?.Report(new DownloadProgress(
                index + 1,
                urls.Count,
                "",
                $"正在连接第 {index + 1}/{urls.Count} 个下载地址…",
                0,
                0,
                0,
                index * 100.0 / urls.Count));

            using var response = await client.GetAsync(
                uri,
                HttpCompletionOption.ResponseHeadersRead,
                cancellationToken).ConfigureAwait(false);
            if (!response.IsSuccessStatusCode)
            {
                throw new HttpRequestException(
                    $"下载地址返回 HTTP {(int)response.StatusCode}：{url}");
            }

            var fileName = ResolveFileName(response, uri, index);
            var finalPath = GetUniqueFilePath(destinationDirectory, fileName);
            var partPath = finalPath + ".part";
            var total = response.Content.Headers.ContentLength ?? -1;
            long done = 0;
            var stopwatch = Stopwatch.StartNew();

            try
            {
                await using var input = await response.Content
                    .ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);
                await using var output = new FileStream(
                    partPath,
                    FileMode.Create,
                    FileAccess.Write,
                    FileShare.None,
                    1024 * 1024,
                    FileOptions.Asynchronous | FileOptions.SequentialScan);
                var buffer = new byte[1024 * 1024];
                int read;
                while ((read = await input.ReadAsync(buffer.AsMemory(), cancellationToken)
                           .ConfigureAwait(false)) > 0)
                {
                    await output.WriteAsync(buffer.AsMemory(0, read), cancellationToken)
                        .ConfigureAwait(false);
                    done += read;
                    var elapsed = Math.Max(0.1, stopwatch.Elapsed.TotalSeconds);
                    var fileRatio = total > 0
                        ? Math.Clamp(done * 1.0 / total, 0, 1)
                        : 0;
                    var overall = (index + fileRatio) * 100.0 / urls.Count;
                    progress?.Report(new DownloadProgress(
                        index + 1,
                        urls.Count,
                        Path.GetFileName(finalPath),
                        $"正在下载第 {index + 1}/{urls.Count} 个文件",
                        done,
                        total,
                        done / 1024.0 / 1024.0 / elapsed,
                        overall));
                }

                await output.FlushAsync(cancellationToken).ConfigureAwait(false);
                output.Close();
                File.Move(partPath, finalPath);
            }
            catch
            {
                try
                {
                    if (File.Exists(partPath))
                    {
                        File.Delete(partPath);
                    }
                }
                catch
                {
                    // 下载失败时保留原始错误，临时文件清理失败不覆盖它。
                }
                throw;
            }

            results.Add(new DownloadItemResult(url, finalPath));
            progress?.Report(new DownloadProgress(
                index + 1,
                urls.Count,
                Path.GetFileName(finalPath),
                $"第 {index + 1}/{urls.Count} 个文件下载完成",
                done,
                total,
                0,
                (index + 1) * 100.0 / urls.Count));
        }

        return new DownloadResult(results);
    }

    private static string ResolveFileName(
        HttpResponseMessage response,
        Uri uri,
        int index)
    {
        var disposition = response.Content.Headers.ContentDisposition;
        var candidate = disposition?.FileNameStar ?? disposition?.FileName;
        candidate = candidate?.Trim().Trim('"');
        if (string.IsNullOrWhiteSpace(candidate))
        {
            candidate = Uri.UnescapeDataString(Path.GetFileName(uri.LocalPath));
        }
        if (string.IsNullOrWhiteSpace(candidate))
        {
            candidate = $"download-{index + 1}";
        }

        candidate = Path.GetFileName(candidate);
        foreach (var invalid in Path.GetInvalidFileNameChars())
        {
            candidate = candidate.Replace(invalid, '_');
        }
        if (string.IsNullOrWhiteSpace(candidate))
        {
            candidate = $"download-{index + 1}";
        }

        if (!Path.HasExtension(candidate))
        {
            candidate += ExtensionForContentType(response.Content.Headers.ContentType?.MediaType);
        }
        return candidate;
    }

    private static string ExtensionForContentType(string? mediaType)
    {
        return mediaType?.ToLowerInvariant() switch
        {
            "application/zip" or "application/x-zip-compressed" => ".zip",
            "application/x-7z-compressed" => ".7z",
            "application/x-rar-compressed" or "application/vnd.rar" => ".rar",
            "application/vnd.microsoft.portable-executable" or
                "application/x-msdownload" => ".exe",
            _ => ".download",
        };
    }

    private static string GetUniqueFilePath(string directory, string fileName)
    {
        var path = Path.Combine(directory, fileName);
        if (!File.Exists(path))
        {
            return path;
        }

        var stem = Path.GetFileNameWithoutExtension(fileName);
        var extension = Path.GetExtension(fileName);
        for (var index = 1; index < 10000; index++)
        {
            path = Path.Combine(directory, $"{stem} ({index}){extension}");
            if (!File.Exists(path))
            {
                return path;
            }
        }
        throw new IOException("无法为下载文件生成唯一名称：" + fileName);
    }
}
