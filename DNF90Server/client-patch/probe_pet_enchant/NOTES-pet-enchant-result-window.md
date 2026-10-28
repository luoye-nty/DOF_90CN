# 宠物附魔结果窗显示错误 —— 追查记录（2026-09-19，已按操作者要求暂停）

## 现象

宠物用附魔石附魔成功后，客户端「附魔成功!!」公告窗显示的**物品不对**（例：显示成宠物装备
「绿色鹰翼图腾」/另一只宠物），或者干脆**不弹窗**。附魔本身在服务端是成功的。
操作者原话："目前没找到规律"。

## 客户端链路（已反汇编确认）

`op272 (ENCHANT_BY_BEAD)` 的 class-1 回包由 `sub_1D0B960`（VA，image base 0x400000）处理：

```
cmp  byte ptr [ebp+0xC], 0        ; 结果字节（0 = 失败）
call sub_34E3970                  ; 读 1 字节 → [ebp+0xF]  物品空间
call sub_34E39E0                  ; 读 2 字节 → [ebp-4]    槽位
list==7 -> ecx=[0x51B2780]        ; 选择视图（不是原始容器）
list==3 -> 装备 vtable[0xB28]
其它    -> ecx=[0x51B2768]
call sub_2326860                  ; 按值解析物品对象
mov  esi, eax
mov  ecx, [0x51B2764]             ; ← 常被挂钩的位置（0x01D0B9F6，6 字节）
push 0 / push esi / push 0x3D9    ; UI 985 = 附魔结果窗
call sub_2FDD1A0                  ; 开窗（入口 10 字节 prologue: 55 8B EC 6A FF 68 68 9A EC 03）
```

服务端 `buildEnchantByBeadSuccessAck` 发的 4 字节体 = `[result][listType u8][slot u16]`，
字段口径**与客户端读取一致**（已实测）。

## 实测证据（独立探针 DLL，均已清理）

- 附魔那一刻：`[ack] list=7 slot=0 native_id=400990167` → 客户端解析出的**就是被附魔的那只宠物** ✓
- 视图快照：`view[6]`（= `0x51B2780`）`ids{0:400990167(宠物), 140:10006785(宠物装备)}` → 槽位口径正确 ✓
- 结果窗 key 全局 `0x518B798` 当时 = 0 → 经 view[6] 也解析出宠物 ✓
- **但弹窗渲染出来的是 `view[6][140]` = 上一次宠物装备附魔的那件**；弹窗里的**属性是本次新的**、**物品是上次的** ⇒ 两条数据来源不同。

结论：ACK 处理链里能读到的东西全是对的，**渲染阶段另读了一处我们尚未定位的状态**（疑似结果窗/宠物窗对象里保存的"上一次目标槽位 = 140"）。

## 踩过的坑（重要，避免重复）

1. **MSVC 内联汇编的绝对地址陷阱**：`mov ecx, dword ptr [0x51B2764]` 会被编成
   `mov ecx, 0x51B2764`（把地址当立即数，`B9` 而非 `8B 0D`）。重放被替换指令必须用
   `_emit 0x8B,0x0D,0x64,0x27,0x1B,0x05` 这类**逐字节**写法。当时导致"解析正确但弹窗
   彻底不出"（开窗 call 的 ecx 成了常数）。
2. 0x01D0B9F6 这个位置**原本没有函数调用**，插入 C 调用会破坏 xmm0-7 / x87；
   挂钩时必须 `pushfd/pushad + movups 保存 xmm0-7 + fnsave` → 调用 → `frstor + 恢复`。
3. **RVA = VA − 0x400000**：曾把 VA `0x2FDD1A0` 当 RVA 用，钩子等了 180 秒签名不匹配。
4. 90CN.dll 的 `pet enchant display compatibility` 钩子占着 `0x01D739B9`（op14 行处理的
   同模板动态状态点），独立探针不能抢这个点。

## 复现与观测工具（保留源码，二进制已清理）

- 探针源码：`client-patch/probe_pet_enchant/pet_enchant_probe.cpp`（只读诊断；不给生产 DLL 打补丁）
- 探针日志存档：`tools/_pet_enchant_probe_20260919.log`
- 关键全局：`0x51B2768..0x51B27A8`（16 个"选择视图"，`+6*4 = 0x51B2780` 是宠物栏视图）、
  `0x51B2764` UI 管理器、`0x518B798` 结果窗目标 key、`0x50DB294`（开窗前被置 5）
- 解析函数：`sub_2326860(view, index)`；开窗函数：`sub_2FDD1A0(ecx=UI 管理器, 0x3D9, item, 0)`

## 下一步（若日后继续）

用独立探针钩住**结果窗对象**（先拿到 `sub_2FDD1A0` 的返回值或经 UI 管理器取窗口），
扫描其内部保存的"目标槽位/物品指针"，定位渲染时读取的那处状态；确认后在附魔成功那一刻
把它同步成本次宠物的槽位/物品。不要再改 90CN.dll 的既有行为。

## 环境还原状态

- `D:\test\地下城与勇士\90CN.dll` 与 `client-patch\Release\90CN.dll` 均为**原始版本**：
  891904 字节，sha256 `23CEF407656C8BEAC8A77D77D2A5D38E5E590747FB4610A9E4F053B4432B345B`
  （与 `90CN.dll.bak_before_pet_enchant_result_20260919` 一致）
- `client-patch\90CN.cpp` 中本次实验代码已整段撤销（grep 无残留）
- 客户端目录内所有 `pet_enchant_probe*.dll` 与探针日志已删除
