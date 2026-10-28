using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;

namespace DNFLogin.Services
{
    /// <summary>
    /// 背景图片目录服务：扫描支持的图片文件（自然排序）、文件名自然序比较。
    /// 登录器与设置页共用，保证"自定义图片目录"校验与轮播逻辑一致。
    /// </summary>
    public static class BackgroundService
    {
        /// <summary>支持的图片扩展名（gif 有意排除：暂不支持动图）</summary>
        public static readonly string[] SupportedExtensions =
        {
            ".jpg", ".jpeg", ".png", ".bmp", ".tif", ".tiff"
        };

        /// <summary>
        /// 扫描指定目录直接子文件中的支持图片，按文件名自然排序（1、2、10 而非 1、10、2）。
        /// 目录不存在 / 为空 / 无图片时返回空列表。
        /// </summary>
        public static List<string> CollectImageFiles(string folder)
        {
            var result = new List<string>();
            if (string.IsNullOrWhiteSpace(folder) || !Directory.Exists(folder))
            {
                return result;
            }

            try
            {
                foreach (var file in Directory.EnumerateFiles(folder))
                {
                    var ext = Path.GetExtension(file);
                    if (SupportedExtensions.Contains(ext, StringComparer.OrdinalIgnoreCase))
                    {
                        result.Add(file);
                    }
                }
            }
            catch
            {
                // 目录不可读时按空处理，调用方回退默认背景
                return new List<string>();
            }

            result.Sort((a, b) => CompareNatural(Path.GetFileName(a), Path.GetFileName(b)));
            return result;
        }

        /// <summary>
        /// 文件名自然序比较：数字段按数值比较（2 &lt; 10），其余按字符忽略大小写比较。
        /// </summary>
        public static int CompareNatural(string a, string b)
        {
            if (string.IsNullOrEmpty(a) && string.IsNullOrEmpty(b))
            {
                return 0;
            }
            if (string.IsNullOrEmpty(a))
            {
                return -1;
            }
            if (string.IsNullOrEmpty(b))
            {
                return 1;
            }

            int ia = 0, ib = 0;
            while (ia < a.Length && ib < b.Length)
            {
                char ca = a[ia], cb = b[ib];
                if (char.IsDigit(ca) && char.IsDigit(cb))
                {
                    // 提取数字段
                    int sa = ia, sb = ib;
                    while (ia < a.Length && char.IsDigit(a[ia])) ia++;
                    while (ib < b.Length && char.IsDigit(b[ib])) ib++;

                    var na = a[sa..ia].TrimStart('0');
                    var nb = b[sb..ib].TrimStart('0');
                    if (na.Length != nb.Length)
                    {
                        return na.Length.CompareTo(nb.Length);
                    }
                    var cmp = string.CompareOrdinal(na, nb);
                    if (cmp != 0)
                    {
                        return cmp;
                    }
                }
                else
                {
                    var cmp = char.ToUpperInvariant(ca).CompareTo(char.ToUpperInvariant(cb));
                    if (cmp != 0)
                    {
                        return cmp;
                    }
                    ia++;
                    ib++;
                }
            }

            return (a.Length - ia).CompareTo(b.Length - ib);
        }
    }
}
