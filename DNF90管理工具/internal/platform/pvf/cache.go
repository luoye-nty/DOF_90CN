// 来源: DNF90GM工具_独立版 vendor/longheng.io/server/internal (platform/pvf) 于 2026-09-06 复制。
// 维护纪律: 以 GM 工具源码为唯一事实源; 同步升级时逐文件对比; import path 保持本端 module 内部路径。
package pvf

import (
	"container/list"
	"sync"
	"sync/atomic"
)

const (
	DefaultChunkCacheBytes int64 = 256 * 1024 * 1024
	DefaultTextCacheBytes  int64 = 256 * 1024 * 1024
)

type byteLRUEntry struct {
	key   int
	value any
	bytes int64
}

type byteLRUSnapshot struct {
	entries   int
	bytes     int64
	hits      uint64
	misses    uint64
	evictions uint64
}

// byteLRU bounds decoded PVF data by resident bytes. Cached values are never
// mutated by callers: raw reads return copies and decoded text is immutable.
type byteLRU struct {
	mu        sync.Mutex
	limitB    int64
	bytesB    int64
	entries   map[int]*list.Element
	order     *list.List
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

func newByteLRU(limit int64) *byteLRU {
	if limit <= 0 {
		limit = 1
	}
	return &byteLRU{limitB: limit, entries: make(map[int]*list.Element), order: list.New()}
}

func (c *byteLRU) get(key int) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.order.MoveToFront(element)
	c.hits.Add(1)
	return element.Value.(byteLRUEntry).value, true
}

func (c *byteLRU) add(key int, value any, size int64) {
	if c == nil || value == nil || size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		entry := existing.Value.(byteLRUEntry)
		c.bytesB -= entry.bytes
		entry.value = value
		entry.bytes = size
		existing.Value = entry
		c.bytesB += size
		c.order.MoveToFront(existing)
	} else {
		element := c.order.PushFront(byteLRUEntry{key: key, value: value, bytes: size})
		c.entries[key] = element
		c.bytesB += size
	}
	for c.bytesB > c.limitB && c.order.Len() > 0 {
		oldest := c.order.Back()
		entry := oldest.Value.(byteLRUEntry)
		delete(c.entries, entry.key)
		c.order.Remove(oldest)
		c.bytesB -= entry.bytes
		c.evictions.Add(1)
	}
}

func (c *byteLRU) snapshot() byteLRUSnapshot {
	if c == nil {
		return byteLRUSnapshot{}
	}
	c.mu.Lock()
	entries, bytes := c.order.Len(), c.bytesB
	c.mu.Unlock()
	return byteLRUSnapshot{
		entries: entries, bytes: bytes,
		hits: c.hits.Load(), misses: c.misses.Load(), evictions: c.evictions.Load(),
	}
}

func (c *byteLRU) limit() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitB
}
