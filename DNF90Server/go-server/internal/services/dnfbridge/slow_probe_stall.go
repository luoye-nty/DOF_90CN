package dnfbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 本文件是「进图加载卡顿」调查用的第二支探针，与 slow_path_probe.go 并列。
//
// 背景：slow_path_probe.go 的分步探针只覆盖了 completeRuntimePartyDungeonLoading
// 里 5 个**猜出来的**候选调用点。两台服务器 38 次实测下来，那 5 步**一次都没有
// 超过阈值**，而 complete_loading_total 稳定在 7.6~10.2 秒 —— 即「时间花在探针
// 没有覆盖的地方」。再按猜测补探针，就是重复同一个错误。
//
// 所以这里换成两支**结构上不可能空手而归**的探针：
//
//	1. 卡顿栈采样（startStallStackSampler）
//	   定时抓「全部 goroutine 的栈」，只在内容变化时落盘。无论那几秒是在算、
//	   在等锁、在等数据库、还是在等 socket，栈上都会写明是谁、在哪一行。
//
//	2. 无缝分段（slowProbeSegments）
//	   把整段代码的墙钟按语句顺序切成**连续、无缝隙**的段落，
//	   Σ段 + 未归因 ≡ 总时长（构造上恒等），因此不存在盲区。
//
// 两者都只**读**运行状态、只**写**独立日志文件，不改任何既有行为。

// ---------------------------------------------------------------------------
// 1. 卡顿栈采样
// ---------------------------------------------------------------------------

const (
	// stallSamplerDefaultInterval 是采样间隔。300ms 对 4~10 秒的卡顿足够密，
	// 又不至于在正常运行（栈内容不变）时产生任何落盘。
	stallSamplerDefaultInterval = 300 * time.Millisecond
	// stallSamplerMaxBytes 是单个文件的封顶字节数，防止长时间开启把磁盘写满。
	stallSamplerMaxBytes = 64 << 20
	// stallSamplerStackBuffer 是一次 runtime.Stack 的缓冲区上限。
	// 本服务实测约 137~150 个 goroutine，8MB 有充足余量。
	stallSamplerStackBuffer = 8 << 20
)

// startStallStackSampler 启动卡顿栈采样。
//
// 开关是环境变量 DNF90_STALL_SAMPLER。**默认关闭**：只有显式给
// 1 / on / true / yes 才启动。这样线上不带这个变量跑就完全不受影响，
// 与 goroutine_watchdog.go 的 DNF90_GOROUTINE_WATCHDOG 是同一套约定。
//
// 落盘策略是**按内容去重**：只有栈快照与上一份不同才追加。正常运行期间
// 栈内容几乎不变，所以文件会很小；一旦发生卡顿，栈会变化，时间线自然浮现。
func (s *Service) startStallStackSampler(ctx context.Context) {
	if s == nil || !stallSamplerEnabled() {
		return
	}
	path := filepath.Join("logs", "stall_stacks.txt")
	interval := stallSamplerDefaultInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastKey string
		var written int64
		var samples, emitted int64
		truncated := false
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-ticker.C:
				buf := make([]byte, stallSamplerStackBuffer)
				size := runtime.Stack(buf, true)
				body := buf[:size]
				samples++

				sum := sha256.Sum256(body)
				key := hex.EncodeToString(sum[:])
				if key == lastKey {
					// 与上一份逐字节相同：正常运行，不落盘。
					continue
				}
				lastKey = key

				if written >= stallSamplerMaxBytes {
					if !truncated {
						truncated = true
						appendStallSample(path, []byte(fmt.Sprintf(
							"\n===== %s sampler stopped at %d bytes (cap) =====\n",
							at.Format(time.RFC3339Nano), stallSamplerMaxBytes)))
					}
					continue
				}

				header := fmt.Sprintf(
					"\n===== %s goroutines=%d sample=%d =====\n",
					at.Format(time.RFC3339Nano), runtime.NumGoroutine(), samples)
				chunk := make([]byte, 0, len(header)+len(body)+1)
				chunk = append(chunk, header...)
				chunk = append(chunk, body...)
				chunk = append(chunk, '\n')
				if appendStallSample(path, chunk) != nil {
					return
				}
				written += int64(len(chunk))
				emitted++
				_ = emitted
			}
		}
	}()
	s.logPacketEvent("game-stall-stack-sampler-started",
		"path", path,
		"interval_ms", interval.Milliseconds(),
		"max_bytes", stallSamplerMaxBytes)
}

func appendStallSample(path string, chunk []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(chunk); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// stallSamplerEnabled 只在显式开启时返回 true（默认关闭）。
func stallSamplerEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DNF90_STALL_SAMPLER"))) {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 2. 无缝分段
// ---------------------------------------------------------------------------

// slowProbeSegments 把一段代码的墙钟切成连续、无缝隙的段落。
//
// 用法：
//
//	seg := newSlowProbeSegments()
//	defer seg.report(s, session, "party-dungeon-loading-release")
//	... 做 A ...
//	seg.mark("A")
//	... 做 B ...
//	seg.mark("B")
//
// report 打出的 total 与各段之和的差即「两个 mark 之外的时间」，所以
// **Σ段 + 未归因 ≡ 总时长** —— 无论慢在哪，都会被某一段或未归因余量吃掉，
// 不可能出现「哪一段都不慢但总共很慢」的空白结论。
type slowProbeSegments struct {
	started time.Time
	last    time.Time
	parts   []string
}

func newSlowProbeSegments() *slowProbeSegments {
	now := time.Now()
	return &slowProbeSegments{started: now, last: now}
}

// mark 把「上一次 mark 到现在」这段耗时记到 name 名下。
func (p *slowProbeSegments) mark(name string) {
	if p == nil {
		return
	}
	now := time.Now()
	p.parts = append(p.parts, fmt.Sprintf("%s=%d", name, now.Sub(p.last).Milliseconds()))
	p.last = now
}

// report 无条件打出一行：总时长 + 未归因余量 + 各段明细。
//
// 注意是**无条件**：不设阈值。这样即使某一轮卡得轻，也能拿到完整分布，
// 不会出现「跑了一整轮却什么都没记录」。
func (p *slowProbeSegments) report(
	s *Service, session *gameSession, source string, extra ...any,
) {
	if p == nil || s == nil {
		return
	}
	total := time.Since(p.started)
	// 各段之和 = 最后一次 mark 距起点的时长。
	covered := p.last.Sub(p.started)
	residual := total - covered
	args := []any{
		"source", source,
		"total_ms", total.Milliseconds(),
		"unattributed_ms", residual.Milliseconds(),
		"segments", strings.Join(p.parts, ","),
	}
	args = append(args, extra...)
	s.logGameEvent(session, "game-slow-segments", args...)
}
