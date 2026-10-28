package main

import "strconv"

// DNF90 职业/转职/觉醒名称映射（对齐 web 版 GM 参考 class_tree.go，纯数据，无数据库/网络依赖）。
// 字段语义与 GM 一致：job=职业大类(0..15)；first_grow=转职分支(0=基础,1..n)；
// second_grow=觉醒阶段(0=未觉醒,1=一次觉醒,2=二次觉醒)。
// grow_type 位布局：高4位=secondGrow，低4位=firstGrow（由执行端 GM 负责写入，此处仅做名称映射）。

type acctGrowDef struct {
	FirstGrow int
	Name      string
	Awakes    []string // 觉醒名列表：[一觉名, 二觉名]；空表示该分支不可觉醒
}

type acctClassDef struct {
	JobID int
	Name  string
	Grows []acctGrowDef
}

var acctClassTree = []acctClassDef{
	{JobID: 0, Name: "鬼剑士(男)", Grows: []acctGrowDef{
		{0, "鬼剑士", nil},
		{1, "剑魂", []string{"剑圣", "剑神"}},
		{2, "鬼泣", []string{"弑魂", "黑暗君主"}},
		{3, "狂战士", []string{"狱血魔神", "帝血弑天"}},
		{4, "阿修罗", []string{"大暗黑天", "天帝"}},
	}},
	{JobID: 1, Name: "格斗家(女)", Grows: []acctGrowDef{
		{0, "格斗家", nil},
		{1, "气功师", []string{"百花缭乱", "念帝"}},
		{2, "散打", []string{"武神", "极武圣"}},
		{3, "街霸", []string{"毒王", "毒神绝"}},
		{4, "柔道家", []string{"暴风眼", "风暴女皇"}},
	}},
	{JobID: 2, Name: "神枪手(男)", Grows: []acctGrowDef{
		{0, "神枪手", nil},
		{1, "漫游枪手", []string{"枪神", "掠天之翼"}},
		{2, "枪炮师", []string{"狂暴者", "毁灭者"}},
		{3, "机械师", []string{"机械战神", "机械元首"}},
		{4, "弹药专家", []string{"大将军", "战场统治者"}},
	}},
	{JobID: 3, Name: "魔法师(女)", Grows: []acctGrowDef{
		{0, "魔法师", nil},
		{1, "元素师", []string{"大魔导师", "元素圣灵"}},
		{2, "召唤师", []string{"月之女皇", "召月大师"}},
		{3, "战斗法师", []string{"贝亚娜斗神", "伊斯塔战灵"}},
		{4, "魔道学者", []string{"魔术师", "古灵精怪"}},
	}},
	{JobID: 4, Name: "圣职者(男)", Grows: []acctGrowDef{
		{0, "圣职者", nil},
		{1, "圣骑士", []string{"天启者", "神思者"}},
		{2, "蓝拳圣使", []string{"神之手", "正义仲裁者"}},
		{3, "驱魔师", []string{"龙斗士", "真龙星君"}},
		{4, "复仇者", []string{"末日审判者", "永生者"}},
	}},
	{JobID: 5, Name: "神枪手(女)", Grows: []acctGrowDef{
		{0, "神枪手", nil},
		{1, "漫游枪手", []string{"沾血蔷薇", "绯红玫瑰"}},
		{2, "枪炮师", []string{"重炮掌控者", "风暴骑兵"}},
		{3, "机械师", []string{"机械之心", "机械之灵"}},
		{4, "弹药专家", []string{"战争女神", "芙蕾雅"}},
	}},
	{JobID: 6, Name: "暗夜使者", Grows: []acctGrowDef{
		{0, "暗夜使者", nil},
		{1, "刺客", []string{"银月", "月影星劫"}},
		{2, "死灵术士", []string{"死亡术士", "暗黑君主"}},
		{3, "忍者", []string{"不知火", "冥王"}},
		{4, "影舞者", []string{"梦魇", "幽冥"}},
	}},
	{JobID: 7, Name: "格斗家(男)", Grows: []acctGrowDef{
		{0, "格斗家", nil},
		{1, "气功师", []string{"风拳流大师", "念皇"}},
		{2, "散打", []string{"武极", "极皇"}},
		{3, "街霸", []string{"千手罗汉", "暗街之王"}},
		{4, "柔道家", []string{"风林火山", "宗师"}},
	}},
	{JobID: 8, Name: "魔法师(男)", Grows: []acctGrowDef{
		{0, "魔法师", nil},
		{1, "元素爆破师", []string{"魔皇", "湮灭之瞳"}},
		{2, "冰结师", []string{"冰冻之心", "刹那永恒"}},
		{3, "血法师", []string{"血皇", "血狱君主"}},
		{4, "逐风者", []string{"御风者", "疾风之神"}},
		{5, "次元行者", []string{"虚空行者", "混沌行者"}},
	}},
	{JobID: 9, Name: "黑暗武士", Grows: []acctGrowDef{
		{0, "黑暗武士", []string{"时间统治者", "永恒者"}},
	}},
	{JobID: 10, Name: "缔造者", Grows: []acctGrowDef{
		{0, "缔造者", []string{"创世者", "创世之神"}},
	}},
	{JobID: 11, Name: "鬼剑士(女)", Grows: []acctGrowDef{
		{0, "鬼剑士", nil},
		{1, "驭剑士", []string{"剑宗", "剑皇"}},
		{2, "暗殿骑士", []string{"暗帝", "神女"}},
		{3, "契魔者", []string{"剑魔", "弑神者"}},
		{4, "流浪武士", []string{"剑豪", "剑仙"}},
	}},
	{JobID: 12, Name: "守护者", Grows: []acctGrowDef{
		{0, "守护者", nil},
		{1, "精灵骑士", []string{"战争女神", "天穹守护者"}},
		{2, "混沌魔灵", []string{"黑魔后", "魔神"}},
		{3, "帕拉丁", []string{"黎明之翼", "破晓女神"}},
		{4, "龙骑士", []string{"龙皇", "龙神"}},
	}},
	{JobID: 13, Name: "魔枪士", Grows: []acctGrowDef{
		{0, "魔枪士", nil},
		{1, "征战者", []string{"战魂", "不灭战神"}},
		{2, "决战者", []string{"无双之魂", "圣武枪魂"}},
		{3, "狩猎者", []string{"征服之魂", "屠戮之魂"}},
		{4, "暗枪士", []string{"狂怒恶鬼", "幽冥"}},
	}},
	{JobID: 14, Name: "圣职者(女)", Grows: []acctGrowDef{
		{0, "圣职者", nil},
		{1, "圣骑士", []string{"福音传道者", "炽天使"}},
		{2, "异端审判者", []string{"神焰处刑官", "炎狱裁决者"}},
		{3, "巫女", []string{"神女", "天仙娘娘"}},
		{4, "诱魔者", []string{"断罪者", "救世主"}},
	}},
	{JobID: 15, Name: "枪剑士", Grows: []acctGrowDef{
		{0, "枪剑士", nil},
		{1, "暗刃", []string{"统御者", "铁血统帅"}},
		{2, "特工", []string{"绝命谍影", "无间影帝"}},
		{3, "战线佣兵", []string{"爆裂旋风", "战场王牌"}},
		{4, "源能专家", []string{"源力掌控者", "未来开拓者"}},
	}},
}

// acctAllJobNames 返回职业大类下拉项（按 classTree 顺序，index 即选择值）。
func acctAllJobNames() []string {
	out := make([]string, 0, len(acctClassTree))
	for _, cls := range acctClassTree {
		out = append(out, cls.Name)
	}
	return out
}

// acctGrowNamesByJob 返回某职业大类的转职下拉项（含 0=基础，顺序与 Grows 一致）。
func acctGrowNamesByJob(job int) []string {
	for _, cls := range acctClassTree {
		if cls.JobID == job {
			out := make([]string, 0, len(cls.Grows))
			for _, gt := range cls.Grows {
				out = append(out, gt.Name)
			}
			return out
		}
	}
	return []string{"未收录职业"}
}

// acctAwakeNamesByGrow 返回觉醒下拉项：未觉醒 + 该转职分支的可选觉醒名。
func acctAwakeNamesByGrow(job, firstGrow int) []string {
	out := []string{"未觉醒"}
	for _, cls := range acctClassTree {
		if cls.JobID != job {
			continue
		}
		for _, gt := range cls.Grows {
			if gt.FirstGrow == firstGrow {
				out = append(out, gt.Awakes...)
				break
			}
		}
		break
	}
	return out
}

// acctClassIndexByJob 返回职业大类在 acctClassTree 中的下标，找不到返回 -1。
func acctClassIndexByJob(job int) int {
	for i, cls := range acctClassTree {
		if cls.JobID == job {
			return i
		}
	}
	return -1
}

// acctGrowIndexByValue 返回转职分支值在下拉顺序中的下标，找不到返回 -1。
func acctGrowIndexByValue(job, firstGrow int) int {
	for _, cls := range acctClassTree {
		if cls.JobID != job {
			continue
		}
		for i, gt := range cls.Grows {
			if gt.FirstGrow == firstGrow {
				return i
			}
		}
	}
	return -1
}

// acctJobDisplayName 拼接 "大类 / 转职 / 觉醒" 名称展示；缺失的段回退为数字。
func acctJobDisplayName(job, firstGrow, secondGrow int) string {
	jobName := strconv.Itoa(job)
	growName := strconv.Itoa(firstGrow)
	awakeName := ""
	hasAwake := secondGrow > 0
	awakeNameFallback := strconv.Itoa(secondGrow)
	for _, cls := range acctClassTree {
		if cls.JobID != job {
			continue
		}
		jobName = cls.Name
		for _, gt := range cls.Grows {
			if gt.FirstGrow != firstGrow {
				continue
			}
			growName = gt.Name
			if secondGrow > 0 && secondGrow <= len(gt.Awakes) {
				awakeName = gt.Awakes[secondGrow-1]
				awakeNameFallback = ""
			}
			break
		}
		break
	}
	if !hasAwake {
		return jobName + " / " + growName
	}
	if awakeName == "" {
		return jobName + " / " + growName + " / " + awakeNameFallback
	}
	return jobName + " / " + growName + " / " + awakeName
}
