# 落叶 DNF 90 源码与服务部署手册

> 最后整理：2026-10-28  
> 适用目录：本 README 所在的 `90CN源码` 根目录

本文档说明当前源码目录中各组件的用途、构建方法、构建参数、客户端配置、服务端安装与更新流程。

## 一、目录结构

本地完整发布目录：

```text
90CN源码部署包
├─ 90CN源码
│  └─ 90CN服务端部署包
├─ DOF90客户端
├─ 登录器配套（仅本地保留，不上传 GitHub）
└─ PVF
```

当前源码目录：

```text
90CN源码
├─ Build-Services.cmd
├─ DNF90Server
├─ DNF90运维工具
├─ DNF90管理工具
├─ 登录器
└─ 90CN服务端部署包
```

主要目录用途：

| 目录或文件 | 用途 |
|---|---|
| `Build-Services.cmd` | Windows 统一构建入口 |
| `DNF90Server\go-server` | Go 游戏服务端源码 |
| `DNF90Server\client-patch` | `90CN.dll`、客户端补丁和独立插件源码 |
| `DNF90Server\client-lua` | `90CNLua.dll` 及 Lua 脚本源码 |
| `DNF90运维工具` | 登录认证服务和网页 GM 服务源码 |
| `DNF90管理工具` | Windows 桌面管理客户端源码，可选 |
| `登录器` | WPF 登录器源码 |
| `90CN服务端部署包\linux` | 上传 Linux 服务器的部署包 |

相邻目录：

| 目录 | 用途 |
|---|---|
| `..\DOF90客户端` | 本地测试客户端 |
| `..\登录器配套` | 客户端运行文件整理副本；体积较大，仅本地保留，不纳入 GitHub 源码仓库 |
| `..\PVF\Script.pvf` | 当前实际游戏 PVF |
| `..\PVF\更新包` | PVF 客户端更新包暂存目录 |

## 二、构建环境

建议在 Windows 10/11 上构建。

| 工具 | 用途 | 当前要求 |
|---|---|---|
| PowerShell | 执行构建脚本内嵌 PowerShell 逻辑 | 5.1 或更高 |
| Go | 构建服务端、认证服务和网页 GM | Go 1.26.5 或兼容版本 |
| .NET SDK | 构建登录器 | .NET 8 SDK |
| Visual Studio C++ 生成工具 | 构建 `90CN.dll`、`90CNLua.dll` 和客户端插件 | Win32，`v143` 或 `v142` |
| MSBuild | 编译 Visual C++ 工程 | Visual Studio Build Tools 自带 |

`Build-Services.cmd` 会优先查找 PATH、项目内或临时目录中的 Go，并自动选择当前安装的 Win32 C++ 工具集。构建失败时会提示失败摘要和临时日志路径，日志默认位于：

```text
%TEMP%\DNF90SourceBuild
```

## 三、统一构建入口

双击或运行：

```text
Build-Services.cmd
```

菜单选项：

```text
1 - 服务端
2 - 认证服务
3 - GM工具
4 - 90CN.dll
5 - 登录器
0 - 退出
```

构建规则：

- 每个组件只输出到自己的 `build` 目录。
- 每次构建覆盖该目录上一次的同名产物。
- 不生成时间戳、日期或版本号副本。
- 成功后输入 `0` 返回一级菜单。
- 一级菜单输入 `0` 退出脚本。

### 构建顺序

只修改服务端时：

```text
1 - 服务端
```

只修改认证服务或登录器版本号时：

```text
2 - 认证服务
5 - 登录器
```

修改 `90CN.dll` 时：

```text
4 - 90CN.dll
5 - 登录器
```

第 4 项会把 DLL 同步到 `登录器\Helper\90CN.dll`。必须再执行第 5 项，新的 `90CN.dll` 才会嵌入登录器。否则玩家使用登录器启动时，仍可能得到旧 DLL。

## 四、各组件构建

### 4.1 游戏服务端 `dnf90server`

| 项目 | 内容 |
|---|---|
| 源码 | `DNF90Server\go-server` |
| 构建入口 | `Build-Services.cmd` → `1` |
| 构建目标 | `./cmd/server/dnf90` |
| 平台 | Linux amd64，静态编译 |
| 产物 | `DNF90Server\go-server\build\dnf90server` |
| 部署位置 | `/opt/dnf90/bin/dnf90server` |

构建脚本实际使用 Linux 交叉编译参数：

```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -buildvcs=false -mod=readonly -trimpath -ldflags "-s -w" `
  -o "build\dnf90server" ./cmd/server/dnf90
```

手工构建时，在 `DNF90Server\go-server` 目录执行上述命令。

### 4.2 登录认证服务 `dnf90auth`

| 项目 | 内容 |
|---|---|
| 源码 | `DNF90运维工具` |
| 构建入口 | `Build-Services.cmd` → `2` |
| 构建目标 | `./cmd/serve` |
| 平台 | Linux amd64，静态编译 |
| 产物 | `DNF90运维工具\cmd\serve\build\dnf90auth` |
| 部署位置 | `/opt/dnf90/bin/dnf90auth` |

认证服务负责登录器账号接口、登录票据、登录器版本校验、PVF 发布信息、公告和运维接口。

修改认证服务内置登录器版本后，必须重新构建并部署：

```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -buildvcs=false -mod=readonly -trimpath -ldflags "-s -w" `
  -o "cmd\serve\build\dnf90auth" ./cmd/serve
```

### 4.3 网页 GM `dnf90admin`

| 项目 | 内容 |
|---|---|
| 源码 | `DNF90运维工具\cmd\admin` |
| 构建入口 | `Build-Services.cmd` → `3` |
| 平台 | Linux amd64，静态编译 |
| 产物 | `DNF90运维工具\cmd\admin\build\dnf90admin` |
| 部署位置 | `/opt/dnf90/bin/dnf90admin` |
| 默认端口 | `18114` |

主要功能：

- 账号和角色查询
- 点券、代币券、金币和物品发放
- 封禁、解封和踢下线
- 团本奖励池查询与权重调整
- 团本入场、奖励次数重置
- 服务器状态和通关记录

网页 GM 不是 Windows 版 `DNF90管理工具`。

更新网页 GM 时可重新上传部署包后执行：

```bash
bash /root/linux/补装-网页后台.sh
```

也可以只替换二进制并重启：

```bash
install -m 755 dnf90admin /opt/dnf90/bin/dnf90admin
systemctl restart dnf90admin
```

### 4.4 登录器

| 项目 | 内容 |
|---|---|
| 源码 | `登录器` |
| 工程 | `登录器\DNFLogin.csproj` |
| 构建入口 | `Build-Services.cmd` → `5` |
| 配置 | `登录器\DNFLogin.build.json` |
| 产物 | `登录器\build\落叶DNF登录器.exe` |

构建脚本执行单文件、自包含发布：

```powershell
dotnet publish DNFLogin.csproj -c Release -r win-x64 `
  --self-contained true -p:PublishSingleFile=true -o build
```

登录器会把下列内容嵌入单文件 EXE：

- .NET 运行时
- WPF 和 Windows Forms 运行库
- `90CN.dll`
- 登录器构建参数
- 其他必要辅助资源

因此客户端通常只需要一个 `落叶DNF登录器.exe`。

### 4.5 客户端补丁 `90CN.dll`

| 项目 | 内容 |
|---|---|
| 生产工程 | `DNF90Server\client-patch\_op205gate_build\90CN.vcxproj` |
| 基础备份工程 | `DNF90Server\client-patch\_op205gate_base_build`，不要作为生产源码修改 |
| 构建入口 | `Build-Services.cmd` → `4` |
| 配置 | `Release / Win32` |
| 工具集 | 自动选择可用的 `v143`、`v142`、`v141` 或其它 Win32 工具集 |
| 产物 | `DNF90Server\client-patch\_op205gate_build\build\90CN.dll` |
| 登录器嵌入副本 | `登录器\Helper\90CN.dll` |

手工构建命令示例：

```powershell
msbuild 90CN.vcxproj /t:Rebuild /p:Configuration=Release `
  /p:Platform=Win32 /p:PlatformToolset=v143
```

`90CN.dll` 负责客户端协议兼容、登录认证、场景与 UI 修补、商城网页跳转等客户端侧逻辑。它依赖固定客户端版本，替换 `DNF.exe` 后必须重新验证兼容性。

### 4.6 客户端 Lua 插件 `90CNLua.dll`

| 项目 | 内容 |
|---|---|
| 源码 | `DNF90Server\client-lua` |
| 工程 | `DNF90Server\client-lua\90CNLua.vcxproj` |
| 配置 | `Release / Win32` |
| 默认产物 | `DNF90Server\client-lua\Release\90CNLua.dll` |
| 客户端位置 | 与 `90CN.dll`、`DNF.exe` 同级 |

该组件目前未加入 `Build-Services.cmd`，需要手工构建：

```powershell
msbuild 90CNLua.vcxproj /t:Rebuild /p:Configuration=Release `
  /p:Platform=Win32 /p:PlatformToolset=v143
```

工程构建后会同时准备：

```text
Release\90CNLua.dll
Release\lua\init.lua
Release\lua\ai\init.lua
```

`90CN.dll` 会自动加载同目录的 `90CNLua.dll`。删除该 DLL 不会影响 `90CN.dll` 的基础功能，但 Lua 扩展、怪物 AI 接管和脚本事件功能会停用。

`90CNLua.dll` 与固定客户端版本绑定。替换 `DNF.exe` 后必须测试 Lua 初始化和相关 Hook。

### 4.7 客户端独立插件

独立插件放在：

```text
..\DOF90客户端\LYPlugins
```

登录器启动时会自动扫描并注入该目录中的 DLL。

当前示例插件：

```text
DNF90Server\client-patch\room_kill_hotkey
```

构建：

```text
DNF90Server\client-patch\room_kill_hotkey\build_kill_room_hotkey.bat
```

插件功能是在副本内按 `~` 清理当前房间。若批处理没有找到新版客户端目录，会保留：

```text
room_kill_hotkey\Release\KillRoomHotkey.dll
```

此时请手动复制到：

```text
..\DOF90客户端\LYPlugins\KillRoomHotkey.dll
```

覆盖插件前必须完全关闭登录器和游戏。

### 4.8 Windows 桌面管理工具

| 项目 | 内容 |
|---|---|
| 源码 | `DNF90管理工具` |
| 运行平台 | Windows |
| 构建入口 | 未加入 `Build-Services.cmd` |

源码说明中该工具被标记为“暂时没啥用”，服务器运行不依赖它。需要 Windows 图形化管理时可手工编译：

```powershell
cd DNF90管理工具
go build -trimpath -ldflags "-s -w" -o "build\DNF90管理工具.exe" .
```

它可用于连接认证服务并管理：

- 登录器版本校验
- 登录器下载直链
- PVF 加密和更新直链
- 服务器选项
- 账号和角色操作

网页 GM 与 Windows 管理工具不是同一个程序。

## 五、构建参数

### 5.1 登录器参数

文件：

```text
登录器\DNFLogin.build.json
```

字段：

| 字段 | 说明 |
|---|---|
| `profileId` | 客户端版本，当前固定为 `90` |
| `launcherVersion` | 登录器版本号，不带 `v` 前缀 |
| `loginMode` | `Remote` 或 `Local` |
| `remoteIp` | 认证服务公网 IP |
| `remotePort` | 认证服务端口，默认 `18112` |

示例：

```json
{
  "profileId": "90",
  "launcherVersion": "1.0.3",
  "loginMode": "Remote",
  "remoteIp": "你的公网IP",
  "remotePort": 18112
}
```

### 5.2 认证服务登录器版本

文件：

```text
DNF90运维工具\internal\launcherver\DNF90Ops.build.json
```

示例：

```json
{
  "launcherVersion": "1.0.3"
}
```

这里的 `launcherVersion` 必须与登录器配置完全一致。发布新登录器时：

1. 修改登录器 `launcherVersion`。
2. 修改认证服务 `launcherVersion`。
3. 选择构建菜单 `2 - 认证服务`。
4. 选择构建菜单 `5 - 登录器`。
5. 上传并重启认证服务。

只改其中一处会导致版本校验失败。

### 5.3 Linux 安装参数

文件：

```text
90CN服务端部署包\linux\conf\dnf90.env
```

关键字段：

| 字段 | 说明 |
|---|---|
| `PUBLIC_IP` | 服务器公网 IP，必填 |
| `MYSQL_PASSWORD` | MySQL 密码；留空自动生成 |
| `REGISTRATION_MODE` | `invite` 邀请码注册或 `open` 自助注册 |
| `ALLOW_REGISTRATION` | `open` 模式下是否允许自助注册 |
| `MAX_REGISTER_PER_IP` | 单 IP 注册数量限制；空或 `0` 表示不限 |
| `PCROOM_PERIOD_LOOP` | 累计在线 25 次奖励是否循环 |
| `REGION_NAME` | 区服名 |
| `ADMIN_PASSWORD` | 网页 GM 后台密码 |
| `ADMIN_PORT` | 网页 GM 端口，默认 `18114` |
| `OPEN_ADMIN_FIREWALL` | 是否在 UFW 中放行网页 GM 端口 |

修改后重新执行 `install.sh`。数据库和玩家数据不会被清除，但服务端配置会按该文件重新生成。

### 5.4 客户端商城网页

文件：

```text
..\DOF90客户端\90CN.ini
```

内容：

```ini
[web]
shop_url=https://你的商城或充值页面
```

规则：

- 文件放在客户端根目录，与 `DNF.exe` 同级。
- 点券充值和黑钻会员当前共用这个地址。
- 文件不存在、值无效或为空时，使用 `90CN.cpp` 中的编译默认地址。
- 修改后需要完全重启游戏。

## 六、登录器更新配置

登录器启动和点击登录时会请求：

```text
GET /launcher/v1/version-check
```

配置入口一：Windows `DNF90管理工具`

```text
服务器选项
→ 登录器版本校验
→ 登录器下载直链
```

配置入口二：服务器配置文件

```text
/opt/dnf90/auth/config.json
```

关键字段：

```json
{
  "launcher_version_check": true,
  "launcher_download_url": "https://你的直链/launcher.zip"
}
```

手工修改该文件后需要：

```bash
systemctl restart dnf90auth
```

注意：当前 `install.sh` 会重新生成 `/opt/dnf90/auth/config.json`，并在生成模板中写入默认的 `launcher_version_check` 和 `launcher_download_url`。重新执行 `install.sh` 会覆盖手工或管理工具写入的值。重装前应同步修改 `install.sh` 中的模板，或安装完成后重新保存登录器更新配置。

更新包可以是 ZIP、7Z、RAR，也可以按当前自更新器支持的方式提供裸 EXE。压缩包中只放登录器 EXE，避免选择到错误文件。

发布登录器的顺序：

1. 构建登录器。
2. 上传到公开直链。
3. 更新登录器和认证服务版本号。
4. 构建并部署认证服务。
5. 在管理工具或 `auth/config.json` 中填写直链并开启版本校验。
6. 重启 `dnf90auth`，重新打开登录器验证。

## 七、PVF 更新

PVF 更新由登录器、认证服务和 MySQL 三方联动。

登录器请求：

```text
GET /launcher/v1/pvf/latest
```

更新包直链保存在 MySQL：

```text
dnf_local.pvf_keys.download_url
```

完整客户端直链保存在：

```text
dnf_local.pvf_keys.client_download_url
```

### 7.1 制作更新包

压缩包根目录必须直接包含：

```text
Script.pvf
本次变更的其他文件
```

不要把这些文件再套一层子目录，否则登录器解压后 SHA256 校验会失败。

### 7.2 使用 `publish-pvf.sh` 发布

建议把脚本放在服务器：

```text
/root/publish-pvf.sh
```

基本发布：

```bash
bash /root/publish-pvf.sh \
  /opt/dnf90/runtime/data/dnf/Script.pvf \
  "https://你的直链/update.zip" \
  2026.10.28
```

同时配置完整客户端直链：

```bash
bash /root/publish-pvf.sh \
  /opt/dnf90/runtime/data/dnf/Script.pvf \
  "https://你的直链/update.zip" \
  2026.10.28 \
  "https://你的直链/client.zip"
```

检查但不写库：

```bash
bash /root/publish-pvf.sh --check \
  /opt/dnf90/runtime/data/dnf/Script.pvf \
  "https://你的直链/update.zip"
```

查看发布记录：

```bash
bash /root/publish-pvf.sh --list
```

下架：

```bash
bash /root/publish-pvf.sh --unpublish
```

### 7.3 使用 Windows 管理工具发布

也可以使用：

```text
DNF90管理工具
→ PVF 加密
→ 客户端更新包直链
→ 客户端整包直链
→ 保存更新包
```

该方式与 `publish-pvf.sh` 写入同一张 `pvf_keys` 表，任选一种即可。

PVF 发布后：

1. 替换服务端 `/opt/dnf90/runtime/data/dnf/Script.pvf`。
2. 重启 `dnf90server`。
3. 重新打开登录器。
4. 登录器发现 SHA256 不一致后显示“更新”。

## 八、Linux 服务端部署包

本地位置：

```text
90CN服务端部署包\linux
```

主要文件：

```text
linux
├─ bin
│  ├─ dnf90server
│  ├─ dnf90auth
│  └─ dnf90admin
├─ conf
│  └─ dnf90.env
├─ data
├─ systemd
├─ install.sh
├─ uninstall.sh
├─ publish-pvf.sh
├─ 补装-网页后台.sh
├─ README_部署说明.md
└─ README_网页后台.md
```

### 8.1 首次安装

把整个 `linux` 目录上传到服务器，例如：

```text
/root/linux
```

编辑：

```text
/root/linux/conf/dnf90.env
```

至少填写：

```ini
PUBLIC_IP=你的公网IP
```

执行：

```bash
cd /root/linux
sudo bash install.sh
```

### 8.2 安装后的服务器目录

```text
/opt/dnf90
├─ bin
│  ├─ dnf90server
│  ├─ dnf90auth
│  └─ dnf90admin
├─ auth
│  └─ config.json
├─ runtime
│  ├─ configs
│  └─ data/dnf/Script.pvf
├─ conf
│  ├─ dnf90.env
│  └─ server.env
└─ logs
```

### 8.3 必须放行的端口

| 协议 | 端口 | 用途 |
|---|---|---|
| TCP | `7001` | 频道登录入口 |
| TCP | `18112` | 登录器认证 HTTPS |
| TCP | `10000-10300` | 游戏频道端口 |
| TCP | `18114` | 网页 GM，可按需关闭公网访问 |
| UDP | `2311-2312` | 组队探测 |
| UDP | `30000-31023` | 组队数据中转 |

云服务器安全组和 Ubuntu 防火墙是两层配置，两层都需要检查。

### 8.4 常用运维命令

```bash
systemctl status dnf90server dnf90auth dnf90admin
systemctl restart dnf90server dnf90auth dnf90admin
systemctl stop dnf90server dnf90auth dnf90admin

tail -f /opt/dnf90/logs/dnf90server.log
tail -f /opt/dnf90/logs/dnf90auth.log

journalctl -u dnf90server -f --no-pager
journalctl -u dnf90auth -f --no-pager
journalctl -u dnf90admin -f --no-pager
```

### 8.5 更新和重装

将新的 `linux` 目录覆盖上传后：

```bash
cd /root/linux
sudo bash install.sh
```

`install.sh` 会停止旧服务、部署新二进制、生成配置、重装 systemd 并重启服务。

`install.sh` 会按 `conf/dnf90.env` 重新生成配置。如果直接手改过 `/opt/dnf90` 内配置，重装前应同步回 `dnf90.env`。

登录器版本校验和下载直链目前是写死在 `install.sh` 的认证配置模板中的。重新安装会覆盖 `/opt/dnf90/auth/config.json` 里的现有值，因此重装后应再次检查并恢复正确的登录器更新开关和直链。

### 8.6 卸载

```bash
cd /root/linux
sudo bash uninstall.sh
```

彻底清理时：

```bash
sudo bash uninstall.sh --purge
```

执行 `--purge` 前先确认是否需要保留数据库和玩家数据。

## 九、客户端发布

本地客户端：

```text
..\DOF90客户端
```

客户端根目录通常需要：

```text
DNF.exe
Script.pvf
90CN.dll
90CN.ini
落叶DNF登录器.exe
ImagePacks2\
LYPlugins\
```

可选的 Lua 扩展：

```text
90CNLua.dll
lua\init.lua
lua\ai\init.lua
```

发布前：

1. 完全关闭登录器和游戏。
2. 备份当前可运行客户端。
3. 替换 `Script.pvf`、登录器、`90CN.dll` 和必要资源。
4. 核对 `90CN.ini` 的商城地址。
5. 检查 `ImagePacks2` 是否包含本次新增资源。
6. 检查 `LYPlugins` 中是否保留了不需要的测试插件。
7. 启动登录器，让登录器完成 `90CN.dll` 哈希自检。

如果登录器发现客户端 `90CN.dll` 与内嵌版本不一致，会提示替换。确认替换前应关闭游戏。

## 十、发布检查清单

### 服务端

- `dnf90server` 来自最新 `build`。
- `conf/dnf90.env` 已填写正确公网 IP。
- 服务端 PVF 已同步到 `/opt/dnf90/runtime/data/dnf/Script.pvf`。
- 端口和安全组已放行。
- `dnf90server.log` 无 PVF、数据库或频道加载错误。

### 认证服务和登录器

- `DNFLogin.build.json` 与 `DNF90Ops.build.json` 的 `launcherVersion` 一致。
- `remoteIp`、`remotePort` 指向正确认证服务。
- 登录器更新直链可公开下载。
- `launcher_version_check`、`launcher_download_url` 配置正确。

### PVF

- 压缩包根目录直接包含 `Script.pvf`。
- `publish-pvf.sh` 使用的是即将发布的同一个 PVF。
- `pvf_keys` 当前只有一条 `active=1` 记录。
- 登录器能按 SHA256 判断更新并成功解压覆盖。

### 客户端

- 登录器和游戏均已关闭后才覆盖 DLL。
- `90CN.dll` 与登录器内嵌版本一致。
- `90CN.ini` 地址正确。
- `LYPlugins` 只保留需要的插件。
- `90CNLua.dll` 与当前客户端 `DNF.exe` 兼容。

## 十一、常见问题

### `90CN.dll` 构建提示找不到 `v142`

运行 `Build-Services.cmd`，它会自动寻找可用的 Win32 工具集。手工 MSBuild 时显式添加：

```powershell
/p:PlatformToolset=v143
```

或安装对应的 `v142` C++ 生成工具。

### 修改 `90CN.dll` 后客户端仍加载旧版本

必须执行：

```text
Build-Services.cmd → 4 → 5
```

第 4 项只生成 DLL 并同步到 `登录器\Helper`，第 5 项才会把它嵌入登录器。

### 登录器版本不一致

同时检查：

```text
登录器\DNFLogin.build.json
DNF90运维工具\internal\launcherver\DNF90Ops.build.json
/opt/dnf90/auth/config.json
```

### PVF 显示需要更新但无法下载

检查：

1. `pvf_keys.download_url` 是否为可公开访问的完整 URL。
2. 压缩包是否为 ZIP、7Z 或 RAR。
3. `Script.pvf` 是否位于压缩包根目录。
4. 登录器是否可以访问外网直链。
5. PVF 发布记录的 SHA256 是否与实际文件一致。

### 登录器读不到网页 GM

检查：

- `dnf90admin` 是否正在运行。
- 服务器安全组是否放行 `ADMIN_PORT`。
- `OPEN_ADMIN_FIREWALL` 是否与访问方式匹配。
- `ADMIN_PASSWORD` 是否正确。

### 服务端启动后频道不完整

检查：

```bash
ss -lntp | grep dnf90server
tail -n 200 /opt/dnf90/logs/dnf90server.log
```

服务端启动后需要加载 PVF 和频道表，端口不会在同一时刻全部可用。

## 十二、产物去向汇总

| 组件 | 构建产物 | 部署位置 |
|---|---|---|
| 游戏服务端 | `DNF90Server\go-server\build\dnf90server` | `/opt/dnf90/bin/dnf90server` |
| 认证服务 | `DNF90运维工具\cmd\serve\build\dnf90auth` | `/opt/dnf90/bin/dnf90auth` |
| 网页 GM | `DNF90运维工具\cmd\admin\build\dnf90admin` | `/opt/dnf90/bin/dnf90admin` |
| 登录器 | `登录器\build\落叶DNF登录器.exe` | 客户端根目录或部署包 |
| 90CN.dll | `DNF90Server\client-patch\_op205gate_build\build\90CN.dll` | 客户端根目录 |
| 90CNLua.dll | `DNF90Server\client-lua\Release\90CNLua.dll` | 客户端根目录 |
| 90CN.ini | 手工配置，不是构建产物 | 客户端根目录 |
| 客户端插件 | 对应插件 `build` 或 `Release` 目录 | `..\DOF90客户端\LYPlugins` |

正式归档目录：

```text
90CN服务端部署包
```

