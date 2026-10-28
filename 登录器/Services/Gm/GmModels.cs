using System;
using System.Collections.Generic;

namespace DNFLogin.Gm
{
    /// <summary>角色列表视图（与 Go 版 characterView 对齐）</summary>
    public sealed class CharacterView
    {
        public string CharacterID { get; set; } = "";
        public string AccountID { get; set; } = "";
        public string Name { get; set; } = "";
        public int Level { get; set; }
        public long Gold { get; set; }
        public long Cera { get; set; }
    }

    /// <summary>角色属性块</summary>
    public sealed class StatBlock
    {
        public string CharacterID { get; set; } = "";
        public string Name { get; set; } = "";
        public int Level { get; set; }
        public string Job { get; set; } = "";
        public int GrowType { get; set; }

        public long HPMax { get; set; }
        public long MPMax { get; set; }
        public int Strength { get; set; }
        public int Intelligence { get; set; }
        public int Vitality { get; set; }
        public int Spirit { get; set; }
        public int PhysicalAttack { get; set; }
        public int PhysicalDefense { get; set; }
        public int MagicalAttack { get; set; }
        public int MagicalDefense { get; set; }
        public int IndependentAtk { get; set; }

        public int FireRes { get; set; }
        public int WaterRes { get; set; }
        public int DarkRes { get; set; }
        public int LightRes { get; set; }

        public List<int> AbnormalResistance { get; } = new(new int[18]);

        public long InventoryLimit { get; set; }
        public int HPRegenSpeed { get; set; }
        public int MPRegenSpeed { get; set; }
        public long MoveSpeed { get; set; }
        public int AttackSpeed { get; set; }
        public int CastSpeed { get; set; }
        public int HitRecovery { get; set; }
        public int JumpPower { get; set; }
        public long Weight { get; set; }
        public int StatLevel { get; set; }
    }

    public static class AbnormalLabels
    {
        public static readonly string[] Labels =
        {
            "减速", "冰冻", "中毒", "眩晕", "诅咒", "失明", "感电", "石化",
            "睡眠", "灼伤", "即死", "出血", "穿刺", "回避率", "混乱", "束缚", "所有异常",
        };
    }

    /// <summary>SP/TP 块</summary>
    public sealed class SptpBlock
    {
        public string CharacterID { get; set; } = "";
        public string Name { get; set; } = "";
        public int Level { get; set; }
        public int TotalSP { get; set; }
        public int RemainingSP { get; set; }
        public int TotalTP { get; set; }
        public int RemainingTP { get; set; }
    }

    /// <summary>职业信息</summary>
    public sealed class JobInfo
    {
        public int JobID { get; set; }
        public int FirstGrow { get; set; }
        public int SecondGrow { get; set; }
        public string JobName { get; set; } = "";
        public string GrowTypeName { get; set; } = "";
        public string AwakeningName { get; set; } = "";
    }

    /// <summary>转职分支定义</summary>
    public sealed class GrowTypeDef
    {
        public int GrowType { get; set; }
        public string Name { get; set; } = "";
        public List<string> Awakenings { get; set; } = new();

        public override string ToString() => string.IsNullOrEmpty(Name) ? $"转职 {GrowType}" : Name;
    }

    public sealed class ClassDef
    {
        public int JobID { get; set; }
        public string JobName { get; set; } = "";
        public List<GrowTypeDef> GrowTypes { get; set; } = new();

        public override string ToString() => string.IsNullOrEmpty(JobName) ? $"职业 {JobID}" : JobName;
    }

    // ── 物品发放模型（移植 direct_items.go） ─────────────────────

    public enum DirectItemKind
    {
        Stackable,
        Equipment,
    }

    public sealed class DirectItemDefinition
    {
        public uint ItemID { get; set; }
        public string Name { get; set; } = "";
        public DirectItemKind Kind { get; set; }
        public string PVFPath { get; set; } = "";
        public string AttachType { get; set; } = "";
        public string StackableType { get; set; } = "";
        public string EquipmentType { get; set; } = "";
        public long StackLimit { get; set; }
        public short SlotStart { get; set; }
        public short SlotEnd { get; set; }
        public byte ListType { get; set; }
        public string Route { get; set; } = "";
        public bool SupportsUpgrade { get; set; }
        public byte ReinforceLevel { get; set; }
        public byte AmplifyLevel { get; set; }
        public byte AmplifyType { get; set; }
        public ushort AmplifyValue { get; set; }
        public ushort Durability { get; set; }
        public DateTime ExpirationDate { get; set; }
        public long UsablePeriodDays { get; set; }
    }

    public sealed class DirectItemSearchView
    {
        public uint ItemID { get; set; }
        public string Name { get; set; } = "";
        public string Kind { get; set; } = "";
        public string Category { get; set; } = "";
        public string PVFPath { get; set; } = "";
        public string StackableType { get; set; } = "";
        public string EquipmentType { get; set; } = "";
        public long StackLimit { get; set; }
        public byte ListType { get; set; }
        public string Destination { get; set; } = "";
        public bool SupportsUpgrade { get; set; }
    }

    public sealed class DirectItemSearchResult
    {
        public List<DirectItemSearchView> Items { get; set; } = new();
        public int Total { get; set; }
        public int Page { get; set; }
        public int PageSize { get; set; }
        public int PageCount { get; set; }
    }

    public sealed class DirectStoredItem
    {
        public string Key { get; set; } = "";
        public long ItemID { get; set; }
        public long Count { get; set; }
        public bool Bind { get; set; }
        public DateTime ExpireAt { get; set; }
        public byte[] Raw { get; set; } = Array.Empty<byte>();
        public Dictionary<string, string> Extra { get; set; } = new();
    }

    public sealed class DirectItemPlan
    {
        public CharacterView Character { get; set; } = new();
        public DirectItemDefinition Definition { get; set; } = new();
        public uint Amount { get; set; }
        public string Route { get; set; } = "";
        public long Existing { get; set; }
        public long After { get; set; }
        public int SlotsFree { get; set; }
        public List<short> Touched { get; set; } = new();
        public Dictionary<string, DirectStoredItem> CharacterDB { get; set; } = new();
        public Dictionary<string, DirectStoredItem> AccountDB { get; set; } = new();
        public HashSet<string> CharTouched { get; set; } = new();
        public HashSet<string> AcctTouched { get; set; } = new();
    }

    public sealed class DirectItemPlanView
    {
        public CharacterView Character { get; set; } = new();
        public DirectItemDefinition Definition { get; set; } = new();
        public uint Amount { get; set; }
        public string Route { get; set; } = "";
        public long Existing { get; set; }
        public long After { get; set; }
        public int SlotsFree { get; set; }
        public List<short> Slots { get; set; } = new();
    }

    /// <summary>PVF 文件信息</summary>
    public sealed class PvfInfo
    {
        public string Path { get; set; } = "";
        public long Size { get; set; }
        public string Sha256 { get; set; } = "";
        public string Format { get; set; } = "";
        public int FileCount { get; set; }
    }
}
