package pvf

import (
	"sync"
	"testing"
)

func TestByteLRUEvictsByResidentBytes(t *testing.T) {
	cache := newByteLRU(5)
	cache.add(1, "1234", 4)
	cache.add(2, "56", 2)
	if _, ok := cache.get(1); ok {
		t.Fatal("oldest entry was not evicted")
	}
	value, ok := cache.get(2)
	if !ok || value.(string) != "56" {
		t.Fatalf("newest entry=%v ok=%v", value, ok)
	}
	snapshot := cache.snapshot()
	if snapshot.entries != 1 || snapshot.bytes != 2 || snapshot.evictions != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestByteLRUReplacesEntryWithoutExceedingLimit(t *testing.T) {
	cache := newByteLRU(8)
	cache.add(1, []byte("1234"), 4)
	cache.add(1, []byte("12345678"), 8)
	value, ok := cache.get(1)
	if !ok || string(value.([]byte)) != "12345678" {
		t.Fatalf("value=%v ok=%v", value, ok)
	}
	if got := cache.snapshot(); got.entries != 1 || got.bytes != 8 {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestByteLRUConcurrentAccess(t *testing.T) {
	cache := newByteLRU(1024)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for index := 0; index < 500; index++ {
				key := worker*500 + index
				cache.add(key, "value", 5)
				cache.get(key)
			}
		}(worker)
	}
	wg.Wait()
	if got := cache.snapshot(); got.bytes > 1024 {
		t.Fatalf("resident bytes=%d exceed limit", got.bytes)
	}
}

func TestArchiveReadRemainsStableAfterCacheEviction(t *testing.T) {
	archive, err := OpenArchive(&Bundle{
		data:            buildSyntheticNKPIArchive(t),
		textCacheBytes:  1,
		chunkCacheBytes: 1,
	})
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	first, err := archive.ReadText("dir/test.ani")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := archive.ReadText("dir/test.ani")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if first != second {
		t.Fatalf("read changed after eviction: first=%q second=%q", first, second)
	}
	snapshot := archive.Snapshot()
	if snapshot.CachedChunkBytes > 1 || snapshot.CachedTextBytes > 1 {
		t.Fatalf("cache limits exceeded: %+v", snapshot)
	}
}

func TestArchiveMaterializesFileMetadataOnDemand(t *testing.T) {
	archive, err := OpenBytes(buildSyntheticNKPIArchive(t))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	if got := archive.FileCount(); got != 1 {
		t.Fatalf("file count=%d want=1", got)
	}
	file, ok := archive.FindFile("DIR\\TEST.ANI")
	if !ok {
		t.Fatal("find normalized archive path")
	}
	if file.Index != 0 || file.Path != "dir" || file.Name != "test.ani" || file.ArchivePath != "dir/test.ani" {
		t.Fatalf("file=%+v", file)
	}
	files := archive.Files()
	if len(files) != 1 || files[0] != file {
		t.Fatalf("files=%+v want=%+v", files, file)
	}
	var paths []string
	archive.RangePaths(func(archivePath string) bool {
		paths = append(paths, archivePath)
		return true
	})
	if len(paths) != 1 || paths[0] != "dir/test.ani" {
		t.Fatalf("paths=%v", paths)
	}
}
