using System.Windows;

namespace DNF90.Setup.Installer;

public partial class App : Application
{
    private void OnStartup(object sender, StartupEventArgs e)
    {
        var processPath = Environment.ProcessPath
            ?? throw new InvalidOperationException("无法定位安装程序文件。");
        var window = new MainWindow(processPath);
        MainWindow = window;
        window.Show();
    }
}
