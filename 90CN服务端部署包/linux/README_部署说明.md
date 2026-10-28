# DNF90 服务端 · Ubuntu 一键部署包

> 面向小白。整个部署只有 3 步，命令可以直接复制粘贴。

---

## 一、准备什么

1. 一台 **Ubuntu** 服务器（20.04 / 22.04 / 24.04，64 位），有 **公网 IP**。
   - 云服务器（腾讯云 / 阿里云 / 华为云等）最好，买最便宜的 2 核 4G 起步。
   - 家里电脑做服务器也行，但要在路由器里做端口映射，比较麻烦，不推荐新手。
2. 记住你的 **公网 IP**（云服务器控制台首页就能看到）。

---

## 二、开始安装（3 步）

### 第 1 步：把整个 `linux` 文件夹上传到服务器

用 WinSCP / Xftp / FinalShell 等工具，把本文件夹（`linux`）整个上传到服务器的 `/root/` 下。  
上传后服务器上会有 `/root/linux/`。

### 第 2 步：改一个配置

打开 `/root/linux/conf/dnf90.env`，把 **PUBLIC_IP** 填成你的公网 IP：

```
PUBLIC_IP=123.45.67.89
```

（`MYSQL_PASSWORD` 可以留空，脚本会自动生成一个密码并打印出来，记得抄下来。）

### 第 3 步：执行安装

用 SSH 登录服务器（FinalShell / Xshell / 云服务器自带的「远程登录」都行），依次执行：

```bash
cd /root/linux
sudo bash install.sh
```

看到一大堆绿字 `[  OK  ]` 和最后的公网 IP 提示，就装好了。

---

## 三、装完之后

### 1）放行端口（重要！）

云服务器有两层防火墙，**两层都要放行**：

**第一层：云服务器控制台的「安全组 / 防火墙」**，放行下面这些：

| 协议  | 端口          | 用途           |
| --- | ----------- | ------------ |
| TCP | 7001        | 频道登录入口       |
| TCP | 18112       | 登录器认证（HTTPS） |
| TCP | 10000-10300 | 各频道游戏端口      |
| UDP | 2311-2312   | 组队连接探测       |
| UDP | 30000-31023 | 组队数据中转（联机必须） |

**第二层：Ubuntu 系统自带防火墙**——安装脚本已经自动用 `ufw` 配好了，一般不用管。

> 组队联机一定要放行 UDP 30000-31023，否则「队伍里看不见队友在动」。  
> 人越多需要的 UDP 端口越多，脚本已按 1024 个端口（约 85 个 4 人队）预留。

### 2）把公网 IP 填进登录器

在玩家的电脑上打开登录器 → **游戏配置** → 选择 **远程模式** → **服务器 IP** 填你的公网 IP → 保存。

然后就能注册账号、登录、进游戏了。

### 注册方式（`conf/dnf90.env` 里配，安装前选好）

`REGISTRATION_MODE` 两种取值：

- **`invite`（邀请码注册，一人一码）**：玩家必须填对邀请码才能注册。
  认证服务**首次启动会自动生成一个「引导邀请码」**，写进 `logs/dnf90auth.log`（也可以在 `launcher_invites` 表里查到），拿它当入口；之后批量发码用 `gen_invite_codes.sql`——把它放到服务器上，改开头的数量，执行
  `mysql -udnf90 -p"数据库密码" dnf_local < gen_invite_codes.sql`。
- **`open`（自助注册）**：是否开放由同一文件里的 `ALLOW_REGISTRATION` 决定（`false` 时只能管理员建号）。

想改注册方式：改这一行后重新执行 `sudo bash install.sh` 即可（不会动玩家数据）。

---

## 三之二、网页管理后台（点券发放 / 封禁 / 通关记录）

安装脚本会顺带装好一个**网页管理后台**，浏览器打开就能做日常运营，不用再开 Windows 图形工具。

- **地址**：安装结束时打印的 `http://你的公网IP:18114`（端口可在 `conf/dnf90.env` 的 `ADMIN_PORT` 改）
- **密码**：安装结束时打印的「后台密码」（就是 `conf/dnf90.env` 里的 `ADMIN_PASSWORD`，留空则自动生成）
- **功能**：点券/代币/金币发放、账号封禁与解封、踢下线、通关记录查询、账号与角色查询、服务器状态概览

**务必先做两件事**：

1. 云服务器**安全组**放行 `TCP 18114`；
2. 强烈建议把 `conf/dnf90.env` 里的 `ADMIN_PASSWORD` 改成复杂密码后重跑 `install.sh`。

> 想更安全：把 `OPEN_ADMIN_FIREWALL=false` 后重跑安装，再用 SSH 隧道访问：  
> `ssh -L 18114:127.0.0.1:18114 root@你的公网IP`，然后浏览器开 `http://127.0.0.1:18114`。

详细说明见 **`README_网页后台.md`**。

## 四、常用命令

```bash
systemctl status dnf90server dnf90auth      # 看两个服务状态
systemctl restart dnf90server dnf90auth     # 重启
systemctl stop dnf90server dnf90auth        # 停止
journalctl -u dnf90server -f --no-pager     # 实时看游戏服务日志
journalctl -u dnf90auth   -f --no-pager     # 实时看认证服务日志
tail -f /opt/dnf90/logs/dnf90server.log      # 另一处日志
```

---

## 五、目录说明（安装后）

```
/opt/dnf90/
├─ bin/dnf90server      游戏服务端主程序
├─ bin/dnf90auth        认证服务（登录器 HTTPS 接口）
├─ runtime/             游戏服务端工作目录
│   ├─ configs/         dnfbridge.toml / logic.toml / plan.json
│   └─ data/dnf/        Script.pvf、channel_info.etc
├─ auth/                认证服务工作目录
│   ├─ config.json      认证服务配置
│   └─ tls/             自动生成的自签证书
├─ conf/
│   ├─ dnf90.env        安装配置（就是你改的那个）
│   └─ server.env       游戏服务端环境变量（自动生成）
└─ logs/                日志
```

---

## 五之二、更新 / 重装（不用卸载）

**只需要重新上传 `linux` 这一个文件夹**（覆盖服务器上的 `/root/linux`），然后：

```bash
sudo systemctl stop dnf90server dnf90auth   # 可省略，install.sh 会自动停
cd /root/linux
sudo bash install.sh
```

`install.sh` 会自己完成：停旧服务 → 重置失败状态 → 部署新二进制 → 重新生成配置 →  
清理历史遗留文件 → 配置预检 → 重装 systemd → 重启服务。

**数据库和玩家数据不会丢**（脚本不动 `dnf_local` 库），但**服务器配置会被按 `conf/dnf90.env` 重新生成**，  
所以如果你手改过 `/opt/dnf90` 里的东西，记得同步回 `conf/dnf90.env`。

想彻底重来（含删库）：

```bash
sudo systemctl stop dnf90server dnf90auth
sudo mysql -uroot -e "DROP DATABASE IF EXISTS dnf_local; CREATE DATABASE dnf_local CHARACTER SET utf8mb4;"
cd /root/linux && sudo bash install.sh
```

## 六、常见问题

**Q：登录器连不上 / 显示离线？**

1. 检查云服务器「安全组」是否放行了 TCP 18112 和 TCP 7001；
2. 在服务器上执行 `systemctl status dnf90auth`，确认是 `active (running)`；
3. 在服务器上执行 `curl -k https://127.0.0.1:18112/launcher/v1/meta`，能返回 JSON 就说明认证服务正常。

**Q：能登录、能进游戏，但组队互相看不见？**  
UDP 端口没放行。到云服务器安全组放行 **UDP 2311-2312** 和 **UDP 30000-31023**。

**Q：改了公网 IP 或密码，要重新装吗？**  
改 `conf/dnf90.env` 后重新执行 `sudo bash install.sh` 即可（会覆盖配置、重启服务，数据库数据不丢）。

**Q：想清空所有角色数据重来？**

```bash
sudo mysql -uroot -e "DROP DATABASE dnf_local; CREATE DATABASE dnf_local CHARACTER SET utf8mb4;"
sudo systemctl restart dnf90server dnf90auth
```

**Q：怎么彻底卸载？**

```bash
sudo bash uninstall.sh --purge
```

---

## 七、关于「人数限制」

本包使用的认证服务是 **不限制在线人数** 的构建（原版免费版限制 8 人，本版本 `MaxOnline=0`）。  
在线上限最终由服务器性能决定，与授权无关。
