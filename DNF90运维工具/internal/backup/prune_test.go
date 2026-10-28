package backup

// 备份保留（清理旧备份）测试：定时备份必须只增不减的时代已经过去了 ——
// 没有上限时每小时一份备份会把磁盘写满（本项目真出过磁盘 0 可用的事故）。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedBackups 造 n 个假备份 zip，按给定顺序设置修改时间（越靠后越新）。
func seedBackups(t *testing.T, dir string, n int) {
	t.Helper()
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	for i := 0; i < n; i++ {
		// 名字必须唯一：早先按 i%10 拼序号，超过 10 个就互相覆盖，用例自己把自己骗了。
		name := filepath.Join(dir, fmt.Sprintf("dnf90db_20260914_%06d.zip", i))
		if err := os.WriteFile(name, []byte("zip"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(name, stamp, stamp); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
}

func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	seedBackups(t, dir, 7)
	// 再放一个非备份文件，清理不能碰它。
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneBackups(dir, 3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 4 {
		t.Fatalf("删除数量 = %d, want 4", len(removed))
	}
	entries, err := ListBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("剩余备份 = %d, want 3", len(entries))
	}
	// 剩下的必须是最新的 3 个（ListBackups 新→旧）。
	if entries[0].ModTime.Before(entries[1].ModTime) {
		t.Fatal("剩余备份顺序不是新→旧")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("非 zip 文件被误删了")
	}

	// 再清理一次：数量已在限制内，不应该再删。
	again, err := PruneBackups(dir, 3)
	if err != nil || len(again) != 0 {
		t.Fatalf("第二次清理 removed=%v err=%v", again, err)
	}
}

func TestPruneBackupsDefaultsAndMissingDir(t *testing.T) {
	dir := t.TempDir()
	seedBackups(t, dir, DefaultKeep+5)
	// keep<=0 用默认值兜底，而不是"删光"。
	if _, err := PruneBackups(dir, 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	entries, _ := ListBackups(dir)
	if len(entries) != DefaultKeep {
		t.Fatalf("keep=0 时应保留默认 %d 份，实际 %d", DefaultKeep, len(entries))
	}
	// 目录不存在不能报错（首次运行还没有备份目录）。
	if removed, err := PruneBackups(filepath.Join(dir, "nope"), 5); err != nil || len(removed) != 0 {
		t.Fatalf("目录不存在时 removed=%v err=%v", removed, err)
	}
}
