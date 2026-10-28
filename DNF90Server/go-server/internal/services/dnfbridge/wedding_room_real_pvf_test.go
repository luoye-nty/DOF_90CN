package dnfbridge

import (
	"os"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestWeddingRoomItemInfoRealPVFDump 枚举真 PVF 里所有婚房/装饰相关文件，并打印头部内容。
//
// 背景（2026-10-08）：结婚戒指 tooltip 上的"婚房装饰奖励"（HP+200 / MP+200 /
// 城镇移动速度+4% / 所有属性强化+1）在服务器侧查不到任何来源：
//   - decoration/coupleroomiteminfo.etc 只有 [position list] 与 [coupleroom item]（装饰 + .act）
//   - decoration/coupletree.etc 只有种子/生长/浇水/奖励
//   - etc/weddingsystem.etc 只有仪式/礼物/信函
//   - 服务器没有任何婚房属性聚合
//
// 现场现象：城镇里四属性强化 +14，进副本变 +13 —— 差的正是那一行里的"所有属性强化 +1"。
// 这个测试用来找出这份奖励数值定义在哪张表里（客户端按它算，服务器要照抄进副本属性）。
//
// 需要真 PVF：DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf 路径>
func TestWeddingRoomItemInfoRealPVFDump(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to enumerate the wedding/decoration tables")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for index, file := range archive.Files() {
		lower := strings.ToLower(file.Path)
		if !strings.Contains(lower, "couple") && !strings.Contains(lower, "decoration") &&
			!strings.Contains(lower, "wedding") {
			continue
		}
		matches++
		text, textErr := archive.FileText(index)
		head := ""
		if textErr == nil {
			lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
			if len(lines) > 10 {
				lines = lines[:10]
			}
			head = strings.Join(lines, " | ")
		}
		t.Logf("FILE %-56s head=%s", file.Path, head)
	}
	t.Logf("婚房/装饰相关文件共 %d 个（PVF 总文件数 %d）", matches, archive.FileCount())
}
