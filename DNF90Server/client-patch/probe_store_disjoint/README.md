# StoreDisjointProbe.dll —— 独立探针（未并入 90CN.dll，只读、不改行为）

> 目标：在写"个人商店分解机批量分解"的钩子**之前**，用一次实机运行把 4 个会改变钩子形态的未知点问清楚。
> 状态：**探针阶段**。验证通过后才考虑并入 `client-patch/90CN.cpp`。

## 一、为什么需要这个探针

计划是把 op235（个人商店/玩家分解机）按 op26（NPC/系统分解机）已有的方式批量化：
拦掉逐件发送 → 发**一个**带标记的信封（含全部选中槽位）→ 服务端一个事务结算。

op26 那套之所以成立，是因为 op26 的循环**在一趟里把 N 件全部发出去**，DLL 才能捕到 N 个槽位
（`SendCurrentEquipmentDisjointBatch` 硬要求 `capture.count == expectedCount`；生产日志里
`kind=game-disjoint-item-batch-committed slot_count=24` 就是这条链跑通的证据）。

静态分析显示 op235 的循环 `sub_2C38840` 与 op26 的 `sub_2C38260` 同形，但下面 4 件事**必须实机确认**，
因为它们决定钩子写成哪种形态：

| # | 问题 | 为什么重要 |
|---|---|---|
| **Q1** | 一趟 `sub_2C38840` 到底调用发送器 **N 次**（多选一次性连发），还是**只调 1 次**（单件路径，每件一个往返）？ | 决定钩子是"照抄 op26 模板"还是"跨趟累积后再发批量包"。**这是最关键的一条。** |
| **Q2** | "全部分解"时 `[win+0x1EC]` 是否为 0？ | 非 0 表示走单件分支，循环发完第一件就退出 → 只捕到 1 件 → 批量永远不满足前置条件、退回逐件 |
| **Q3** | 循环运行期间 `[win+0x1B8]` 是否恒为 1？ | 若循环内被改成 2，后续迭代会被跳过，同样是逐件往返 |
| **Q4** | 每次发送时 `[listMgr+0x7C]` 是否为 0？ | 原生 op235 发送器在该字节非 0 时**静默不发包但仍返回 1**（`0x1F30D9A cmp byte [obj+0x7C],0 / jne`）。钩子必须复刻这个判定，否则会给客户端本来要跳过的道具发批量请求 |

探针同时记录每趟的选中集合、选中数、`[win+0x180]`（店主 ID）、`[win+0x190]`（机器耐久）、
`[win+0x1B0]`（mode/部位号），以及**每次进入窗口状态设置器 `sub_2C313A0` 的时间戳** ——
"N 个包一起走" 和 "一个包一个往返" 在日志里能直接看出来。

**它不改任何行为**：每个代理都调用原函数并原样返回其结果。最坏情况只是日志没内容。

## 二、构建与注入

```bat
client-patch\probe_store_disjoint\build_store_disjoint_probe.bat
```

构建后会把 `StoreDisjointProbe.dll` / `.ini` 复制到 `D:\test` 下含 `DNF.exe` 的目录
（和 `StoreStandInFix` 同一套约定）。**复制不等于加载** —— 用你平时那套注入方式把它注入 `DNF.exe`
（和注入 `StoreStandInFix.dll` 一样）。

建议**只注入一台客户端**，方便和另一台对照。

构建产物（本次）：

```
StoreDisjointProbe.dll  476FF5665683A07EA6AE1780F9D76C39DBEAF44DDB733E0FF9CA199E465029E3  125440 bytes
```

## 三、操作步骤（照这个顺序走一遍就够）

1. 注入 DLL，登录，进入城镇。
2. 先找 **NPC 分解机**（对照组）：选中 **≥3 件**装备，点"全部分解"。
3. 再找 **个人商店分解机**（副职业分解机，另一名玩家开的店）：同样选中 **≥3 件**，点"全部分解"。
   - 若手边没有别人的店，就用自己的分解师机器也行（模式号可能不同，日志里能看到）。
4. 开 `<游戏目录>\StoreDisjointProbe.log`，只看 `ENTER` / `EXIT` 行即可得出全部答案。
5. 顺手把 `SEND235` 行扫一眼，确认 Q4。

### 日志长什么样

```
StoreDisjointProbe loaded: process=1234 module=0x00400000 mode=0 logSends=1 control=1
prologues matched after 0 ms
hook installed op235-loop  sub_2C38840 target=0x02C38840 len=5 trampoline=...
hook installed op235-send  sub_1F30B80 target=0x01F30B80 len=6 trampoline=...
hook installed op26-loop   sub_2C38260 target=0x02C38260 len=5 trampoline=...
hook installed op26-send   sub_1F30630 target=0x01F30630 len=6 trampoline=...
hook installed state-set   sub_2C313A0 target=0x02C313A0 len=6 trampoline=...
install done: 5/5 hooks live

ENTER 235 #1 win=0x0A1B2C3C part=0xF sel=5 [0x0A1B4000..0x0A1B4014] st=0x1 pend=0x00000000 owner=3456 endur=268 f184=1 lim18C=0x0 f438=0x0 f434=0
SEND235 #1 mgr=0x0A1B5000 slot=12 owner=3456 mgr7C=0x00 -> native will emit the packet
SEND235 #2 mgr=0x0A1B5000 slot=17 owner=3456 mgr7C=0x00 -> native will emit the packet
...
EXIT  235 #1 sends=5 refused=0 wouldSkip7C=0 selAtEntry=5 firstSlot=12 lastSlot=31 sendSpanMs=0 | after: st=0x2 pend=0x00000000 f434=5 f438=0x0 endur=268
```

## 四、结果怎么读（判定表）

先看 **EXIT 行**的 `sends=` 和 **ENTER 行**的 `sel=`：

| 观察到的现象 | 结论 | 客户端钩子该怎么做 |
|---|---|---|
| op26：`sel=5 sends=5`（`sends == sel`） | 对照组正常，一趟发完 N 件（与生产日志一致） | — |
| op235：`sel=5 sends=5` | **多选一趟连发** | **照抄 op26 模板**（`expectedCount == count`），改动最小 |
| op235：`sel=5 sends=1`，且反复出现多趟 `ENTER/EXIT`（`STATE ... -> 1` 夹在中间） | **逐件往返**：一趟只发一件，靠 UI 刷新重新武装后再发下一件 | 钩子改成**按窗口跨趟累积**：发送器代理缓冲 slot，等选中集合耗尽 / 状态转 2 时再发一个批量包 |
| op235 完全没有 `ENTER 235` 行，但有 `SEND235` | 走的不是 `sub_2C38840` | 需要重新定位入口，先别写钩子 |
| `ENTER` 行里 `pend != 0` | 走单件分支（Q2 命中） | 同上：必须跨趟累积 |
| `ENTER` 行里 `st != 1` | 状态不是"可发一件"（Q3 命中） | 钩子需要处理这种时机，或先查清是谁改的状态 |
| 任一 `SEND235` 行里 `mgr7C != 0x00` | 客户端会静默跳过该件（Q4 命中） | **代理必须复刻** `[mgr+0x7C] == 0` 判定，非 0 时不纳入批量 |
| 出现 `SEND235 refused by native (ret=0)` | 原生拒绝发送 | 批量包里必须排除这些槽位 |

另外两个附带信息：

- `selAtEntry` 与 `firstSlot/lastSlot`：确认批量信封要携带的槽位取值范围。
- `sendSpanMs`：一趟里第一件到最后一件的间隔。**若 ≈0 说明 N 个包是同一瞬间一起发出的**（那就是服务端/呈现慢）；
  **若随件数线性增长**说明发送本身在被串行化。这一条能把"慢"的归属一次性分清楚。

## 五、回滚

- 最快：把 `StoreDisjointProbe.ini` 的 `mode` 改成 `off`（或干脆不注入）。
- 它不改任何文件、不写 90CN.dll、不改 EXE；只在内存里改十几个字节，进程退出即消失。
- 万一前导字节不匹配（客户端版本不同），它会**什么都不打**并写一行
  `install aborted: prologues never matched ... nothing was patched`。

## 五之二、实测踩坑记录：钩子地址必须加模块基址（2026-09-23）

**探针最早几版全是白跑的，根因是探针自己的一行地址算术错误**，不是客户端的问题：

```
resolved op235-loop by fingerprint ... at 0x2C38840
scan-view @0x2C38840 : 55 8B EC 6A FF 68 B8 0A          ← 扫描器读到真实代码
NOT installing: prologue re-check failed (83 C1 04 ...)  ← 钩子地址读到别的内存
```

`kRvaXXX` 是 **RVA**，真实地址是 `base + RVA`。特征码扫描用的是 `mod + va + i`（正确），而钩子地址当时写成
`rva + delta`，`delta = base - 0x400000 = 0` → 算成了裸 RVA `0x2C38840`，**整整少了 0x400000**，读到的是
另一块已映射内存。于是：

- 前导字节检查**永远失败**（`0x2C38840` 里当然没有那段代码）；
- 特征码扫描却**命中正确**（它走的是正确的地址）；
- 两者看起来"同一地址结果不同"，我一度误判成"客户端按需分页解密"——**那个结论是错的，客户端是普通的**，
  镜像里 `.text` 与磁盘文件一致（`EXE PE` 指纹逐字段相同）。

修法：`address = base + RVA`；重定位同样 `base + foundRva`。**教训**：以后凡是"扫描器能找到、按地址读却不对"，
第一件事就是核对两边是否用了同一套地址基。

另外两条同样影响成败的坑（已修）：

1. **安装不能互相等待**：原来 gate 在"5 个目标全部定位成功"，而 op26 对照组是可选目标，一旦它没解析出来，
   三个关键钩子一个都装不上。改成**定位一个立刻装一个**。
2. **可选目标要能后补**：看守线程会持续尝试未定位的目标，并在页面可用时装上。

## 六、合并前必须记住的两件事（自检已经踩过）

`tools\_dis_probe_selfcheck.py` 在本模块上抓到两个**编译不报错、但会在运行时坏事**的问题，
并入 90CN 时要照抄修好的版本：

1. **发送器代理不能返回 `void`**。原函数返回 `bool`（`al`），调用方是
   `test al,al; je <失败分支>`。代理若声明成 `void`，`al` 会带着垃圾值回去，
   整趟分解会被判成失败并弹错误框。→ 必须声明为 `unsigned char` 并 `return` 原函数的返回值。
2. **`sub_2C313A0` 的钩子长度必须是 6，不是 5**。前 5 字节是
   `55 8B EC 8B 45`，而第三条指令 `mov eax,[ebp+8]` 是 3 字节 —— 5 字节会切在指令中间，
   trampoline 会执行半条指令。→ 长度取 6（`55 8B EC 8B 45 08`）。

自检覆盖：① 每个代理结尾的 `ret N` 与目标 `ret N` 一致（0/8/8/4）；
② 5 组前导常量确实在模块里且按整指令对齐；③ 模块无导出；④ 前导常量与**真实客户端 EXE**
的目标地址逐字节一致。当前结果：**ALL CHECKS PASSED**。

```bat
python tools\_dis_probe_selfcheck.py
```

## 七、文件

| 文件 | 说明 |
|---|---|
| `StoreDisjointProbe.cpp` | 全部实现（自包含，无 CRT DLL 依赖，`/MT` 静态 CRT） |
| `StoreDisjointProbe.vcxproj` | Win32 / v143 / Release，含 map 文件 |
| `StoreDisjointProbe.ini` | 运行期配置（`mode` / `logSends` / `control`） |
| `build_store_disjoint_probe.bat` | 构建并（尽力）复制到 `D:\test` 下含 DNF.exe 的目录 |
| `..\..\tools\_dis_probe_selfcheck.py` | 构建后自检（见第六节） |

钩子目标（模块基址 `0x00400000`）：

| 名称 | RVA | 前导字节 | 长度 | 约定 |
|---|---|---|---|---|
| op235 循环 `sub_2C38840` | `0x02C38840` | `55 8B EC 6A FF 68 B8 0A ED 03` | 5 | `__thiscall void(this)`, `ret` |
| op235 发送 `sub_1F30B80` | `0x01F30B80` | `55 8B EC 83 EC 18 56 8B F1` | 6 | `__thiscall bool(this, slot, ownerID)`, `ret 8` |
| op26 循环 `sub_2C38260`（对照） | `0x02C38260` | `55 8B EC 6A FF 68 28 AF F1 03` | 5 | 同上 |
| op26 发送 `sub_1F30630`（对照） | `0x01F30630` | `55 8B EC 83 EC 28 A1 3C 67 0E 05` | 6 | `__thiscall bool(this, slot, ctx)`, `ret 8` |
| 窗口状态设置器 `sub_2C313A0` | `0x02C313A0` | `55 8B EC 8B 45 08 56 8B F1` | **6** | `__thiscall void(this, newState)`, `ret 4` |
