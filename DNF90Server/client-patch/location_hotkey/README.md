# LocationHotkey.dll

在游戏窗口内按 `Ctrl+Alt+P`，向服务端查询当前登录角色的位置。结果显示在游戏私聊聊天记录中。

## 支持的数据

- 城镇场景：城镇名称和 ID、区域 ID、区域地图文件名、X/Y、频道 ID。
- 副本场景：副本 ID、房间坐标、角色 X/Y、频道 ID。
- 坐标来自服务端当前会话；客户端只发送固定只读请求，不读取或修改客户端角色结构。

## 协议

复用 `op1456` 私有控制通道，单次请求体为 8 字节：

```text
u32 "LOC1"
u8  version = 1
u8  reserved = 0
u16 token
```

服务端严格校验 magic、版本、长度和 token，然后使用角色自己的会话生成私聊通知。请求不能指定其他角色。

## 编译

```bat
build_location_hotkey.bat
```

产物：

```text
client-patch\location_hotkey\build\LocationHotkey.dll
..\DOF90客户端\LYPlugins\LocationHotkey.dll
```

替换 DLL 前必须完全关闭登录器和游戏。登录器下次启动时会自动注入 `LYPlugins` 中的 DLL。

## 日志

运行日志位于 `DNF.exe` 同目录的 `LocationHotkey.log`。
