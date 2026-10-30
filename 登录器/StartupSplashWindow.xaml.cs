using System.Windows;

namespace DNFLogin
{
    /// <summary>
    /// Small, per-pixel transparent startup window. Keeping this as a real WPF
    /// window avoids the opaque square produced by the native SplashScreen path.
    /// </summary>
    public partial class StartupSplashWindow : Window
    {
        public StartupSplashWindow()
        {
            InitializeComponent();
        }
    }
}
