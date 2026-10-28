package dnfbridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The goroutine watchdog keeps a rolling stack dump on disk.
//
// Live incident 2026-09-12 11:23: both game sessions stopped sending anything the
// moment the initial-town route finished (no RECV at all afterwards) while the
// channel accept loop kept logging, and a client restart did not help. That is the
// signature of a handler blocked on a shared mutex: the reader loop and the
// handler share one goroutine, so a blocked handler also stops the RECV logging
// for that connection, and a second session blocking on the same mutex looks
// exactly the same. MySQL showed no stuck transaction and the admin port stayed
// healthy, so the frame that blocks has to be read off the stacks.
//
// Switch: DNF90_GOROUTINE_WATCHDOG / goroutine_watchdog.txt
//
//	off      disable
//	<anything else or empty> dump every currentGoroutineWatchdogInterval to
//	         runtime/goroutine_dump.txt (overwritten each round, always the latest
//	         state, no growth)
func (s *Service) startGoroutineWatchdog(ctx context.Context) {
	if s == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(goroutineWatchdogMode()), "off") {
		return
	}
	// 快照与包日志同目录；收件人公钥固化在二进制里，服务端不需要任何密钥文件。
	logDir := filepath.Dir(s.options.packetLogPath)
	go func() {
		ticker := time.NewTicker(currentGoroutineWatchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-ticker.C:
				buf := make([]byte, 1<<20)
				size := runtime.Stack(buf, true)
				payload := fmt.Sprintf("# goroutine dump %s goroutines=%d\n%s",
					at.Format(time.RFC3339), runtime.NumGoroutine(), buf[:size])
				// 落盘即密文（快照含函数名/源码路径/内存地址）；公钥不可用时只告警、不写明文。
				if err := writeEncryptedGoroutineDump(logDir, []byte(payload)); err != nil {
					slog.Default().Warn("dnfbridge goroutine dump not written",
						"dir", logDir, "error", err)
				}
			}
		}
	}()
}

const currentGoroutineWatchdogInterval = 20 * time.Second

func goroutineWatchdogMode() string {
	const key = "DNF90_GOROUTINE_WATCHDOG"
	raw := strings.TrimSpace(os.Getenv(key))
	return raw
}
