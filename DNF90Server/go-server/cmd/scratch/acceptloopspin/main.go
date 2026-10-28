// 最小复现：旧 acceptLoop 在“listener 被关闭但 ctx 仍然有效”时的行为。
//
// 场景来源：频道热重载关掉一个游戏端口（closeGamePort）只 Close listener，不取消 runCtx
// （取消会把整个服务停掉）。旧循环只判 ctx.Err()，于是 Accept 的 closed 错误被当作可重试，
// continue 变成死循环：一个核 100% 空转 + 每圈一条 warn。
//
// 跑法：go run ./cmd/scratch/acceptloopspin
package main

import (
	"errors"
	"fmt"
	"net"
	"time"
)

func isClosedErr(err error) bool { return errors.Is(err, net.ErrClosed) }

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		panic(err)
	}

	// 旧逻辑：ctx 永远不取消（模拟热重载只关 listener）。
	oldIterations := 0
	oldWarnings := 0
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, acceptErr := listener.Accept()
		if acceptErr != nil {
			// 旧代码在这一点上只看 ctx.Err()（此处为 nil），于是 continue。
			oldIterations++
			oldWarnings++
			continue
		}
	}

	// 新逻辑：closed 错误直接退出。
	newIterations := 0
	newDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(newDeadline) {
		_, acceptErr := listener.Accept()
		if acceptErr != nil {
			if isClosedErr(acceptErr) {
				break // 修复后的行为：第一次就退出
			}
			newIterations++
			continue
		}
	}

	fmt.Printf("listener=%s\n", addr)
	fmt.Printf("旧逻辑 200ms 内 Accept 失败次数 = %d（每条还会写一行 warn，等于日志刷屏）\n", oldIterations)
	fmt.Printf("新逻辑 200ms 内 Accept 重试次数 = %d（closed 直接退出）\n", newIterations)
	fmt.Printf("结论：旧逻辑空转 %d 次/200ms ≈ %.0f 次/秒\n", oldIterations, float64(oldIterations)*5)
}
