package main

// 锁住频道名映射表本身：这张表是纯显示层的人肉配置，改错一个字不会编译报错，
// 只会让人看到错名字，所以用一条表驱动用例把它钉死。

import (
	"testing"
)

func TestChannelDisplayNameMappings(t *testing.T) {
	cases := map[string]string{
		"granfloris":   "格兰之森",
		"sky_catle":    "天空之城",
		"Alfhlyra":     "阿法利亚",
		"north_myre":   "诺斯玛尔",
		"Antwer":       "安特贝鲁峡谷",
		"seatrain":     "海上列车",
		"timedoor":     "时空之门",
		"powerstation": "能源中心",
		"noblsky":      "苍穹贵族号",
		"metro":        "地轨中心",
		"ancient":      "远古地下城",
		"deathtower":   "亡者峡谷",
		"crack":        "异次元裂缝",
		"pvp":          "决斗场",
		"anton_raid":   "安徒恩团队模式",
		"luke_raid":    "卢克团队模式",
	}
	if len(cases) != len(channelNameZh) {
		t.Fatalf("映射表有 %d 条，用例只覆盖 %d 条（新增频道名时请同步用例）", len(channelNameZh), len(cases))
	}
	for key, want := range cases {
		if got := channelDisplayName(key); got != want {
			t.Fatalf("channelDisplayName(%q) = %q, want %q", key, got, want)
		}
	}
	// 大小写不敏感：channel_info.etc 里 Alfhlyra / Antwer 首字母是大写。
	for _, key := range []string{"ALFHLYRA", "alfhlyra", "Antwer", "ANTWER"} {
		if got := channelDisplayName(key); got == key {
			t.Fatalf("channelDisplayName(%q) 没有命中映射（大小写敏感了）", key)
		}
	}
	// 未收录的名键原样显示，不猜测、不置空。
	for _, key := range []string{"elven_guard", "stormpass", "behemoth", "fiendwar", "goldenroad"} {
		if got := channelDisplayName(key); got != key {
			t.Fatalf("channelDisplayName(%q) = %q，未收录的名键应当原样显示", key, got)
		}
	}
	// 空值与空白。
	if got := channelDisplayName("   "); got != "" {
		t.Fatalf("channelDisplayName(空) = %q, want 空", got)
	}
	if got := channelDisplayName("  granfloris  "); got != "格兰之森" {
		t.Fatalf("channelDisplayName(带空格) = %q, want 格兰之森", got)
	}
}

func TestChannelDisplayNameWithKey(t *testing.T) {
	if got := channelDisplayNameWithKey("Alfhlyra"); got != "阿法利亚（Alfhlyra）" {
		t.Fatalf("带名键的详情写法 = %q", got)
	}
	// 未收录的只显示名键本身（不要出现空括号）。
	if got := channelDisplayNameWithKey("stormpass"); got != "stormpass" {
		t.Fatalf("未收录的详情写法 = %q", got)
	}
}

// TestChanModelShowsChineseGroupName 走一遍表格模型的显示路径：
// 频道管理表格的"分组"列取的就是第 2 列，必须是中文。
func TestChanModelShowsChineseGroupName(t *testing.T) {
	model := &chanModel{items: []ChannelEntry{
		{ID: 10, Type: 11, Group: "granfloris", Enabled: true},
		{ID: 21, Type: 11, Group: "sky_catle", Enabled: true},
		{ID: 23, Type: 23, Group: "anton_raid", Enabled: false},
		{ID: 2, Type: 2, Group: "stormpass", Enabled: true}, // 未收录：原样显示
	}}
	wants := []string{"格兰之森", "天空之城", "安徒恩团队模式", "stormpass"}
	for row, want := range wants {
		if got := model.Value(row, 2); got != want {
			t.Fatalf("第 %d 行分组列 = %v, want %q", row, got, want)
		}
	}
	// 越界不能 panic（表格刷新时会出现行数短暂不一致）。
	if got := model.Value(len(model.items), 2); got != nil {
		t.Fatalf("越界行 = %v, want nil", got)
	}
	if got := model.Value(0, 99); got != nil {
		t.Fatalf("越界列 = %v, want nil", got)
	}
}
