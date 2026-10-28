package main

// 频道名显示：channel_info.etc 里频道的"分组"字段是一个英文名键（granfloris、sky_catle……），
// 频道管理页直接显示它不好认，这里映射成中文。
//
// 只收录明确指定的条目；**没有收录的名键按原样显示**（例如 elven_guard、stormpass、
// behemoth、fiendwar 等），不猜测——猜错的名字比英文键更误导人。要补哪条直接加进下面的表。
//
// 查找不区分大小写：channel_info.etc 里的键大小写并不统一（Alfhlyra、Antwer 首字母大写）。

import "strings"

// channelNameZh 频道名键（小写）→ 中文显示名。
var channelNameZh = map[string]string{
	"granfloris":   "格兰之森",
	"sky_catle":    "天空之城",
	"alfhlyra":     "阿法利亚",
	"north_myre":   "诺斯玛尔",
	"antwer":       "安特贝鲁峡谷",
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

// channelDisplayName 返回频道的显示名：命中映射给中文，否则原样返回名键。
func channelDisplayName(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	if name, ok := channelNameZh[strings.ToLower(trimmed)]; ok {
		return name
	}
	return trimmed
}

// channelDisplayNameWithKey 详情行用的写法：有中文名时补上原始名键，便于对着
// channel_info.etc 排查；没有中文名时就是名键本身。
func channelDisplayNameWithKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	if name, ok := channelNameZh[strings.ToLower(trimmed)]; ok {
		return name + "（" + trimmed + "）"
	}
	return trimmed
}
