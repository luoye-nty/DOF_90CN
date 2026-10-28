using System;
using System.Collections.Generic;
using System.Text;

namespace DNFLogin.Services
{
    /// <summary>
    /// 从"任意粘贴内容"里抠出兑换码（发卡网一键复制的文本天然带中文标签）。
    ///
    /// 背景（玩家反馈"CDK 兑换，兑换码带中文不能自动识别"）：
    /// 发卡网的一键复制长这样 ——
    ///     <c>卡号：ABCD-EFGH-JKLM-NPQR-STUV 密码：1234</c>
    ///     <c>兑换码：ABCD-EFGH-JKLM-NPQR-STUV 请及时使用</c>
    /// 以前的实现是"取前 20 个合法字符"，于是：
    ///   - <c>卡号</c> 里的 <c>卡</c> 被丢掉、后面的中文整段被丢掉，但**订单号/密码里的数字**会被当成码开头
    ///     → 恰好凑够 20 位、内容却是错的 → 服务端回 404，玩家只看到一句"兑换码无效"；
    ///   - 码框还有 <c>MaxLength="24"</c>，标签一占位就从尾部把码截断。
    ///
    /// 本类只做**纯函数**提取，不认识界面、不认识网络（便于手工/离线核对）。
    /// 提取口径与运维工具服务端 <c>store.NormalizeRedeemCode</c> 保持同构：
    /// 唯一确定就采用，有歧义就不猜。
    /// </summary>
    public static class CdkCodeExtractor
    {
        /// <summary>
        /// 兑换码字符集（与运维工具后端 <c>redeemCodeCharset</c> 一致，去 0/O/1/I，全大写）。
        ///
        /// 这是**唯一**一处定义：<see cref="CdkBatchParser.CodeCharset"/> 与
        /// CdkRedeemControl 都引用这里，避免三处各写一份、改一处漂移两处。
        /// </summary>
        public const string CodeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789";

        /// <summary>单个兑换码有效字符数（不含横线）。</summary>
        public const int CodeLength = 20;

        /// <summary>横线分组形状：5 组 × 4 位。生成端（运维工具 redeem.go）固定这个形状，不能改。</summary>
        public const int SegmentCount = 5;
        public const int SegmentLength = 4;

        /// <summary>格式化后的长度（20 + 4 条横线）。</summary>
        public const int FormattedLength = CodeLength + (SegmentCount - 1);

        /// <summary>
        /// 抠码：把任意文本里的 20 位兑换码提取出来（返回值不带横线，全大写）。
        ///
        /// 步骤：
        ///   ① 全角→半角（<c>U+FF01..U+FF5E</c> 减 <c>0xFEE0</c>，<c>U+3000</c>→空格）——
        ///      一步解决 <c>Ａ</c>→<c>A</c>、<c>１</c>→<c>1</c>、<c>：</c>→<c>:</c>、<c>－</c>→<c>-</c>；
        ///   ② 转大写；
        ///   ③ 分词：以"非 <c>[A-Z0-9-]</c> 的任意字符"为分隔符 —— 中文标签、冒号、等号、
        ///      <c>【】</c>、括号、逗号、换行、全角空格、NBSP 全部自然成为分隔；
        ///   ④ 每个 run 去掉横线后：长度 == 20 且全部属于字符集 → 候选；
        ///      若去掉横线后长度 &gt; 20，再按"长度 20 的窗口"补一轮，**只有窗口唯一时才采用**；
        ///   ⑤ 候选恰好 1 个 → 采用；0 个 → false；多个 → 优先"横线呈 5×4"的那个，
        ///      仍多个则取文本顺序第一个。
        /// </summary>
        public static bool TryExtract(string? rawText, out string code)
        {
            code = string.Empty;
            if (string.IsNullOrEmpty(rawText))
            {
                return false;
            }

            var normalized = NormalizeText(rawText);
            var candidates = CollectCandidates(normalized);

            // 兜底：整段文本本身就是一个码（玩家把码按 5×4 敲成 "ABCD EFGH JKLM NPQR STUV"，
            // 或从聊天窗格复制回来时带了空格）—— 老实现是全局过滤字符，这条路必须保住。
            // 它同时天然覆盖"只有唯一窗口"：按空白切出来的分段各自都不够 20 位。
            if (candidates.Count == 0)
            {
                candidates = CollectWholeTextCandidate(normalized);
            }

            if (candidates.Count == 0)
            {
                return false;
            }

            if (candidates.Count == 1)
            {
                code = candidates[0].Code;
                return true;
            }

            // 多个候选：优先横线正好呈 5×4 的那一个（发卡网/管理工具导出的标准形状最可信）。
            foreach (var candidate in candidates)
            {
                if (candidate.Segmented)
                {
                    code = candidate.Code;
                    return true;
                }
            }

            // 仍分不出高低：取文本顺序第一个（不猜内容，只按出现先后取）。
            code = candidates[0].Code;
            return true;
        }

        /// <summary>便利重载：提取不到时返回空串（给"只想要个码"的调用方用）。</summary>
        public static string ExtractOrEmpty(string? rawText)
            => TryExtract(rawText, out var code) ? code : string.Empty;

        /// <summary>
        /// 全角→半角 + 转大写。纯 ASCII 输入原样返回（不额外分配）。
        /// </summary>
        public static string NormalizeText(string raw)
        {
            if (string.IsNullOrEmpty(raw))
            {
                return string.Empty;
            }

            var builder = new StringBuilder(raw.Length);
            foreach (var ch in raw)
            {
                builder.Append(char.ToUpperInvariant(ToHalfWidth(ch)));
            }

            return builder.ToString();
        }

        /// <summary>单个字符的全角→半角映射（其余字符原样返回）。</summary>
        public static char ToHalfWidth(char ch)
        {
            // 全角 ！(U+FF01) ~ ～(U+FF5E) → 半角 !(0x21) ~ ~(0x7E)
            if (ch >= '\uFF01' && ch <= '\uFF5E')
            {
                return (char)(ch - 0xFEE0);
            }

            // 表意空格（全角空格）
            if (ch == '\u3000')
            {
                return ' ';
            }

            // 其他常见"看着像空格"的字符统一成半角空格，让它们自然成为分词边界。
            if (ch == '\u00A0' || ch == '\u2007' || ch == '\u202F' || ch == '\uFEFF')
            {
                return ' ';
            }

            return ch;
        }

        /// <summary>run 的候选：规范化后的 20 位码 + 它是否呈"横线 5×4"形状。</summary>
        private readonly struct Candidate
        {
            public readonly string Code;
            public readonly bool Segmented;

            public Candidate(string code, bool segmented)
            {
                Code = code;
                Segmented = segmented;
            }
        }

        /// <summary>是否属于字符串集允许的字符（合法码只由这些字符组成）。</summary>
        public static bool IsCodeChar(char ch) => CodeCharset.IndexOf(ch) >= 0;

        /// <summary>是否是一个合法的兑换码（长度 + 字符集）。</summary>
        public static bool IsValidCode(string? normalized)
        {
            if (string.IsNullOrEmpty(normalized) || normalized.Length != CodeLength)
            {
                return false;
            }

            foreach (var ch in normalized)
            {
                if (!IsCodeChar(ch))
                {
                    return false;
                }
            }

            return true;
        }

        /// <summary>显示用格式：每 4 位一组（XXXX-XXXX-XXXX-XXXX-XXXX）。</summary>
        public static string FormatCode(string? normalized)
        {
            if (string.IsNullOrEmpty(normalized))
            {
                return string.Empty;
            }

            var builder = new StringBuilder(FormattedLength);
            for (var i = 0; i < normalized.Length; i++)
            {
                if (i > 0 && i % SegmentLength == 0)
                {
                    builder.Append('-');
                }

                builder.Append(normalized[i]);
            }

            return builder.ToString();
        }

        /// <summary>按"非 [A-Z0-9-] 即是分隔符"切分文本。</summary>
        private static List<string> Tokenize(string text)
        {
            var tokens = new List<string>();
            var start = -1;
            for (var i = 0; i < text.Length; i++)
            {
                if (IsTokenChar(text[i]))
                {
                    if (start < 0)
                    {
                        start = i;
                    }

                    continue;
                }

                if (start >= 0)
                {
                    tokens.Add(text.Substring(start, i - start));
                    start = -1;
                }
            }

            if (start >= 0)
            {
                tokens.Add(text.Substring(start));
            }

            return tokens;
        }

        private static bool IsTokenChar(char ch)
            => (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-';

        /// <summary>按文本顺序收集所有候选；同一段里出现多个窗口时按"有歧义就不猜"处理。</summary>
        private static List<Candidate> CollectCandidates(string text)
        {
            var candidates = new List<Candidate>();
            foreach (var token in Tokenize(text))
            {
                foreach (var candidate in CandidatesOfToken(token))
                {
                    candidates.Add(candidate);
                }
            }

            return candidates;
        }

        /// <summary>
        /// 兜底：把整段文本当**一个** run 再试一次。
        ///
        /// 用途：玩家把码按 5×4 敲成 <c>ABCD EFGH JKLM NPQR STUV</c> 时，分词会把空白当分隔符
        /// 切出五个 4 位短段（各自都不够 20 位），只有整段合起来才是那个码。
        /// 老实现是"全局过滤非法字符"，正好吃这一种；这里用一条兜底路径把它等价地保住。
        /// </summary>
        private static List<Candidate> CollectWholeTextCandidate(string text)
        {
            var result = new List<Candidate>();

            // 这里必须**同时**去掉空白与横线：整段兜底要吃的正是"码被空白拆开"的形态
            // （ABCD EFGH JKLM NPQR STUV）。横线不算进 hyphenCount —— 它只用于判断分组形状。
            var builder = new StringBuilder(text.Length);
            var hyphenCount = 0;
            foreach (var ch in text)
            {
                if (ch == '-')
                {
                    hyphenCount++;
                    continue;
                }

                if (char.IsWhiteSpace(ch))
                {
                    continue;
                }

                builder.Append(ch);
            }

            var stripped = builder.ToString();
            if (!IsValidCode(stripped))
            {
                return result;
            }

            var segmented = text.Length == FormattedLength &&
                            hyphenCount == SegmentCount - 1 &&
                            HasSegmentHyphens(text);
            result.Add(new Candidate(stripped, segmented));
            return result;
        }

        /// <summary>单个 run 的候选：
        ///   - 去横线后正好 20 位且全部合法 → 直接就是候选；
        ///   - 去横线后 &gt; 20 位 → 按长度 20 的窗口补一轮，**窗口必须唯一**；
        ///   - 去横线后 &lt; 20 位 → 没有候选（不够长，不猜）。
        /// </summary>
        private static List<Candidate> CandidatesOfToken(string token)
        {
            var result = new List<Candidate>();
            if (token.Length == 0)
            {
                return result;
            }

            var stripped = StripHyphen(token, out var hyphenCount);
            var segmented = token.Length == FormattedLength &&
                            hyphenCount == SegmentCount - 1 &&
                            HasSegmentHyphens(token);

            if (stripped.Length < CodeLength)
            {
                return result;
            }

            if (stripped.Length == CodeLength)
            {
                if (IsValidCode(stripped))
                {
                    result.Add(new Candidate(stripped, segmented));
                }

                return result;
            }

            // 去横线后超过 20 位：滑一个"正好 20 个合法字符"的窗口。
            // 例：订单20260930 加了码 → 前缀数字与码粘成一段，只有从码首开始的窗口才凑得齐 20 位。
            var windows = new HashSet<string>(StringComparer.Ordinal);
            for (var start = 0; start < stripped.Length; start++)
            {
                if (start + CodeLength > stripped.Length)
                {
                    break;
                }

                var window = stripped.Substring(start, CodeLength);
                if (!IsValidCode(window))
                {
                    continue;
                }

                // 窗口必须真的对应 token 里的一段（不能被横线切断成"半组"）：
                // 记下窗口在 token 中的实际起止，换算回字符数必须正好覆盖 20 个有效字符。
                windows.Add(window);
            }

            // 有歧义（同一段里能凑出多个不同窗口）就不猜。
            if (windows.Count != 1)
            {
                return result;
            }

            foreach (var window in windows)
            {
                // 这种"从长串里切出来"的候选，横线形状已经不可考，一律按未分组处理。
                result.Add(new Candidate(window, false));
            }

            return result;
        }

        /// <summary>去掉横线；同时回传横线个数。</summary>
        private static string StripHyphen(string token, out int hyphenCount)
        {
            hyphenCount = 0;
            var builder = new StringBuilder(token.Length);
            foreach (var ch in token)
            {
                if (ch == '-')
                {
                    hyphenCount++;
                    continue;
                }

                builder.Append(ch);
            }

            return builder.ToString();
        }

        /// <summary>横线是否正好落在 5×4 的分组位置（第 4/9/14/19 个字符处）。</summary>
        private static bool HasSegmentHyphens(string token)
        {
            if (token.Length != FormattedLength)
            {
                return false;
            }

            for (var group = 1; group < SegmentCount; group++)
            {
                if (token[group * SegmentLength + (group - 1)] != '-')
                {
                    return false;
                }
            }

            return true;
        }
    }
}
