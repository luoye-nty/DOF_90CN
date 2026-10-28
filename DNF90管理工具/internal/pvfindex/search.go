package pvfindex

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// SearchRequest 物品搜索请求（对照 GM directItemSearchRequest）。
// Category 取值：all（全部）/ equipment（装备）/ item（消耗道具），与 GM buildSearchIndex 语义一致。
type SearchRequest struct {
	Keyword  string
	Category string
	Page     int
	PageSize int
}

// SearchView 搜索结果行（对照 GM directItemSearchView）。
type SearchView struct {
	ItemID          uint32
	Name            string
	Kind            string
	Category        string
	PVFPath         string
	StackableType   string
	EquipmentType   string
	StackLimit      int64
	ListType        byte
	Destination     string
	SupportsUpgrade bool
}

// SearchResult 搜索结果页（对照 GM directItemSearchResult）。
type SearchResult struct {
	Items     []SearchView
	Total     int
	Page      int
	PageSize  int
	PageCount int
}

// buildSearchIndex 解析全部列表条目并排序缓存（对照 GM directPVFCatalog.buildSearchIndex）。
func (c *Catalog) buildSearchIndex(ctx context.Context) ([]SearchView, error) {
	ids := make([]uint32, 0, len(c.refs))
	for itemID := range c.refs {
		ids = append(ids, itemID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	items := make([]SearchView, 0, len(ids))
	for index, itemID := range ids {
		if index%128 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		definition, err := c.resolve(itemID)
		if err != nil || Validate(definition) != nil {
			continue
		}
		category := "item"
		if definition.Kind == KindEquipment {
			category = "equipment"
		}
		items = append(items, SearchView{
			ItemID:          definition.ItemID,
			Name:            definition.Name,
			Kind:            string(definition.Kind),
			Category:        category,
			PVFPath:         definition.PVFPath,
			StackableType:   definition.StackableType,
			EquipmentType:   definition.EquipmentType,
			StackLimit:      definition.StackLimit,
			ListType:        definition.ListType,
			Destination:     definition.Route,
			SupportsUpgrade: definition.SupportsUpgrade,
		})
	}
	return items, nil
}

// buildOrLoadSearchIndex 优先载入 DFIX 落盘缓存（§6.3：指纹匹配即跳过全量解析）；
// 未命中则全量构建，并在成功后尽力回写缓存（写失败不阻塞内存索引）。
func (c *Catalog) buildOrLoadSearchIndex(ctx context.Context) ([]SearchView, error) {
	if items, ok, err := c.tryLoadDiskIndex(); err != nil {
		return nil, err
	} else if ok {
		return items, nil
	}
	items, err := c.buildSearchIndex(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.saveDiskIndex(items); err != nil {
		// 缓存写入尽力而为：失败不影响内存索引本次搜索结果。
		_ = err
	}
	return items, nil
}

// Search 内存搜索（对照 GM searchDirectItems：keyword 名称/路径包含匹配，纯数字视为 item_id 精确匹配）。
// 调用方需保证 Page >= 1 且 PageSize > 0（GUI 层入参校验，同 GM handleItemSearch）。
func (c *Catalog) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	c.searchOnce.Do(func() {
		c.searchItems, c.searchErr = c.buildOrLoadSearchIndex(ctx)
	})
	if c.searchErr != nil {
		return SearchResult{}, c.searchErr
	}
	keyword := strings.ToLower(strings.TrimSpace(req.Keyword))
	numericID, numericErr := strconv.ParseUint(keyword, 10, 32)
	numeric := keyword != "" && numericErr == nil
	matches := make([]SearchView, 0, len(c.searchItems))
	for _, item := range c.searchItems {
		if req.Category != "all" && item.Category != req.Category {
			continue
		}
		if numeric {
			if uint64(item.ItemID) != numericID {
				continue
			}
		} else if keyword != "" && !strings.Contains(strings.ToLower(item.Name), keyword) &&
			!strings.Contains(strings.ToLower(item.PVFPath), keyword) {
			continue
		}
		matches = append(matches, item)
	}
	pageCount := 0
	if len(matches) > 0 {
		pageCount = (len(matches) + req.PageSize - 1) / req.PageSize
	}
	start := (req.Page - 1) * req.PageSize
	if start > len(matches) {
		start = len(matches)
	}
	end := start + req.PageSize
	if end > len(matches) {
		end = len(matches)
	}
	items := append([]SearchView(nil), matches[start:end]...)
	return SearchResult{Items: items, Total: len(matches), Page: req.Page, PageSize: req.PageSize, PageCount: pageCount}, nil
}
