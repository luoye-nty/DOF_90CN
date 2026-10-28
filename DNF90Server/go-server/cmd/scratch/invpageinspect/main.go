// invpageinspect 只读查看某个角色主背包某一页的落点分布，判断"这一页到底还能不能放"。
//
// 用途（现场 2026-10-03 服主报）：时装分解的徽章产出会溢出邮件、领取还报"物品栏不足"。
// 要分清是"页真的满了"、"[stack limit] 顶住"、还是"页没满但发放/领取路径算错了"，
// 只能看库里真实的行分布。
//
// 只读：只 SELECT。
//
// 跑法：
//
//	go run ./cmd/scratch/invpageinspect -character 1 -page 289-344
//	go run ./cmd/scratch/invpageinspect -character 7 -page 289-344 -dsn "<mysql dsn>"
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
	characterID := flag.String("character", "", "character id to inspect (required)")
	page := flag.String("page", "289-344", "main-inventory slot range, e.g. 289-344")
	listType := flag.Int("list", 0, "inventory list type stored in the entry key")
	itemID := flag.Int64("item", 0, "optional item id to dump across the whole container")
	mailID := flag.Int64("mail", 0, "optional mail id to dump (state + attachments)")
	questID := flag.Int64("quest", 0, "optional quest id to dump (state + extra)")
	dsn := flag.String("dsn", "", "mysql dsn (default: the deployed runtime logic.toml)")
	limit := flag.Int("limit", 200, "max rows to print")
	flag.Parse()
	if *characterID == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -character is required")
		os.Exit(2)
	}
	start, end, err := parsePage(*page)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(2)
	}
	resolvedDSN := *dsn
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	fmt.Printf("schema=%s table=%s character=%s list=%d page=%d..%d\n\n",
		schema, table, *characterID, *listType, start, end)

	rows, err := db.QueryContext(ctx,
		"SELECT entry_key, item_id, item_count FROM `"+schema+"`.`"+table+
			"` WHERE character_id = ? ORDER BY entry_key", *characterID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: query:", err)
		os.Exit(1)
	}
	defer rows.Close()
	type row struct {
		key   string
		item  int64
		count int64
	}
	inPage := map[int]row{}
	allLists := map[string]int{}
	for rows.Next() {
		var r row
		var item, count sql.NullInt64
		if err := rows.Scan(&r.key, &item, &count); err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: scan:", err)
			os.Exit(1)
		}
		r.item, r.count = item.Int64, count.Int64
		keyList, slot, ok := parseEntryKey(r.key)
		if !ok {
			allLists["unparsed:"+r.key]++
			continue
		}
		allLists[fmt.Sprintf("list %d", keyList)]++
		if keyList != *listType || slot < start || slot > end {
			continue
		}
		inPage[slot] = r
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: rows:", err)
		os.Exit(1)
	}

	slots := make([]int, 0, len(inPage))
	for slot := range inPage {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	for index, slot := range slots {
		if index >= *limit {
			fmt.Printf("... (%d more rows)\n", len(slots)-index)
			break
		}
		r := inPage[slot]
		fmt.Printf("  slot %3d  item=%-12d count=%-6d key=%s\n", slot, r.item, r.count, r.key)
	}

	free := make([]int, 0, end-start+1)
	for slot := start; slot <= end; slot++ {
		if _, occupied := inPage[slot]; !occupied {
			free = append(free, slot)
		}
	}
	fmt.Printf("\n这一页：行数=%d  空格数=%d\n", len(slots), len(free))
	fmt.Printf("空格: %s\n", compactSlots(free))
	if len(slots) > 0 {
		fmt.Printf("占用: %s\n", compactSlots(slots))
	}

	totals := map[int64]int64{}
	for _, r := range inPage {
		totals[r.item] += r.count
	}
	if len(totals) > 0 {
		fmt.Printf("\n本页各道具总量（[stack limit] 判定用的是整个容器，不只是本页）：\n")
		ids := make([]int64, 0, len(totals))
		for id := range totals {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			containerTotal, containerRows, err := containerItemTotal(ctx, db, schema, table, *characterID, *listType, id)
			if err != nil {
				return
			}
			fmt.Printf("  item %-12d 本页 %-6d 整容器 %-6d（%d 行）\n", id, totals[id], containerTotal, containerRows)
		}
	}

	otherLists := make([]string, 0, len(allLists))
	for name := range allLists {
		otherLists = append(otherLists, name)
	}
	sort.Strings(otherLists)
	fmt.Printf("\n该角色所有容器：\n")
	for _, name := range otherLists {
		fmt.Printf("  %-20s %d 行\n", name, allLists[name])
	}

	if *itemID > 0 {
		fmt.Printf("\n指定道具 item=%d 在容器里的分布：\n", *itemID)
		itemRows, err := db.QueryContext(ctx,
			"SELECT entry_key, item_count FROM `"+schema+"`.`"+table+
				"` WHERE character_id = ? AND item_id = ? ORDER BY entry_key", *characterID, *itemID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: item query:", err)
			os.Exit(1)
		}
		defer itemRows.Close()
		var total int64
		for itemRows.Next() {
			var key string
			var count sql.NullInt64
			if err := itemRows.Scan(&key, &count); err != nil {
				fmt.Fprintln(os.Stderr, "FAILED: item scan:", err)
				os.Exit(1)
			}
			fmt.Printf("  key=%-12s count=%d\n", key, count.Int64)
			total += count.Int64
		}
		fmt.Printf("  合计 %d\n", total)
	}

	if *mailID > 0 {
		fmt.Printf("\n邮件 mail_id=%d：\n", *mailID)
		if err := dumpMail(ctx, db, schema, *mailID); err != nil {
			fmt.Fprintln(os.Stderr, "mail dump FAILED:", err)
		}
	}

	if *questID > 0 {
		fmt.Printf("\n任务 quest_id=%d（char=%s）：\n", *questID, *characterID)
		if err := dumpQuestState(ctx, db, schema, *characterID, *questID); err != nil {
			fmt.Fprintln(os.Stderr, "quest dump FAILED:", err)
		}
	}
}

// dumpQuestState 打印一条任务状态行（含 reward_state / completion_key 与结算回执摘要）。
//
// 现场 2026-10-03：交任务后客户端不显示"完成"，要小退才看见 —— 需要直接看库里
// 这条任务到底是 completed 还是仍 active，以及回执有没有落库。
func dumpQuestState(ctx context.Context, db *sql.DB, schema, characterID string, questID int64) error {
	table, err := firstExistingTable(ctx, db, schema, "quest_states")
	if err != nil || table == "" {
		return fmt.Errorf("no quest_states table: %v", err)
	}
	columns, err := tableColumns(ctx, db, schema, table)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx,
		"SELECT "+strings.Join(columns, ", ")+" FROM `"+schema+"`.`"+table+"` WHERE character_id = ? AND quest_id = ?",
		characterID, questID)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	found := false
	for rows.Next() {
		found = true
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for i, name := range columns {
			if strings.Contains(name, "receipt") {
				fmt.Printf("  %s = <%d bytes>\n", strings.Trim(name, "`"), len(fmt.Sprint(values[i])))
				continue
			}
			fmt.Printf("  %s = %v\n", strings.Trim(name, "`"), values[i])
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		fmt.Printf("  没有 quest_id=%d 的状态行\n", questID)
	}
	extraTable, err := firstExistingTable(ctx, db, schema, "quest_state_extra")
	if err != nil || extraTable == "" {
		return nil
	}
	extraColumns, err := tableColumns(ctx, db, schema, extraTable)
	if err != nil {
		return err
	}
	extraRows, err := db.QueryContext(ctx,
		"SELECT "+strings.Join(extraColumns, ", ")+" FROM `"+schema+"`.`"+extraTable+"` WHERE character_id = ? AND quest_id = ?",
		characterID, questID)
	if err != nil {
		return err
	}
	defer extraRows.Close()
	extraValues := make([]any, len(extraColumns))
	extraPointers := make([]any, len(extraColumns))
	for i := range extraValues {
		extraPointers[i] = &extraValues[i]
	}
	fmt.Printf("  extra：\n")
	for extraRows.Next() {
		if err := extraRows.Scan(extraPointers...); err != nil {
			return err
		}
		parts := make([]string, 0, len(extraColumns))
		for i, name := range extraColumns {
			value := fmt.Sprint(extraValues[i])
			if len(value) > 220 {
				value = value[:220] + "..."
			}
			parts = append(parts, fmt.Sprintf("%s=%s", strings.Trim(name, "`"), value))
		}
		fmt.Printf("    %s\n", strings.Join(parts, "  "))
	}
	return extraRows.Err()
}

// dumpMail 打印一封邮件的状态与附件。表名与列名都从 information_schema 里现查，
// 分库分表前缀变了也不用改这里。
func dumpMail(ctx context.Context, db *sql.DB, schema string, mailID int64) error {
	mailsTable, err := firstExistingTable(ctx, db, schema, "mails")
	if err != nil {
		return err
	}
	if mailsTable == "" {
		return fmt.Errorf("no mails table in schema %s", schema)
	}
	columns, err := tableColumns(ctx, db, schema, mailsTable)
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("table %s has no columns", mailsTable)
	}
	fmt.Printf("  表 %s\n", mailsTable)
	rows, err := db.QueryContext(ctx,
		"SELECT "+strings.Join(columns, ", ")+" FROM `"+schema+"`.`"+mailsTable+"` WHERE mail_id = ?", mailID)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	found := false
	for rows.Next() {
		found = true
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for i, name := range columns {
			fmt.Printf("  %s = %v\n", strings.Trim(name, "`"), values[i])
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		fmt.Printf("  没有 mail_id=%d 的行\n", mailID)
	}

	attachmentsTable, err := firstExistingTable(ctx, db, schema, "mail_attachments")
	if err != nil || attachmentsTable == "" {
		return err
	}
	attachmentColumns, err := tableColumns(ctx, db, schema, attachmentsTable)
	if err != nil {
		return err
	}
	attachmentRows, err := db.QueryContext(ctx,
		"SELECT "+strings.Join(attachmentColumns, ", ")+" FROM `"+schema+"`.`"+attachmentsTable+"` WHERE mail_id = ?", mailID)
	if err != nil {
		return err
	}
	defer attachmentRows.Close()
	attachmentValues := make([]any, len(attachmentColumns))
	attachmentPointers := make([]any, len(attachmentColumns))
	for i := range attachmentValues {
		attachmentPointers[i] = &attachmentValues[i]
	}
	fmt.Printf("  附件（表 %s）：\n", attachmentsTable)
	for attachmentRows.Next() {
		if err := attachmentRows.Scan(attachmentPointers...); err != nil {
			return err
		}
		parts := make([]string, 0, len(attachmentColumns))
		for i, name := range attachmentColumns {
			parts = append(parts, fmt.Sprintf("%s=%v", strings.Trim(name, "`"), attachmentValues[i]))
		}
		fmt.Printf("    %s\n", strings.Join(parts, "  "))
	}
	return attachmentRows.Err()
}

// firstExistingTable 在 schema 里找以 suffix 结尾的表（分库分表会带前缀）。
func firstExistingTable(ctx context.Context, db *sql.DB, schema, suffix string) (string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_name LIKE ? ORDER BY LENGTH(table_name)",
		schema, "%_"+suffix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", rows.Err()
}

func tableColumns(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position",
		schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name == "body" || name == "raw_entry" || name == "extra_json" {
			continue
		}
		columns = append(columns, "`"+name+"`")
	}
	return columns, rows.Err()
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

func containerItemTotal(
	ctx context.Context,
	db *sql.DB,
	schema, table, characterID string,
	listType int,
	itemID int64,
) (int64, int, error) {
	prefix := strconv.Itoa(listType) + ":%"
	row := db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(item_count),0), COUNT(*) FROM `"+schema+"`.`"+table+
			"` WHERE character_id = ? AND entry_key LIKE ? AND item_id = ?", characterID, prefix, itemID)
	var total int64
	var count int
	if err := row.Scan(&total, &count); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: container total:", err)
		return 0, 0, err
	}
	return total, count, nil
}

func parsePage(value string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(value), "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad -page %q, want START-END", value)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad -page start %q: %w", parts[0], err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad -page end %q: %w", parts[1], err)
	}
	if start < 0 || end < start {
		return 0, 0, fmt.Errorf("bad -page %q", value)
	}
	return start, end, nil
}

// dsnFromRuntimeConfig 读部署目录里的 logic.toml，用与线上完全同一个 DSN。
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

// locateInventoryTable 找出真正的角色背包表（区分表名必须精确，account_inventory_items
// 也含 "inventory_items" 子串，分库分表后还会带区服/世界前缀）。
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
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("no character inventory_items table found")
}

// compactSlots 把连续槽位压成 "289-296, 300" 便于人读。
func compactSlots(slots []int) string {
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
