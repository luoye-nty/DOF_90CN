using System;
using System.Collections.Generic;
using System.Linq;

namespace DNFLogin.Pvf;

/// <summary>`.lst` 中的 id 到 PVF 路径引用（照抄 Go ListEntry）。</summary>
public sealed class ListEntry
{
    public long ID { get; set; }
    public string Path { get; set; } = "";
}

/// <summary>可以按路径读取 PVF 文本的内存来源（对应 Go Source 接口）。</summary>
public interface IPvfSource
{
    string ReadText(string relativePath);
    List<PvfFile> Files();
}

/// <summary>控制 DNF PVF 内存索引的预加载范围（对应 Go BuildOptions）。</summary>
public sealed class BuildOptions
{
    public bool All { get; set; }
    public List<string> Paths { get; set; } = new();
    public List<string> Lists { get; set; } = new();
    public List<string> Prefixes { get; set; } = new();
}

/// <summary>
/// DNF PVF 文档与 `.lst` 引用索引。从已加载的 PVF source 中按路径解析文档，
/// 并对指定 `.lst` 建立 id → PVF 路径引用表。
/// 自 Go 版 vendor/longheng.io/server/internal/modules/dnf/pvf 照抄移植
/// （list.go / index.go）。
/// </summary>
public sealed class DnfPvfIndex
{
    private readonly Dictionary<string, DnfDocument> _docs = new();
    private readonly Dictionary<string, List<ListEntry>> _lists = new();
    private readonly Dictionary<string, Dictionary<long, string>> _refs = new();
    private readonly List<string> _paths = new();

    /// <summary>按路径返回已解析文档，不会重新读取 PVF。</summary>
    public DnfDocument? Document(string docPath)
    {
        _docs.TryGetValue(PvfArchive.PathKey(docPath), out var doc);
        return doc;
    }

    /// <summary>当前索引中的文档路径副本。</summary>
    public List<string> Paths()
    {
        return new List<string>(_paths);
    }

    /// <summary>返回指定 `.lst` 文档的引用列表副本。</summary>
    public List<ListEntry>? List(string listPath)
    {
        _lists.TryGetValue(PvfArchive.PathKey(listPath), out var entries);
        return entries == null ? null : new List<ListEntry>(entries);
    }

    /// <summary>从指定 `.lst` 索引中按 id 查找 PVF 路径。</summary>
    public string? Resolve(string listPath, long id)
    {
        _refs.TryGetValue(PvfArchive.PathKey(listPath), out var refs);
        if (refs == null)
            return null;
        return refs.TryGetValue(id, out string? value) ? value : null;
    }

    /// <summary>当前内存索引的文档、列表和引用数量。</summary>
    public (int Documents, int Lists, int Refs) Snapshot()
    {
        int refs = 0;
        foreach (var entries in _lists.Values)
            refs += entries.Count;
        return (_docs.Count, _lists.Count, refs);
    }

    /// <summary>从 PVF source 构建项目层内存索引（对应 Go Build）。</summary>
    public static DnfPvfIndex Build(IPvfSource source, BuildOptions options)
    {
        if (source == null)
            throw new PvfException("dnf pvf source is required");
        options ??= new BuildOptions();

        var sourcePaths = SourcePathSet(source);
        var paths = CollectPaths(source, options, sourcePaths);
        var listKeys = ListKeySet(options.Lists);
        var listPaths = new List<string>(options.Lists);
        var index = new DnfPvfIndex();
        var docs = index._docs;
        var pathList = index._paths;
        // pendingKeys 与 paths 一一对应，避免 AddPath 线性扫描导致 O(n²)。
        var pendingKeys = new HashSet<string>();

        for (int pos = 0; pos < paths.Count; pos++)
        {
            string docPath = paths[pos];
            string text = source.ReadText(docPath);
            var doc = DnfDocument.Parse(docPath, text);
            string key = PvfArchive.PathKey(docPath);
            docs[key] = doc;
            pathList.Add(doc.Path);
            if (listKeys.Contains(key))
            {
                foreach (var entry in ParseList(doc))
                {
                    string resolved = ResolveListRef(doc.Path, entry.Path, sourcePaths);
                    AddPath(paths, docs, pendingKeys, resolved);
                    if (resolved.EndsWith(".lst", StringComparison.OrdinalIgnoreCase))
                    {
                        string resolvedKey = PvfArchive.PathKey(resolved);
                        if (!listKeys.Contains(resolvedKey))
                        {
                            listKeys.Add(resolvedKey);
                            listPaths.Add(resolved);
                        }
                    }
                }
            }
        }
        pathList.Sort(StringComparer.Ordinal);
        index.BuildLists(listPaths, sourcePaths);
        return index;
    }

    private void BuildLists(List<string> lists, Dictionary<string, string> sourcePaths)
    {
        foreach (string listPath in lists)
        {
            string key = PvfArchive.PathKey(listPath);
            if (!_docs.TryGetValue(key, out var doc))
                continue;
            var entries = ParseList(doc);
            for (int i = 0; i < entries.Count; i++)
                entries[i].Path = ResolveListRef(doc.Path, entries[i].Path, sourcePaths);
            _lists[key] = entries;
            var refMap = new Dictionary<long, string>(entries.Count);
            foreach (var entry in entries)
            {
                if (!refMap.ContainsKey(entry.ID))
                    refMap[entry.ID] = entry.Path;
            }
            _refs[key] = refMap;
        }
    }

    /// <summary>从 `.lst` 文档中提取 id 到 PVF 路径的引用（照抄 Go ParseList）。</summary>
    public static List<ListEntry> ParseList(DnfDocument doc)
    {
        if (doc == null || doc.Tokens.Count == 0)
            return new List<ListEntry>();
        var entries = new List<ListEntry>();
        var tokens = doc.Tokens;
        for (int idx = 0; idx + 1 < tokens.Count; idx++)
        {
            var id = tokens[idx];
            if (id.Kind != TokenKind.Int)
                continue;
            var refToken = tokens[idx + 1];
            if (refToken.Kind != TokenKind.String && refToken.Kind != TokenKind.Ident)
                continue;
            string refPath = PvfArchive.NormalizePath(refToken.Value);
            if (refPath.Length == 0)
                continue;
            entries.Add(new ListEntry { ID = id.Int, Path = refPath });
            idx++;
        }
        return entries;
    }

    // ---------- index.go 内部工具 ----------

    private static Dictionary<string, string> SourcePathSet(IPvfSource source)
    {
        var files = source.Files();
        if (files == null || files.Count == 0)
            return new Dictionary<string, string>();
        var outMap = new Dictionary<string, string>(files.Count);
        foreach (var file in files)
        {
            string clean = PvfArchive.NormalizePath(file.ArchivePath);
            string key = PvfArchive.PathKey(clean);
            if (key.Length == 0)
                continue;
            if (!outMap.ContainsKey(key))
                outMap[key] = clean;
        }
        return outMap;
    }

    private static List<string> CollectPaths(IPvfSource source, BuildOptions options, Dictionary<string, string> sourcePaths)
    {
        var pathSet = new HashSet<string>();
        var paths = new List<string>(options.Paths.Count + options.Lists.Count);
        void AddPathValue(string value)
        {
            string clean = PvfArchive.NormalizePath(value);
            if (clean.Length == 0)
                return;
            string key = PvfArchive.PathKey(clean);
            if (!pathSet.Add(key))
                return;
            if (sourcePaths.TryGetValue(key, out string? actual))
                paths.Add(actual!);
            else
                paths.Add(clean);
        }

        foreach (var value in options.Paths)
            AddPathValue(value);
        foreach (var value in options.Lists)
            AddPathValue(value);

        var files = source.Files();
        if (files != null && (options.All || options.Prefixes.Count > 0))
        {
            foreach (var file in files)
            {
                if (options.All || MatchPrefix(file.ArchivePath, options.Prefixes))
                    AddPathValue(file.ArchivePath);
            }
        }
        paths.Sort(StringComparer.Ordinal);
        return paths;
    }

    private static string ResolveListRef(string listPath, string refPath, Dictionary<string, string> sourcePaths)
    {
        string refClean = PvfArchive.NormalizePath(refPath);
        if (refClean.Length == 0)
            return "";
        if (sourcePaths.TryGetValue(PvfArchive.PathKey(refClean), out string? actual))
            return actual!;
        string listDir = PvfArchive.CleanPath(System.IO.Path.GetDirectoryName(listPath.Replace('/', '\\')) ?? ".")
            .Replace('\\', '/');
        if (listDir == ".")
            listDir = "";
        if (listDir.Length != 0)
        {
            string candidate = PvfArchive.NormalizePath(listDir + "/" + refClean);
            if (sourcePaths.TryGetValue(PvfArchive.PathKey(candidate), out actual))
                return actual!;
            string listDirKey = PvfArchive.PathKey(listDir);
            string refKey = PvfArchive.PathKey(refClean);
            if (listDirKey.Length != 0 && refKey.StartsWith(listDirKey + "/", StringComparison.Ordinal))
                return refClean;
            return candidate;
        }
        return refClean;
    }

    private static void AddPath(List<string> paths, Dictionary<string, DnfDocument> docs, HashSet<string> pendingKeys, string value)
    {
        string clean = PvfArchive.NormalizePath(value);
        if (clean.Length == 0)
            return;
        string key = PvfArchive.PathKey(clean);
        if (docs.ContainsKey(key))
            return;
        if (!pendingKeys.Add(key))
            return;
        paths.Add(clean);
    }

    private static HashSet<string> ListKeySet(List<string> lists)
    {
        var outSet = new HashSet<string>(lists.Count);
        foreach (var value in lists)
        {
            string key = PvfArchive.PathKey(value);
            if (key.Length != 0)
                outSet.Add(key);
        }
        return outSet;
    }

    private static bool MatchPrefix(string filePath, List<string> prefixes)
    {
        if (prefixes == null || prefixes.Count == 0)
            return false;
        string key = PvfArchive.PathKey(filePath);
        foreach (var prefix in prefixes)
        {
            string prefixKey = PvfArchive.PathKey(prefix);
            if (prefixKey.Length == 0)
                continue;
            if (key == prefixKey || key.StartsWith(prefixKey + "/", StringComparison.Ordinal))
                return true;
        }
        return false;
    }
}
