package pvf

import (
	"os"
	"runtime"
	"testing"
)

func TestRealArchiveRetainedMemory(t *testing.T) {
	archivePath := os.Getenv("DNF90_REAL_PVF_MEMORY")
	if archivePath == "" {
		t.Skip("set DNF90_REAL_PVF_MEMORY to run the real archive memory regression probe")
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	archive, err := LoadArchive(Options{
		Path:            archivePath,
		ChunkCacheBytes: 1,
		TextCacheBytes:  1,
	})
	if err != nil {
		t.Fatalf("load real archive: %v", err)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(archive)

	retainedHeap := after.HeapAlloc - before.HeapAlloc
	retainedObjects := after.HeapObjects - before.HeapObjects
	t.Logf("files=%d retained_heap_bytes=%d retained_heap_objects=%d", archive.FileCount(), retainedHeap, retainedObjects)
	// The current compatibility archive has 1,057,625 files. Keeping a second
	// materialized File table pushes its retained heap above 500 MiB; the compact
	// metadata representation must remain below 400 MiB for this exact fixture.
	if archive.FileCount() == 1_057_625 && retainedHeap > 400*1024*1024 {
		t.Fatalf("real archive retained heap=%d exceeds compact index budget", retainedHeap)
	}
}
