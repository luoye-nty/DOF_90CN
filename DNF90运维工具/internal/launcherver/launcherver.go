// Package launcherver 保存"当前应使用的登录器版本号" —— 三端联动的唯一事实源。
//
// ★ 发布新登录器时修改同目录下的 DNF90Ops.build.json，然后：
//  1. 重新编译认证服务（dnf90auth / 运维工具）并重启；
//  2. 登录器侧 LauncherVersion.cs 的 Current 必须与它逐字符一致；
//  3. 先把新登录器压缩包传到直链，再打开 config.json 的 launcher_version_check。
//
// 之所以单独成包：运维工具有两个可执行入口（GUI 主程序 cmd: 根目录 main，
// 以及无 GUI 的认证服务 cmd/serve）。两边都要读同一个版本号，常量放在
// 任何一侧的 package main 里，另一侧都拿不到 —— 那就会长出第二份会漂移的副本。
package launcherver

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed DNF90Ops.build.json
var buildConfigJSON []byte

type buildConfig struct {
	LauncherVersion string `json:"launcherVersion"`
}

// Required 登录器必须与之一致才能放行的版本号（与登录器 LauncherVersion.cs
// 的 Current 逐字符一致：大小写敏感、不带 "v" 前缀、无首尾空白）。
//
// 留空表示"服务端没配版本号"→ 对外一律按未开启校验返回，登录器直接放行
// （防止把全服玩家锁在更新界面外面）。
var Required = loadRequired()

func loadRequired() string {
	var config buildConfig
	if err := json.Unmarshal(buildConfigJSON, &config); err != nil {
		panic("parse DNF90Ops.build.json: " + err.Error())
	}
	version := strings.TrimSpace(config.LauncherVersion)
	if version == "" || strings.HasPrefix(version, "v") || len(version) > 64 {
		panic("DNF90Ops.build.json: launcherVersion must be non-empty, have no leading v, and be at most 64 bytes")
	}
	return version
}
