package pvfindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	dnfpvf "dnf90ops/internal/dnfpvf"
	platformpvf "dnf90ops/internal/platform/pvf"
)

// Catalog 本地 PVF 物品目录（内存态），由 Ensure 获得。
// 结构对照 GM directPVFCatalog；落盘缓存（DFIX）在实施阶段 4 接入，本文件仅保留进程内缓存 + 指纹校验。
type Catalog struct {
	archive *platformpvf.Archive
	refs    map[uint32]reference
	srcPath string
	srcSize int64
	srcMod  int64

	mu          sync.Mutex
	definitions map[uint32]Definition
	searchOnce  sync.Once
	searchItems []SearchView
	searchErr   error
}

// Stats 索引统计（设计 §6.4）。
type Stats struct {
	Path      string
	Size      int64
	ModTime   int64
	Entries   int
	Stackable int
	Equipment int
}

type cacheEntry struct {
	size    int64
	modTime int64
	catalog *Catalog
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]cacheEntry{}
)

// Ensure 懒加载/构建 PVF 目录索引。
// pvfPath 非空且文件存在；指纹（size+mtime UnixNano）命中缓存时直接复用，否则重新解析并构建搜索索引。
// onProgress 为落盘构建（阶段 4）预留的进度钩子，当前内存构建阶段可传 nil。
func Ensure(ctx context.Context, pvfPath string, onProgress func(done, total int)) (*Catalog, error) {
	pvfPath = strings.TrimSpace(pvfPath)
	if pvfPath == "" {
		return nil, errors.New("PVF 路径为空")
	}
	absPath, err := pathAbs(pvfPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("读取 PVF 文件: %w", err)
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if cached, ok := catalogCache[absPath]; ok && cached.size == info.Size() && cached.modTime == info.ModTime().UnixNano() {
		return cached.catalog, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	archive, err := platformpvf.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("解析 PVF 文件: %w", err)
	}
	refs := make(map[uint32]reference)
	if err := loadItemList(archive, stackableListPath, KindStackable, refs); err != nil {
		return nil, err
	}
	if err := loadItemList(archive, equipmentListPath, KindEquipment, refs); err != nil {
		return nil, err
	}
	catalog := &Catalog{
		archive:     archive,
		refs:        refs,
		srcPath:     absPath,
		srcSize:     info.Size(),
		srcMod:      info.ModTime().UnixNano(),
		definitions: make(map[uint32]Definition),
	}
	catalogCache = map[string]cacheEntry{
		absPath: {size: info.Size(), modTime: info.ModTime().UnixNano(), catalog: catalog},
	}
	return catalog, nil
}

func pathAbs(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("解析 PVF 路径: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func loadItemList(archive *platformpvf.Archive, listPath string, kind Kind, refs map[uint32]reference) error {
	text, err := archive.ReadText(listPath)
	if err != nil {
		return fmt.Errorf("读取 PVF 列表 %s: %w", listPath, err)
	}
	document, err := dnfpvf.Parse(listPath, text)
	if err != nil {
		return fmt.Errorf("解析 PVF 列表 %s: %w", listPath, err)
	}
	for _, entry := range dnfpvf.ParseList(document) {
		if entry.ID <= 0 || entry.ID > int64(^uint32(0)) {
			continue
		}
		itemID := uint32(entry.ID)
		if _, exists := refs[itemID]; exists {
			continue
		}
		refs[itemID] = reference{kind: kind, path: cleanPVFPath(entry.Path)}
	}
	return nil
}

func readItemText(archive *platformpvf.Archive, root, listed string) (string, string, error) {
	listed = cleanPVFPath(listed)
	candidates := []string{listed}
	if !strings.HasPrefix(strings.ToLower(listed), strings.ToLower(root)+"/") {
		candidates = append(candidates, cleanPVFPath(root+"/"+listed))
	}
	for _, candidate := range candidates {
		text, err := archive.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
	}
	return "", "", fmt.Errorf("PVF 路径不存在: %s", listed)
}

func cleanPVFPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	value = strings.TrimPrefix(value, "./")
	cleaned := filepath.ToSlash(filepath.Clean("/" + value))
	return strings.TrimPrefix(cleaned, "/")
}

// Resolve 解析单个物品定义（对照 GM directPVFCatalog.resolve）。
func (c *Catalog) Resolve(itemID uint32) (Definition, bool) {
	def, err := c.resolve(itemID)
	if err != nil {
		return Definition{}, false
	}
	return def, true
}

func (c *Catalog) resolve(itemID uint32) (Definition, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if definition, ok := c.definitions[itemID]; ok {
		return definition, nil
	}
	ref, ok := c.refs[itemID]
	if !ok {
		return Definition{}, fmt.Errorf("当前 PVF 中不存在物品 ID %d", itemID)
	}
	docPath, text, err := readItemText(c.archive, string(ref.kind), ref.path)
	if err != nil {
		return Definition{}, fmt.Errorf("读取物品 %d: %w", itemID, err)
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return Definition{}, fmt.Errorf("解析物品 %d: %w", itemID, err)
	}
	definition := Definition{ItemID: itemID, Kind: ref.kind, PVFPath: docPath}
	definition.Name, _ = document.Text("name")
	definition.Name = strings.TrimSpace(definition.Name)
	if definition.Name == "" {
		definition.Name = "未命名物品"
	}
	definition.AttachType, _ = document.Text("attach type")
	if raw, found := document.Text("expiration date"); found && strings.TrimSpace(raw) != "" {
		parsed, parseErr := timeParseInLocation(strings.TrimSpace(raw))
		if parseErr != nil {
			return Definition{}, fmt.Errorf("过期时间 %q: %w", raw, parseErr)
		}
		definition.ExpirationDate = parsed
	}
	if ref.kind == KindEquipment {
		definition.EquipmentType, _ = document.Text("equipment type")
		applyEquipmentRoute(&definition)
		for _, field := range []string{"durability", "maximum durability", "max durability"} {
			if value, found := document.Int(field); found && value > 0 {
				if value > int64(^uint16(0)) {
					value = int64(^uint16(0))
				}
				definition.Durability = uint16(value)
				break
			}
		}
	} else {
		definition.StackableType, _ = document.Text("stackable type")
		applyStackableRoute(&definition)
		if value, found := document.Int("stack limit"); found && value > 0 {
			definition.StackLimit = value
		}
		if value, found := document.Int("usable period"); found && value > 0 {
			definition.UsablePeriodDays = value
		}
	}
	c.definitions[itemID] = definition
	return definition, nil
}

// Stats 返回索引统计。
func (c *Catalog) Stats() Stats {
	stackable, equipment := 0, 0
	for _, ref := range c.refs {
		if ref.kind == KindStackable {
			stackable++
		} else {
			equipment++
		}
	}
	return Stats{
		Path:      c.srcPath,
		Size:      c.srcSize,
		ModTime:   c.srcMod,
		Entries:   len(c.refs),
		Stackable: stackable,
		Equipment: equipment,
	}
}

// Close 释放内存缓存（platform/pvf Archive 为内存态，无外部句柄；阶段 4 落盘句柄接入后扩展）。
func (c *Catalog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.definitions = nil
	c.searchItems = nil
	return nil
}

func timeParseInLocation(value string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04:05", value, time.FixedZone("CST", 8*60*60))
}
