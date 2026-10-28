using System;
using System.Collections.Generic;
using System.Text;

namespace DNFLogin.Services
{
    /// <summary>
    /// CDK 批量兑换的文本解析（导入外部 txt：**一行一个 20 位兑换码**）。
    ///
    /// 口径（2026-10 更新：兼容发卡网/管理工具导出的"带中文标签"的整段文本）：
    ///   - 每行先交给 <see cref="CdkCodeExtractor.TryExtract"/> 抠码：一行可以有**多个**候选
    ///     （发卡网导出常把「卡号 + 密码」放在同一行），每个候选都收；
    ///   - 抠不出来时按老口径兜底：逐段（空白/制表符/全角空格分隔）做校验，
    ///     管理工具导出的 <c>码 备注 状态</c> 一行照样能吃；
    ///   - 带不带横线都吃（兑换码本身 20 位，界面显示时每 4 位一组）；
    ///   - 忽略空行、前后空白、UTF-8 BOM、CRLF；
    ///   - 文件内重复的码只兑换一次（重复的那几个计入"重复跳过"，避免白白浪费一次请求）；
    ///   - 超过上限的部分不处理并如实报数（不静默截断）。
    /// </summary>
    public static class CdkBatchParser
    {
        /// <summary>兑换码字符集（唯一出处见 <see cref="CdkCodeExtractor.CodeCharset"/>，避免两处漂移）。</summary>
        public const string CodeCharset = CdkCodeExtractor.CodeCharset;

        /// <summary>单个兑换码长度（不含横线）。</summary>
        public const int CodeLength = CdkCodeExtractor.CodeLength;

        /// <summary>解析结果。</summary>
        public sealed class ParseResult
        {
            /// <summary>待兑换的码（已去横线、大写、去重，按文件顺序）。</summary>
            public List<string> Codes { get; } = new List<string>();
            /// <summary>无效行数（长度不对/含非法字符）。</summary>
            public int InvalidLines { get; set; }
            /// <summary>重复码个数（文件内重复出现、已跳过）。</summary>
            public int DuplicateCodes { get; set; }
            /// <summary>因超过上限未处理的个数。</summary>
            public int OverLimitCodes { get; set; }
        }

        /// <summary>
        /// 解析文本行。
        ///
        /// 每行两条路，先抠码、再兜底：
        ///   ① <see cref="CdkCodeExtractor.TryExtract"/>：能吃下发卡网那种
        ///      <c>卡号：XXXX-XXXX-XXXX-XXXX-XXXX 密码：1234</c>（一行可能抠出多个候选）；
        ///   ② 抠不出来时按空白切段逐个校验：管理工具导出的
        ///      <c>4EAL-AB9M-SB5A-SPMD-TVTX 测试批量cdk 未兑换</c> 后面跟的名称/状态是给人看的备注，
        ///      校验不过自然被丢掉。整行就是一个码的情况当然也照样认。
        /// </summary>
        public static ParseResult Parse(IEnumerable<string> lines, int maxCodes)
        {
            var result = new ParseResult();
            if (lines == null)
            {
                return result;
            }
            if (maxCodes <= 0)
            {
                maxCodes = 500;
            }

            var seen = new HashSet<string>(StringComparer.Ordinal);
            foreach (var rawLine in lines)
            {
                var line = (rawLine ?? "").Trim().TrimStart('\uFEFF').Trim();
                if (line.Length == 0)
                {
                    continue;
                }

                var found = ExtractLineCodes(line);
                if (found.Count == 0)
                {
                    result.InvalidLines++;
                    continue;
                }

                foreach (var code in found)
                {
                    if (!seen.Add(code))
                    {
                        result.DuplicateCodes++;
                        continue;
                    }
                    if (result.Codes.Count >= maxCodes)
                    {
                        result.OverLimitCodes++;
                        continue;
                    }
                    result.Codes.Add(code);
                }
            }
            return result;
        }

        /// <summary>
        /// 一行里所有可用的兑换码（按出现顺序、已去重）：
        /// 先整行抠码；抠不出来再按空白切段逐个校验（兼容"码 + 中文备注"的老导出格式）。
        /// </summary>
        private static List<string> ExtractLineCodes(string line)
        {
            var codes = new List<string>();
            var seenInLine = new HashSet<string>(StringComparer.Ordinal);

            // ① 整行抠码：一行里所有 run 的候选都要（发卡网导出会是"卡号 + 密码"同一行）。
            foreach (var token in SplitRuns(line))
            {
                AddCandidates(codes, seenInLine, token);
            }

            if (codes.Count > 0)
            {
                return codes;
            }

            // ② 兜底：整行抠不出来时，按空白/制表符/全角空格切段，逐段当码校验。
            //    这条只为兼容"码 + 中文备注"的老导出格式，不做任何猜测。
            foreach (var segment in SplitByWhitespace(line))
            {
                var normalized = Normalize(segment);
                if (IsValidCode(normalized) && seenInLine.Add(normalized))
                {
                    codes.Add(normalized);
                }
            }

            return codes;
        }

        /// <summary>把一段文本交给 extractor，收回它抠出来的所有候选（含"长串里唯一 20 位窗口"那一类）。</summary>
        private static void AddCandidates(List<string> codes, HashSet<string> seen, string token)
        {
            if (string.IsNullOrEmpty(token))
            {
                return;
            }

            // extractor 的单一入口是"整段文本"；一个 run 就是一个天然的分词单元，
            // 直接喂给它等价于喂整行（TryExtract 内部同一套分词），这里逐段喂是为了
            // 一行多码时按出现顺序收齐。
            if (!CdkCodeExtractor.TryExtract(token, out var code))
            {
                return;
            }

            if (IsValidCode(code) && seen.Add(code))
            {
                codes.Add(code);
            }
        }

        /// <summary>按"非 [A-Z0-9-] 即分隔"切分（与 extractor 的分词口径一致）。</summary>
        private static List<string> SplitRuns(string line)
        {
            var normalized = CdkCodeExtractor.NormalizeText(line);
            var runs = new List<string>();
            var start = -1;
            for (var i = 0; i < normalized.Length; i++)
            {
                var ch = normalized[i];
                var isRunChar = (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-';
                if (isRunChar)
                {
                    if (start < 0)
                    {
                        start = i;
                    }
                    continue;
                }

                if (start >= 0)
                {
                    runs.Add(normalized.Substring(start, i - start));
                    start = -1;
                }
            }

            if (start >= 0)
            {
                runs.Add(normalized.Substring(start));
            }

            return runs;
        }

        /// <summary>按半角/全角空白切分整行（兜底路径用）。</summary>
        private static List<string> SplitByWhitespace(string line)
        {
            var segments = new List<string>();
            var builder = new StringBuilder();
            foreach (var ch in line)
            {
                if (ch == ' ' || ch == '\t' || ch == '\u3000' || ch == '\u00A0' || ch == '\uFEFF')
                {
                    if (builder.Length > 0)
                    {
                        segments.Add(builder.ToString());
                        builder.Clear();
                    }
                    continue;
                }

                builder.Append(ch);
            }

            if (builder.Length > 0)
            {
                segments.Add(builder.ToString());
            }

            return segments;
        }

        /// <summary>取一行里的第一段（按半角/全角空白切分）。</summary>
        public static string FirstToken(string line)
        {
            if (string.IsNullOrEmpty(line))
            {
                return "";
            }
            var cut = line.Length;
            for (var i = 0; i < line.Length; i++)
            {
                var ch = line[i];
                if (ch == ' ' || ch == '\t' || ch == '\u3000' || ch == '\u00A0')
                {
                    cut = i;
                    break;
                }
            }
            return line.Substring(0, cut);
        }

        /// <summary>去掉横线与空白并转大写（先做全角→半角，全角码/全角横线/全角空格都能吃）。</summary>
        public static string Normalize(string raw)
        {
            if (string.IsNullOrEmpty(raw))
            {
                return "";
            }
            var builder = new StringBuilder(raw.Length);
            foreach (var ch in CdkCodeExtractor.NormalizeText(raw))
            {
                if (ch == '-' || ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n')
                {
                    continue;
                }
                builder.Append(ch);
            }
            return builder.ToString();
        }

        /// <summary>是否是一个合法的兑换码（长度 + 字符集；校验而非提取）。</summary>
        public static bool IsValidCode(string normalized) => CdkCodeExtractor.IsValidCode(normalized);

        /// <summary>显示用格式：每 4 位一组（XXXX-XXXX-XXXX-XXXX-XXXX）。</summary>
        public static string FormatCode(string normalized) => CdkCodeExtractor.FormatCode(normalized);
    }
}
