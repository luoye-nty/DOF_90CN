using System.Windows;

namespace DNFLogin
{
    /// <summary>
    /// 通用深色确认弹窗：标题 + 消息 + 确认/取消，样式与登录器其它弹窗一致。
    /// 确认返回 true，取消返回 false。
    /// </summary>
    public partial class ConfirmDialog : Window
    {
        public ConfirmDialog(string title, string message, string confirmText = "确认", string cancelText = "取消")
        {
            InitializeComponent();
            Title = title;
            TitleText.Text = title;
            MessageText.Text = message;
            ConfirmButton.Content = confirmText;
            CancelButton.Content = cancelText;
        }

        private void ConfirmButton_Click(object sender, RoutedEventArgs e)
        {
            DialogResult = true;
        }

        private void CancelButton_Click(object sender, RoutedEventArgs e)
        {
            DialogResult = false;
        }
    }
}
