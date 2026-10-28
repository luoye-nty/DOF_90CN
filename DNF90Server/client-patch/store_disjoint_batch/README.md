# StoreDisjointBatch.dll —— 独立批量分解模块（未并入 90CN.dll）

> 目标：把个人商店分解机（op235）"全部分解"的 N 个逐件命令**合成一个**，让服务端一个事务结算。
> 状态：**实验阶段**。验证通过后才考虑并入 `client-patch/90CN.cpp`。
> 姊妹探针（只读、已跑通）：`client-patch/probe_store_disjoint/`

## 一、为什么要做

用只读探针在实机测出来的事实（2026-09-23 07:45，`StoreDisjointProbe.log`）：

```
ENTER 235 #363  part=0xF  sel=8  st=0x1  pend=0x00000000  owner=5  endur=144
SEND235 #1..8   slot=9,10,11,12,13,14,15,16   owner=5   mgr7C=0x00     ← 同一毫秒
EXIT  235 #903  sends=8  refused=0  selAtEntry=8  sendSpanMs=0
                | after: st=0x2  f434=8
```

**客户端一趟循环、1 毫秒内就把 8 件全发出去**，与 op26（NPC 分解机）的循环完全同构。
所以慢的不是客户端发包节奏，而是"N 个包 = N 个事务"，而每个事务还会：

- **重建 5 个容器列表**（`currentSelectedItemListTypes = {0,1,2,宠物,公会勋章}`）
- 给店主发一次金币状态包
- 发一次 op205 职业信息

N 个回复各自相隔一个 RTT 到达、各自触发一次整容器刷新 → 界面上就是**一件一件消失**，延迟越高越慢。

## 二、它怎么做的

| 钩子 | RVA | 长度 | 约定 | 作用 |
|---|---|---|---|---|
| `sub_2C38840` op235 循环 | `0x02C38840` | 5 | `__thiscall void(this)`, `ret` | 进入时读选中数；`≥2` 且 `st==1` 且 `pend==0` 就武装捕获，退出时决定发批量包还是回退 |
| `sub_1F30B80` op235 发送器 | `0x01F30B80` | 6 | `__thiscall bool(this, slot, ownerID)`, `ret 8` | 捕获期间**丢掉这个包**并返回 `1`（返回 0 会让循环跳进失败分支） |

循环退出后：

- 捕获数 == 选中数 且 ≥2 且无异常 → 用**客户端自己的 writer** 发**一个**带标记的 op235：

  ```
  u16 235 | u16 ownerID | u16 0xFFFF | u8 listType | u32 "B35\x01" | u8 count | u16 slots[count]
  ```

- **任何异常**（重复槽位、sender 对象变了、ownerID 变了、数量对不上）→ 把**已经丢掉的那几个**按原顺序用原发送器补发 —— 也就是退回原生行为。
  设计原则：**宁可退化，不可静默少分解**。

服务端（`go-server .../expert_job_store.go`）认这个信封 → **一个** `mutateCurrentExpertJobStorePair` 事务结算 N 件 → **一个**合并回包 → **一次**增量槽位刷新。

## 三、构建与注入

```bat
client-patch\store_disjoint_batch\build_store_disjoint_batch.bat
```

会把 `StoreDisjointBatch.dll` / `.ini` 复制到 `D:\test` 下含 `DNF.exe` 的目录。
**复制 ≠ 加载**，用你平时那套注入方式注入（与 `StoreStandInFix.dll` 相同）。

> **`.ini` 必须是 ANSI + CRLF 无 BOM**。这条是踩过的坑：`GetPrivateProfileString` 读不了 UTF-8 BOM（BOM 会混进 section 名）也不接受纯 LF，DLL 会**静默回落默认值**。
> 本仓库的 `StoreDisjointBatch.ini` 已按此写入，修改时请保持编码（构建脚本不会重写已存在的 ini）。

## 四、验证流程（**务必按顺序**）

### 第 1 步：`mode=log`（只观察，不放拦）

把 ini 改成 `mode=log`，注入，重启客户端，然后：

1. 打开个人商店分解机，选 **≥3 件**，按"全部分解"
2. 看日志：

```
StoreDisjointBatch loaded: process=... module=0x00400000 mode=0 exe=D:\...\DNF.exe
prologue matched op235-loop  sub_2C38840 at 0x03038840 after 0 ms
hook installed op235-loop  sub_2C38840 target=0x03038840 len=5 trampoline=...
  patched bytes now: E9 .. .. .. .. 68
prologue matched op235-send  sub_1F30B80 at 0x02330B80 after 200 ms
hook installed op235-send  sub_1F30B80 target=0x02330B80 len=6 trampoline=...
install done: 2/2 hooks live (mode=0)
log-only pass: win=0x........ sel=8 state=0x1 pending=0x00000000 owner=5 observedSends=8 mode=0
```

**通过标准**：`observedSends == sel`（本次实测应为 8 == 8），且分解**行为与以前完全一样**（这步就应该什么都没变）。

### 第 2 步：`mode=batch`（真正生效）

ini 改回 `mode=batch`，重启客户端，同样操作，看日志：

```
batch queued win=0x........ sel=8 slots=8 first=9 last=16 owner=5 mgr7C=0x00
```

**通过标准**：

| # | 项目 | 通过标准 |
|---|---|---|
| 1 | 日志 | 出现 `batch queued ... slots=8 first=9 last=16` |
| 2 | 服务端日志 | 出现 `game-expert-job-store-disjoint-batch-committed slot_count=8`（**关键**：证明服务端是**一个事务**结算的） |
| 3 | 装备 | 8 件**全部**被分解，背包里 8 个源槽都空了 |
| 4 | 产物 | 分解产物数量与逐件分解一致（晶块/灵魂等） |
| 5 | 界面 | 不再一件一件消失；**不应该**出现残留的幽灵行 |
| 6 | 耐久 | 机器耐久按 8 件的总量扣减，耐久归零时**关店且弹出提示**（提示文案应为「分解机的当前耐久度为0，无法进行分解」，见下） |
| 7 | 金币 | 别人用你的机器时，手续费按 8 件合计扣除/入账 |
| 8 | 回退 | 故意混一件**锁定**装备进选择集合 → 整批失败并给提示（这是**预期**的整批原子行为），不应出现"分了一半" |

**若第 1 条不出现**：看有没有 `batch fallback: replaying N of M`。有 = 自动化退回了原生逐件发送（不会坏事，但说明捕获条件不满足），把那行发我。

## 五、与本次一起来的服务端改动

| 改动 | 说明 |
|---|---|
| op235 批量信封 | 识别 `B35\x01`，一个事务结算 N 件，一个合并回包 + 逐槽 op14 |
| **耐久失败码 189 → 190** | 189 在客户端 op235 显式失败码表和通用映射器里**都不存在** → 默认分支 `xor al,al; ret` → **耐久不足时玩家点什么都没反应、没有任何提示**。190 才是 dstr 44069「分解机的当前耐久度为0，无法进行分解」 |

服务端**继续接受 5 字节单件请求**，所以没注入本 DLL 的客户端行为完全不变（灰度期新旧共存）。

## 六、回滚

- 最快：ini 改 `mode=pass`（或不注入 DLL）。进程退出即消失，不改任何文件。
- 服务端：`_backup_store_disjoint_batch_20260923-074942\说明.md` 里有源码回滚与线上 exe 回滚命令。
- 本 DLL 只改内存里 5/6 个字节，并有 3 小时看守线程：被还原就重装。

## 七、自检覆盖（合并进 90CN 前必须保持通过）

```bat
python tools\_dis_batch_dll_selfcheck.py
```

当前结果 **ALL CHECKS PASSED**，覆盖 5 项：

1. **混淆名**：调用约定 `__fastcall`、栈参数个数、**返回类型**。
   两个必须记住的坑：
   - **发送器代理绝不能声明成 `void`** —— 原函数用 `al` 返回 bool，调用方 `test al,al; je <失败分支>`，
     声明成 `void` 会让 `al` 带垃圾值回去 → 整趟分解判失败并弹错误框（本项目已踩两次）；
   - **`sub_2C313A0` 这类目标钩子长度必须是 6**，5 会切断 `mov eax,[ebp+8]`。
2. 每个代理结尾的 `ret N` 与目标一致（`ret 0` / `ret 8`）。
3. 前导常量在模块里且按整指令对齐。
4. 前导常量与**真实客户端 EXE** 的目标地址逐字节一致。
5. 模块无导出表。

> 另一个已固化的坑：**钩子地址 = 模块基址 + RVA**。曾经写成裸 RVA（`rva + delta`，delta 恰好为 0），
> 结果前导字节检查永远失败、而特征码扫描却能命中（扫描走的是正确算法），一度被误判成"客户端按页解密"。
