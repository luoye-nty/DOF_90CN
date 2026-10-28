package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestSaveIsSafeAgainstConcurrentMutate 锁定"配置写锁"这条不变量：
// Save（json.Marshal 读整份 Config）与 Mutate（改 map/slice 字段）必须互斥。
//
// 背景（2026-09-25 审计实测）：改用例若像老代码那样"直接写 cfg.Welfare.LastRuns"
// 再 Save，`go test -race` 会报 reflect.maplen 读 vs runtime.mapassign_faststr 写的数据竞争；
// 线上后果是 Go 运行时抛 fatal error: concurrent map read and map write（不可恢复，
// 整个运维工具进程退出）。所以凡是改 map/slice 的地方都要走 cfg.Mutate。
//
// 这条用例在 -race 下才有意义（普通 go test 只验证不死锁、不互相阻塞）：
//
//	go test -race -tags paid ./internal/config/
func TestSaveIsSafeAgainstConcurrentMutate(t *testing.T) {
	cfg := &Config{path: filepath.Join(t.TempDir(), "config.json")}
	cfg.Welfare = &WelfareConfig{LastRuns: map[string]string{}}
	cfg.AnnounceSchedule = []ScheduledAnnouncement{{ID: "a", Content: "t", Mode: "daily"}}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// 模拟调度协程：welfareMarkRun / announceScheduleMarkRun。
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			period := fmt.Sprintf("2026-09-%02d", i%28+1)
			if err := cfg.Mutate(func() {
				cfg.Welfare.LastRuns["daily"] = period
				cfg.AnnounceSchedule[0].LastRun = period
			}, nil); err != nil {
				t.Errorf("Mutate() error = %v", err)
				return
			}
		}
		close(stop)
	}()

	// 模拟 UI 线程 / manage 协程：并发 Save。
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := cfg.Save(); err != nil {
					t.Errorf("Save() error = %v", err)
					return
				}
			}
		}
	}()

	wg.Wait()
}

// TestMutateRollsBackOnSaveFailure 锁"落盘失败回滚内存"的语义：
// welfare.go / announce_schedule.go 都依赖它保证"内存 == 磁盘"。
func TestMutateRollsBackOnSaveFailure(t *testing.T) {
	// 目录名非法（Windows 下 NUL 是保留名）→ MkdirAll/落盘必然失败。
	cfg := &Config{path: filepath.Join(t.TempDir(), "NUL", "config.json")}
	cfg.Welfare = &WelfareConfig{LastRuns: map[string]string{"daily": "old"}}

	err := cfg.Mutate(func() {
		cfg.Welfare.LastRuns["daily"] = "new"
	}, func() {
		cfg.Welfare.LastRuns["daily"] = "old"
	})
	if err == nil {
		t.Skip("当前平台允许写入该路径，跳过回滚断言（不影响写锁用例）")
	}
	if got := cfg.Welfare.LastRuns["daily"]; got != "old" {
		t.Fatalf("落盘失败后应回滚成 old，实际 %q", got)
	}
}
