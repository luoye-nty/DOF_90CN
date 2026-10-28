package dnfbridge

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseCurrentPurchaseLimitSectionReadsNewField(t *testing.T) {
	document := mustCurrentPurchaseLimitDocument(t, "[purchase limit]\n`account` `weekly` 10\n")
	policy, found, err := parseCurrentPurchaseLimitSection(490004806, document)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if policy.ItemID != 490004806 || policy.Scope != purchaselimit.ScopeAccount ||
		policy.Period != purchaselimit.PeriodWeekly || policy.Limit != 10 ||
		policy.Section != currentPurchaseLimitSection {
		t.Fatalf("policy=%+v", policy)
	}
}

func TestParseCurrentPurchaseLimitSectionReadsLegacyDailyField(t *testing.T) {
	document := mustCurrentPurchaseLimitDocument(t, "[daily purchase limit]\n`character` 1\n")
	policy, found, err := parseCurrentPurchaseLimitSection(10154547, document)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if policy.Scope != purchaselimit.ScopeCharacter || policy.Period != purchaselimit.PeriodDaily ||
		policy.Limit != 1 || policy.Section != currentDailyPurchaseLimitSection {
		t.Fatalf("policy=%+v", policy)
	}
}

func TestParseCurrentPurchaseLimitSectionRejectsUnknownShape(t *testing.T) {
	document := mustCurrentPurchaseLimitDocument(t, "[purchase limit]\n`account` 10\n")
	if _, found, err := parseCurrentPurchaseLimitSection(1, document); err == nil || found {
		t.Fatalf("found=%t err=%v want malformed error", found, err)
	}
	document = mustCurrentPurchaseLimitDocument(t, "[purchase limit]\n`clan` `weekly` 10\n")
	if _, _, err := parseCurrentPurchaseLimitSection(1, document); err == nil {
		t.Fatal("unknown scope accepted")
	}
	document = mustCurrentPurchaseLimitDocument(t, "[purchase limit]\n`account` `fortnightly` 10\n")
	if _, _, err := parseCurrentPurchaseLimitSection(1, document); err == nil {
		t.Fatal("unknown period accepted")
	}
}

func TestParseCurrentPurchaseLimitSectionAbsent(t *testing.T) {
	document := mustCurrentPurchaseLimitDocument(t, "[stack limit]\n999\n")
	if _, found, err := parseCurrentPurchaseLimitSection(1, document); found || err != nil {
		t.Fatalf("found=%t err=%v", found, err)
	}
}

// 真机 PVF：确认三件真实道具的限购字段解析结果（新字段 / 旧字段 / 装备）。
func TestRealPVFPurchaseLimitSections(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		itemID  uint32
		scope   purchaselimit.Scope
		period  purchaselimit.Period
		limit   uint32
		section string
	}{
		{490004806, purchaselimit.ScopeAccount, purchaselimit.PeriodWeekly, 10, currentPurchaseLimitSection},
		{10154547, purchaselimit.ScopeCharacter, purchaselimit.PeriodDaily, 1, currentDailyPurchaseLimitSection},
		{400350073, purchaselimit.ScopeCharacter, purchaselimit.PeriodAccumulate, 1, currentPurchaseLimitSection},
	}
	for _, want := range cases {
		definition, err := items.ResolveItem(want.itemID)
		if err != nil {
			t.Fatalf("resolve item=%d: %v", want.itemID, err)
		}
		if definition.PurchaseLimitError != "" {
			t.Fatalf("item=%d purchase limit parse error: %s", want.itemID, definition.PurchaseLimitError)
		}
		if !definition.PurchaseLimitFound {
			t.Fatalf("item=%d has no purchase limit (path=%s)", want.itemID, definition.PVFPath)
		}
		got := definition.PurchaseLimit
		if got.Scope != want.scope || got.Period != want.period || got.Limit != want.limit || got.Section != want.section {
			t.Fatalf("item=%d policy=%+v want scope=%s period=%s limit=%d section=%s",
				want.itemID, got, want.scope, want.period, want.limit, want.section)
		}
	}
}

func mustCurrentPurchaseLimitDocument(t *testing.T, text string) *dnfpvf.Document {
	t.Helper()
	document, err := dnfpvf.Parse("test.stk", text)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// 全库回归：Script.pvf 里每一个带限购字段的脚本都必须能被解析器读出来。
// 这是“畸形字段按不限购处理”这条兜底的守门人——真出现读不懂的写法时要在这里
// 暴露出来，而不是悄悄放行。
func TestRealPVFPurchaseLimitCorpusParses(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	bothSections := 0
	shapes := make(map[string]int)
	buckets := make(map[string]int)
	failures := make([]string, 0, 8)
	for _, file := range archive.Files() {
		// 只有道具脚本参与限购：stackable/equipment 才是道具目录解析的对象。
		// etc/** 里的同名段落是别的系统（如 pvpcolosseum.etc 的两个数字）。
		if !strings.HasPrefix(file.ArchivePath, "stackable/") && !strings.HasPrefix(file.ArchivePath, "equipment/") {
			continue
		}
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		lower := strings.ToLower(text)
		hasNew := strings.Contains(lower, "["+currentPurchaseLimitSection+"]")
		hasLegacy := strings.Contains(lower, "["+currentDailyPurchaseLimitSection+"]")
		if !hasNew && !hasLegacy {
			continue
		}
		files++
		// 两套字段同时出现的道具：解析器只取新字段（旧字段被忽略）。这里统计规模，
		// 一旦不是 0 就要确认"同一件道具是否需要同时受两条限购约束"。
		if hasNew && hasLegacy {
			bothSections++
		}
		document, parseErr := dnfpvf.Parse(file.ArchivePath, text)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", file.ArchivePath, parseErr)
		}
		policy, found, err := parseCurrentPurchaseLimitSection(1, document)
		if err != nil {
			// 空的段落与语义不明的取值（如 `trade`）是“没有可执行限购”，
			// 解析器已经把它记成 PurchaseLimitError 并放行；这里只要求它
			// 不把可识别的写法判错，畸形写法单独归档。
			raw := currentPurchaseLimitRawSection(text)
			switch {
			case strings.TrimSpace(raw) == "["+currentDailyPurchaseLimitSection+"]" ||
				strings.TrimSpace(raw) == "["+currentPurchaseLimitSection+"]":
				buckets["empty_section"]++
			case strings.Contains(raw, "`"+currentPurchaseLimitUnknownTradeScope+"`"):
				buckets["unknown_scope_trade"]++
			default:
				failures = append(failures, file.ArchivePath+" :: "+raw)
			}
			continue
		}
		if !found {
			t.Fatalf("limit section of %s was not detected", file.ArchivePath)
		}
		shapes[fmt.Sprintf("%s %s %s %d", policy.Section, policy.Scope, policy.Period, policy.Limit)]++
	}
	if files < 400 {
		t.Fatalf("only %d files declared a purchase limit; the corpus scan looks wrong", files)
	}
	t.Logf("purchase-limit corpus: files=%d shapes=%d both_sections=%d buckets=%v unparsed=%d", files, len(shapes), bothSections, buckets, len(failures))
	for shape, count := range shapes {
		t.Logf("  %4d  %s", count, shape)
	}
	for _, failure := range failures {
		t.Logf("  UNPARSED %s", failure)
	}
	if len(failures) > 0 {
		t.Fatalf("%d purchase-limit sections could not be parsed", len(failures))
	}
}

// currentPurchaseLimitRawSection 取限购字段的原文，便于诊断畸形写法。
func currentPurchaseLimitRawSection(text string) string {
	lower := strings.ToLower(text)
	index := strings.Index(lower, "["+currentPurchaseLimitSection+"]")
	if index < 0 {
		index = strings.Index(lower, "["+currentDailyPurchaseLimitSection+"]")
	}
	if index < 0 {
		return ""
	}
	rest := text[index:]
	if end := strings.Index(rest, "\n["); end > 0 {
		rest = rest[:end]
	}
	return strings.Join(strings.Fields(rest), " ")
}
