# StoreStandInFix.dll —— 独立实验 DLL（未并入 90CN.dll）

> 目标：消掉"**别人看店主时被一具默认外观精灵挡住**"的现象。
> 状态：**实验阶段**。验证通过后才考虑并入 `client-patch/90CN.cpp`。

## 一、结论回顾（全部来自本客户端代码 + 现场内存实证）

客户端收到服务端 op538（副职业商店）后，自己走这条链：

```
op538 handler 0x01D8AE70
  entity = sub_0268FDF0(host, kind')          -> host+0x534   (0x3D4B)
  setter: 位置/名字/费用/mode(entity+0x3C4)
  sub_02694BB0(host)                          收尾
     └ sub_0268FF40(host, kind'')             -> host+0x53C  ← 那具"默认外观"
           kind'' ∈ {1,2}: new(0x624) + sub_02F7E820(obj, host, kind)   类 vtable 0x048559D4
           kind'' == 0   : new(0x608) + sub_02F7EBD0(obj, host, 0)      摆摊人偶，必须保留
```

现场实证（两台客户端同时开着店时）：

| 客户端 | 对象 | 指向 | kind | mode | gate |
|---|---|---|---|---|---|
| A | 自己的店 | — | 1/3 | **0** | **1** |
| A | 别人的店 | 商店实体 | 1/3 | **2** | **0** |

- **自己的店不建这个对象**（所以自己视角正常），**别人的店必建**（所以只有别人看得到那具精灵）；
- 它的创建**只按商店类型分支，与 `mode`/`gate` 无关** → 改服务端那个字节治不了它；
- 538 里没有任何外观字段 → **服务端的通知内容改不动它的外观**。

## 二、本 DLL 做了什么

在 `sub_0268FF40`（`0x0268FF40`）入口做 5 字节 inline hook，**只对 `kind ∈ {1,2}` 返回 null**：

- 影响面已被证明是精确的：
  - `sub_0268FF40` 全 .text 只有 **2 个调用点**：商店收尾 `0x02694C56`（kind=1/2）和 `0x023D943F`（`push 0`，即摆摊人偶）；
  - `kind=1/2` 的构造器 `sub_02F7E820` **只**被该工厂内部调用（`0x0268FF9C`）。
- 两个调用点都对返回值判空（`test edi,edi; je`）→ 返回 null **不会崩**。
- 商店标签/面板画的是 **entity**（`[window+0x358]` → entity），不是这个对象 → 标签不受影响。
- hook 的返回约定与被 hook 函数一致（都是 `ret 4`），已反汇编自验。

## 三、用法

1. 注入（注入方式你自己那套）：把 `StoreStandInFix.dll` 注入到 **想验证的那台客户端**。
   测试期建议**只注入一台**（看别人那台），方便和另一台对照。
2. 配置 `StoreStandInFix.ini`（与 DLL 同目录，缺省即 `skip12`）：

```ini
[fix]
mode=skip12    ; skip12 = 拦掉 kind 1/2（本次实验）；log = 只记日志不改行为；pass = 完全放行
kinds=1,2      ; skip12 作用于哪些 kind（0 = 摆摊人偶，永远不动）
```

3. 日志：`<游戏目录>\StoreStandInFix.log`。看到这些行说明钩子工作正常：

```
StoreStandInFix loaded: process=... module=0x00400000 mode=0 kinds=1,2
prologue matched after 0 ms: 55 8B EC 6A FF | 68 16 37 E3 03
hook installed target=0x0268FF40 trampoline=... detour=...
skip stand-in #1 host=0x........ kind=1 (returning null)
```

若出现 `install aborted: prologue never matched` → 地址/版本不匹配，本次不动任何代码（安全失败）。

## 四、验证清单（必须逐条过，尤其第 3 条）

| # | 项目 | 通过标准 |
|---|---|---|
| 1 | 对方视角：那具默认精灵 | **消失**，能看到店主的真实外观 |
| 2 | 商店标签/面板 | 仍然显示"附魔/分解 + 手续费 x金币" |
| 3 | **对方还能不能开店**（点店主/点原位） | **能打开商店窗口**（最关键：那具替身可能同时是点击目标） |
| 4 | 关店后 | 无残留、无报错 |
| 5 | 自己视角 | 完全无变化 |
| 6 | 附魔店 + 分解店 | 两种都试 |
| 7 | 摆摊（人偶） | 摆摊人偶仍正常（证明 kind=0 未被影响） |

**若第 3 条失败**：说明那具替身是点击目标 → 换成"保留对象、只让它不可见/换外观"的方案（下一步再做），不要用当前这个版本。

## 五、回滚

- 最快：`StoreStandInFix.ini` 改 `mode=pass`（或直接不注入 DLL）。
- 它不修改任何文件、不写 90CN.dll、不改 EXE；只在内存里改 5 字节，进程退出即消失。

## 六、文件与构建

| 文件 | 说明 |
|---|---|
| `StoreStandInFix.cpp` | 全部实现（自包含，无 CRT DLL 依赖，`/MT` 静态 CRT） |
| `StoreStandInFix.vcxproj` | Win32 / v143 / Release，含 map 文件 |
| `StoreStandInFix.ini` | 运行期配置 |
| `build_store_standin_fix.bat` | 构建并（尽力）复制到 `D:\test` 下含 DNF.exe 的目录 |

构建产物 SHA256（2026-09-21）：

```
StoreStandInFix.dll  BBC5BFA7B084CB8AF8C269800D2DA5F43EEDA77CDA023E64804D2FCB4FB0223E  119296 bytes
```

## 七、后续并入 90CN.dll（仅在验证通过后）

1. 把本文件的 hook 逻辑搬进 `client-patch/90CN.cpp`（沿用其既有的"校验前导字节后再打补丁"的风格）；
2. 目标地址与本次完全相同：`0x0268FF40`；kind 过滤保持 `{1,2}`；
3. 并入后 `StoreStandInFix.dll` 即可下架。
