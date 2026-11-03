using System.Windows;
using DNF90.Downloader.Shared;

namespace DNF90.DirectDownloader;

public partial class App : Application
{
    private void OnStartup(object sender, StartupEventArgs e)
    {
        try
        {
            var config = DownloaderConfigFooter.ReadCurrent();
            var window = new MainWindow(config);
            MainWindow = window;
            window.Show();
        }
        catch (Exception ex)
        {
            MessageBox.Show(
                "无法读取下载器配置，请使用下载器配置器重新生成。\n\n" + ex.Message,
                "下载器配置无效",
                MessageBoxButton.OK,
                MessageBoxImage.Error);
            Shutdown(1);
        }
    }
}
