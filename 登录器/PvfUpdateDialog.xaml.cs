using System;
using System.Diagnostics;
using System.IO;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using SharpCompress.Archives;
using SharpCompress.Common;

namespace DNFLogin
{
    /// <summary>
    /// PVF 客户端自动更新窗口：从直链下载压缩包（zip/7z/rar）→ 解压覆盖客户端根目录。
    /// 显示实时进度/速度；可取消，取消会删除已下载的临时文件。
    /// </summary>
    public partial class PvfUpdateDialog : Window
    {
        private readonly string _downloadUrl;
        private readonly string _targetDir;
        private readonly string _expectedSha;   // 服务端最新 Script.pvf 的 SHA-256（解压完成后复核）
        private readonly CancellationTokenSource _cts = new();
        private string? _tempFile;

        public PvfUpdateDialog(string downloadUrl, string targetDir, string expectedSha = "")
        {
            InitializeComponent();
            _downloadUrl = downloadUrl;
            _targetDir = targetDir;
            _expectedSha = expectedSha ?? "";
            Loaded += OnLoaded;
        }

        private void OnLoaded(object sender, RoutedEventArgs e)
        {
            _ = RunAsync();
        }

        private async Task RunAsync()
        {
            try
            {
                _tempFile = await DownloadAsync();
                SetState("正在解压更新包…", 100, false);
                var ok = await Task.Run(() => TryExtract(_tempFile));
                if (!ok)
                {
                    Fail("解压失败：更新包可能已损坏或格式不支持（支持 zip/7z/rar）。");
                    return;
                }

                // 解压后复核 Script.pvf 与服务端最新哈希是否一致
                if (!string.IsNullOrWhiteSpace(_expectedSha))
                {
                    var pvfPath = System.IO.Path.Combine(_targetDir, "Script.pvf");
                    var localSha = File.Exists(pvfPath) ? ComputeFileSha256Hex(pvfPath) : "";
                    if (!string.Equals(localSha, _expectedSha, StringComparison.OrdinalIgnoreCase))
                    {
                        Fail("更新包解压完成，但 Script.pvf 校验不一致，请通知服主重新发布更新包。");
                        return;
                    }
                }

                DialogResult = true;
            }
            catch (OperationCanceledException)
            {
                DialogResult = false;
            }
            catch (Exception ex)
            {
                Fail("更新失败：" + ex.Message);
            }
        }

        /// <summary>下载到临时目录（%TEMP%），带实时进度与速度，可取消</summary>
        private async Task<string> DownloadAsync()
        {
            SetState("正在连接服务器…", 0, false);
            var stopwatch = Stopwatch.StartNew();
            var temp = System.IO.Path.Combine(System.IO.Path.GetTempPath(),
                $"dnf_pvf_update_{Guid.NewGuid():N}.pkg");

            using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(30) };
            using (var response = await client.GetAsync(_downloadUrl, HttpCompletionOption.ResponseHeadersRead, _cts.Token))
            {
                response.EnsureSuccessStatusCode();
                var total = response.Content.Headers.ContentLength ?? -1L;
                await using var src = await response.Content.ReadAsStreamAsync(_cts.Token);
                await using var dst = new FileStream(temp, FileMode.Create, FileAccess.Write, FileShare.None, 81920, true);

                var buffer = new byte[81920];
                long done = 0;
                int read;
                SetState("正在下载更新包…", 0, true);
                while ((read = await src.ReadAsync(buffer, 0, buffer.Length, _cts.Token)) > 0)
                {
                    await dst.WriteAsync(buffer, 0, read, _cts.Token);
                    done += read;
                    var pct = total > 0 ? (int)(done * 100 / total) : 0;
                    var speed = done / 1024.0 / 1024.0 / Math.Max(stopwatch.Elapsed.TotalSeconds, 0.1);
                    SetState("正在下载更新包…", pct, true, speed, total > 0 ? done : total, total);
                }
            }

            if (_cts.IsCancellationRequested)
            {
                throw new OperationCanceledException(_cts.Token);
            }
            return temp;
        }

        /// <summary>把压缩包内条目安全解压到目标根目录，有啥替换啥；只读文件先清只读属性</summary>
        private bool TryExtract(string archivePath)
        {
            using var fileStream = new FileStream(archivePath, FileMode.Open, FileAccess.Read, FileShare.Read);
            using var archive = ArchiveFactory.OpenArchive(fileStream);
            foreach (var entry in archive.Entries)
            {
                _cts.Token.ThrowIfCancellationRequested();
                if (entry.IsDirectory)
                {
                    continue;
                }

                var rel = entry.Key?.Replace('\\', '/') ?? "";
                var dest = SafeCombine(_targetDir, rel);
                if (dest is null)
                {
                    continue; // 非法条目（绝对路径/../穿越）跳过
                }

                var dir = System.IO.Path.GetDirectoryName(dest);
                if (!string.IsNullOrEmpty(dir))
                {
                    Directory.CreateDirectory(dir);
                }

                if (File.Exists(dest))
                {
                    var attrs = File.GetAttributes(dest);
                    if ((attrs & FileAttributes.ReadOnly) != 0)
                    {
                        File.SetAttributes(dest, attrs & ~FileAttributes.ReadOnly);
                    }
                }

                using var stream = entry.OpenEntryStream();
                using var outStream = new FileStream(dest, FileMode.Create, FileAccess.Write, FileShare.Read);
                stream.CopyTo(outStream);
            }
            return true;
        }

        /// <summary>条目相对路径安全拼接到目标目录；存在穿越/绝对路径时返回 null</summary>
        private static string? SafeCombine(string root, string rel)
        {
            if (string.IsNullOrWhiteSpace(rel))
            {
                return null;
            }
            var full = System.IO.Path.GetFullPath(System.IO.Path.Combine(root, rel));
            var rootFull = System.IO.Path.GetFullPath(root);
            if (!full.StartsWith(rootFull.TrimEnd('\\', '/') + "\\", StringComparison.OrdinalIgnoreCase) &&
                !string.Equals(full, rootFull, StringComparison.OrdinalIgnoreCase))
            {
                return null;
            }
            return full;
        }

        /// <summary>计算文件 SHA-256（小写 hex）；文件不存在返回空串</summary>
        private static string ComputeFileSha256Hex(string filePath)
        {
            try
            {
                using var stream = File.OpenRead(filePath);
                using var sha = System.Security.Cryptography.SHA256.Create();
                var hash = sha.ComputeHash(stream);
                return Convert.ToHexString(hash).ToLowerInvariant();
            }
            catch
            {
                return "";
            }
        }

        private void SetState(string text, int pct, bool cancelEnabled, double speedMbPerSec = 0, long done = 0, long total = 0)
        {
            Dispatcher.Invoke(() =>
            {
                StatusText.Text = text;
                Progress.Value = pct;
                CancelButton.IsEnabled = cancelEnabled;
                if (total > 0)
                {
                    var doneMb = done / 1024.0 / 1024.0;
                    var totalMb = total / 1024.0 / 1024.0;
                    PercentText.Text = $"{pct}%  {doneMb:0.0}MB / {totalMb:0.0}MB    {speedMbPerSec:0.0}MB/s";
                }
                else
                {
                    PercentText.Text = speedMbPerSec > 0 ? $"{speedMbPerSec:0.0}MB/s" : "";
                }
            });
        }

        private void Fail(string message)
        {
            try
            {
                DeleteTemp();
            }
            catch { }
            Dispatcher.Invoke(() =>
            {
                MessageBox.Show(this, message, "更新失败", MessageBoxButton.OK, MessageBoxImage.Error);
                DialogResult = false;
            });
        }

        private void DeleteTemp()
        {
            if (_tempFile is not null && File.Exists(_tempFile))
            {
                File.Delete(_tempFile);
            }
        }

        private void CancelButton_Click(object sender, RoutedEventArgs e)
        {
            CancelButton.IsEnabled = false;
            StatusText.Text = "正在取消…";
            try
            {
                _cts.Cancel();
            }
            catch { }
        }

        protected override void OnClosed(EventArgs e)
        {
            DeleteTemp();
            _cts.Dispose();
            base.OnClosed(e);
        }
    }
}
