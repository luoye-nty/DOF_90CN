package main

// 时间选择：界面上所有"时分"输入统一改成两个下拉（小时 0-23 / 分钟 0-59）。
//
// 为什么不再用文本框：运营在文本框里要手打 "20:00"，中文冒号、漏补零（8:00）、
// 打成 25:00 都会被服务端拒绝或需要来回翻译错误；下拉框把这些错误从源头消掉。
//
// 服务端协议不变：仍然收 "HH:MM"，所以这里只负责"下拉索引 ↔ HH:MM"的换算。

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lxn/walk"
)

// timeHourLabels / timeMinuteLabels 下拉选项（"00".."23" / "00".."59"）。
var (
	timeHourLabels   = clockLabels(24)
	timeMinuteLabels = clockLabels(60)
)

// clockLabels 生成 n 个两位数字选项（"00"、"01" …）。
func clockLabels(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%02d", i))
	}
	return out
}

// clockText 由两个下拉索引拼 "HH:MM"（索引越界按 0 处理，绝不产生非法时间）。
func clockText(hourIdx, minuteIdx int) string {
	if hourIdx < 0 || hourIdx >= len(timeHourLabels) {
		hourIdx = 0
	}
	if minuteIdx < 0 || minuteIdx >= len(timeMinuteLabels) {
		minuteIdx = 0
	}
	return timeHourLabels[hourIdx] + ":" + timeMinuteLabels[minuteIdx]
}

// clockIndexFromText 把 "HH:MM" 拆成两个下拉索引；无法解析时 ok=false。
// 容错：中文冒号、前后空格、不补零（"8:5" 也认）。
func clockIndexFromText(raw string) (hourIdx, minuteIdx int, ok bool) {
	normalized := strings.TrimSpace(strings.ReplaceAll(raw, "：", ":"))
	parts := strings.Split(normalized, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// clockComboText 读一对下拉的当前值（控件缺失时返回 "00:00"）。
func clockComboText(hourCb, minuteCb *walk.ComboBox) string {
	hourIdx, minuteIdx := 0, 0
	if hourCb != nil {
		hourIdx = hourCb.CurrentIndex()
	}
	if minuteCb != nil {
		minuteIdx = minuteCb.CurrentIndex()
	}
	return clockText(hourIdx, minuteIdx)
}

// clockComboSet 把 "HH:MM" 回填到一对下拉；解析失败时回退到给定的兜底索引
// （服务端配置里存着历史脏值时，界面不该显示成 00:00 让人误以为被改了）。
func clockComboSet(hourCb, minuteCb *walk.ComboBox, raw string, fallbackHour, fallbackMinute int) {
	hourIdx, minuteIdx, ok := clockIndexFromText(raw)
	if !ok {
		hourIdx, minuteIdx = fallbackHour, fallbackMinute
	}
	if hourCb != nil && hourIdx >= 0 && hourIdx < len(timeHourLabels) {
		hourCb.SetCurrentIndex(hourIdx)
	}
	if minuteCb != nil && minuteIdx >= 0 && minuteIdx < len(timeMinuteLabels) {
		minuteCb.SetCurrentIndex(minuteIdx)
	}
}
