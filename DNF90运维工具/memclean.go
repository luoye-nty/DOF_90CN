package main

// DNF90 运维工具 —— 定时清理内存（免费版/赞助版共用，无 build tag）。
//
// 做什么：按间隔调用 Windows 的 EmptyWorkingSet，把目标进程工作集里的闲置物理页交还系统。
//
// 必须说清楚它做不到什么（否则会被当成"内存没降=功能没生效"）：
//   - EmptyWorkingSet 只影响"工作集"（物理内存占用），不释放进程已提交的虚拟内存，
//     更不会触碰 Go 堆里仍在引用的对象。所以看到的现象是：任务管理器里内存数字掉下来，
//     但过一会儿会因为重新访问被换出的页面而慢慢涨回去。
//   - 它解决的是"服务器内存被闲置页占满、影响其他程序"这类问题；
//     如果内存是持续单调上涨（真实泄漏），唯一可靠的办法是重启服务端，不是回收工作集。
//   - 全部调用都不改目标进程状态、不断线，失败也只是这一轮没回收。
//
// 目标进程：
//   - DNF90Server.exe / DNF90Control.exe：按服务端根目录匹配（复用 serverdir.ListRootProcesses，
//     与「停止服务」用的是同一套进程发现逻辑，避免两处口径不一致）；
//   - mysqld.exe：同上；
//   - 本工具自身。

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"dnf90ops/internal/serverdir"
)

const (
	// memCleanMinInterval / memCleanMaxInterval 间隔（分钟）允许区间，与 config 校验一致。
	memCleanMinInterval = 1
	memCleanMaxInterval = 1440
	// processQueryLimitedInformation + PROCESS_SET_QUOTA：回收工作集所需的最小权限集。
	processQueryLimitedInformation = 0x1000
	processSetQuota                = 0x0100
)

// 延迟加载的系统 API。任何一个缺失都不致命：对应能力静默降级，日志里会写出来。
var (
	modPsapi    = syscall.NewLazyDLL("psapi.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procEmptyWorkingSet        = modPsapi.NewProc("EmptyWorkingSet")
	procGetProcessMemoryInfo   = modPsapi.NewProc("GetProcessMemoryInfo")
	procSetProcessWorkingSetSz = modKernel32.NewProc("SetProcessWorkingSetSize")
	procOpenProcess            = modKernel32.NewProc("OpenProcess")
	procCloseHandle            = modKernel32.NewProc("CloseHandle")
)

// processMemoryCounters 对应 PROCESS_MEMORY_COUNTERS（psapi）。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// memCleanState 内存回收运行时状态。
type memCleanState struct {
	mu          sync.Mutex
	lastRun     time.Time
	lastFreed   int64 // 最近一次总回收字节数
	totalFreed  int64 // 进程生命周期内累计回收字节数
	runCount    int64
	failCount   int64
	lastTargets int
}

var memClean memCleanState

// memCleanStatus 内存回收运行快照（供「运行维护」页展示）。
type memCleanStatus struct {
	Enabled      bool
	IntervalMin  int
	LastRun      time.Time
	NextRun      time.Time
	LastFreed    int64
	TotalFreed   int64
	RunCount     int64
	FailCount    int64
	SelfWS       int64 // 本工具当前工作集
	ServerWS     int64 // 服务端进程当前工作集合计
	MySQLWS      int64 // mysqld 当前工作集合计
	LastErrorMsg string
}

// memCleanConfig 读取当前内存回收配置（空则给默认值）。
func (a *App) memCleanConfig() memCleanConfigView {
	view := memCleanConfigView{IntervalMinutes: 30, Server: true, Self: true}
	if a == nil || a.cfg == nil || a.cfg.MemoryClean == nil {
		return view
	}
	cfg := a.cfg.MemoryClean
	view.Enabled = cfg.Enabled
	if cfg.IntervalMinutes >= memCleanMinInterval && cfg.IntervalMinutes <= memCleanMaxInterval {
		view.IntervalMinutes = cfg.IntervalMinutes
	}
	view.Server = cfg.Server
	view.MySQL = cfg.MySQL
	view.Self = cfg.Self
	view.LogEveryRun = cfg.LogEveryRun
	return view
}

// memCleanConfigView 配置只读视图（避免在后台协程里直接摸 cfg 指针）。
type memCleanConfigView struct {
	Enabled         bool
	IntervalMinutes int
	Server          bool
	MySQL           bool
	Self            bool
	LogEveryRun     bool
}

// memCleanLoop 定时回收循环（进程生命周期内常驻，配置变更下一轮生效）。
func (a *App) memCleanLoop() {
	_ = a.guardGoroutine("内存回收循环", func() {
		a.memCleanLoopBody()
	})
}

func (a *App) memCleanLoopBody() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var lastRun time.Time
	for range tick.C {
		cfg := a.memCleanConfig()
		if !cfg.Enabled {
			continue
		}
		interval := time.Duration(cfg.IntervalMinutes) * time.Minute
		if !lastRun.IsZero() && time.Since(lastRun) < interval {
			continue
		}
		lastRun = time.Now()
		a.runMemClean(cfg, false)
	}
}

// runMemClean 执行一轮回收。manual=true 表示用户点了「立即回收」（无论是否有回收量都打日志）。
func (a *App) runMemClean(cfg memCleanConfigView, manual bool) {
	targets := make([]memCleanTarget, 0, 8)
	root := a.currentServerRoot()

	if cfg.Server && root != "" {
		if procs, err := serverdir.ListRootProcesses(root, "DNF90Server.exe", "DNF90Control.exe"); err == nil {
			for _, proc := range procs {
				targets = append(targets, memCleanTarget{Name: proc.Name, PID: proc.PID})
			}
		}
	}
	if cfg.MySQL && root != "" {
		if procs, err := serverdir.ListRootProcesses(root, "mysqld.exe"); err == nil {
			for _, proc := range procs {
				targets = append(targets, memCleanTarget{Name: proc.Name, PID: proc.PID})
			}
		}
	}
	if cfg.Self {
		targets = append(targets, memCleanTarget{Name: "本工具", PID: syscall.Getpid(), Self: true})
	}

	if len(targets) == 0 {
		if manual {
			a.logf("内存回收：没有找到可回收的目标进程（服务端未运行？）")
		}
		return
	}

	freedTotal := int64(0)
	okCount := 0
	failMsgs := make([]string, 0, 2)
	details := make([]string, 0, len(targets))
	for _, target := range targets {
		before, after, err := reclaimWorkingSet(target.PID)
		if err != nil {
			failMsgs = append(failMsgs, fmt.Sprintf("%s(pid=%d): %v", target.Name, target.PID, err))
			continue
		}
		okCount++
		freed := before - after
		if freed < 0 {
			freed = 0
		}
		freedTotal += freed
		details = append(details, fmt.Sprintf("%s(pid=%d) %s→%s", target.Name, target.PID, humanBytes(before), humanBytes(after)))
	}

	memClean.mu.Lock()
	memClean.lastRun = time.Now()
	memClean.lastFreed = freedTotal
	memClean.totalFreed += freedTotal
	memClean.runCount++
	if len(failMsgs) > 0 {
		memClean.failCount++
	}
	memClean.lastTargets = okCount
	// 累计值必须在临界区里取出快照：定时循环与「立即回收一次」按钮可以并发进来，
	// 出了锁再读 memClean.totalFreed 就是典型的数据竞争（go test -race 会直接报）。
	totalFreed := memClean.totalFreed
	memClean.mu.Unlock()

	// 日志策略：手动触发、有实际回收量、或有失败，都记；纯空转（<1MB）默认不刷屏。
	if manual || freedTotal >= 1<<20 || len(failMsgs) > 0 || cfg.LogEveryRun {
		a.logf("内存回收完成：回收 %s，累计 %s（%d 个进程）｜ %s",
			humanBytes(freedTotal), humanBytes(totalFreed), okCount, strings.Join(details, "，"))
	}
	for _, msg := range failMsgs {
		a.logf("内存回收失败：%s", msg)
	}
}

// memCleanTarget 回收目标。
type memCleanTarget struct {
	Name string
	PID  int
	Self bool
}

// reclaimWorkingSet 回收一个进程的工作集，返回回收前/后的工作集字节数。
func reclaimWorkingSet(pid int) (int64, int64, error) {
	if pid <= 0 {
		return 0, 0, fmt.Errorf("pid 无效")
	}
	handle := syscall.Handle(0)
	if pid == syscall.Getpid() {
		// 自身进程直接取伪句柄，不需要 OpenProcess 权限。
		handle, _ = syscall.GetCurrentProcess()
	} else {
		opened, _, callErr := procOpenProcess.Call(
			uintptr(processQueryLimitedInformation|processSetQuota), 0, uintptr(pid))
		if opened == 0 {
			return 0, 0, fmt.Errorf("OpenProcess 失败: %v", callErr)
		}
		handle = syscall.Handle(opened)
		defer procCloseHandle.Call(uintptr(handle))
	}

	before := workingSetSize(handle)
	if ret, _, callErr := procEmptyWorkingSet.Call(uintptr(handle)); ret == 0 {
		// 回退路径：SetProcessWorkingSetSize(h, -1, -1) 是同一件事的另一种写法，
		// 在部分精简系统上 psapi 被裁掉时还能用。
		if ret2, _, callErr2 := procSetProcessWorkingSetSz.Call(uintptr(handle), ^uintptr(0), ^uintptr(0)); ret2 == 0 {
			return before, before, fmt.Errorf("EmptyWorkingSet 与 SetProcessWorkingSetSize 均失败: %v / %v", callErr, callErr2)
		}
	}
	after := workingSetSize(handle)
	if before == 0 && after == 0 {
		// 拿不到计数不影响回收本身，只是日志里没法报数字。
		return 0, 0, nil
	}
	return before, after, nil
}

// workingSetSize 读取进程当前工作集字节数；失败返回 0（不视为错误）。
func workingSetSize(handle syscall.Handle) int64 {
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	ret, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ret == 0 {
		return 0
	}
	return int64(counters.WorkingSetSize)
}

// memCleanStatusSnapshot 汇总当前内存状态（供「运行维护」页展示）。
func (a *App) memCleanStatusSnapshot() memCleanStatus {
	cfg := a.memCleanConfig()
	status := memCleanStatus{
		Enabled:     cfg.Enabled,
		IntervalMin: cfg.IntervalMinutes,
	}
	memClean.mu.Lock()
	status.LastRun = memClean.lastRun
	status.LastFreed = memClean.lastFreed
	status.TotalFreed = memClean.totalFreed
	status.RunCount = memClean.runCount
	status.FailCount = memClean.failCount
	memClean.mu.Unlock()
	if !status.LastRun.IsZero() {
		status.NextRun = status.LastRun.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
	}
	selfHandle, _ := syscall.GetCurrentProcess()
	status.SelfWS = workingSetSize(selfHandle)

	root := a.currentServerRoot()
	if root != "" {
		if procs, err := serverdir.ListRootProcesses(root, "DNF90Server.exe", "DNF90Control.exe"); err == nil {
			for _, proc := range procs {
				status.ServerWS += processWorkingSetByPID(proc.PID)
			}
		}
		if procs, err := serverdir.ListRootProcesses(root, "mysqld.exe"); err == nil {
			for _, proc := range procs {
				status.MySQLWS += processWorkingSetByPID(proc.PID)
			}
		}
	}
	return status
}

// processWorkingSetByPID 读某个 PID 的工作集（拿不到权限时返回 0）。
func processWorkingSetByPID(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	opened, _, _ := procOpenProcess.Call(uintptr(processQueryLimitedInformation), 0, uintptr(pid))
	if opened == 0 {
		return 0
	}
	handle := syscall.Handle(opened)
	defer procCloseHandle.Call(uintptr(handle))
	return workingSetSize(handle)
}

// humanBytes 字节数转可读文本（用于日志与状态行）。
func humanBytes(size int64) string {
	if size < 0 {
		size = 0
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	value := float64(size)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, name := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, name)
		}
	}
	return fmt.Sprintf("%.1fPB", value/unit)
}
