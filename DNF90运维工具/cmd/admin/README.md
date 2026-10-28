# dnf90admin 网页管理后台

这是 `README_网页后台.md` 第 5 点所述的 `cmd/admin` 子项目。它和线上
`/opt/dnf90/bin/dnf90admin` 使用同一套：

- 配置格式：`/opt/dnf90/admin/admin.json`
- 监听端口：默认 `:18114`
- systemd 服务：`dnf90admin.service`
- 网页界面：`web/index.html`（编译时嵌入二进制）
- 游戏管理链路：`http://127.0.0.1:18111` 的 `/dnf/gm/*`、`/local/dnf-gm/*`
- 账号/封禁库：`launcher_accounts`、`launcher_bans`

## 文件结构

| 文件 | 作用 |
| --- | --- |
| `main.go` | 配置加载、登录会话、路由、通用 JSON 工具 |
| `api.go` | `/api/*` 处理逻辑，以及数据库/游戏 GM 接口的字段适配 |
| `gm.go` | 游戏服务端管理口的 HTTP 客户端 |
| `batch.go` | 全服批量发放任务的内存队列 |
| `webui.go` | 内嵌网页与副本难度表 |
| `class_names.go` | 职业/转职/觉醒名称表 |
| `web/index.html` | 网页界面（单文件） |
| `web/dungeons_max_difficulty.json` | 一键解锁副本用的难度表 |

## 编译

在 `DNF90运维工具` 目录执行：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o dnf90admin ./cmd/admin
```

ARM64 服务器把 `GOARCH` 改成 `arm64`。

Windows 上编译时先设置环境变量：

```powershell
$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"
go build -trimpath -ldflags "-s -w" -o dnf90admin ./cmd/admin
```

## 部署

```bash
install -m 755 dnf90admin /opt/dnf90/bin/dnf90admin
systemctl restart dnf90admin
systemctl status dnf90admin --no-pager
```

自检：

```bash
curl -s -c /tmp/admin.cookie \
  -X POST http://127.0.0.1:18114/api/login \
  -H 'Content-Type: application/json' \
  -d '{"password":"你的后台密码"}'
curl -s -b /tmp/admin.cookie http://127.0.0.1:18114/api/status
```

## 修改界面

直接改 `web/index.html`，然后重新编译。`go:embed` 会把新页面打进二进制，
部署时只需要替换一个 `dnf90admin` 文件。

## 团本奖励池

概览页的“读取奖励池”依赖游戏服务端提供：

- `GET /dnf/gm/gameplay/raid-reward-pools`
- `GET /dnf/gm/gameplay/raid-reward-rate`
- `POST /dnf/gm/gameplay/raid-reward-rate`

这组路由位于 `DNF90Server` 的 GM 管理口（默认只监听 `127.0.0.1:18111`，
需要 `X-Admin-Token`），不会暴露给游戏客户端。

权重语义：`100` = PVF 原值，`0` = 永不抽出，`1000` = 十倍权重。
覆盖只保存在 `dnf90server` 进程内存中，重启服务后恢复 PVF 原值；
网页后台的“全部恢复原值”会立即清空覆盖。

## 团本次数重置

“角色管理 → 角色操作 → 团本次数”提供安徒恩、卢克、超时空三组计数：

- 账号日入场次数
- 账号周入场次数
- 角色周入场次数

每个按钮只重置一项。账号级按钮会影响同账号下的全部角色；角色周次数只影响当前角色。
网页后台通过游戏服务端接口执行重置，在线角色会立即收到 op1303/op1206 次数刷新包。

- `GET /dnf/gm/characters/{character_id}/raid-counters`
- `POST /dnf/gm/characters/{character_id}/raid-counters/reset`
