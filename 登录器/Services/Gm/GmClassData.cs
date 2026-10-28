using System;
using System.Collections.Generic;
using System.IO;
using System.Reflection;
using System.Text.Json;

namespace DNFLogin.Gm
{
    /// <summary>技能定义（class_skills.json）</summary>
    public sealed class SkillDef
    {
        public int Id { get; set; }
        public int Level { get; set; }
    }

    public sealed class GrowTypeSkillData
    {
        public List<SkillDef> Skills { get; set; } = new();
        public Dictionary<string, List<SkillDef>> Awakenings { get; set; } = new();
    }

    public sealed class ClassSkillData
    {
        public List<SkillDef> BaseSkills { get; set; } = new();
        public Dictionary<string, GrowTypeSkillData> GrowTypes { get; set; } = new();
    }

    public sealed class SkillsDB
    {
        public Dictionary<string, ClassSkillData> Classes { get; set; } = new();
    }

    /// <summary>
    /// GM 工具静态数据：职业树 / 默认技能 / 副本最高难度（内嵌资源）。
    /// 与 Go 版 class_tree.go / class_skills.go / dungeons.go 对齐。
    /// </summary>
    public static class GmClassData
    {
        public static readonly List<ClassDef> ClassTree = BuildClassTree();

        private static List<ClassDef> BuildClassTree()
        {
            GrowTypeDef A(int gt, string name, params string[] awakes) =>
                new() { GrowType = gt, Name = name, Awakenings = new List<string>(awakes) };

            return new List<ClassDef>
            {
                new() { JobID = 0, JobName = "鬼剑士(男)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "鬼剑士"), A(1, "剑魂", "剑圣", "剑神"), A(2, "鬼泣", "弑魂", "黑暗君主"),
                    A(3, "狂战士", "狱血魔神", "帝血弑天"), A(4, "阿修罗", "大暗黑天", "天帝") } },
                new() { JobID = 1, JobName = "格斗家(女)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "格斗家"), A(1, "气功师", "百花缭乱", "念帝"), A(2, "散打", "武神", "极武圣"),
                    A(3, "街霸", "毒王", "毒神绝"), A(4, "柔道家", "暴风眼", "风暴女皇") } },
                new() { JobID = 2, JobName = "神枪手(男)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "神枪手"), A(1, "漫游枪手", "枪神", "掠天之翼"), A(2, "枪炮师", "狂暴者", "毁灭者"),
                    A(3, "机械师", "机械战神", "机械元首"), A(4, "弹药专家", "大将军", "战场统治者") } },
                new() { JobID = 3, JobName = "魔法师(女)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "魔法师"), A(1, "元素师", "大魔导师", "元素圣灵"), A(2, "召唤师", "月之女皇", "召月大师"),
                    A(3, "战斗法师", "贝亚娜斗神", "伊斯塔战灵"), A(4, "魔道学者", "魔术师", "古灵精怪") } },
                new() { JobID = 4, JobName = "圣职者(男)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "圣职者"), A(1, "圣骑士", "天启者", "神思者"), A(2, "蓝拳圣使", "神之手", "正义仲裁者"),
                    A(3, "驱魔师", "龙斗士", "真龙星君"), A(4, "复仇者", "末日审判者", "永生者") } },
                new() { JobID = 5, JobName = "神枪手(女)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "神枪手"), A(1, "漫游枪手", "沾血蔷薇", "绯红玫瑰"), A(2, "枪炮师", "重炮掌控者", "风暴骑兵"),
                    A(3, "机械师", "机械之心", "机械之灵"), A(4, "弹药专家", "战争女神", "芙蕾雅") } },
                new() { JobID = 6, JobName = "暗夜使者", GrowTypes = new List<GrowTypeDef>{
                    A(0, "暗夜使者"), A(1, "刺客", "银月", "月影星劫"), A(2, "死灵术士", "死亡术士", "暗黑君主"),
                    A(3, "忍者", "不知火", "冥王"), A(4, "影舞者", "梦魇", "幽冥") } },
                new() { JobID = 7, JobName = "格斗家(男)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "格斗家"), A(1, "气功师", "风拳流大师", "念皇"), A(2, "散打", "武极", "极皇"),
                    A(3, "街霸", "千手罗汉", "暗街之王"), A(4, "柔道家", "风林火山", "宗师") } },
                new() { JobID = 8, JobName = "魔法师(男)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "魔法师"), A(1, "元素爆破师", "魔皇", "湮灭之瞳"), A(2, "冰结师", "冰冻之心", "刹那永恒"),
                    A(3, "血法师", "血皇", "血狱君主"), A(4, "逐风者", "御风者", "疾风之神"),
                    A(5, "次元行者", "虚空行者", "混沌行者") } },
                new() { JobID = 9, JobName = "黑暗武士", GrowTypes = new List<GrowTypeDef>{
                    A(0, "黑暗武士", "时间统治者", "永恒者") } },
                new() { JobID = 10, JobName = "缔造者", GrowTypes = new List<GrowTypeDef>{
                    A(0, "缔造者", "创世者", "创世之神") } },
                new() { JobID = 11, JobName = "鬼剑士(女)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "鬼剑士"), A(1, "驭剑士", "剑宗", "剑皇"), A(2, "暗殿骑士", "暗帝", "神女"),
                    A(3, "契魔者", "剑魔", "弑神者"), A(4, "流浪武士", "剑豪", "剑仙") } },
                new() { JobID = 12, JobName = "守护者", GrowTypes = new List<GrowTypeDef>{
                    A(0, "守护者"), A(1, "精灵骑士", "战争女神", "天穹守护者"), A(2, "混沌魔灵", "黑魔后", "魔神"),
                    A(3, "帕拉丁", "黎明之翼", "破晓女神"), A(4, "龙骑士", "龙皇", "龙神") } },
                new() { JobID = 13, JobName = "魔枪士", GrowTypes = new List<GrowTypeDef>{
                    A(0, "魔枪士"), A(1, "征战者", "战魂", "不灭战神"), A(2, "决战者", "无双之魂", "圣武枪魂"),
                    A(3, "狩猎者", "征服之魂", "屠戮之魂"), A(4, "暗枪士", "狂怒恶鬼", "幽冥") } },
                new() { JobID = 14, JobName = "圣职者(女)", GrowTypes = new List<GrowTypeDef>{
                    A(0, "圣职者"), A(1, "圣骑士", "福音传道者", "炽天使"), A(2, "异端审判者", "神焰处刑官", "炎狱裁决者"),
                    A(3, "巫女", "神女", "天仙娘娘"), A(4, "诱魔者", "断罪者", "救世主") } },
                new() { JobID = 15, JobName = "枪剑士", GrowTypes = new List<GrowTypeDef>{
                    A(0, "枪剑士"), A(1, "暗刃", "统御者", "铁血统帅"), A(2, "特工", "绝命谍影", "无间影帝"),
                    A(3, "战线佣兵", "爆裂旋风", "战场王牌"), A(4, "源能专家", "源力掌控者", "未来开拓者") } },
            };
        }

        /// <summary>查找职业名称 / 转职名称 / 觉醒名称</summary>
        public static (string JobName, string GrowName, string AwakeName) LookupClassName(int jobID, int firstGrow, int secondGrow)
        {
            foreach (var cls in ClassTree)
            {
                if (cls.JobID != jobID)
                {
                    continue;
                }
                foreach (var gt in cls.GrowTypes)
                {
                    if (gt.GrowType != firstGrow)
                    {
                        continue;
                    }
                    string awake = "";
                    if (secondGrow > 0 && secondGrow <= gt.Awakenings.Count)
                    {
                        awake = gt.Awakenings[secondGrow - 1];
                    }
                    return (cls.JobName, gt.Name, awake);
                }
                return (cls.JobName, "", "");
            }
            return ("", "", "");
        }

        private static SkillsDB? _skillDb;
        private static readonly object SkillDbLock = new();

        /// <summary>默认技能库（内嵌 class_skills.json）</summary>
        public static SkillsDB Skills
        {
            get
            {
                if (_skillDb is not null)
                {
                    return _skillDb;
                }
                lock (SkillDbLock)
                {
                    if (_skillDb is null)
                    {
                        var json = ReadEmbedded("DNFLogin.Resources.Gm.class_skills.json")
                                   ?? throw new InvalidOperationException("内嵌资源 class_skills.json 缺失");
                        _skillDb = JsonSerializer.Deserialize<SkillsDB>(json, JsonOpts)
                                   ?? throw new InvalidOperationException("class_skills.json 解析失败");
                    }
                    return _skillDb;
                }
            }
        }

        private static Dictionary<string, int>? _dungeonMaxDifficulty;
        private static readonly object DungeonLock = new();

        /// <summary>副本最高难度（内嵌 dungeons_max_difficulty.json）</summary>
        public static Dictionary<string, int> DungeonsMaxDifficulty
        {
            get
            {
                if (_dungeonMaxDifficulty is not null)
                {
                    return _dungeonMaxDifficulty;
                }
                lock (DungeonLock)
                {
                    if (_dungeonMaxDifficulty is null)
                    {
                        var json = ReadEmbedded("DNFLogin.Resources.Gm.dungeons_max_difficulty.json")
                                   ?? throw new InvalidOperationException("内嵌资源 dungeons_max_difficulty.json 缺失");
                        _dungeonMaxDifficulty = JsonSerializer.Deserialize<Dictionary<string, int>>(json, JsonOpts)
                                                ?? throw new InvalidOperationException("dungeons_max_difficulty.json 解析失败");
                    }
                    return _dungeonMaxDifficulty;
                }
            }
        }

        private static readonly JsonSerializerOptions JsonOpts = new()
        {
            PropertyNameCaseInsensitive = true,
        };

        private static string? ReadEmbedded(string name)
        {
            var assembly = Assembly.GetExecutingAssembly();
            using var stream = assembly.GetManifestResourceStream(name);
            if (stream is null)
            {
                return null;
            }
            using var reader = new StreamReader(stream);
            return reader.ReadToEnd();
        }
    }
}
