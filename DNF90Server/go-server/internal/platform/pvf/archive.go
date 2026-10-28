package pvf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	headerSize     = 0x30
	fileItemSize   = 0x18
	groupItemSize  = 8
	magicSignature = 0x69706B6E
)

type ArchiveFormat string

const (
	// FormatNKPI 表示旧版 DNF PVF `nkpi` 归档格式。
	FormatNKPI ArchiveFormat = "nkpi"
	// FormatProtectedNKPI 表示 23.4.15.0 之后使用 UTF-16 seed 密钥流的 `nkpi` 归档格式。
	FormatProtectedNKPI ArchiveFormat = "protected_nkpi"
)

var (
	ErrInvalidArchive = errors.New("pvf archive is invalid")
	ErrFileNotFound   = errors.New("pvf file not found")
)

type ArchiveSnapshot struct {
	Format           ArchiveFormat `json:"format"`
	Path             string        `json:"path"`
	Size             int64         `json:"size"`
	Checksum         string        `json:"checksum"`
	LoadedAt         string        `json:"loaded_at"`
	FileCount        int           `json:"file_count"`
	GroupCount       int           `json:"group_count"`
	CachedChunks     int           `json:"cached_chunks"`
	CachedChunkBytes int64         `json:"cached_chunk_bytes"`
	CachedTexts      int           `json:"cached_texts"`
	CachedTextBytes  int64         `json:"cached_text_bytes"`
	CacheHits        uint64        `json:"cache_hits"`
	CacheMisses      uint64        `json:"cache_misses"`
	CacheEvictions   uint64        `json:"cache_evictions"`
	ChunkCacheLimit  int64         `json:"chunk_cache_limit"`
	TextCacheLimit   int64         `json:"text_cache_limit"`
}

type PreloadResult struct {
	Groups int `json:"groups"`
	Cached int `json:"cached"`
}

type File struct {
	Index       int    `json:"index"`
	Path        string `json:"path"`
	Name        string `json:"name"`
	ArchivePath string `json:"archive_path"`
	DataType    int    `json:"data_type"`
	Size        int    `json:"size"`
}

type Archive struct {
	snapshot Snapshot
	format   ArchiveFormat
	header   pvfHeader

	// data 是启动期读入的完整 PVF 字节，后续查询不再访问磁盘。
	data   []byte
	items  []fileItem
	groups []groupItem

	// pathIdx 保存归一化路径到文件表下标的映射，查询时避免扫描目录。
	pathIdx map[string]int
	// tableOff 是文件表在归档里的绝对偏移（parseNKPITables 里算出），
	// 供定点改写表项字段（如 dataType）使用。
	tableOff int
	bodyOff  int
	strA     []byte
	strW     []byte

	// chunks 缓存已解密解压的 body chunk，texts 缓存已解码的脚本文本。
	chunks       *byteLRU
	texts        *byteLRU
	chunkFlights indexFlight
	textFlights  indexFlight
}

func Open(path string) (*Archive, error) {
	return LoadArchive(Options{Path: path})
}

func OpenBytes(data []byte) (*Archive, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty pvf data", ErrInvalidArchive)
	}
	copied := append([]byte(nil), data...)
	sum := sha256.Sum256(copied)
	return OpenArchive(&Bundle{
		snapshot: Snapshot{
			Size:     int64(len(copied)),
			Checksum: hex.EncodeToString(sum[:]),
			LoadedAt: time.Now().UTC(),
		},
		data: copied,
	})
}

func LoadArchive(options Options) (*Archive, error) {
	bundle, err := Load(options)
	if err != nil {
		return nil, err
	}
	archive, err := OpenArchive(bundle)
	if err != nil {
		return nil, err
	}
	return archive, nil
}

func OpenArchive(bundle *Bundle) (*Archive, error) {
	if bundle == nil || len(bundle.data) == 0 {
		return nil, fmt.Errorf("%w: empty pvf data", ErrInvalidArchive)
	}
	chunkCacheBytes := bundle.chunkCacheBytes
	if chunkCacheBytes <= 0 {
		chunkCacheBytes = DefaultChunkCacheBytes
	}
	textCacheBytes := bundle.textCacheBytes
	if textCacheBytes <= 0 {
		textCacheBytes = DefaultTextCacheBytes
	}
	archive := &Archive{
		snapshot: bundle.snapshot,
		data:     bundle.data,
		pathIdx:  make(map[string]int),
		chunks:   newByteLRU(chunkCacheBytes),
		texts:    newByteLRU(textCacheBytes),
	}
	if err := archive.parse(); err != nil {
		return nil, err
	}
	return archive, nil
}

func (a *Archive) Snapshot() ArchiveSnapshot {
	if a == nil {
		return ArchiveSnapshot{}
	}
	chunkSnapshot := a.chunks.snapshot()
	textSnapshot := a.texts.snapshot()
	return ArchiveSnapshot{
		Format:           a.format,
		Path:             a.snapshot.Path,
		Size:             a.snapshot.Size,
		Checksum:         a.snapshot.Checksum,
		LoadedAt:         a.snapshot.LoadedAt.Format(rfc3339Nano),
		FileCount:        len(a.items),
		GroupCount:       len(a.groups),
		CachedChunks:     chunkSnapshot.entries,
		CachedChunkBytes: chunkSnapshot.bytes,
		CachedTexts:      textSnapshot.entries,
		CachedTextBytes:  textSnapshot.bytes,
		CacheHits:        chunkSnapshot.hits + textSnapshot.hits,
		CacheMisses:      chunkSnapshot.misses + textSnapshot.misses,
		CacheEvictions:   chunkSnapshot.evictions + textSnapshot.evictions,
		ChunkCacheLimit:  a.chunks.limit(),
		TextCacheLimit:   a.texts.limit(),
	}
}

// Format 返回当前 PVF 归档格式，供 smoke 报告和兼容排查使用。
func (a *Archive) Format() ArchiveFormat {
	if a == nil {
		return ""
	}
	return a.format
}

func (a *Archive) Files() []File {
	if a == nil || len(a.items) == 0 {
		return nil
	}
	out := make([]File, len(a.items))
	for idx := range a.items {
		out[idx] = a.fileAt(idx)
	}
	return out
}

// RangePaths visits the archive's normalized lookup paths without allocating a
// second million-entry file table. The archive is immutable after loading, so
// callers may safely use this method concurrently with reads.
func (a *Archive) RangePaths(yield func(string) bool) {
	if a == nil || yield == nil {
		return
	}
	for archivePath := range a.pathIdx {
		if !yield(archivePath) {
			return
		}
	}
}

func (a *Archive) FileCount() int {
	if a == nil {
		return 0
	}
	return len(a.items)
}

func (a *Archive) CanReadFileData() bool {
	if a == nil {
		return false
	}
	return a.format == FormatNKPI || a.format == FormatProtectedNKPI
}

func (a *Archive) FindFile(relativePath string) (File, bool) {
	if a == nil {
		return File{}, false
	}
	idx, ok := a.pathIdx[pathKey(relativePath)]
	if !ok {
		return File{}, false
	}
	return a.fileAt(idx), true
}

func (a *Archive) fileAt(idx int) File {
	if idx < 0 || idx >= len(a.items) {
		return File{}
	}
	item := a.items[idx]
	name := a.resolveString(item.nameOffset)
	dir := a.resolveString(item.pathOffset)
	return File{
		Index:       idx,
		Path:        dir,
		Name:        name,
		ArchivePath: joinArchivePath(dir, name),
		DataType:    item.dataType,
		Size:        item.dataSize,
	}
}

func (a *Archive) FindFileIndex(relativePath string) int {
	if a == nil {
		return -1
	}
	idx, ok := a.pathIdx[pathKey(relativePath)]
	if !ok {
		return -1
	}
	return idx
}

func (a *Archive) ReadText(relativePath string) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: archive is nil", ErrInvalidArchive)
	}
	idx, ok := a.pathIdx[pathKey(relativePath)]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrFileNotFound, relativePath)
	}
	return a.readTextIndex(idx)
}

func (a *Archive) ReadRaw(relativePath string) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: archive is nil", ErrInvalidArchive)
	}
	idx, ok := a.pathIdx[pathKey(relativePath)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, relativePath)
	}
	return a.readRawIndex(idx)
}

// ResolveString exposes one script token's string-pool value. PVF type-1
// scripts store labels and quoted values as encoded offsets into the archive's
// shared ANSI/UTF-16 pools; callers must keep the encoded offset unchanged
// unless they rebuild every referring script token.
func (a *Archive) ResolveString(magicOffset int) string {
	if a == nil {
		return ""
	}
	return a.resolveString(magicOffset)
}

// FindStringMagicOffsets returns one current-archive token offset for every
// requested shared-string value that is already present. It is intended for
// safe type-1 script transplantation: raw token offsets from another PVF are
// archive-local and must be resolved by value before reuse.
func (a *Archive) FindStringMagicOffsets(values []string) map[string]int {
	result := make(map[string]int)
	if a == nil || len(values) == 0 {
		return result
	}
	wanted := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			wanted[value] = struct{}{}
		}
	}
	for start := 0; start < len(a.strA) && len(result) < len(wanted); {
		end := bytes.IndexByte(a.strA[start:], 0)
		if end < 0 {
			break
		}
		if end > 0 {
			value := readUTF8String(a.strA, start)
			if _, requested := wanted[value]; requested {
				if _, found := result[value]; !found {
					result[value] = start << 1
				}
			}
		}
		start += end + 1
	}
	for start := 0; start+1 < len(a.strW) && len(result) < len(wanted); {
		end := start
		for end+1 < len(a.strW) && (a.strW[end] != 0 || a.strW[end+1] != 0) {
			end += 2
		}
		if end > start {
			value := readUTF16String(a.strW, start)
			if _, requested := wanted[value]; requested {
				if _, found := result[value]; !found {
					result[value] = ((start / 2) << 1) | 1
				}
			}
		}
		start = end + 2
	}
	return result
}

func (a *Archive) FileText(idx int) (string, error) {
	if a == nil {
		return "", fmt.Errorf("%w: archive is nil", ErrInvalidArchive)
	}
	return a.readTextIndex(idx)
}

func (a *Archive) FileRaw(idx int) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: archive is nil", ErrInvalidArchive)
	}
	return a.readRawIndex(idx)
}

func (a *Archive) PreloadAll() (PreloadResult, error) {
	if a == nil {
		return PreloadResult{}, fmt.Errorf("%w: archive is nil", ErrInvalidArchive)
	}
	result := PreloadResult{Groups: len(a.groups)}
	for idx := range a.groups {
		if _, err := a.chunk(idx); err != nil {
			return result, err
		}
		result.Cached++
	}
	return result, nil
}

func (a *Archive) Bytes() []byte {
	if a == nil {
		return nil
	}
	out := make([]byte, len(a.data))
	copy(out, a.data)
	return out
}

func (a *Archive) Reader() *bytes.Reader {
	if a == nil {
		return bytes.NewReader(nil)
	}
	return bytes.NewReader(a.data)
}
