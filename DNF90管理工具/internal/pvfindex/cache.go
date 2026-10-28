package pvfindex

// DFIX 落盘缓存索引（设计 §6.2/6.3，两端独立编译、格式一致）。
// 只存搜索/展示所需的 SearchView 全量（约 11 字段）；逐物品发放运行时字段
// （强化/增幅/有效期/耐久等）不落盘——grant 时由服务端对单物品 Resolve 补齐。
// 指纹 = 源 PVF 大小 + mtime UnixNano，与 GM directCatalogCacheEntry 语义对齐：
// 换 PVF（升级 Script.pvf）自动重建，索引不跨 PVF 指纹复用。
//
// 文件布局（v1，未启用 gzip）：
//
//	[magic 4B "DFIX"] [version u16=1] [flags u8] [entryCount u32]
//	[bodyCRC32 u32] [bodyLen u64]
//	[指纹头] src_pvf_path(u16 len + UTF-8) + src_size(i64) + src_modtime_unixnano(i64)
//	[body] 每条：kind(u16len+str) item_id(u32) name(u16len+str) category(u16len+str)
//	       pvf_path(u16len+str) stackable_type(u16len+str) equipment_type(u16len+str)
//	       stack_limit(i64) list_type(u8) destination(u16len+str) supports_upgrade(u8)
//
// 写入原子性：先写 <name>.idx.tmp 再 rename 为 <name>.idx；读取时做 magic/版本/CRC32 校验，
// 损坏即视为未命中（走全量构建并重建）。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
)

const (
	dfixMagicA   = 0x44 // D
	dfixMagicB   = 0x46 // F
	dfixMagicC   = 0x49 // I
	dfixMagicD   = 0x58 // X
	dfixVersion  = 1
	dfixFlagGzip = 1 << 0
)

var (
	cacheDirMu      sync.RWMutex
	cacheDirOverride string
)

// SetCacheDir 指定 DFIX 索引缓存目录（两端在工具初始化时调用一次；
// 未设置时回退到源 PVF 同目录生成 <Script.pvf>.idx）。
func SetCacheDir(dir string) {
	cacheDirMu.Lock()
	defer cacheDirMu.Unlock()
	cacheDirOverride = stringsTrimRightSlash(dir)
}

func getCacheDir() string {
	cacheDirMu.RLock()
	defer cacheDirMu.RUnlock()
	return cacheDirOverride
}

func stringsTrimRightSlash(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '/' || value[len(value)-1] == '\\') {
		value = value[:len(value)-1]
	}
	return value
}

// dfixCachePath 返回缓存索引文件路径：优先工具配置目录 <dir>/<pvf-basename>.idx，
// 未配置则源 PVF 同目录 <pvf>.idx。
func dfixCachePath(pvfAbs string) (string, error) {
	dir := getCacheDir()
	base := filepath.Base(pvfAbs)
	if dir == "" {
		return pvfAbs + ".idx", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建 PVF 索引缓存目录: %w", err)
	}
	return filepath.Join(dir, base+".idx"), nil
}

// tryLoadDiskIndex 尝试载入 DFIX 缓存：指纹匹配且 CRC32 通过 → (items, true, nil)；
// 不存在/指纹不符/损坏 → (nil, false, nil)（调用方走全量构建）；文件级错误如实返回。
func (c *Catalog) tryLoadDiskIndex() ([]SearchView, bool, error) {
	path, err := dfixCachePath(c.srcPath)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取 PVF 索引缓存: %w", err)
	}
	items, err := parseDFIX(data, c.srcPath, c.srcSize, c.srcMod)
	if err != nil {
		// 损坏/指纹不符：旧缓存不可用，调用方重建（重建入口会覆盖本文件）。
		return nil, false, nil
	}
	return items, true, nil
}

// saveDiskIndex 将全量搜索索引写入 DFIX 缓存（尽力而为；失败不影响内存索引使用）。
func (c *Catalog) saveDiskIndex(items []SearchView) error {
	path, err := dfixCachePath(c.srcPath)
	if err != nil {
		return err
	}
	data := marshalDFIX(c.srcPath, c.srcSize, c.srcMod, items)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("写入 PVF 索引缓存临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("PVF 索引缓存原子落盘失败: %w", err)
	}
	return nil
}

// ---- 序列化 ----

type dfixWriter struct {
	buf []byte
}

func (w *dfixWriter) u8(v uint8)   { w.buf = append(w.buf, v) }
func (w *dfixWriter) u16(v uint16) { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *dfixWriter) u32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *dfixWriter) u64(v uint64) { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *dfixWriter) i64(v int64)  { w.buf = binary.LittleEndian.AppendUint64(w.buf, uint64(v)) }
func (w *dfixWriter) str(v string) {
	if len(v) > 0xFFFF {
		v = v[:0xFFFF]
	}
	w.u16(uint16(len(v)))
	w.buf = append(w.buf, v...)
}

func marshalDFIX(srcPath string, srcSize, srcMod int64, items []SearchView) []byte {
	body := &dfixWriter{}
	for _, item := range items {
		body.str(item.Kind)
		body.u32(item.ItemID)
		body.str(item.Name)
		body.str(item.Category)
		body.str(item.PVFPath)
		body.str(item.StackableType)
		body.str(item.EquipmentType)
		body.i64(item.StackLimit)
		body.u8(item.ListType)
		body.str(item.Destination)
		if item.SupportsUpgrade {
			body.u8(1)
		} else {
			body.u8(0)
		}
	}
	header := &dfixWriter{}
	header.str(srcPath)
	header.i64(srcSize)
	header.i64(srcMod)

	out := &dfixWriter{}
	out.u8(dfixMagicA)
	out.u8(dfixMagicB)
	out.u8(dfixMagicC)
	out.u8(dfixMagicD)
	out.u16(dfixVersion)
	out.u8(0) // flags：未启用 gzip
	out.u32(uint32(len(items)))
	out.u32(crc32.ChecksumIEEE(body.buf))
	out.u64(uint64(len(body.buf)))
	out.buf = append(out.buf, header.buf...)
	out.buf = append(out.buf, body.buf...)
	return out.buf
}

func parseDFIX(data []byte, srcPath string, srcSize, srcMod int64) ([]SearchView, error) {
	if len(data) < 4+2+1+4+4+8 {
		return nil, errors.New("DFIX 文件过短")
	}
	offset := 0
	readByte := func() (byte, error) {
		if offset >= len(data) {
			return 0, errors.New("DFIX 越界")
		}
		v := data[offset]
		offset++
		return v, nil
	}
	readN := func(n int) ([]byte, error) {
		if offset+n > len(data) {
			return nil, errors.New("DFIX 越界")
		}
		v := data[offset : offset+n]
		offset += n
		return v, nil
	}
	readU16 := func() (uint16, error) {
		v, err := readN(2)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint16(v), nil
	}
	readU32 := func() (uint32, error) {
		v, err := readN(4)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(v), nil
	}
	readU64 := func() (uint64, error) {
		v, err := readN(8)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(v), nil
	}
	readStr := func() (string, error) {
		n, err := readU16()
		if err != nil {
			return "", err
		}
		raw, err := readN(int(n))
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}

	if data[0] != dfixMagicA || data[1] != dfixMagicB || data[2] != dfixMagicC || data[3] != dfixMagicD {
		return nil, errors.New("DFIX 魔数不符")
	}
	offset = 4
	versionRaw, err := readU16()
	if err != nil || versionRaw != dfixVersion {
		return nil, errors.New("DFIX 版本不符")
	}
	flags, err := readByte()
	if err != nil || flags&dfixFlagGzip != 0 {
		return nil, errors.New("DFIX flags 不支持")
	}
	entryCount, err := readU32()
	if err != nil {
		return nil, err
	}
	if entryCount > 10_000_000 {
		return nil, errors.New("DFIX 条目数异常")
	}
	bodyCRC, err := readU32()
	if err != nil {
		return nil, err
	}
	bodyLen, err := readU64()
	if err != nil || bodyLen > uint64(len(data)) {
		return nil, errors.New("DFIX 长度异常")
	}

	// 指纹头：src_pvf_path + src_size + src_modtime
	fpPath, err := readStr()
	if err != nil {
		return nil, err
	}
	srcSizeRaw, err := readN(8)
	if err != nil {
		return nil, err
	}
	srcModRaw, err := readN(8)
	if err != nil {
		return nil, err
	}
	if fpPath != srcPath || int64(binary.LittleEndian.Uint64(srcSizeRaw)) != srcSize ||
		int64(binary.LittleEndian.Uint64(srcModRaw)) != srcMod {
		return nil, errors.New("DFIX 源指纹不匹配")
	}
	bodyStart := offset
	bodyEnd := bodyStart + int(bodyLen)
	if bodyEnd > len(data) {
		return nil, errors.New("DFIX body 越界")
	}
	body := data[bodyStart:bodyEnd]
	if crc32.ChecksumIEEE(body) != bodyCRC {
		return nil, errors.New("DFIX body CRC 校验失败")
	}

	items := make([]SearchView, 0, entryCount)
	rest := body
	parseStr := func() (string, error) {
		if len(rest) < 2 {
			return "", errors.New("DFIX body 越界")
		}
		n := int(binary.LittleEndian.Uint16(rest))
		rest = rest[2:]
		if n > len(rest) {
			return "", errors.New("DFIX body 越界")
		}
		s := string(rest[:n])
		rest = rest[n:]
		return s, nil
	}
	for index := uint32(0); index < entryCount; index++ {
		var item SearchView
		var err error
		if item.Kind, err = parseStr(); err != nil {
			return nil, err
		}
		if len(rest) < 4 {
			return nil, errors.New("DFIX body 越界")
		}
		item.ItemID = binary.LittleEndian.Uint32(rest)
		rest = rest[4:]
		if item.Name, err = parseStr(); err != nil {
			return nil, err
		}
		if item.Category, err = parseStr(); err != nil {
			return nil, err
		}
		if item.PVFPath, err = parseStr(); err != nil {
			return nil, err
		}
		if item.StackableType, err = parseStr(); err != nil {
			return nil, err
		}
		if item.EquipmentType, err = parseStr(); err != nil {
			return nil, err
		}
		if len(rest) < 8 {
			return nil, errors.New("DFIX body 越界")
		}
		item.StackLimit = int64(binary.LittleEndian.Uint64(rest))
		rest = rest[8:]
		if len(rest) < 1 {
			return nil, errors.New("DFIX body 越界")
		}
		item.ListType = rest[0]
		rest = rest[1:]
		if item.Destination, err = parseStr(); err != nil {
			return nil, err
		}
		if len(rest) < 1 {
			return nil, errors.New("DFIX body 越界")
		}
		item.SupportsUpgrade = rest[0] != 0
		rest = rest[1:]
		items = append(items, item)
	}
	return items, nil
}
