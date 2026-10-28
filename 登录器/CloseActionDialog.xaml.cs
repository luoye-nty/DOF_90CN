using System.Windows;

namespace DNFLogin
{
    /// <summary>
    /// 首次点击右上角 X 的询问弹窗：选择关闭行为，可勾选"记住此选项，以后不再询问"。
    /// 仅负责展示与收集选择，是否写入 JSON 及后续动作由 LoginWindow 在 ShowDialog 后处理。
    /// </summary>
    public partial class CloseActionDialog : Window
    {
        public CloseActionDialog()
        {
            InitializeComponent();
        }

        /// <summary>是否勾选"记住此选项，以后不再询问"</summary>
        public bool Remember { get; private set; }

        /// <summary>用户选择：CloseAction 枚举的字符串（"Exit" / "MinimizeToTray"）</summary>
        public string SelectedAction { get; private set; } = "";

        private void ExitButton_Click(object sender, RoutedEventArgs e)
        {
            Remember = RememberBox.IsChecked == true;
            SelectedAction = CloseAction.Exit.ToString();
            DialogResult = true;
        }

        private void TrayButton_Click(object sender, RoutedEventArgs e)
        {
            Remember = RememberBox.IsChecked == true;
            SelectedAction = CloseAction.MinimizeToTray.ToString();
            DialogResult = true;
        }
    }
}
