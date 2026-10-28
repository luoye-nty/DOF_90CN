using System;
using System.Collections.Generic;
using System.Globalization;
using System.Text;

namespace DNFLogin.Pvf;

public enum TokenKind
{
    /// <summary>PVF 文本中的 `[section]` 段落标记。</summary>
    Section,
    /// <summary>反引号、单引号或双引号包裹的文本值。</summary>
    String,
    /// <summary>整数值。</summary>
    Int,
    /// <summary>浮点值。</summary>
    Float,
    /// <summary>未加引号的普通标识符。</summary>
    Ident,
    /// <summary>结构符号，例如 `{`、`}`、`=`。</summary>
    Symbol,
}

/// <summary>PVF 文本解析后的最小节点（照抄 Go Token）。</summary>
public sealed class Token
{
    public TokenKind Kind { get; set; }
    public string Raw { get; set; } = "";
    public string Value { get; set; } = "";
    public long Int { get; set; }
    public double Float { get; set; }
    public int Line { get; set; }
    public int Column { get; set; }
}

/// <summary>一个 `[section]` 段及其 token 范围（Start 含、End 不含）。</summary>
public sealed class Section
{
    public string Name { get; set; } = "";
    public int Start { get; set; }
    public int End { get; set; }
}

/// <summary>
/// 单个 PVF 文本文档的解析结果，提供 section 内按名称取值的能力。
/// 自 Go 版 vendor/longheng.io/server/internal/modules/dnf/pvf 照抄移植
/// （parse.go / types.go / value.go）。
/// </summary>
public sealed class DnfDocument
{
    public string Path { get; set; } = "";
    public List<Token> Tokens { get; } = new();
    public List<Section> Sections { get; } = new();

    /// <summary>返回指定段落内的 token 副本（照抄 Go Document.Section）。</summary>
    public List<Token>? GetSection(string name)
    {
        string key = SectionKey(name);
        foreach (var section in Sections)
        {
            if (SectionKey(section.Name) != key)
                continue;
            if (section.Start < 0 || section.End > Tokens.Count || section.Start > section.End)
                return null;
            var outTokens = new List<Token>(section.End - section.Start);
            for (int i = section.Start; i < section.End; i++)
                outTokens.Add(Tokens[i]);
            return outTokens;
        }
        return null;
    }

    /// <summary>返回指定 section 内所有文本和标识符值。</summary>
    public List<string> Texts(string name)
    {
        var tokens = GetSection(name);
        if (tokens == null)
            return new List<string>();
        var outList = new List<string>(tokens.Count);
        foreach (var token in tokens)
        {
            if (token.Kind == TokenKind.String || token.Kind == TokenKind.Ident)
                outList.Add(token.Value);
        }
        return outList;
    }

    /// <summary>返回指定 section 的第一个文本或标识符值。</summary>
    public string? Text(string name)
    {
        var values = Texts(name);
        return values.Count > 0 ? values[0] : null;
    }

    /// <summary>返回指定 section 内所有整数值。</summary>
    public List<long> Ints(string name)
    {
        var tokens = GetSection(name);
        if (tokens == null)
            return new List<long>();
        var outList = new List<long>(tokens.Count);
        foreach (var token in tokens)
        {
            if (token.Kind == TokenKind.Int)
                outList.Add(token.Int);
        }
        return outList;
    }

    /// <summary>返回指定 section 的第一个整数值。</summary>
    public long? Int(string name)
    {
        var values = Ints(name);
        return values.Count > 0 ? values[0] : null;
    }

    /// <summary>返回指定 section 内所有数字值（int 与 float 统一为 double）。</summary>
    public List<double> Numbers(string name)
    {
        var tokens = GetSection(name);
        if (tokens == null)
            return new List<double>();
        var outList = new List<double>(tokens.Count);
        foreach (var token in tokens)
        {
            switch (token.Kind)
            {
                case TokenKind.Int:
                    outList.Add(token.Int);
                    break;
                case TokenKind.Float:
                    outList.Add(token.Float);
                    break;
            }
        }
        return outList;
    }

    /// <summary>返回指定 section 的第一个数字值。</summary>
    public double? Number(string name)
    {
        var values = Numbers(name);
        return values.Count > 0 ? values[0] : null;
    }

    internal static string SectionKey(string value) => value.Trim().ToLowerInvariant();

    /// <summary>把单个 PVF 文本文档解析成通用 token 和 section（照抄 Go Parse）。</summary>
    public static DnfDocument Parse(string docPath, string text)
    {
        docPath = PvfArchive.NormalizePath(docPath);
        if (docPath.Length == 0)
            throw new PvfException("dnf pvf path is required");
        var parser = new DocParser { Doc = new DnfDocument { Path = docPath } };
        string[] lines = text.Split('\n');
        for (int idx = 0; idx < lines.Length; idx++)
            parser.ParseLine(idx + 1, lines[idx]);
        if (parser.Pending != null)
            throw new PvfException($"dnf pvf string at {parser.Pending.Line}:{parser.Pending.Column} is not closed");
        parser.CloseSection();
        return parser.Doc;
    }

    private sealed class PendingString
    {
        public char Quote;
        public int Line;
        public int Column;
        public readonly StringBuilder Raw = new();
        public readonly StringBuilder Value = new();

        public Token Token() => new()
        {
            Kind = TokenKind.String,
            Raw = Raw.ToString(),
            Value = Value.ToString(),
            Line = Line,
            Column = Column,
        };
    }

    private sealed class DocParser
    {
        public DnfDocument Doc = new();
        public PendingString? Pending;

        public void ParseLine(int lineNo, string line)
        {
            if (Pending != null)
            {
                ContinueString(lineNo, line);
                return;
            }
            string trimmed = line.Trim();
            if (trimmed.Length == 0 || trimmed.StartsWith("#") || trimmed.StartsWith("//"))
                return;
            if (trimmed.StartsWith("["))
            {
                int end = trimmed.IndexOf(']');
                if (end > 0)
                {
                    int column = line.IndexOf('[') + 1;
                    string name = trimmed.Substring(1, end - 1).Trim();
                    AddToken(new Token
                    {
                        Kind = TokenKind.Section,
                        Raw = trimmed.Substring(0, end + 1),
                        Value = name,
                        Line = lineNo,
                        Column = column,
                    });
                    ScanValues(lineNo, column + end + 1, trimmed.Substring(end + 1));
                    return;
                }
            }
            int col = line.Length - line.TrimStart().Length + 1;
            ScanValues(lineNo, col, trimmed);
        }

        private void AddToken(Token token)
        {
            if (token.Kind == TokenKind.Section)
            {
                CloseSection();
                Doc.Sections.Add(new Section { Name = token.Value, Start = Doc.Tokens.Count + 1 });
            }
            Doc.Tokens.Add(token);
        }

        public void CloseSection()
        {
            if (Doc.Sections.Count == 0)
                return;
            var section = Doc.Sections[^1];
            if (section.End == 0)
                section.End = Doc.Tokens.Count;
        }

        private void ScanValues(int lineNo, int baseColumn, string line)
        {
            for (int pos = 0; pos < line.Length;)
            {
                if (IsPvfSpace(line[pos]))
                {
                    pos++;
                    continue;
                }
                if (pos + 1 < line.Length && line[pos] == '/' && line[pos + 1] == '/')
                    return;
                if (line[pos] == '#')
                    return;
                int column = baseColumn + pos;
                switch (line[pos])
                {
                    case '`':
                    case '\'':
                    case '"':
                    {
                        (Token token, int next, bool complete) = BeginString(lineNo, column, line, pos);
                        if (!complete)
                            return;
                        AddToken(token);
                        pos = next;
                        break;
                    }
                    case '{':
                    case '}':
                    case '=':
                    case ',':
                    case ':':
                    {
                        string raw = line.Substring(pos, 1);
                        AddToken(new Token { Kind = TokenKind.Symbol, Raw = raw, Value = raw, Line = lineNo, Column = column });
                        pos++;
                        break;
                    }
                    default:
                    {
                        (string raw, int next) = ScanBare(line, pos);
                        if (raw.Length == 0)
                        {
                            pos++;
                            continue;
                        }
                        AddToken(ParseBare(lineNo, column, raw));
                        pos = next;
                        break;
                    }
                }
            }
        }

        private (Token, int, bool) BeginString(int lineNo, int column, string line, int start)
        {
            var pending = new PendingString { Quote = line[start], Line = lineNo, Column = column };
            pending.Raw.Append(line[start]);
            (int next, bool complete) = AppendStringSegment(pending, line, start + 1);
            if (!complete)
            {
                Pending = pending;
                return (null!, 0, false);
            }
            return (pending.Token(), next, true);
        }

        private void ContinueString(int lineNo, string line)
        {
            var pending = Pending!;
            pending.Raw.Append('\n');
            pending.Value.Append('\n');
            (int next, bool complete) = AppendStringSegment(pending, line, 0);
            if (!complete)
                return;
            Token token = pending.Token();
            Pending = null;
            AddToken(token);
            ScanValues(lineNo, next + 1, line.Substring(next));
        }

        private static (int, bool) AppendStringSegment(PendingString pending, string line, int start)
        {
            for (int pos = start; pos < line.Length; pos++)
            {
                char value = line[pos];
                pending.Raw.Append(value);
                if (value == pending.Quote)
                    return (pos + 1, true);
                if (pending.Quote != '`' && value == '\\' && pos + 1 < line.Length)
                {
                    pos++;
                    pending.Raw.Append(line[pos]);
                    pending.Value.Append(line[pos]);
                    continue;
                }
                pending.Value.Append(value);
            }
            return (line.Length, false);
        }

        private static (string, int) ScanBare(string line, int start)
        {
            int pos = start;
            while (pos < line.Length)
            {
                if (IsPvfSpace(line[pos]) || "{},:=".IndexOf(line[pos]) >= 0)
                    break;
                if (pos + 1 < line.Length && line[pos] == '/' && line[pos + 1] == '/')
                    break;
                pos++;
            }
            return (line.Substring(start, pos - start), pos);
        }

        private static Token ParseBare(int lineNo, int column, string raw)
        {
            if (TryParseInt(raw, out long intValue))
                return new Token { Kind = TokenKind.Int, Raw = raw, Value = raw, Int = intValue, Line = lineNo, Column = column };
            if (TryParseFloat(raw, out double floatValue))
                return new Token { Kind = TokenKind.Float, Raw = raw, Value = raw, Float = floatValue, Line = lineNo, Column = column };
            return new Token { Kind = TokenKind.Ident, Raw = raw, Value = raw, Line = lineNo, Column = column };
        }

        private static bool TryParseInt(string raw, out long value)
        {
            if (raw.IndexOfAny(new[] { '.', 'e', 'E' }) >= 0)
            {
                value = 0;
                return false;
            }
            return long.TryParse(raw, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out value);
        }

        private static bool TryParseFloat(string raw, out double value)
        {
            if (raw.IndexOfAny(new[] { '.', 'e', 'E' }) < 0)
            {
                value = 0;
                return false;
            }
            return double.TryParse(raw, NumberStyles.Float, CultureInfo.InvariantCulture, out value);
        }
    }

    private static bool IsPvfSpace(char value) => value == ' ' || value == '\t' || value == '\r';

    private static bool IsUnicodeSpace(char value) => char.IsWhiteSpace(value);
}
