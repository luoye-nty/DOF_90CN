package serverdir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInstance(t *testing.T, root, content string) string {
	t.Helper()
	cfgDir := filepath.Join(root, "runtime", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(cfgDir, "instance.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// countOf 从 JSON 里取 server.partyUdpRelayPortCount（解析失败即报错）。
func countOf(t *testing.T, raw string) int {
	t.Helper()
	var doc struct {
		Server struct {
			Count int `json:"partyUdpRelayPortCount"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("改写后的 instance.json 不是合法 JSON: %v\n%s", err, raw)
	}
	return doc.Server.Count
}

const sampleInstance = `{
  "schemaVersion": 1,
  "server": {
    "advertiseIp": "AUTO_DETECT",
    "channelListen": "127.0.0.1:7001",
    "partyUdpRelayEnabled": true,
    "partyUdpRelayPortStart": 30000,
    "partyUdpRelayPortCount": 64
  },
  "database": {
    "mode": "portable",
    "port": 13306,
    "name": "dnf_local"
  }
}
`

func TestEnsurePartyUDPRelayPortCountRewrites(t *testing.T) {
	root := t.TempDir()
	p := writeInstance(t, root, sampleInstance)

	changed, err := EnsurePartyUDPRelayPortCount(root, 1024, nil)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	if !changed {
		t.Fatal("64 → 1024 应当返回 changed=true")
	}
	got := readFile(t, p)
	if countOf(t, got) != 1024 {
		t.Fatalf("改写后 count = %d, want 1024\n%s", countOf(t, got), got)
	}
	// 其余内容必须原样保留（逐行比对，只允许那一行不同）
	before := strings.Split(sampleInstance, "\n")
	after := strings.Split(got, "\n")
	if len(before) != len(after) {
		t.Fatalf("行数变了: before=%d after=%d", len(before), len(after))
	}
	diffLines := 0
	for i := range before {
		if before[i] != after[i] {
			diffLines++
			if !strings.Contains(after[i], "partyUdpRelayPortCount") {
				t.Fatalf("第 %d 行被意外改动:\n before=%q\n after =%q", i+1, before[i], after[i])
			}
		}
	}
	if diffLines != 1 {
		t.Fatalf("改动行数 = %d, want 1", diffLines)
	}
	// 备份必须是原始内容
	bak := p + ".bak"
	if readFile(t, bak) != sampleInstance {
		t.Fatalf("instance.json.bak 内容不是改前原文")
	}
}

func TestEnsurePartyUDPRelayPortCountIdempotent(t *testing.T) {
	root := t.TempDir()
	p := writeInstance(t, root, sampleInstance)
	if _, err := EnsurePartyUDPRelayPortCount(root, 1024, nil); err != nil {
		t.Fatalf("第一次改写失败: %v", err)
	}
	first := readFile(t, p)

	changed, err := EnsurePartyUDPRelayPortCount(root, 1024, nil)
	if err != nil {
		t.Fatalf("第二次检查失败: %v", err)
	}
	if changed {
		t.Fatal("已经是 1024，第二次不应再改写")
	}
	if second := readFile(t, p); second != first {
		t.Fatalf("第二次不应改动文件")
	}
}

func TestEnsurePartyUDPRelayPortCountInsertsMissingField(t *testing.T) {
	root := t.TempDir()
	// 老文件没有 partyUdpRelayPortCount 这一项
	content := `{
  "server": {
    "partyUdpRelayPortStart": 30000,
    "partyUdpRelayEnabled": true
  }
}
`
	p := writeInstance(t, root, content)

	changed, err := EnsurePartyUDPRelayPortCount(root, 1024, nil)
	if err != nil {
		t.Fatalf("补写失败: %v", err)
	}
	if !changed {
		t.Fatal("字段缺失时应当补写")
	}
	got := readFile(t, p)
	if v := countOf(t, got); v != 1024 {
		t.Fatalf("补写后 count = %d, want 1024\n%s", v, got)
	}
	if !strings.Contains(got, "    \"partyUdpRelayPortCount\": 1024,") {
		t.Fatalf("补写行的缩进/格式不对:\n%s", got)
	}
}

func TestEnsurePartyUDPRelayPortCountKeepsCRLFAndStyle(t *testing.T) {
	root := t.TempDir()
	content := strings.ReplaceAll(sampleInstance, "\n", "\r\n")
	content = strings.Replace(content, `"partyUdpRelayPortCount": 64`, `"partyUdpRelayPortCount":64`, 1)
	p := writeInstance(t, root, content)

	if _, err := EnsurePartyUDPRelayPortCount(root, 1024, nil); err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	got := readFile(t, p)
	if !strings.Contains(got, "\r\n") {
		t.Fatal("CRLF 应当被保留")
	}
	if !strings.Contains(got, `"partyUdpRelayPortCount":1024`) {
		t.Fatalf("紧凑写法（冒号后无空格）应保持原样式:\n%s", got)
	}
	if v := countOf(t, got); v != 1024 {
		t.Fatalf("count = %d, want 1024", v)
	}
}

func TestEnsurePartyUDPRelayPortCountMissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := EnsurePartyUDPRelayPortCount(root, 1024, nil); err == nil {
		t.Fatal("instance.json 缺失时应当返回错误")
	}
}
