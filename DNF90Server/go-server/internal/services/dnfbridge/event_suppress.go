package dnfbridge

import (
	"os"
	"strconv"
	"strings"
)

// suppressedEventIDs 是运营在 /opt/dnf90/conf/server.env 里用
// DNF90_SUPPRESS_EVENT_IDS=2365,2395 指定的"不下发"活动 ID（逗号分隔）。
//
// 用途：op108 活动表里有哪个 ID，客户端就画哪个活动入口图标。要让某个活动
// "看起来被关掉"，最直接的办法就是整行不下发（全职Lv预约 222 就是这么关的）。
// 进程启动时读一次，改完重启 dnf90server 生效，不需要重新编译。
var suppressedEventIDs = parseSuppressedEventIDs(os.Getenv("DNF90_SUPPRESS_EVENT_IDS"))

func parseSuppressedEventIDs(raw string) map[uint16]struct{} {
	out := make(map[uint16]struct{})
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if id, err := strconv.ParseUint(trimmed, 10, 16); err == nil {
			out[uint16(id)] = struct{}{}
		}
	}
	return out
}

// eventSuppressed 报告某个活动 ID 是否被运营屏蔽。
func eventSuppressed(id uint16) bool {
	_, found := suppressedEventIDs[id]
	return found
}

// suppressAllEventRows 是排查开关：DNF90_SUPPRESS_ALL_EVENT_ROWS=1 时 op108 只发
// 活动表的锚点行（event 9），其余基础行/活跃行全部不发。
//
// 用途：判断"某个活动图标到底是不是由服务端活动表驱动"——全关掉图标还在，
// 就说明那个图标是客户端本地画的，服务端这边怎么改都不会消失。
var suppressAllEventRows = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DNF90_SUPPRESS_ALL_EVENT_ROWS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()

// suppressActiveEventRows：只清空"活跃事件"段（基础行照旧下发）。
// 用来判断某个活动图标是不是由活跃行驱动 —— 活跃行决定"事件处于开启状态"，
// 很多图标是看这一位而不是看基础行。
var suppressActiveEventRows = func() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DNF90_SUPPRESS_ACTIVE_EVENT_ROWS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}()
