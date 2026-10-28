package channelstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// realChannelInfo 指向本机正在使用的真实 channel_info.etc。
// 不存在时相关用例整体跳过，保证换机器后 go test ./... 不会因为缺文件而红。
const realChannelInfo = `DNF_REAL_CHANNEL_INFO`

// copyFixture 把真实文件复制到临时目录（用例都改副本，绝不碰生产文件）。
func copyFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(realChannelInfo)
	if err != nil {
		t.Skipf("真实 channel_info.etc 不可读，跳过：%v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "channel_info.etc")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("写临时 fixture: %v", err)
	}
	return path
}

// writeFixture 写一个内容自定的 fixture（用于构造保护规则的边界场景）。
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "channel_info.etc")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}
	return path
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	return strings.Split(string(raw), "\n")
}

// TestLoadRealFileParsesChannels 真实文件必须解析出频道，且保护频道被正确标记。
func TestLoadRealFileParsesChannels(t *testing.T) {
	path := copyFixture(t)
	status, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if status.Total < 100 {
		t.Fatalf("Total=%d，真实文件应有上百个频道", status.Total)
	}
	if status.Disabled != 0 {
		t.Fatalf("全新文件的 Disabled=%d，应为 0", status.Disabled)
	}
	if status.ServerID != 1 {
		t.Fatalf("ServerID=%d，期望 1", status.ServerID)
	}
	byID := make(map[int]Entry, status.Total)
	for _, entry := range status.Channels {
		byID[entry.ID] = entry
	}
	for _, id := range []int{bootstrapChannelID, defaultGameChannelID} {
		entry, ok := byID[id]
		if !ok {
			t.Fatalf("保护频道 %d 未解析到", id)
		}
		if !entry.Required || !entry.Enabled || !entry.Ordinary {
			t.Fatalf("频道 %d = %+v，期望 Required/Enabled/Ordinary 均为 true", id, entry)
		}
	}
	if entry := byID[6]; entry.Type != 36 || entry.Group != "goldenroad" {
		t.Fatalf("频道 6 = %+v，期望 goldenroad/36", entry)
	}
}

// TestCloseReopenIsByteExact 关闭再开启必须逐字节还原原文件（含 BOM/缩进/换行）。
func TestCloseReopenIsByteExact(t *testing.T) {
	path := copyFixture(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读原文件: %v", err)
	}
	before, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	changed, status, err := ApplyFile(path, []int{12, 13}, false)
	if err != nil {
		t.Fatalf("关闭频道: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed=%v，期望 2 个频道", changed)
	}
	if status.Total != before.Total {
		t.Fatalf("关闭后 Total=%d，期望仍为 %d（被关频道仍应出现在列表里以便重新开启）", status.Total, before.Total)
	}
	if status.Disabled != 2 || status.Enabled != before.Total-2 {
		t.Fatalf("关闭后 Enabled=%d Disabled=%d，期望 %d/2", status.Enabled, status.Disabled, before.Total-2)
	}
	for _, entry := range status.Channels {
		if (entry.ID == 12 || entry.ID == 13) && entry.Enabled {
			t.Fatalf("频道 %d 关闭后仍标记为 Enabled", entry.ID)
		}
	}

	changed, status, err = ApplyFile(path, []int{12, 13}, true)
	if err != nil {
		t.Fatalf("开启频道: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed=%v，期望 2 个频道", changed)
	}
	if status.Disabled != 0 {
		t.Fatalf("开启后 Disabled=%d，应为 0", status.Disabled)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读还原后文件: %v", err)
	}
	if !bytes.Equal(original, restored) {
		t.Fatalf("关闭→开启后文件与原文件不一致（len %d vs %d）", len(restored), len(original))
	}
}

// TestCloseOnlyRemovesTargetLine 关闭单个频道只允许少掉目标那一行，其余行逐字不变。
func TestCloseOnlyRemovesTargetLine(t *testing.T) {
	path := copyFixture(t)
	before := fileLines(t, path)
	if _, _, err := ApplyFile(path, []int{12}, false); err != nil {
		t.Fatalf("关闭频道 12: %v", err)
	}
	after := fileLines(t, path)
	if len(after) != len(before)-1 {
		t.Fatalf("行数 %d → %d，期望只少 1 行", len(before), len(after))
	}
	removed := 0
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		if before[i] == after[j] {
			i++
			j++
			continue
		}
		if removed > 0 {
			t.Fatalf("除了目标行之外还有差异：before[%d]=%q after[%d]=%q", i, before[i], j, after[j])
		}
		if !strings.Contains(before[i], " 12 ") {
			t.Fatalf("被删的行不是频道 12：%q", before[i])
		}
		removed++
		i++
	}
	if removed != 1 || i != len(before)-1 && i != len(before) {
		t.Fatalf("removed=%d i=%d len(before)=%d，期望恰好删掉 1 行", removed, i, len(before))
	}
}

// TestBootstrapAndDefaultChannelsCanBeClosed 2026-09-30 起，引导频道 19 与默认城镇频道 11
// 允许关闭（服务端把"缺 19 起不来""缺 11 拒绝热重载"都降级为告警），
// 且关闭后必须仍能从侧车清单逐字还原。
func TestBootstrapAndDefaultChannelsCanBeClosed(t *testing.T) {
	path := copyFixture(t)
	before := fileLines(t, path)
	beforeStatus, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// Status.Disabled 是"文件里当前已关闭的频道数"，所以逐个关闭时它是累加的。
	for index, id := range []int{bootstrapChannelID, defaultGameChannelID} {
		changed, status, err := ApplyFile(path, []int{id}, false)
		if err != nil {
			t.Fatalf("频道 %d 现在应允许关闭，却被拒绝: %v", id, err)
		}
		if len(changed) != 1 || changed[0] != id {
			t.Fatalf("频道 %d 关闭结果 changed=%v", id, changed)
		}
		if status.Disabled != index+1 {
			t.Fatalf("关闭 %d 后 Disabled=%d，期望 %d", id, status.Disabled, index+1)
		}
	}
	// 重新开启：Status.Enabled/Disabled 都是"文件里的当前总数"，不是本次改动量。
	if _, status, err := ApplyFile(path, []int{bootstrapChannelID, defaultGameChannelID}, true); err != nil {
		t.Fatalf("重新开启 19/11 失败: %v", err)
	} else if status.Enabled != beforeStatus.Enabled || status.Disabled != 0 {
		t.Fatalf("重新开启后 Enabled/Disabled = %d/%d，期望 %d/0",
			status.Enabled, status.Disabled, beforeStatus.Enabled)
	}
	if after := fileLines(t, path); len(after) != len(before) {
		t.Fatalf("关闭→开启后行数 %d → %d，期望逐字还原", len(before), len(after))
	}
}

// TestLastOrdinaryChannelCanBeClosedButTheLastChannelCannot 关掉最后一个普通城镇频道
// 现在允许（服务端只告警），但"一个频道都不剩"仍然必须被拒绝。
func TestLastOrdinaryChannelCanBeClosedButTheLastChannelCannot(t *testing.T) {
	path := writeFixture(t, "[dungeon]\n`[town]`\n<4::dname_1>\n1\n[/dungeon]\n"+
		"[server] 1\n"+
		"   11   `<chn_channel_info_028>`   36   `[goldenroad]`   0   0   ``\n"+
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\n"+
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"[/server]\n")
	if _, status, err := ApplyFile(path, []int{50}, false); err != nil {
		t.Fatalf("关掉最后一个普通城镇频道现在应被允许: %v", err)
	} else if status.Disabled != 1 {
		t.Fatalf("Disabled=%d，期望 1", status.Disabled)
	}
	// 只剩一个频道时不再允许继续关。
	path2 := writeFixture(t, "[server] 1\n"+
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\n"+
		"[/server]\n")
	if _, _, err := ApplyFile(path2, []int{19}, false); err == nil {
		t.Fatal("一个频道都不剩时必须拒绝")
	}
}

// TestLastSpecialAreaChannelCannotBeClosed 特殊区域频道（type 22）必须至少留一个开启：
// 两条 22 时能关一条，关最后一条必须被拒绝；非 22 频道不受这条规则影响。
func TestLastSpecialAreaChannelCannotBeClosed(t *testing.T) {
	path := writeFixture(t, "[dungeon]\n`[town]`\n<4::dname_1>\n1\n[/dungeon]\n"+
		"[server] 1\n"+
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\n"+
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   121  `<chn_channel_info_014>`   22   `[ancient]`   0   0   ``\n"+
		"   171  `<chn_channel_info_001>`   22   `[deathtower]`   0   0   ``\n"+
		"[/server]\n")
	// 还有 171 兜底，关 121 应放行。
	if _, status, err := ApplyFile(path, []int{121}, false); err != nil {
		t.Fatalf("有另一个特殊区域频道兜底时应允许关闭 121: %v", err)
	} else if status.Disabled != 1 {
		t.Fatalf("关闭 121 后 Disabled=%d，期望 1", status.Disabled)
	}
	// 此时 171 是最后一个 type 22，必须拒绝。
	if _, _, err := ApplyFile(path, []int{171}, false); err == nil {
		t.Fatal("关掉最后一个特殊区域频道（type 22）必须被拒绝")
	}
	// 非 22 频道不受这条规则影响（11/19 已放行）。
	if _, status, err := ApplyFile(path, []int{11, 19, 50}, false); err != nil {
		t.Fatalf("普通/默认/引导频道现在应允许关闭: %v", err)
	} else if status.Disabled != 4 {
		t.Fatalf("再关 3 个后 Disabled=%d，期望 4", status.Disabled)
	}
	// 文件里本来就没有 type 22 时，不该把这条规则算到本次操作头上。
	noSpecial := writeFixture(t, "[server] 1\n"+
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"[/server]\n")
	if _, _, err := ApplyFile(noSpecial, []int{50}, false); err != nil {
		t.Fatalf("表里本来就没有 type 22 时，关别的频道不该被这条规则拦下: %v", err)
	}
}

// TestSpecialAreaFlagIsExposed 解析结果里要能看出哪些是特殊区域频道（UI 用来提示）。
func TestSpecialAreaFlagIsExposed(t *testing.T) {
	path := copyFixture(t)
	status, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	special := 0
	for _, entry := range status.Channels {
		if entry.SpecialArea {
			special++
			if entry.Type != specialAreaChannelType {
				t.Fatalf("频道 %d 被标成特殊区域但 type=%d", entry.ID, entry.Type)
			}
		}
	}
	if special == 0 {
		t.Fatal("真实 channel_info.etc 里应有 type 22 的特殊区域频道（121/171）")
	}
}

// TestEnableWithoutManifestRowIsRejected 清单里没有行文本的频道不允许"凭空开启"。
func TestEnableWithoutManifestRowIsRejected(t *testing.T) {
	path := writeFixture(t, "[server] 1\n"+
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   19   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"[/server]\n")
	if _, _, err := ApplyFile(path, []int{777}, true); err == nil {
		t.Fatal("清单里没有 777 的行文本，开启应被拒绝")
	}
}

// TestEnableInsertsInIDOrder 开启时必须按频道号插回原位置，保持文件既有升序。
func TestEnableInsertsInIDOrder(t *testing.T) {
	path := writeFixture(t, "[server] 1\n"+
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   19   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\n"+
		"[/server]\n")
	if _, _, err := ApplyFile(path, []int{30}, false); err != nil {
		// 30 号不在文件里，关闭是 no-op，不算错。
		t.Fatalf("关闭不存在的频道应是无操作而不是报错: %v", err)
	}
	// 先把 30 手工塞进清单，再开启，验证插入位置落在 19 与 50 之间。
	rows, meta := readManifest(path)
	rows["30"] = "   30   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``"
	meta["30"] = manifestMeta{ServerID: 1, Type: 11, Group: "granfloris", NameKey: "chn_channel_info_028"}
	if err := writeManifestForTest(path, rows, meta); err != nil {
		t.Fatalf("写清单: %v", err)
	}
	if _, _, err := ApplyFile(path, []int{30}, true); err != nil {
		t.Fatalf("开启频道 30: %v", err)
	}
	lines := fileLines(t, path)
	idx30, idx19, idx50 := -1, -1, -1
	for i, line := range lines {
		if strings.Contains(line, "   30 ") {
			idx30 = i
		}
		if strings.Contains(line, "   19 ") {
			idx19 = i
		}
		if strings.Contains(line, "   50 ") {
			idx50 = i
		}
	}
	if idx30 < 0 {
		t.Fatalf("频道 30 未被插回：%v", lines)
	}
	if !(idx19 < idx30 && idx30 < idx50) {
		t.Fatalf("插入位置 idx19=%d idx30=%d idx50=%d，期望 19 < 30 < 50", idx19, idx30, idx50)
	}
}

// TestApplyWritesBackup 成功的改动必须留下 .bak_ 备份。
func TestApplyWritesBackup(t *testing.T) {
	path := copyFixture(t)
	_, status, err := ApplyFile(path, []int{12}, false)
	if err != nil {
		t.Fatalf("关闭频道: %v", err)
	}
	if status.Backup == "" {
		t.Fatal("未返回备份文件名")
	}
	if _, err := os.Stat(status.Backup); err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
}

// TestManifestSidecarWritten 侧车清单必须落盘，且包含被关掉频道的原始行。
func TestManifestSidecarWritten(t *testing.T) {
	path := copyFixture(t)
	if _, _, err := ApplyFile(path, []int{12}, false); err != nil {
		t.Fatalf("关闭频道: %v", err)
	}
	rows, meta := readManifest(path)
	raw, ok := rows["12"]
	if !ok {
		t.Fatal("侧车清单缺少频道 12")
	}
	if !strings.Contains(raw, "chn_channel_info_028") {
		t.Fatalf("频道 12 的原始行不像原文件内容：%q", raw)
	}
	if meta["12"].Type != ordinaryChannelType || meta["12"].Group != "granfloris" {
		t.Fatalf("频道 12 元信息 = %+v", meta["12"])
	}
}

// TestApplyIsIdempotent 对已经是目标状态的频道重复操作必须是无操作、不写盘、不报错。
func TestApplyIsIdempotent(t *testing.T) {
	path := copyFixture(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读原文件: %v", err)
	}
	// 关闭一个本来就开着的频道：第一次有变化，第二次必须无变化。
	if _, status, err := ApplyFile(path, []int{12}, false); err != nil {
		t.Fatalf("第一次关闭: %v", err)
	} else if status.Disabled != 1 {
		t.Fatalf("第一次关闭后 Disabled=%d，期望 1", status.Disabled)
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读第一次关闭后文件: %v", err)
	}
	changed, status, err := ApplyFile(path, []int{12}, false)
	if err != nil {
		t.Fatalf("重复关闭不应报错: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("重复关闭应返回 0 个变化，实际 %v", changed)
	}
	if status.Disabled != 1 {
		t.Fatalf("重复关闭后 Disabled=%d，期望仍为 1", status.Disabled)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读第二次关闭后文件: %v", err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Fatal("重复关闭改动了文件（应完全不动）")
	}

	// 开启回去，再开启一次同样应无变化。
	if _, _, err := ApplyFile(path, []int{12}, true); err != nil {
		t.Fatalf("开启: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读还原后文件: %v", err)
	}
	if !bytes.Equal(original, restored) {
		t.Fatal("关闭→开启后未还原原文件")
	}
	changed, _, err = ApplyFile(path, []int{12}, true)
	if err != nil {
		t.Fatalf("重复开启不应报错: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("重复开启应返回 0 个变化，实际 %v", changed)
	}
}

// TestConcurrentApplyKeepsFileIntact 并发开关频道时，文件必须始终可解析、不截断、无残留 tmp。
//
// 这条用例对应一个真实的并发缺陷：ApplyFile 是 read→mergeManifest→rewrite→backup→write 的
// 读-改-写，本地「频道管理」页与远程管理工具的 /manage/v1/channels/apply 是两条独立链路，
// 并发进入时旧实现会把对方写了一半的临时文件 Rename 成 channel_info.etc。
// 本机 MinGW 是 i686、编不出 windows/amd64 的 race runtime，所以用"并发跑完再校验文件完好"
// 来固定住这个不变量（锁被去掉时，这条用例会稳定失败）。
func TestConcurrentApplyKeepsFileIntact(t *testing.T) {
	path := copyFixture(t)
	before, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	// 选号必须真正存在于当前 fixture 里，且不含保护频道。
	// 这一层断言很关键：真实 channel_info.etc 的号段并不连续（没有 22、没有 40），
	// 选到不存在的号时"关闭"是无操作、"开启"会被清单守护正确拒绝——那会让用例
	// 以一个看起来像产品缺陷的报错失败，白白误导排查方向。宁可在这里直接报"用例失效"。
	rangeA := []int{12, 13, 14, 15, 16, 17, 18, 20}
	rangeB := []int{42, 43, 44, 45, 46, 47, 48, 49}
	exists := make(map[int]bool, before.Total)
	ordinary := make(map[int]bool, before.Total)
	for _, entry := range before.Channels {
		exists[entry.ID] = true
		ordinary[entry.ID] = entry.Ordinary
	}
	for _, id := range append(append([]int(nil), rangeA...), rangeB...) {
		if !exists[id] {
			t.Fatalf("用例失效：频道 %d 不在 fixture 的频道表里（真实文件号段不连续）", id)
		}
		if id == bootstrapChannelID || id == defaultGameChannelID {
			t.Fatalf("用例失效：频道 %d 是保护频道，无法用于制造并发写", id)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*3)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ids := []int{rangeA[worker], rangeB[worker]}
			// 每个 worker 反复关闭/开启同一组频道，制造高频读-改-写。
			for round := 0; round < 3; round++ {
				if _, _, err := ApplyFile(path, ids, false); err != nil {
					errs <- fmt.Errorf("worker %d 关闭 %v: %w", worker, ids, err)
					return
				}
				if _, _, err := ApplyFile(path, ids, true); err != nil {
					errs <- fmt.Errorf("worker %d 开启 %v: %w", worker, ids, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发执行失败: %v", err)
	}

	// 文件必须仍然是完整可解析的频道表，且频道数回到原值。
	after, err := LoadFile(path)
	if err != nil {
		t.Fatalf("并发改写后 LoadFile 失败（文件可能被截断）: %v", err)
	}
	if after.Total != before.Total {
		t.Fatalf("并发改写后频道数 %d，期望回到 %d", after.Total, before.Total)
	}
	if after.Disabled != 0 {
		t.Fatalf("全部开启后 Disabled=%d，期望 0（说明有 worker 的写入被覆盖丢失）", after.Disabled)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	// BOM 状态必须与改动前一致。注意真实 channel_info.etc **没有** BOM
	// （首字节是 '['），所以这里断言的是"不变"而不是"存在"——
	// 早先误以为有 BOM，是因为 read 工具渲染时补了一个，属于观察假象。
	original, err := os.ReadFile(realChannelInfo)
	if err != nil {
		t.Skipf("真实 channel_info.etc 不可读，跳过：%v", err)
	}
	if hasBOM(raw) != hasBOM(original) {
		t.Fatalf("并发改写后 BOM 状态变了：原 %v → 现 %v", hasBOM(original), hasBOM(raw))
	}
	if !bytes.HasSuffix(raw, []byte("[/server]\r\n")) && !bytes.HasSuffix(raw, []byte("[/server]\n")) {
		t.Fatalf("文件结尾不是完整的 [/server] 段尾，疑似被截断：%q", tailBytes(raw, 40))
	}

	// 目录里不允许残留 *.tmp-* 临时文件。
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("残留临时文件: %s", entry.Name())
		}
	}
}

// tailBytes 取字节切片尾部（用例失败时打印用）。
func tailBytes(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[len(raw)-n:])
}

// TestBOMAndCRLFPreserved 带 BOM 的变体文件在开关后必须逐字节还原（含 BOM）。
//
// 真实 channel_info.etc 没有 BOM，所以上面那条并发用例覆盖不到 BOM 分支；
// 这里用合成 fixture 把"保留 BOM + CRLF"这条防御性代码路径固定住——
// 别的地方（例如别人手工另存的 UTF-8-BOM 版本）确实会出现带 BOM 的文件。
func TestBOMAndCRLFPreserved(t *testing.T) {
	body := "[dungeon]\r\n`[town]`\r\n<4::dname_1>\r\n1\r\n[/dungeon]\r\n" +
		"[server] 1\r\n" +
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n" +
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\r\n" +
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n" +
		"   51   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n" +
		"[/server]\r\n"
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...)

	dir := t.TempDir()
	path := filepath.Join(dir, "channel_info.etc")
	if err := os.WriteFile(path, withBOM, 0o644); err != nil {
		t.Fatalf("写 fixture: %v", err)
	}
	if !hasBOM(withBOM) {
		t.Fatal("用例失效：fixture 未带 BOM")
	}

	if _, _, err := ApplyFile(path, []int{50}, false); err != nil {
		t.Fatalf("关闭频道: %v", err)
	}
	closed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if !hasBOM(closed) {
		t.Fatal("关闭后 BOM 丢失")
	}
	if bytes.Contains(closed, []byte("   50 ")) {
		t.Fatal("关闭后频道 50 的行仍在")
	}
	if bytes.Contains(closed, []byte("\n\n")) {
		t.Fatal("关闭后出现了空行，说明删行时把换行处理错了")
	}

	if _, _, err := ApplyFile(path, []int{50}, true); err != nil {
		t.Fatalf("开启频道: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读还原后文件: %v", err)
	}
	if !bytes.Equal(withBOM, restored) {
		t.Fatalf("带 BOM 的文件关闭→开启后未逐字节还原\n原: %q\n现: %q", withBOM, restored)
	}
}

// TestSectionScopingIgnoresOtherSections 只允许在 [server] 段内增删频道行。
//
// 这条固定的是一个"识别口径"缺陷：频道记录靠字段形态识别（int `str` int `str`），
// 这在多段文件里不唯一。若不加段范围限制，[dungeon] 段里一旦出现同形态的行，
// 关闭会从别的段删行、开启会把恢复行插到 [server] 之外（频道看着开了却永不生效）。
func TestSectionScopingIgnoresOtherSections(t *testing.T) {
	// [dungeon] 段里塞一行恰好符合 `int \`str\` int \`str\`` 形态的内容。
	path := writeFixture(t, "[dungeon]\r\n"+
		"   77   `<dungeon_key>`   11   `[granfloris]`   something\r\n"+
		"[/dungeon]\r\n"+
		"[server] 1\r\n"+
		"   11   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n"+
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\r\n"+
		"   50   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n"+
		"   51   `<chn_channel_info_028>`   11   `[granfloris]`   0   0   ``\r\n"+
		"[/server]\r\n")

	// 77 只存在于 [dungeon] 段，不能被当成频道：解析结果里不应出现它。
	status, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for _, entry := range status.Channels {
		if entry.ID == 77 {
			t.Fatalf("[dungeon] 段里的同形态行被误认成频道 77：%+v", entry)
		}
	}

	// 关闭 50 时，[dungeon] 段那一行必须原样保留。
	if _, _, err := ApplyFile(path, []int{50}, false); err != nil {
		t.Fatalf("关闭频道 50: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if !bytes.Contains(after, []byte("   77   `<dungeon_key>`   11   `[granfloris]`   something")) {
		t.Fatalf("[dungeon] 段的行被改动了：\n%s", after)
	}

	// 开启 50 时，恢复行必须落回 [server] 段内，而不是被插到段外。
	if _, _, err := ApplyFile(path, []int{50}, true); err != nil {
		t.Fatalf("开启频道 50: %v", err)
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	serverStart := bytes.Index(final, []byte("[server] 1"))
	serverEnd := bytes.Index(final, []byte("[/server]"))
	idx50 := bytes.Index(final, []byte("   50   `<chn_channel_info_028>`"))
	if serverStart < 0 || serverEnd < 0 || idx50 < 0 {
		t.Fatalf("文件结构异常：\n%s", final)
	}
	if idx50 < serverStart || idx50 > serverEnd {
		t.Fatalf("频道 50 被插到了 [server] 段之外（[%d, %d] vs 行偏移 %d）", serverStart, serverEnd, idx50)
	}
}

// TestOrdinaryGuardOnlyAppliesWhenClosingOrdinary 文件本来没有普通频道时，不应阻止关别的频道。
func TestOrdinaryGuardOnlyAppliesWhenClosingOrdinary(t *testing.T) {
	// 没有任何 type 11 行：只有引导频道（type 40）与一个团队频道（type 23）。
	path := writeFixture(t, "[server] 1\r\n"+
		"   19   `<chn_channel_info_046>`   40   `[crack]`   0   0   ``\r\n"+
		"   23   `<chn_channel_info_016>`   23   `[anton_raid]`   0   0   ``\r\n"+
		"[/server]\r\n")
	if _, status, err := ApplyFile(path, []int{23}, false); err != nil {
		t.Fatalf("文件本来就没有普通城镇频道，关闭团队频道不该被拒: %v", err)
	} else if status.Disabled != 1 {
		t.Fatalf("Disabled=%d，期望 1", status.Disabled)
	}
}

// TestTailBytesHelper 确认测试辅助函数本身可用（避免用例因辅助函数写错而误报）。
func TestTailBytesHelper(t *testing.T) {
	if got := tailBytes([]byte("abcdef"), 3); got != "def" {
		t.Fatalf("tailBytes = %q，期望 def", got)
	}
	if got := tailBytes([]byte("ab"), 5); got != "ab" {
		t.Fatalf("tailBytes 短串 = %q，期望 ab", got)
	}
}

// TestApplyEmptyTargetsRejected 空目标必须被拒绝，而不是悄悄什么都不做。
func TestApplyEmptyTargetsRejected(t *testing.T) {
	path := copyFixture(t)
	if _, _, err := ApplyFile(path, nil, false); err == nil {
		t.Fatal("空频道列表应被拒绝")
	}
	if _, _, err := ApplyFile(path, []int{0, -3}, true); err == nil {
		t.Fatal("全是非法频道号也应被拒绝")
	}
}

// TestResolvePathPrefersInstanceConfig instance.json 里的 paths 优先于默认相对路径。
func TestResolvePathPrefersInstanceConfig(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "config"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	payload := `{"game":{"channelInfoPath":"data/dnf/custom_channel_info.etc"}}`
	if err := os.WriteFile(filepath.Join(runtimeDir, "config", "instance.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("写 instance.json: %v", err)
	}
	path, err := ResolvePath(root)
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	want := filepath.Join(runtimeDir, "data", "dnf", "custom_channel_info.etc")
	if path != want {
		t.Fatalf("path=%s，期望 %s", path, want)
	}
	// instance.json 缺失时回退默认路径。
	empty := t.TempDir()
	path, err = ResolvePath(empty)
	if err != nil {
		t.Fatalf("ResolvePath(空目录): %v", err)
	}
	wantDefault := filepath.Join(empty, "runtime", "data", "dnf", "channel_info.etc")
	if path != wantDefault {
		t.Fatalf("path=%s，期望 %s", path, wantDefault)
	}
}

// writeManifestForTest 测试专用：直接写侧车清单。
func writeManifestForTest(path string, rows map[string]string, meta map[string]manifestMeta) error {
	return writeManifestRaw(path, manifest{Rows: rows, Meta: meta})
}
