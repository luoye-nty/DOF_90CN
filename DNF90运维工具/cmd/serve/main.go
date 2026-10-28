package main

// serve 独立启动 90 远程认证服务（18112 HTTPS + 19091 内部验票），
// 复用运维工具的 server/store 包，便于无 GUI 环境下联调 LANA 链路。
// MySQL 凭据实时从服务端 runtime\config\instance.json 读取。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"dnf90ops/internal/config"
	"dnf90ops/internal/launcherver"
	"dnf90ops/internal/server"
	"dnf90ops/internal/store"
)

func loadMySQLPassword(instancePath string) (string, error) {
	raw, err := os.ReadFile(instancePath)
	if err != nil {
		return "", err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	dbRaw, ok := m["database"]
	if !ok {
		return "", fmt.Errorf("instance.json 缺少 database 段")
	}
	var db map[string]interface{}
	if err := json.Unmarshal(dbRaw, &db); err != nil {
		return "", err
	}
	pw, _ := db["password"].(string)
	return strings.TrimSpace(pw), nil
}

func main() {
	exePath, err := os.Executable()
	exeDir := filepath.Dir(exePath)
	if err != nil {
		exeDir = "."
	}
	// systemd 单元里传的是 -config /opt/dnf90/auth/config.json；不带参数时退回
	// "二进制同目录 config.json"（老行为）。2026-10-17：以前这里完全忽略命令行，
	// 一旦换机器/换目录就会去读不存在的文件。
	cfgFlag := flag.String("config", "", "配置文件路径（缺省：二进制同目录 config.json）")
	flag.Parse()
	cfgPath := strings.TrimSpace(*cfgFlag)
	if cfgPath == "" {
		cfgPath = filepath.Join(exeDir, "config.json")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Println("config load failed:", err)
		os.Exit(1)
	}

	// MySQL 连接信息优先用 config.json 的 mysql 段（本部署就是这样配的：
	// host=127.0.0.1 port=3306 user=dnf90 dbname=dnf_local）；只有该段缺密码时
	// 才退回服务端 runtime/config/instance.json。
	//
	// 2026-10-17：这里原来写死 Port=13306 + User="root" 且只从 instance.json 取密码。
	// 本机 MySQL 在 3306，于是新编译的认证服务一启动就 "connection refused" 起不来
	// —— 老二进制显然是读 config.json 的 mysql 段，这次一并改对。
	instancePath := ""
	if root := strings.TrimSpace(cfg.ServerRoot); root != "" {
		instancePath = filepath.Join(root, "runtime", "config", "instance.json")
	}
	if instancePath == "" {
		instancePath = filepath.Join(exeDir, "DNF90", "runtime", "config", "instance.json")
	}
	mcfg := config.MySQLConfig{
		Host:   "127.0.0.1",
		Port:   13306,
		User:   "root",
		DBName: "dnf_local",
	}
	if cfg.MySQL.Host != "" {
		mcfg.Host = cfg.MySQL.Host
	}
	if cfg.MySQL.Port != 0 {
		mcfg.Port = cfg.MySQL.Port
	}
	if cfg.MySQL.User != "" {
		mcfg.User = cfg.MySQL.User
	}
	if cfg.MySQL.DBName != "" {
		mcfg.DBName = cfg.MySQL.DBName
	}
	if pw := strings.TrimSpace(cfg.MySQL.Password); pw != "" {
		mcfg.Password = pw
	} else {
		pw, pwErr := loadMySQLPassword(instancePath)
		if pwErr != nil {
			fmt.Println("instance password load failed:", pwErr)
			os.Exit(1)
		}
		mcfg.Password = pw
	}
	st, err := store.Open(mcfg)
	if err != nil {
		fmt.Println("mysql open failed:", err)
		os.Exit(1)
	}

	svc := server.New(cfg, st, slog.Default(), nil, nil, nil, nil)
	// 登录器版本校验：版本号来自编译期常量（internal/launcherver），总开关与下载直链
	// 来自 config.json（launcher_version_check / launcher_download_url，改完重启生效）。
	// 以前这个入口完全没接，所以 /launcher/v1/version-check 永远返回 required_version=""，
	// 登录器怎么配都不会去更新。
	svc.SetLauncherVersion(func() server.LauncherVersionState {
		return server.LauncherVersionState{
			Enabled:         cfg.LauncherVersionCheck,
			DownloadURL:     strings.TrimSpace(cfg.LauncherDownloadURL),
			RequiredVersion: launcherver.Required,
		}
	})
	// 第③步：yh 授权回调核销装配（paid 版内置回调配置自动开启；免费版不装配；
	// 环境变量 YH_CHECK_URL/YH_CHECK_KEY 可成对覆盖内置配置）。
	if yh, yhErr := server.NewYhCheckAuto(slog.Default()); yhErr != nil {
		fmt.Println("yh callback not configured:", yhErr)
	} else if yh != nil {
		svc.SetYhCheck(yh.Check)
		fmt.Println("yh callback enabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		fmt.Println("service start failed:", err)
		os.Exit(1)
	}
	fmt.Println("auth service started")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("shutting down")
	_ = svc.Stop(context.Background())
}
