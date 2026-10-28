# RenameLogNames.dll —— 角色名称变更记录「角色名」列（**已退役，v5 起为兼容补丁**）

> ## 先读这一段（2026-09-23 结论）
>
> **这个 DLL 不再需要注入。** 角色名本来就在协议里：行的第三个字段不是"键"，而是
> **`u32` 长度 + 该长度的 GBK 字节**（读取器 `0x3457C80`，详见下方"零、真相"一节）。
> 之前服务端把那 4 字节写成了 `0`，客户端于是走"没有名字"的兜底分支，那一列才恒为空。
> 服务端已改为**行内带名字**，客户端原生就会显示。
>
> 现在有三种做法，都不影响该窗口：
> 1. **推荐**：把 `RenameLogNames.dll` 从注入列表里删掉；
> 2. 在游戏目录放一个空文件 `RenameLogNames.off`（本模块自我禁用）；
> 3. 换成 `Release_v5\RenameLogNames.dll`（`替换DLL_v5.bat`）：它会识别新版布局并自动
>    成为空操作。
>
> 为什么还要出 v5：v4 的行遍历写死 `1 + 9*行数`。新版行是变长的，若客户端还挂着 v4 去开
> 这个窗口，它会把名字字节当成"名字个数"（可能读到几万）→ 越界读取风险。v5 先按客户端
> 的真实布局做合法性遍历，新版包直接判为 `native row layout detected (inert)`。
> 旧版服务端（9 字节行 + 尾部名字块）在 v5 下**仍然照旧可用**，方便回滚。
>
> 完整逆向与验证记录：`改名记录_角色名走协议_逆向与实现_20260923.md`

## 零、真相：第三个字段是什么

`0x3457D20(dst, maxLen=0x1F, 兜底串)` → `0x3457C80(dst, maxLen, 兜底)`：

```asm
0x03457C9F  call 0x34E3A50        ; 读 u32 → len
0x03457CAA  cmp  eax, esi         ; len >= 0x1F → 兜底，不再吃字节
0x03457CAE  test eax, eax         ; len == 0    → 兜底，不再吃字节
0x03457CB7  call 0x34E3AC0        ; 读 len 个字节（GBK）
```

所以：

| 发的 4 字节 | 客户端行为 | 结果 |
|---|---|---|
| `0`（旧服务端一直发的） | len==0 → 兜底 | 空白列，只吃 4 字节 |
| 任意 ≥0x1F 的垃圾值 | len≥31 → 兜底 | 空白列，只吃 4 字节 |
| `1..30` | 读该长度字节 | **显示这些字节** |

前两种情况表现完全一样（稳定吃 4 字节 + 空白），这就是当初被误读成"查表失败"的原因；
`0x4B9958C`（本模块钩住的那个资源）只是"没有名字时"的兜底串，正规服务端永远走不到。

## 一、它原本解决什么（v1–v4，已过时）

「角色名称变更记录」窗口由 class-0 **opcode 733** 处理器 `0x015E7D60` 绘制。服务端因此在行数据之后追加一段名字块（stock 客户端只按 `rowCount` 读行，尾部字节它从不读，所以**没打补丁也不会坏**）：

```
u8  rowCount
每行: u32 nameLen + nameLen 字节 GBK 名字 + u8 大区下标 + u32 unix 时间   ← 现行布局（v5 服务端）
u16 nameCount                 ← 旧版名字块（仅旧服务端会发；v5 仍支持解析）
每个名字: u32 字节长度 + GBK 字节
```

## 二、它怎么做（两个钩子）

| 钩子 | 目标 | 动作 |
|---|---|---|
| 1 | 733 处理器入口 `VA 0x015E7D60`（RVA `0x011E7D60`） | 从客户端自己的游标全局（`0x52F8500` 游标 / `0x52F8504` 剩余字节）**只读**解析出名字块并缓存。读这两个全局是安全的：处理器第一步就是从这个游标读行数 |
| 2 | 字符串资源解析器 `VA 0x34D9B20`（RVA `0x030D9B20`） | 处理器每行都会为 `0x4B9958C` 调它一次、并把返回值 `wcsncpy` 进第 3 格。参数命中该资源且还有名字时，改返回下一个缓存名字（最多 30 个宽字符） |

除此之外**不改任何包、任何游戏状态、任何其它内存**。名字块不存在时，行为与 stock 完全一致（那一列照旧为空）。

## 三、构建

```
client-patch\rename_log_names\build_rename_log_names.bat
```

产物：`client-patch\rename_log_names\Release\RenameLogNames.dll`（32 位，仅依赖 KERNEL32，静态 CRT）。
**本工程不往游戏目录拷贝 DLL**——注入由你操作。

客户端运行时该文件被占用，所以新版先编到 `Release_v5\`：

```
MSBuild.exe RenameLogNames.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32 /p:OutDir=Release_v5\
替换DLL_v5.bat        （关掉客户端后运行，把 v5 换到注入器使用的 Release\ 路径）
```

## 四、注入与验证

1. 注入 `RenameLogNames.dll` 到 `DNF.exe`（你惯用的方式）。
2. 注入后立刻会写出 `<游戏目录>\RenameLogNames.log`，首行形如：

   ```
   RenameLogNames attached base=0x00400000 handler=0x015E7D60 resolve=0x034D9B20 resource=0x04B9958C
   hook installed target=0x015E7D60 trampoline=... stolen=6
   hook installed target=0x034D9B20 trampoline=... stolen=6
   ```

   若出现 `hook aborted: prologue mismatch ... got=... want=...`，说明客户端不是我分析的那版
   （把日志给我即可），此时**不会**修改任何代码，客户端行为不变。

3. 进游戏 → 打开「角色名称变更记录」，日志应出现：

   ```
   name block loaded rows=1 names=1
   name served row=0 text=1111
   ```

4. 窗口应显示 **1 行**：

   | 日期 | 大区名 | 角色名 |
   |---|---|---|
   | 2026.09.23 | 奇迹暖暖 | **1111** ← 本次补丁新增 |

> 注意：之前几次实验在窗口里留下的旧行不会自动消失，**重启客户端**再看才是干净结果。

## 五、回滚

卸载/移除该 DLL 即可：那一列立刻恢复为空，其余一切不变（服务端多发的名字块是纯追加，stock 客户端永不读取）。

## 六、合并进 90CN.dll —— **已取消**

原计划是把这两个钩子并进 `90CN.dll`。既然角色名已确认走协议（服务端行内携带），
**不再需要合并**：留一个只做兼容的独立模块，比往 `90CN.dll` 里加两个只在旧版服务端才有
意义的钩子更安全。

保留本模块的唯一用途：服务端若回滚到"9 字节行 + 尾部名字块"的旧版本，注入 v5 即可恢复
那一列。

## 七、修订记录（含一次真实崩溃）

### v1 —— 注入后点窗口闪退（已修）

客户端自己的异常日志（`90CN_trace.log`）与模块日志对上了：

```
hook installed target=015E7D60 trampoline=0C7E0000 stolen=6      ← v1 只偷了 6 字节
name block loaded rows=1 names=1                                 ← 包解析其实是对的
exception code=0xC0000096 eip=0x0C7E000A                         ← 崩在跳板 +0x0A
```

`0xC0000096` = 非法/特权指令。733 处理器序言是
`55 | 8B EC | 6A FF | 68 <imm32>` = 1+2+2+**5** = 10 字节，v1 只偷 6 字节，
把 `push imm32` **从中间切断**：跳板执行完 6 字节后，剩下的立即数字节被当指令执行，
一路走到 `E9` 的尾字节（`+0x0A`）→ 非法指令崩溃。

**同时这次崩溃也证明了设计本身是通的**：崩溃前日志已经打出
`name block loaded rows=1 names=1`（名字块解析成功）和 `name served row=0 text=1111`
（第三格取到了旧名），坏的只是跳板。

### v2（当前）修正

1. **只偷完整指令**：处理器 10 字节、解析器 6 字节。
2. **跳转全部改成绝对间接跳** `FF 25 <abs32>`，不再用相对 `E9`，避免模块被注入到
   离 EXE 超过 ±2GB 时 rel32 溢出。
3. 序言校验允许只比对前 6 字节（`push <SEH handler>` 的立即数随客户端版本变化，
   原样搬运但不要求固定值）。
4. `remaining` 合理性检查（3..65536），异常值直接跳过、不解析。
5. 新增离线自检：`tools\_renamelog_check_prologues.py`（反汇编证明偷的窗口是完整指令）
   与 `tools\_renamelog_verify_addrs.py`（逐个比对 5 个地址常量）。

### v2 —— 注入后进程一起来就死（已修）

```
hook installed target=034D9B20 trampoline=0B6F0000 stolen=6 (whole instructions)
exception code=0xC0000005 eip=0x0B6F0006 access_address=0x041CDCE0
```

`eip = 跳板 + 0x06`，正好在偷来的 6 字节之后。v2 把**回跳指令写到了固定偏移 `0x10`**，
而偷来的窗口只有 6 字节：执行完 6 字节后掉进中间那段填充字节（`00 00` = `add [eax], al`），
往只读段写 → 访问违例。解析器在启动阶段就会被调用（区服名），所以进程一起来就崩。

### v3（当前）修正

1. **回跳指令紧跟偷来的字节**（`trampoline + prologueLen`），不再用固定偏移。
2. 跳板填充改成 `0xCC`（INT3）——万一再发生"掉进填充"，会立刻断在明处而不是执行 `00 00`。
3. 新增 `tools\_renamelog_check_trampoline.py`：按模块同样的算法在离线重建跳板并反汇编，
   验证「偷的字节是完整指令」+「回跳位置正确、目标地址正确」。

三次崩溃都靠日志一次定位（`90CN_trace.log` 的 `exception code/eip` + 模块自己的日志），
所以模块始终把 `remaining`、`rows`、`names`、`served row` 都写进日志。

### v5（当前）—— 退役 + 防越界

1. 查明第三个字段是"长度 + 字符串"后，服务端改为行内带名字，本模块**不再需要**。
2. 新增 `WalkRenameLogRows()`：按客户端真实布局
   （`u32 nameLen` + 仅当 `0 < nameLen < 0x1F` 才有的名字字节 + `u8` 大区 + `u32` 时间）遍历。
   于是：
   * 新版包 → 行块恰好走到包尾 → 判为原生布局，**空操作**（日志 `native row layout detected`）；
   * 旧版包 → 行块之后还有名字块时才解析，且要求 `names` 条**精确用完**剩余字节，否则整段丢弃
     （日志 `name block rejected`）。
3. 修掉 v4 的越界隐患：v4 用 `1 + 9*rows` 定位名字块，遇到变长行会把名字字节当名字个数。
4. 离线自检：`tools\_renamelog_walker_check.py`（8 个用例：新旧布局、多行、中文名、截断包、空列表）。

## 八、一键禁用（不用删 DLL）

在 `<游戏目录>` 放一个空文件 `RenameLogNames.off`，模块就**只加载不挂钩**（日志写一行
`hooks disabled by RenameLogNames.off`）。删掉该文件即恢复。

## 九、已知边界

- 名字最多 30 个宽字符（客户端 `wcsncpy` 上限 31），GBK 编码，超出会被客户端截断。
- 名字块最多 64 条（服务端历史上限 12 条），仅为防畸形包的兜底。
- 本模块只影响这一个窗口；大区名/日期仍由原生逻辑渲染（大区名走 `90CN` 已有的
  `ProxyRegionNameResolver`，`regionId=1` → 「奇迹暖暖」）。
