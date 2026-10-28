# KillRoomHotkey.dll —— 副本内 `~` 秒杀当前房间（测试用）

## 它做什么

在副本里按键盘左上角的 `~`（`VK_OEM_3`），清空**当前所在房间**里所有存活怪物。

不是改客户端血量，而是向服务端发一条私有请求，由服务端调用现成的
`DebugKillCurrentRoom`，逐只走正常的怪物死亡事务。因此：

- 掉落、经验、任务触发、Boss 处理、房间结算都按正常清房走；
- 换房间后怪物不会"复活"；
- 客户端与服务端状态一致。

城镇里按 `~` 不会有任何效果（服务端找不到活动副本房间，只记一条日志）。

## 文件

| 位置 | 说明 |
|---|---|
| `client-patch\room_kill_hotkey\KillRoomHotkey.cpp` | 全部实现 |
| `client-patch\room_kill_hotkey\KillRoomHotkey.vcxproj` | Win32 / v143 / Release，静态 CRT |
| `client-patch\room_kill_hotkey\build_kill_room_hotkey.bat` | 编译并复制到客户端 `LYPlugins` |
| `<客户端>\LYPlugins\KillRoomHotkey.dll` | 最终产物（登录器自动注入） |
| `<客户端>\KillRoomHotkey.log` | 运行日志（加载、发包、异常） |

## 协议

复用 op1456（`CmdPacket` 枚举里刻意留的空洞，90CN 的 `ACK1` / `BMU1` 也走这里）：

```
u16 opcode = 1456
u32 magic  = "KRM1" (0x314D524B, little-endian)
u8  version = 1
u8  reserved = 0
u16 token   （仅用于客户端/服务端日志对应）
```

服务端在 `game_dispatch_upper.go` 里按 magic 分流到
`handleCurrentDebugKillRoom`（`debug_kill_room_hotkey.go`）。

### 服务端还必须认得这个 opcode

只加处理函数是不够的。game 端口是混合流，`SplitLatestGameStream` 会先尝试
`splitUpperPacket`，再用 `isKnownClientUpperMsg` 按白名单确认；op1456 不在原生
消息表里，所以会被 `splitLegacyGamePacket` 当成 legacy 的 `cmd=1` 切走，
**包到不了 `handleGameUpper`**。

因此 `internal/modules/dnf/protocol/stream.go` 增加了严格的 `KRM1` 分支：

```go
case 1456:
    return isCurrentKillRoomUpperBody(body) // len == 8 && body[:4] == "KRM1"
```

首次联调时的现象就是：客户端日志写着 `kill-room request sent`、tcpdump 也能
看到 `KRM1` 字节，但服务端日志一片空白（`stream_kind=3`，被归成 legacy）。

`ACK1`（能力位图）和 `BMU1`（Boss 变异）刻意没有一起放行，保持它们原有的
legacy 行为，避免顺手改变线上未实现命令的失败回包。

## 开关（重要）

服务端环境变量：

```
DNFBRIDGE_DEBUG_KILL_ROOM=true
```

位置：`/opt/dnf90/conf/server.env`，改完 `systemctl restart dnf90server`。

默认值在 `install.sh` 里是 `false`。也就是说**重新执行 install.sh 会把这个开关关掉**，
需要重新改成 `true`。关掉后 DLL 仍然可以注入，但服务端会拒绝并记录
`game-debug-kill-room-rejected reason=debug_kill_room_disabled`。

它与 `DNFBRIDGE_DEBUG_MODE` 相互独立：开启本开关不会附带 +500% 三速、强制 SSS 结算等调试行为。

## 线程与安全

- 轮询线程只读 `GetAsyncKeyState`，并通过 `PostMessage` 把工作交给游戏 UI 线程；
- 真正的发包在游戏主窗口线程里执行（客户端 writer 不是线程安全的）；
- 启动时校验客户端 writer 入口的前导字节，版本不匹配直接禁用，不会乱发；
- 不修改游戏代码、不修改文件，删掉 DLL 即彻底移除。

## 编译

```bat
client-patch\room_kill_hotkey\build_kill_room_hotkey.bat
```

产物同时复制到 `<客户端>\LYPlugins\KillRoomHotkey.dll`。
登录器每次启动都会把 `LYPlugins` 下新增的 DLL 自动加入注入列表（`DllManageControl.EnsureDefaultPluginDlls`）。
