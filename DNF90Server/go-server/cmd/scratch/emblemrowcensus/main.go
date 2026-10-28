// emblemrowcensus 只读统计：每个角色在徽章页 289..344 里有多少行、空格落在哪几段；
// 以及 321..344 这"最后一整行之后"的格子到底有没有被用过。
//
// 现场 2026-10-03（服主报）：服主说徽章栏总共 63 格，而服务端把落点夹到 289..320
// （main_slot_count=0 时），于是 321..344 永远不放东西。要判断这些格子到底能不能用，
// 就看真实库里**已经**有没有行落在 321..344 —— 有，就说明服务端白留着这 24 格。
//
// 只读：只 SELECT。
//
// 跑法：
//
//	go run ./cmd/scratch/emblemrowcensus
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	pageStart := flag.Int("page-start", 289, "emblem page first slot")
	pageEnd := flag.Int("page-end", 344, "emblem page last slot")
	probeStart := flag.Int("probe-start", 321, "the range the server never places into")
	dsn := flag.String("dsn", "", "mysql dsn (default: the deployed runtime logic.toml)")
	flag.Parse()

	resolvedDSN := *dsn
	var err error
	if resolvedDSN == "" {
		resolvedDSN, err = dsnFromRuntimeConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAILED:", err)
			os.Exit(2)
		}
	}
	db, err := sql.Open("mysql", resolvedDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: mysql unreachable:", err)
		os.Exit(1)
	}
	schema, table, err := locateInventoryTable(ctx, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}

	// 逐角色统计这一页。entry_key 形如 "0:<slot>"。
	rows, err := db.QueryContext(ctx,
		"SELECT character_id, entry_key, item_id, item_count FROM `"+schema+"`.`"+table+"` ORDER BY character_id")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: query:", err)
		os.Exit(1)
	}
	defer rows.Close()
	type pageState struct {
		slots map[int]int64
	}
	byCharacter := map[string]*pageState{}
	probeHits := map[string][]int{}
	probeTotals := 0
	for rows.Next() {
		var characterID, key string
		var itemID, count sql.NullInt64
		if err := rows.Scan(&characterID, &key, &itemID, &count); err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: scan:", err)
			os.Exit(1)
		}
		listType, slot, ok := parseEntryKey(key)
		if !ok || listType != 0 {
			continue
		}
		if slot >= *probeStart && slot <= *pageEnd {
			probeHits[characterID] = append(probeHits[characterID], slot)
			probeTotals++
		}
		if slot < *pageStart || slot > *pageEnd {
			continue
		}
		state := byCharacter[characterID]
		if state == nil {
			state = &pageState{slots: map[int]int64{}}
			byCharacter[characterID] = state
		}
		state.slots[slot] = count.Int64
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: rows:", err)
		os.Exit(1)
	}

	ids := make([]string, 0, len(byCharacter))
	for id := range byCharacter {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, _ := strconv.Atoi(ids[i])
		right, _ := strconv.Atoi(ids[j])
		return left < right
	})
	fmt.Printf("徽章页 %d..%d 逐角色统计（只列有行的角色）：\n", *pageStart, *pageEnd)
	for _, id := range ids {
		state := byCharacter[id]
		used := make([]int, 0, len(state.slots))
		for slot := range state.slots {
			used = append(used, slot)
		}
		sort.Ints(used)
		free := make([]int, 0, *pageEnd-*pageStart+1)
		for slot := *pageStart; slot <= *pageEnd; slot++ {
			if _, occupied := state.slots[slot]; !occupied {
				free = append(free, slot)
			}
		}
		lastUsed := 0
		if len(used) > 0 {
			lastUsed = used[len(used)-1]
		}
		fmt.Printf("  char %-6s 行=%-3d 空格=%-3d 最大已用格=%d  空格段=%s\n",
			id, len(used), len(free), lastUsed, compact(free))
	}

	fmt.Printf("\n== %d..%d 段（服务端在 main_slot_count=0 时从不放置的区间）==\n", *probeStart, *pageEnd)
	if probeTotals == 0 {
		fmt.Printf("整库没有任何一行落在这里。\n")
	} else {
		fmt.Printf("整库共 %d 行落在这里，涉及 %d 个角色：\n", probeTotals, len(probeHits))
		probeIDs := make([]string, 0, len(probeHits))
		for id := range probeHits {
			probeIDs = append(probeIDs, id)
		}
		sort.Slice(probeIDs, func(i, j int) bool {
			left, _ := strconv.Atoi(probeIDs[i])
			right, _ := strconv.Atoi(probeIDs[j])
			return left < right
		})
		for _, id := range probeIDs {
			slots := probeHits[id]
			sort.Ints(slots)
			fmt.Printf("  char %-6s %d 行：%s\n", id, len(slots), compact(slots))
		}
	}
}

func parseEntryKey(key string) (int, int, bool) {
	listPart, slotPart, found := strings.Cut(strings.TrimSpace(key), ":")
	if !found {
		return 0, 0, false
	}
	list, err := strconv.Atoi(listPart)
	if err != nil {
		return 0, 0, false
	}
	slot, err := strconv.Atoi(slotPart)
	if err != nil {
		return 0, 0, false
	}
	return list, slot, true
}

func dsnFromRuntimeConfig() (string, error) {
	candidates := []string{
		`runtime\configs\dnf\logic.toml`,
		`..\runtime\configs\dnf\logic.toml`,
	}
	for _, path := range candidates {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "mysql_dsn") {
				continue
			}
			_, value, found := strings.Cut(trimmed, "=")
			if !found {
				continue
			}
			return strings.Trim(strings.TrimSpace(value), `"`), nil
		}
	}
	return "", fmt.Errorf("mysql_dsn not found; pass -dsn explicitly")
}

func locateInventoryTable(ctx context.Context, db *sql.DB) (string, string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_schema, table_name FROM information_schema.tables WHERE table_name LIKE '%\\_inventory\\_items' ORDER BY table_schema, table_name")
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return "", "", err
		}
		if strings.Contains(table, "account") {
			continue
		}
		return schema, table, nil
	}
	return "", "", fmt.Errorf("no character inventory_items table found")
}

func compact(slots []int) string {
	if len(slots) == 0 {
		return "(none)"
	}
	var builder strings.Builder
	runStart, previous := slots[0], slots[0]
	flush := func(from, to int) {
		if builder.Len() > 0 {
			builder.WriteString(", ")
		}
		if from == to {
			fmt.Fprintf(&builder, "%d", from)
			return
		}
		fmt.Fprintf(&builder, "%d-%d", from, to)
	}
	for _, slot := range slots[1:] {
		if slot == previous+1 {
			previous = slot
			continue
		}
		flush(runStart, previous)
		runStart, previous = slot, slot
	}
	flush(runStart, previous)
	return builder.String()
}
