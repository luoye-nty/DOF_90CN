//go:build paid

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
)

// TestPsGrantableAmount 锁住"账号级每日限额"的算术口径。
//
// 口径（与运营确认）：limit=0 不限量；limit>0 时同一账号一天最多拿到 limit 点券，
// 最后一轮按剩余额度发（不超发也不浪费）。
func TestPsGrantableAmount(t *testing.T) {
	cases := []struct {
		name       string
		points     int
		limit      int
		usedToday  int64
		wantAmount int
	}{
		{"不限额按配置发", 100, 0, 0, 100},
		{"不限额且已发很多也不受影响", 100, 0, 999999, 100},
		{"首轮足额发", 100, 1000, 0, 100},
		{"还剩 150 足额发", 100, 1000, 850, 100},
		{"只剩 50 按剩余发", 100, 1000, 950, 50},
		{"刚好用完不发", 100, 1000, 1000, 0},
		{"已超（历史数据）不发", 100, 1000, 1200, 0},
		{"限额小于每轮数量：首轮只发限额", 300, 100, 0, 100},
		{"每轮数量非法不发", 0, 1000, 0, 0},
	}
	for _, item := range cases {
		if got := psGrantableAmount(item.points, item.limit, item.usedToday); got != item.wantAmount {
			t.Fatalf("%s: psGrantableAmount(%d,%d,%d) = %d, want %d",
				item.name, item.points, item.limit, item.usedToday, got, item.wantAmount)
		}
	}
}

// TestPsIsTimeout 锁住"超时"这类错误的识别：只有它才有资格走"超时后核对余额"的补救路径。
func TestPsIsTimeout(t *testing.T) {
	timeoutErrors := []error{
		context.DeadlineExceeded,
		fmt.Errorf("GM 请求失败: %w", context.DeadlineExceeded),
		errors.New(`GM 请求失败: Post "http://127.0.0.1:18111/local/dnf-gm/wallet/update": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`),
		&net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
		errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: i/o timeout"),
	}
	for _, err := range timeoutErrors {
		if !psIsTimeout(err) {
			t.Fatalf("应识别为超时: %v", err)
		}
	}
	notTimeout := []error{
		nil,
		errors.New("服务端拒绝发放: wallet balance exceeds client range"),
		errors.New("连接 DNF90Server 原生 GM 接口: dial tcp 127.0.0.1:18111: connectex: No connection could be made"),
		errors.New("GM 接口 HTTP 428"),
	}
	for _, err := range notTimeout {
		if psIsTimeout(err) {
			t.Fatalf("不应识别为超时: %v", err)
		}
	}
}
