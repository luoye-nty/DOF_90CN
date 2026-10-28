package main

// 副本记录「三合一」：同一局副本服务端会写三条（进入 / 通关 / 离开），
// 查询列表里按"一次副本 = 一行"显示。
//
// 分组键（按优先级）：
//  1. started_at —— 进副本时刻，**三条记录都带同一个值**，是最可靠的分组依据；
//  2. lifecycle_token —— 有就一起带上（同一秒连刷两个本时用它区分）；
//  3. 两者都缺时**不合并**（宁可多显示一行，也不能把两局不相干的副本并成一条）。
//
// 代表记录：离开 > 通关 > 进入。越晚的记录字段越全（用时/击杀/无色消耗都在"离开"那条上）。
// 合并方式：以代表记录为底，把其余两条里**它没有的**字段补进来，
// 并附一个「原始事件」说明（例如"进入副本+通关+离开副本"）。
//
// 为什么不直接在服务端只写一条：底层三条是排查"崩溃 / 掉线 / 被踢"的唯一线索
// （这类局根本没有"离开"记录）。界面合并既满足"一次副本一行"，又不丢证据。

import (
	"fmt"
	"sort"
	"strings"
)

// recMergedEventsField 合并后附加的字段名（说明这条是由哪几个原始事件合成的）。
const recMergedEventsField = "merged_events"

// recordKindIsDungeon 判断本次查询是不是副本类（只有副本才需要三合一）。
func recordKindIsDungeon(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(kind), RecordKindDungeon)
}

// dungeonEventPriority 事件优先级：数字越大越"后"，字段越全，越适合当代表记录。
func dungeonEventPriority(event string) int {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "exit":
		return 3
	case "clear":
		return 2
	case "enter":
		return 1
	default:
		return 0
	}
}

// dungeonMergeKey 一局副本的分组键；ok=false 表示这条记录没法可靠分组（不参与合并）。
func dungeonMergeKey(record map[string]any) (string, bool) {
	startedAt := recString(record, "started_at")
	token := recString(record, "lifecycle_token")
	if startedAt == "" && token == "" {
		return "", false
	}
	// 账号 + 角色 + 副本 + （started_at | token）一起做键：
	// 只按时间分组，会把"同一秒里不同角色/不同副本"的记录并到一起。
	var builder strings.Builder
	builder.WriteString(recString(record, "account_id"))
	builder.WriteString("|")
	builder.WriteString(recString(record, "character_id"))
	builder.WriteString("|")
	builder.WriteString(recString(record, "dungeon_id"))
	builder.WriteString("|")
	builder.WriteString(startedAt)
	builder.WriteString("|")
	builder.WriteString(token)
	return builder.String(), true
}

// mergeDungeonRecords 把一页副本记录按"一局"合并。
//
// 输入是服务端返回的顺序（新的在前），输出保持"每个分组第一次出现的顺序"，
// 也就是仍然新的在前；每个分组内的 ts 取**最早**那条（进副本时刻），
// 这样列表看起来就是"什么时候进的这个本"。
func mergeDungeonRecords(records []map[string]any) []map[string]any {
	if len(records) == 0 {
		return records
	}
	type group struct {
		key     string
		records []map[string]any
	}
	order := make([]string, 0, len(records))
	groups := make(map[string]*group, len(records))
	standalone := make([]map[string]any, 0, len(records))

	for _, record := range records {
		key, ok := dungeonMergeKey(record)
		if !ok {
			standalone = append(standalone, record)
			continue
		}
		item, exists := groups[key]
		if !exists {
			item = &group{key: key}
			groups[key] = item
			order = append(order, key)
		}
		item.records = append(item.records, record)
	}

	merged := make([]map[string]any, 0, len(order)+len(standalone))
	for _, key := range order {
		item := groups[key]
		if len(item.records) == 1 {
			merged = append(merged, item.records[0])
			continue
		}
		merged = append(merged, mergeDungeonGroup(item.records))
	}
	merged = append(merged, standalone...)
	return merged
}

// mergeDungeonGroup 合并同一局的若干条记录。
func mergeDungeonGroup(records []map[string]any) map[string]any {
	// 1) 选代表记录：事件优先级最高者；同优先级取字段最多的（信息最全）。
	representative := records[0]
	for _, record := range records[1:] {
		betterPriority := dungeonEventPriority(recString(record, "event")) > dungeonEventPriority(recString(representative, "event"))
		samePriorityButRicher := dungeonEventPriority(recString(record, "event")) == dungeonEventPriority(recString(representative, "event")) &&
			len(record) > len(representative)
		if betterPriority || samePriorityButRicher {
			representative = record
		}
	}

	out := make(map[string]any, len(representative)+4)
	for key, value := range representative {
		out[key] = value
	}
	// 2) 补字段：其它记录里有、代表记录没有（或为空）的键补进来。
	for _, record := range records {
		if sameRecord(record, representative) {
			continue
		}
		for key, value := range record {
			if existing, ok := out[key]; ok && !isEmptyRecValue(existing) {
				continue
			}
			if isEmptyRecValue(value) {
				continue
			}
			out[key] = value
		}
	}

	// 3) 时间列用**最早**的一条（进副本时刻），便于按"什么时候进的"看列表。
	out["ts"] = earliestRecTimestamp(records, recString(representative, "ts"))
	// 4) 说明这条是合并出来的（详情区会显示为「原始事件（已合并）」）。
	out[recMergedEventsField] = mergedEventNames(records)
	return out
}

// sameRecord 判断两条记录是不是同一条（同一条 map 的浅比较：ts+event 相同且字段数相同）。
func sameRecord(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	return recString(a, "ts") == recString(b, "ts") &&
		recString(a, "event") == recString(b, "event") &&
		recString(a, "event_name") == recString(b, "event_name")
}

// isEmptyRecValue 判断值是否"没内容"（缺失/空串/nil），补字段时以它为准。
func isEmptyRecValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// earliestRecTimestamp 取组内最早的 ts（RFC3339Nano 字符串按字典序即时间序；
// 无法解析时退回代表记录的 ts）。
func earliestRecTimestamp(records []map[string]any, fallback string) string {
	stamps := make([]string, 0, len(records))
	for _, record := range records {
		if stamp := strings.TrimSpace(recString(record, "ts")); stamp != "" {
			stamps = append(stamps, stamp)
		}
	}
	if len(stamps) == 0 {
		return fallback
	}
	sort.Strings(stamps)
	return stamps[0]
}

// mergedEventNames 组内事件的中文名列表（按时间顺序，去重）。
func mergedEventNames(records []map[string]any) string {
	indexed := make([]struct {
		stamp string
		name  string
	}, 0, len(records))
	for _, record := range records {
		name := recString(record, "event_name")
		if name == "" {
			name = recString(record, "event")
		}
		if name == "" {
			continue
		}
		indexed = append(indexed, struct {
			stamp string
			name  string
		}{stamp: recString(record, "ts"), name: name})
	}
	sort.SliceStable(indexed, func(i, j int) bool { return indexed[i].stamp < indexed[j].stamp })

	seen := make(map[string]struct{}, len(indexed))
	names := make([]string, 0, len(indexed))
	for _, item := range indexed {
		if _, dup := seen[item.name]; dup {
			continue
		}
		seen[item.name] = struct{}{}
		names = append(names, item.name)
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%s（共 %d 条原始记录已合并）", strings.Join(names, "+"), len(records))
}
