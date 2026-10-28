// Package serverdir 提供 DNF90 服务端根目录的只读检测、一键初始化与按目录进程管理。

// 检测口径与登录器 ServerManager 保持一致：

//   - 初始化判定：runtime\config\instance.json 是否存在且可解析、数据库是否就绪

//   - 一键初始化（MySQL/portable）：完整复刻原版 DNF90Control start 的便携 MySQL 准备链：
//     database select --prepare（生成 instance.json+解压 mysqld）→ 生成 runtime\config\mysql.ini
//     → mysqld --initialize-insecure 初始化数据 → 直启 mysqld 等端口 → 设 root 密码并建库
//     → 写 runtime\mysql\data-state.json(phase=ready)
//   - sqlite 模式：仍走原版 database select --prepare 生成 instance.json

package serverdir

import (
	"bufio"

	"bytes"

	"context"

	"crypto/sha256"

	"database/sql"

	"encoding/hex"

	"encoding/json"

	"errors"

	"fmt"

	"io"

	"net"

	"os"

	"os/exec"

	"path/filepath"

	"strconv"

	"strings"


	"time"

	_ "github.com/go-sql-driver/mysql"
)

// InstanceInfo instance.json 关键字段（只读）。

type InstanceInfo struct {
	InstallationID string `json:"installationId"`

	DatabaseMode string `json:"mode"`

	DatabasePath string `json:"path"`

	AdvertiseIP string `json:"advertiseIp"`

	ChannelListen string `json:"channelListen"`

	AdminListen string `json:"adminListen"`
	AdminToken  string `json:"adminToken"`

	// database 段（portable MySQL 凭据，实时取自 instance.json）。

	DBHost string `json:"dbHost"`

	DBPort int `json:"dbPort"`

	DBUser string `json:"dbUser"`

	DBPassword string `json:"dbPassword"`

	DBName string `json:"dbName"`

	// server 段扩展（直启 DNF90Server 需要的运行环境）

	AccountID string

	PacketLog bool

	DebugMode bool

	DebugStrikeOnSkillUse bool

	RobotEnabled bool

	PartyUDPRelayEnabled bool

	PartyUDPRelayPortStart int

	PartyUDPRelayPortCount int

	// game 段

	GameShardID               string
	PVFPath                   string
	GamePVFMaxBytes           int64
	ChannelInfoPath           string
	DungeonBatchPickupEnabled bool

	// protocol 段

	GameUpperHeader string

	GameUpperBodyCodec string

	GameUpperClientBodyCodec string

	GameOuterToken string

	ChannelServerIndex int

	ChannelAdvertiseIndex int

	RegionID int
}

// Inspection 服务端目录只读探测结果。

type Inspection struct {
	Root string

	RootExists bool

	StartBatExists bool

	ServerExeExists bool

	ControlExeExists bool

	InstanceExists bool

	InstanceValid bool

	Instance *InstanceInfo

	DatabaseMode string // "" | sqlite | portable

	SqliteDbExists bool

	ChannelPort int

	AdminPort int

	Running bool // 服务端管理口/频道口任一监听即视为运行中

	Ready bool // 已初始化且数据库就绪，可启动

	NeedsInit bool

	Message string
}

// Inspect 只读探测服务端根目录，不产生任何副作用。

func Inspect(root string) *Inspection {

	ins := &Inspection{Root: root}

	if strings.TrimSpace(root) == "" || !DirExists(root) {

		ins.Message = "目录不存在"

		return ins

	}

	ins.RootExists = true

	runtimeDir := filepath.Join(root, "runtime")

	ins.StartBatExists = FileExists(root, "START.bat") || FileExists(root, "start.bat")

	ins.ServerExeExists = FileExists(runtimeDir, "bin", "DNF90Server.exe")

	ins.ControlExeExists = FileExists(runtimeDir, "bin", "DNF90Control.exe")

	instancePath := filepath.Join(runtimeDir, "config", "instance.json")

	ins.InstanceExists = FileExists(instancePath)

	if info, ok := loadInstance(instancePath); ok {

		ins.InstanceValid = true

		ins.Instance = info

		switch {

		case strings.EqualFold(info.DatabaseMode, "sqlite"):

			ins.DatabaseMode = "sqlite"

		default:

			ins.DatabaseMode = "portable"

		}

		ins.ChannelPort = ParseListenPort(info.ChannelListen)

		ins.AdminPort = ParseListenPort(info.AdminListen)

	}

	if !ins.InstanceExists {

		ins.NeedsInit = true

		ins.Message = "未初始化：缺少 runtime\\config\\instance.json"

		ins.collectRunning()

		return ins

	}

	if !ins.InstanceValid {

		ins.NeedsInit = true

		ins.Message = "instance.json 解析失败，请检查文件格式"

		ins.collectRunning()

		return ins

	}

	if ins.DatabaseMode == "sqlite" {

		dbPath := resolveDBPath(runtimeDir, ins.Instance.DatabasePath)

		ins.SqliteDbExists = dbPath != "" && FileExists(dbPath)

		if !ins.SqliteDbExists {

			ins.NeedsInit = true

			ins.Message = "SQLite 数据库不存在，请重新初始化"

			ins.collectRunning()

			return ins

		}

		ins.Ready = true

		ins.Message = "SQLite 就绪（" + dbPath + "）"

	} else {

		// MySQL：便携 MySQL 是否已完整初始化（mysql.ini + data-state ready）。

		mysqlIniPath := filepath.Join(runtimeDir, "config", "mysql.ini")

		if FileExists(mysqlIniPath) && MySqlDataReady(runtimeDir) {
			ins.Message = "MySQL 就绪（内置便携 MySQL 已初始化，一键启动将自动拉起服务）"
			ins.Ready = true
		} else if !FileExists(runtimeDir, "mysql", "server", "bin", "mysqld.exe") {
			ins.NeedsInit = true
			ins.Message = "MySQL 模式（尚未携带内置便携 MySQL，一键初始化将自动解压并完成数据初始化；若使用外部 MySQL 服务请确认端口已监听）"
		} else {
			ins.NeedsInit = true
			ins.Message = "MySQL 数据尚未初始化（缺 runtime\\config\\mysql.ini 或 runtime\\mysql\\data 未 ready），请点击「一键初始化」"
		}
	}

	ins.collectRunning()

	return ins

}

// collectRunning 探测端口监听状态，追加到 Message。

func (ins *Inspection) collectRunning() {

	// 缺 instance.json 时按默认管理口/频道口探测。

	admin := ins.AdminPort

	if admin <= 0 {

		admin = 18111

	}

	channel := ins.ChannelPort

	if channel <= 0 {

		channel = 7001

	}

	adminOpen := ProbePort("127.0.0.1", admin, 300*time.Millisecond)

	chOpen := ProbePort("127.0.0.1", channel, 300*time.Millisecond)

	if adminOpen || chOpen {

		ins.Running = true

		if ins.Message != "" {

			ins.Message += " · "

		}

		if adminOpen {

			ins.Message += fmt.Sprintf("管理口 %d 开放", admin)

		}

		if chOpen {

			if adminOpen {

				ins.Message += "，"

			}

			ins.Message += fmt.Sprintf("频道口 %d 开放", channel)

		}

		ins.Message += "（服务端运行中）"

	}

}

// NeedsDatabasePrep 需要执行一键初始化。

func NeedsDatabasePrep(ins *Inspection) bool {

	return ins != nil && ins.NeedsInit

}

// EnsureInstance 确保 instance.json 存在；缺失时调用 DNF90Control database select --prepare。

// mode 取值 sqlite / mysql（portable）。已存在则跳过。

func EnsureInstance(root, mode string, force bool, onLine func(string), timeout time.Duration) error {

	if strings.TrimSpace(root) == "" {

		return errors.New("服务端根目录为空")

	}

	instancePath := filepath.Join(root, "runtime", "config", "instance.json")

	// 队伍 UDP 中继端口池数量：统一为 WantedPartyUDPRelayPortCount（1024）。
	// 初始化与一键启动都要查一遍 —— 老 instance.json 里是 64，服务端内置默认 512，
	// 组队高峰会把中继端口池耗光。文件还不存在时跳过（下面 prepare 会生成，生成后再查一次）。
	if FileExists(instancePath) {
		if _, err := EnsurePartyUDPRelayPortCount(root, WantedPartyUDPRelayPortCount, onLine); err != nil && onLine != nil {
			onLine("检查 partyUdpRelayPortCount 失败（不阻断初始化）: " + err.Error())
		}
	}

	if FileExists(instancePath) && !force {

		if onLine != nil {

			onLine("instance.json 已存在，跳过初始化")

		}

		return nil

	}

	control := filepath.Join(root, "runtime", "bin", "DNF90Control.exe")

	if !FileExists(control) {

		return fmt.Errorf("找不到原版控制器: %s", control)

	}

	// 代理转发给原版控制器之前，先确保本机有"服务端认可的私有 IPv4"：
	// 控制器解析 server.advertiseIp=AUTO_DETECT 时只接受私有网段地址，
	// 只有公网 IP 的云主机会直接失败（no active private LAN IPv4 ...）。
	// 详见 private_ip.go 顶部注释。

	if err := EnsurePrivateLANIPv4(onLine); err != nil {

		return err

	}

	modeArg := "sqlite"

	if mode == "mysql" {

		modeArg = "mysql"

	}

	if onLine != nil {

		onLine(fmt.Sprintf("调用原版控制器初始化服务端配置: DNF90Control database select --mode %s --prepare", modeArg))

	}

	env := append(os.Environ(), "DNF90_PROJECT_ROOT="+root)

	if err := runProcess(control, []string{"database", "select", "--mode", modeArg, "--prepare"}, root, env, onLine, timeout); err != nil {

		return err

	}

	if !FileExists(instancePath) {

		return errors.New("初始化结束但 instance.json 未生成，请检查上方输出")

	}

	if onLine != nil {

		onLine("初始化完成: runtime\\config\\instance.json 已生成")

	}

	// 刚生成的 instance.json 同样统一队伍 UDP 中继端口池数量（幂等，已是目标值就不动文件）。

	if _, err := EnsurePartyUDPRelayPortCount(root, WantedPartyUDPRelayPortCount, onLine); err != nil && onLine != nil {

		onLine("检查 partyUdpRelayPortCount 失败（不阻断初始化）: " + err.Error())

	}

	return nil

}

// RunDoctor 调用 DNF90Doctor.exe 做启动前预检（可选）。

func RunDoctor(root string, onLine func(string), timeout time.Duration) error {

	doctor := filepath.Join(root, "runtime", "bin", "DNF90Doctor.exe")

	if !FileExists(doctor) {

		return fmt.Errorf("找不到 DNF90Doctor.exe: %s", doctor)

	}

	return runProcess(doctor, nil, root, append(os.Environ(), "DNF90_PROJECT_ROOT="+root), onLine, timeout)

}

// ProcessInfo 进程简表（按目录匹配）。

type ProcessInfo struct {
	PID int `json:"ProcessId"`

	Name string `json:"Name"`

	ExecutablePath string `json:"ExecutablePath"`
}

// ListRootProcesses 枚举主模块路径位于 root 下的指定名称进程（Win32_Process）。

func ListRootProcesses(root string, names ...string) ([]ProcessInfo, error) {

	if !DirExists(root) {

		return nil, nil

	}

	qnames := make([]string, 0, len(names))

	for _, n := range names {

		qnames = append(qnames, "'"+n+"'")

	}

	nameFilter := strings.Join(qnames, ",")

	// 注意：PowerShell -like 是通配符匹配而非正则，反斜杠无需转义；

	// 将 root 反斜杠换成双反斜杠会导致 -like 永远匹配不上真实路径（ExecutablePath 为单反斜杠）。

	// 仅防御路径含单引号破坏 -Command 字符串。

	rootLike := strings.ReplaceAll(root, "'", "''")

	ps := fmt.Sprintf(

		`Get-CimInstance Win32_Process | Where-Object { $_.Name -in @(%s) -and $_.ExecutablePath -like '%s*' } | Select-Object ProcessId,Name,ExecutablePath | ConvertTo-Json -Compress`,

		nameFilter, rootLike)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)

	cmd.SysProcAttr = hideWindowAttr()

	out, err := cmd.Output()

	if err != nil {

		return nil, err

	}

	out = bytes.TrimSpace(out)

	if len(out) == 0 {

		return nil, nil

	}

	var items []ProcessInfo

	// ConvertTo-Json 单对象时不是数组，先包一层。

	if !bytes.HasPrefix(out, []byte("[")) {

		out = append([]byte("["), append(out, ']')...)

	}

	if err := json.Unmarshal(out, &items); err != nil {

		return nil, err

	}

	return items, nil

}

// KillTree 结束进程树（force=false 先优雅，true 强制）。

func KillTree(pid int, force bool) error {

	args := []string{"/PID", strconv.Itoa(pid), "/T"}

	if force {

		args = append(args, "/F")

	}

	cmd := exec.Command("taskkill", args...)

	cmd.SysProcAttr = hideWindowAttr()

	return cmd.Run()

}

// runProcess 执行外部进程并流式回传输出行。

func runProcess(exe string, args []string, workDir string, env []string, onLine func(string), timeout time.Duration) error {

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	defer cancel()

	cmd := exec.CommandContext(ctx, exe, args...)

	if workDir != "" {

		cmd.Dir = workDir

	}

	cmd.Env = env

	cmd.SysProcAttr = hideWindowAttr()

	stdout, err := cmd.StdoutPipe()

	if err != nil {

		return err

	}

	stderr, err := cmd.StderrPipe()

	if err != nil {

		return err

	}

	done := make(chan struct{})

	go func() {

		defer close(done)

		scanPipe(stdout, onLine)

		scanPipe(stderr, onLine)

	}()

	if err := cmd.Start(); err != nil {

		return err

	}

	waitErr := cmd.Wait()

	<-done

	if ctx.Err() == context.DeadlineExceeded {

		return errors.New("执行超时（" + timeout.String() + "），进程可能仍在后台，请用「刷新状态」确认")

	}

	if waitErr != nil {

		if ee, ok := waitErr.(*exec.ExitError); ok {

			return fmt.Errorf("进程退出码 %d（上方输出为原因）", ee.ExitCode())

		}

		return waitErr

	}

	return nil

}

func scanPipe(rc io.Reader, onLine func(string)) {

	br := bufio.NewScanner(rc)

	br.Buffer(make([]byte, 64*1024), 1024*1024)

	for br.Scan() {

		line := strings.TrimRight(br.Text(), "\r\n")

		if line != "" && onLine != nil {

			onLine(line)

		}

	}

}

// loadInstance 解析 instance.json 关键字段；失败返回 ok=false。

func loadInstance(path string) (*InstanceInfo, bool) {

	raw, err := os.ReadFile(path)

	if err != nil {

		return nil, false

	}

	var m map[string]json.RawMessage

	if err := json.Unmarshal(raw, &m); err != nil {

		return nil, false

	}

	info := &InstanceInfo{}

	if rawID, ok := m["installationId"]; ok {
		var id string
		if err := json.Unmarshal(rawID, &id); err == nil {
			info.InstallationID = id
		}
	}
	if db, ok := m["database"]; ok {

		var dbm map[string]interface{}

		if err := json.Unmarshal(db, &dbm); err == nil {

			info.DatabaseMode, _ = dbm["mode"].(string)

			info.DatabasePath, _ = dbm["path"].(string)

			info.DBHost, _ = dbm["host"].(string)

			if pv, ok := dbm["port"].(float64); ok {

				info.DBPort = int(pv)

			}

			info.DBUser, _ = dbm["user"].(string)

			info.DBPassword, _ = dbm["password"].(string)

			info.DBName, _ = dbm["name"].(string)

		}

	}

	if srv, ok := m["server"]; ok {

		var sv map[string]interface{}

		if err := json.Unmarshal(srv, &sv); err == nil {

			info.AdvertiseIP, _ = sv["advertiseIp"].(string)

			info.ChannelListen, _ = sv["channelListen"].(string)

			info.AdminListen, _ = sv["adminListen"].(string)

			info.AdminToken, _ = sv["adminToken"].(string)

			info.AccountID, _ = sv["accountId"].(string)

			info.PacketLog, _ = sv["packetLog"].(bool)

			info.DebugMode, _ = sv["debugMode"].(bool)

			info.DebugStrikeOnSkillUse, _ = sv["debugStrikeOnSkillUse"].(bool)

			info.RobotEnabled, _ = sv["robotEnabled"].(bool)

			info.PartyUDPRelayEnabled, _ = sv["partyUdpRelayEnabled"].(bool)

			if pv, ok := sv["partyUdpRelayPortStart"].(float64); ok {

				info.PartyUDPRelayPortStart = int(pv)

			}

			if pv, ok := sv["partyUdpRelayPortCount"].(float64); ok {

				info.PartyUDPRelayPortCount = int(pv)

			}

		}

	}

	if gm, ok := m["game"]; ok {

		var g map[string]interface{}

		if err := json.Unmarshal(gm, &g); err == nil {

			info.GameShardID, _ = g["shardId"].(string)

			info.PVFPath, _ = g["pvfPath"].(string)

			if n, ok := g["pvfMaxBytes"].(float64); ok && n > 0 {

				info.GamePVFMaxBytes = int64(n)

			}

			info.ChannelInfoPath, _ = g["channelInfoPath"].(string)

			info.DungeonBatchPickupEnabled, _ = g["dungeonBatchPickupEnabled"].(bool)

		}

	}

	if pr, ok := m["protocol"]; ok {

		var pv map[string]interface{}

		if err := json.Unmarshal(pr, &pv); err == nil {

			info.GameUpperHeader, _ = pv["gameUpperHeader"].(string)

			info.GameUpperBodyCodec, _ = pv["gameUpperBodyCodec"].(string)

			info.GameUpperClientBodyCodec, _ = pv["gameUpperClientBodyCodec"].(string)

			info.GameOuterToken, _ = pv["gameOuterToken"].(string)

			if n, ok := pv["channelServerIndex"].(float64); ok {

				info.ChannelServerIndex = int(n)

			}

			if n, ok := pv["channelAdvertiseServerIndex"].(float64); ok {

				info.ChannelAdvertiseIndex = int(n)

			}

			if n, ok := pv["regionId"].(float64); ok {

				info.RegionID = int(n)

			}

		}

	}

	if info.DatabaseMode == "" {

		return nil, false

	}

	return info, true

}

// resolveDBPath 解析 sqlite 数据库绝对路径（相对路径基于 runtime 目录）。

func resolveDBPath(runtimeDir, dbPath string) string {

	dbPath = strings.TrimSpace(dbPath)

	if dbPath == "" {

		return filepath.Join(runtimeDir, "data", "dnf90.db")

	}

	if filepath.IsAbs(dbPath) {

		return dbPath

	}

	return filepath.Join(runtimeDir, filepath.FromSlash(dbPath))

}

// MySqlDataReady runtime\mysql\data 是否已初始化或 data-state.json phase=ready。

// MySqlDataReady 便携 MySQL 数据是否完整就绪：data-state.json phase=ready 且 data\mysql 系统库存在。

func MySqlDataReady(runtimeDir string) bool {

	stPath := filepath.Join(runtimeDir, "mysql", "data-state.json")

	st, ok := readMySQLDataState(stPath)

	if !ok || st.Phase != mysqlPhaseReady {

		return false

	}

	return DirExists(filepath.Join(runtimeDir, "mysql", "data", "mysql"))

}

// 便携 MySQL data-state.json（字段与原版 DNF90Control portableMySQLDataState 对齐）。
type mysqlDataState struct {
	SchemaVersion  int       `json:"schemaVersion"`
	InstallationID string    `json:"installationId"`
	Phase          string    `json:"phase"`
	DataDirectory  string    `json:"dataDirectory"`
	MysqldSHA256   string    `json:"mysqldSha256"`
	AutoCNFSHA256  string    `json:"autoCnfSha256,omitempty"`
	Database       string    `json:"database,omitempty"`
	InitializedAt  time.Time `json:"initializedAt"`
	ReadyAt        time.Time `json:"readyAt,omitempty"`
}

const (
	mysqlDataRelative      = "mysql/data" // data-state.json 中的 dataDirectory 相对值（相对 runtime）
	mysqlPhaseInitializing = "initializing"
	mysqlPhaseInsecure     = "initialized-insecure"
	mysqlPhaseReady        = "ready"
)

// fileSHA256 计算文件 SHA256（大写 hex，与原版一致）。
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func readMySQLDataState(path string) (*mysqlDataState, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var st mysqlDataState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false
	}
	return &st, true
}

func writeMySQLDataState(path string, st mysqlDataState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// EnsureRuntimeConfigs 补齐原版 DNF90Control generateRuntimeConfigs 产出的运行配置四件套：
//   - runtime\configs\dnfbridge.toml       （服务桥主配置，直启 DNF90Server -config 指向）
//   - runtime\configs\dnf\logic.toml       （repository 逻辑库配置，mysql_dsn/sqlite_path 取自 instance.json）
//   - runtime\configs\servergroup\plan.json（分片/路由计划）
//   - runtime\data\dnf\channel_info.etc    （由 deploy\assets\channel_info.etc 拷贝）
//
// 幂等：目录自动创建、内容由 instance.json 派生，可重复执行；旧文件被内容一致的新文件覆盖。
func EnsureRuntimeConfigs(root string, onLine func(string)) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("服务端根目录为空")
	}
	instancePath := filepath.Join(root, "runtime", "config", "instance.json")
	if !FileExists(instancePath) {
		return errors.New("缺少 runtime\\config\\instance.json，无法生成运行配置")
	}
	info, ok := loadInstance(instancePath)
	if !ok || info.DBName == "" {
		return errors.New("instance.json 解析失败或缺少 database 段")
	}
	runtimeDir := filepath.Join(root, "runtime")

	// 1) channel_info.etc：优先从 deploy 资产拷贝，已存在则跳过。
	srcChannel := filepath.Join(root, "deploy", "assets", "channel_info.etc")
	dstChannel := filepath.Join(root, "runtime", "data", "dnf", "channel_info.etc")
	if FileExists(dstChannel) {
		if onLine != nil {
			onLine("channel_info.etc 已存在，跳过拷贝")
		}
	} else if !FileExists(srcChannel) {
		return fmt.Errorf("缺少频道档模板 %s 且 runtime 侧无 channel_info.etc，无法生成", srcChannel)
	} else {
		if err := os.MkdirAll(filepath.Dir(dstChannel), 0o755); err != nil {
			return fmt.Errorf("创建 data\\dnf 目录失败: %w", err)
		}
		data, err := os.ReadFile(srcChannel)
		if err != nil {
			return fmt.Errorf("读取 deploy 频道档失败: %w", err)
		}
		if err := os.WriteFile(dstChannel, data, 0o644); err != nil {
			return fmt.Errorf("写入 channel_info.etc 失败: %w", err)
		}
		if onLine != nil {
			onLine("已从 deploy\\assets 拷贝 channel_info.etc 到 runtime\\data\\dnf")
		}
	}

	// 2) configs 三件套（目录结构与原版一致：configs / configs/dnf / configs/servergroup）。
	configsDir := filepath.Join(runtimeDir, "configs")
	for _, dir := range []string{
		configsDir,
		filepath.Join(configsDir, "dnf"),
		filepath.Join(configsDir, "servergroup"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建运行配置目录失败: %w", err)
		}
	}
	if onLine != nil {
		onLine("生成运行配置 configs\\dnfbridge.toml / dnf\\logic.toml / servergroup\\plan.json ...")
	}
	if err := writeRuntimeConfig(filepath.Join(configsDir, "dnfbridge.toml"), renderDNFBridgeConfigForRoot(root, info)); err != nil {
		return err
	}
	if err := writeRuntimeConfig(filepath.Join(configsDir, "dnf", "logic.toml"), renderLogicConfigForRoot(root, info)); err != nil {
		return err
	}
	plan, err := renderServerGroupPlanForRoot(info)
	if err != nil {
		return err
	}
	if err := writeRuntimeConfig(filepath.Join(configsDir, "servergroup", "plan.json"), plan); err != nil {
		return err
	}
	if onLine != nil {
		onLine("运行配置生成完成（dnfbridge.toml 的 public_addr 为局域网占位地址，启动前如需公网联机请在界面填入公网 IP 自动改写）")
	}
	return nil
}

func writeRuntimeConfig(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// resolveAdvertiseForConfig 将 instance.json 的 advertiseIp 解析为可写入配置的具体 IPv4：
// AUTO_DETECT/空/127.0.0.1 时探测本机私有网卡 IPv4，仍失败则回退 127.0.0.1（后续公网 IP 改写兜底）。
func resolveAdvertiseForConfig(advertise string) string {
	advertise = strings.TrimSpace(advertise)
	if advertise == "" || strings.EqualFold(advertise, "AUTO_DETECT") || advertise == "127.0.0.1" {
		return detectPrivateIPv4()
	}
	return advertise
}

// DetectPrivateIPv4 导出包装：探测本机首个私有 IPv4（供运维工具注入 DNFBRIDGE_GAME_LISTEN_HOST，
// game 端口段绑内网网卡，公网流量经 NAT 可达，云主机上不会出现绑公网 IP 失败的问题）。
func DetectPrivateIPv4() string { return detectPrivateIPv4() }

// detectPrivateIPv4 探测本机首个私有 IPv4 地址（与 DNF90Control loadInstance 的 AUTO_DETECT 口径一致）。
func detectPrivateIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil {
			continue
		}
		if ip4[0] == 10 || (ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) || (ip4[0] == 192 && ip4[1] == 168) {
			return ip4.String()
		}
	}
	return "127.0.0.1"
}

// DetectGameListenHost 返回 game 端口监听地址：
//   - 网卡上存在公网 IPv4（直绑）→ 0.0.0.0（绑所有接口，公网玩家直连；辅助私有 IP 不会被误选）
//   - 只有私有 IPv4（NAT 云主机）→ 返回真实网卡的私有 IP（暴露面最小，公网经 NAT 转发）
//   - 两者都没有 → 0.0.0.0
//
// 跳过 UU/加速器/Docker/VMware/VirtualBox/TAP 等本机虚拟网卡。
func DetectGameListenHost() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "0.0.0.0"
	}
	hasPublic := false
	private := ""
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterfaceName(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			if ip4.IsPrivate() {
				if private == "" {
					private = ip4.String()
				}
			} else {
				hasPublic = true
			}
		}
	}
	if hasPublic {
		return "0.0.0.0"
	}
	if private != "" {
		return private
	}
	return "0.0.0.0"
}

// isVirtualInterfaceName 判断接口名是否为常见本机虚拟网卡（UU/加速器/Docker/虚拟机/TAP 等）。
func isVirtualInterfaceName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, kw := range []string{
		"uu", "docker", "vmware", "virtualbox", "tap-", "tun", "vethernet",
		"hyper-v", "wsl", "zerotier", "tailscale",
	} {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// renderDNFBridgeConfigForRoot 渲染 dnfbridge.toml（逐字对齐原版 config.go renderServiceConfig）。
func renderDNFBridgeConfigForRoot(root string, info *InstanceInfo) string {
	adminPort := ParseListenPort(info.AdminListen)
	channelPort := ParseListenPort(info.ChannelListen)
	if adminPort <= 0 {
		adminPort = 18111
	}
	if channelPort <= 0 {
		channelPort = 7001
	}
	publicAddress := resolveAdvertiseForConfig(info.AdvertiseIP) + ":" + strconv.Itoa(channelPort)
	privateAddress := "127.0.0.1:" + strconv.Itoa(adminPort)
	pvfPath := strings.TrimSpace(info.PVFPath)
	if pvfPath == "" {
		pvfPath = "data/dnf/Script.pvf"
	}
	pvfMaxBytes := info.GamePVFMaxBytes
	if pvfMaxBytes <= 0 {
		pvfMaxBytes = 536870912
	}
	shardID := strings.TrimSpace(info.GameShardID)
	if shardID == "" {
		shardID = "9999"
	}
	adminToken := strings.TrimSpace(info.AdminToken)
	if adminToken == "" {
		adminToken = "auto"
	}
	return fmt.Sprintf(`[service]
name = "dnfbridge"
node_id = "dnfbridge-local-1"
environment = "local"
public_addr = %s
private_addr = %s

[cluster]
id = "local"
gid = 2
virtual_gid = 2
shard_id = %s
zone = "local"
version = "local"
data_version = "local"

[admin]
listen = %s
token = %s

[metrics]
prometheus_enabled = true
require_admin_token = false

[pvf]
enabled = true
path = %s
max_bytes = %d
preload_chunks = false
chunk_cache_bytes = 100663296
text_cache_bytes = 67108864

[server_group]
enabled = true
store_kind = "file"
plan_file = "configs/servergroup/plan.json"

[topology]
enabled = false

[idempotency]
kind = "memory"

[registry]
kind = "memory"

[bus]
kind = "memory"

[cache]
kind = "memory"

[lock]
kind = "memory"

[presence]
kind = "memory"

[worker]
size = 8
queue = 256

[profile_store]
store_kind = "memory"
`,
		quotedToml(publicAddress),
		quotedToml(privateAddress),
		shardID,
		quotedToml(info.AdminListen),
		quotedToml(adminToken),
		quotedToml(filepath.ToSlash(pvfPath)),
		pvfMaxBytes,
	)
}

// renderLogicConfigForRoot 渲染 configs\dnf\logic.toml（逐字对齐原版 config.go renderLogicConfig）。
// database.mode=portable 输出 mysql_dsn；=sqlite 输出 sqlite_path。
func renderLogicConfigForRoot(root string, info *InstanceInfo) string {
	shardID := strings.TrimSpace(info.GameShardID)
	if shardID == "" {
		shardID = "9999"
	}
	if strings.EqualFold(info.DatabaseMode, "sqlite") {
		dbPath := strings.TrimSpace(info.DatabasePath)
		if dbPath == "" {
			dbPath = "data/dnf/dnf_local.db"
		}
		return fmt.Sprintf(`[repository]
enabled = true
driver = "sqlite"
sqlite_path = %s
redis_enabled = false
`, quotedToml(filepath.ToSlash(dbPath)))
	}
	dbHost := strings.TrimSpace(info.DBHost)
	if dbHost == "" {
		dbHost = "127.0.0.1"
	}
	dbPort := info.DBPort
	if dbPort <= 0 {
		dbPort = 13306
	}
	dbName := strings.TrimSpace(info.DBName)
	if dbName == "" {
		dbName = "dnf_local"
	}
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4,utf8",
		info.DBUser,
		info.DBPassword,
		dbHost,
		dbPort,
		dbName,
	)
	return fmt.Sprintf(`[repository]
enabled = true
driver = "mysql"
mysql_dsn = %s
mysql_max_open_conns = 32
mysql_max_idle_conns = 8
mysql_conn_max_lifetime_seconds = 300
shard_id = %s
table_prefix = "dnf"
server_group_plan_file = "configs/servergroup/plan.json"
auto_create_schema = true
csharp_legacy_schema = true
create_databases = false
`,
		quotedToml(dsn),
		quotedToml(shardID),
	)
}

// renderServerGroupPlanForRoot 渲染 configs\servergroup\plan.json（逐字对齐原版 renderServerGroupPlan）。
func renderServerGroupPlanForRoot(info *InstanceInfo) (string, error) {
	shardID := strings.TrimSpace(info.GameShardID)
	if shardID == "" {
		shardID = "9999"
	}
	dbName := strings.TrimSpace(info.DBName)
	if dbName == "" {
		dbName = "dnf_local"
	}
	type planShard struct {
		ID           string `json:"id"`
		GroupID      string `json:"group_id"`
		State        string `json:"state"`
		OpenAt       string `json:"open_at"`
		PublicOpenAt string `json:"public_open_at"`
	}
	type planGroup struct {
		ID       string `json:"id"`
		Service  string `json:"service"`
		MemberID string `json:"member_id"`
		State    string `json:"state"`
	}
	type planRoute struct {
		Feature string            `json:"feature"`
		ShardID string            `json:"shard_id"`
		GroupID string            `json:"group_id"`
		State   string            `json:"state"`
		Meta    map[string]string `json:"meta"`
	}
	plan := struct {
		Version string      `json:"version"`
		Shards  []planShard `json:"shards"`
		Groups  []planGroup `json:"groups"`
		Routes  []planRoute `json:"routes"`
	}{
		Version: "dnf90-local-v1",
		Shards: []planShard{{
			ID:           shardID,
			GroupID:      "logic-local-1",
			State:        "open",
			OpenAt:       "2026-01-01T00:00:00Z",
			PublicOpenAt: "2026-01-01T00:00:00Z",
		}},
		Groups: []planGroup{{
			ID:       "logic-local-1",
			Service:  "logic",
			MemberID: "logic-local-1",
			State:    "open",
		}},
		Routes: []planRoute{{
			Feature: "dnf_repository",
			ShardID: shardID,
			GroupID: "logic-local-1",
			State:   "open",
			Meta: map[string]string{
				"dnf.repository.write_database": dbName,
				"dnf.repository.read_database":  dbName,
			},
		}},
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", fmt.Errorf("编码 server-group plan 失败: %w", err)
	}
	return string(data) + "\n", nil
}

// quotedToml TOML 字符串字面量转义（与 DNF90Control tomlString 等价）。
func quotedToml(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\r", `\r`,
		"\n", `\n`,
		"\t", `\t`,
	)
	return `"` + replacer.Replace(value) + `"`
}

// renderMySQLConfigForRoot 渲染便携 mysqld 配置文件（对齐原版 renderMySQLConfig）。
func renderMySQLConfigForRoot(root string, port int) string {
	mysqlServer := filepath.Join(root, "runtime", "mysql", "server")
	mysqlData := filepath.Join(root, "runtime", "mysql", "data")
	logErr := filepath.Join(root, "runtime", "logs", "mysql-error.log")
	pidFile := filepath.Join(root, "runtime", "state", "mysql.pid")
	quoted := func(v string) string {
		v = filepath.ToSlash(v)
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)
		return `"` + v + `"`
	}
	return fmt.Sprintf(`[mysqld]
basedir=%s
datadir=%s
port=%d
bind-address=127.0.0.1
mysqlx=0
skip-log-bin
local-infile=OFF
symbolic-links=0
character-set-server=utf8mb4
collation-server=utf8mb4_0900_ai_ci
max-connections=128
innodb-flush-log-at-trx-commit=1
log-error=%s
pid-file=%s

[client]
host=127.0.0.1
port=%d
protocol=TCP
default-character-set=utf8mb4
`,
		quoted(mysqlServer),
		quoted(mysqlData),
		port,
		quoted(logErr),
		quoted(pidFile),
		port,
	)
}

// backupInterruptedMySQLData 将中断初始化残留的 data 目录改名备份（保留在 runtime\mysql 下）。
func backupInterruptedMySQLData(runtimeDir string) error {
	dataDir := filepath.Join(runtimeDir, "mysql", "data")
	if !DirExists(dataDir) {
		return nil
	}
	dst := filepath.Join(runtimeDir, "mysql", "data.interrupted-"+time.Now().UTC().Format("20060102-150405"))
	if DirExists(dst) {
		return fmt.Errorf("中断备份目录已存在: %s", dst)
	}
	return os.Rename(dataDir, dst)
}

// readAutoCNFUUID 从 data\auto.cnf 的 [auto] 段解析 server-uuid。
func readAutoCNFUUID(autoCNFPath string) (string, error) {
	data, err := os.ReadFile(autoCNFPath)
	if err != nil {
		return "", err
	}
	inAuto := false
	var found string
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inAuto = strings.EqualFold(strings.TrimSpace(line[1:len(line)-1]), "auto")
			continue
		}
		if !inAuto {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "server-uuid") {
			continue
		}
		found = strings.TrimSpace(value)
	}
	if found == "" {
		return "", errors.New("auto.cnf 中未找到 server-uuid")
	}
	return found, nil
}

// openMySQLRootDB 打开便携 MySQL root 连接。
func openMySQLRootDB(host string, port int, user, password, dbName string) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&timeout=5s&readTimeout=5s&writeTimeout=5s",
		user, password, host, port, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// verifyPortableMySQLIdentity 校验连接的正是本目录数据目录对应的 mysqld 实例。
func verifyPortableMySQLIdentity(db *sql.DB, expectedPort int, dataDir string) error {
	expectedUUID, err := readAutoCNFUUID(filepath.Join(dataDir, "auto.cnf"))
	if err != nil {
		return err
	}
	var actualUUID string
	var actualPort int
	var actualData string
	if err := db.QueryRow("SELECT @@GLOBAL.server_uuid, @@GLOBAL.port, @@GLOBAL.datadir").Scan(&actualUUID, &actualPort, &actualData); err != nil {
		return fmt.Errorf("查询 MySQL 实例身份失败: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(actualUUID), expectedUUID) {
		return fmt.Errorf("连接的 MySQL UUID %q 与数据目录 UUID %q 不一致", strings.TrimSpace(actualUUID), expectedUUID)
	}
	if actualPort != expectedPort {
		return fmt.Errorf("连接的 MySQL 端口 %d 与预期 %d 不一致", actualPort, expectedPort)
	}
	norm := func(v string) string {
		return strings.ToLower(strings.TrimRight(filepath.ToSlash(filepath.Clean(v)), "/"))
	}
	if norm(actualData) != norm(dataDir) {
		return fmt.Errorf("连接的 MySQL datadir %q 与本目录 %q 不一致", actualData, dataDir)
	}
	return nil
}

// provisionPortableMySQLRoot 设置 root 密码并建库（幂等，对齐原版 provisionPortableDatabase）。
func provisionPortableMySQLRoot(db *sql.DB, password, dbName string) error {
	statements := []string{
		fmt.Sprintf("ALTER USER 'root'@'localhost' IDENTIFIED BY '%s'", password),
		fmt.Sprintf("CREATE USER IF NOT EXISTS 'root'@'127.0.0.1' IDENTIFIED BY '%s'", password),
		fmt.Sprintf("ALTER USER 'root'@'127.0.0.1' IDENTIFIED BY '%s'", password),
		"GRANT ALL PRIVILEGES ON *.* TO 'root'@'127.0.0.1' WITH GRANT OPTION",
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci", dbName),
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("执行 %q 失败: %w", q, err)
		}
	}
	return nil
}

// EnsureDatabaseReady 一键初始化入口：按模式分发。
// sqlite → 原版 select --prepare；mysql → 完整便携 MySQL 初始化链。
func EnsureDatabaseReady(root, mode string, onLine func(string), timeout time.Duration) error {
	if strings.EqualFold(strings.TrimSpace(mode), "sqlite") {
		return EnsureInstance(root, "sqlite", false, onLine, timeout)
	}
	return EnsurePortableMySQLReady(root, onLine, timeout)
}

// EnsurePortableMySQLReady 完整便携 MySQL 初始化链（幂等）：
// instance.json prepare → mysql.ini → mysqld --initialize-insecure → 拉起 mysqld → 设密码建库 → data-state ready。
// 目录未携带便携 mysqld 时回退本机 MySQL 服务。
func EnsurePortableMySQLReady(root string, onLine func(string), timeout time.Duration) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("服务端根目录为空")
	}
	runtimeDir := filepath.Join(root, "runtime")
	instancePath := filepath.Join(runtimeDir, "config", "instance.json")
	mysqldExe := filepath.Join(runtimeDir, "mysql", "server", "bin", "mysqld.exe")
	mysqlIni := filepath.Join(runtimeDir, "config", "mysql.ini")
	dataStatePath := filepath.Join(runtimeDir, "mysql", "data-state.json")
	dataDir := filepath.Join(runtimeDir, "mysql", "data")

	// 1) instance.json 缺失或便携 mysqld 未解压：原版 prepare 补齐（幂等，已存在的配置会被保留）。
	if !FileExists(instancePath) || !FileExists(mysqldExe) {
		if onLine != nil {
			onLine("未检测到 instance.json 或便携 MySQL 未解压，调用原版控制器补齐 ...")
		}
		if err := EnsureInstance(root, "mysql", true, onLine, timeout); err != nil {
			return err
		}
	}
	info, ok := loadInstance(instancePath)
	if !ok || info.InstallationID == "" || info.DBName == "" {
		return errors.New("instance.json 解析失败或缺少 installationId/database 段")
	}
	port := info.DBPort
	if port <= 0 {
		port = 13306
	}

	// 目录不携带便携 mysqld：回退本机 MySQL 服务（外部部署兼容）。
	if !FileExists(mysqldExe) {
		if onLine != nil {
			onLine("服务端目录未携带内置便携 MySQL，回退本机 MySQL 服务 ...")
		}
		return EnsureMySql(root, port, onLine, timeout)
	}

	// 2) mysql.ini 缺失：按原版模板渲染。
	if !FileExists(mysqlIni) {
		if onLine != nil {
			onLine("生成便携 MySQL 配置文件 runtime\\config\\mysql.ini ...")
		}
		if err := os.MkdirAll(filepath.Dir(mysqlIni), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(mysqlIni, []byte(renderMySQLConfigForRoot(root, port)), 0o600); err != nil {
			return fmt.Errorf("写入 mysql.ini 失败: %w", err)
		}
	}

	// 3) 数据状态判定。
	st, okState := readMySQLDataState(dataStatePath)
	if okState && st.Phase == mysqlPhaseReady {
		if !MySqlDataReady(runtimeDir) {
			// state 标记 ready 但数据目录缺失/不完整：备份残留后重置为重新初始化。
			if onLine != nil {
				onLine("data-state 标记 ready 但数据目录不完整，备份残留后重新初始化 ...")
			}
			if err := backupInterruptedMySQLData(runtimeDir); err != nil {
				return err
			}
			okState = false
			st = nil
		} else {
			if ProbePort("127.0.0.1", port, 800*time.Millisecond) {
				if onLine != nil {
					onLine(fmt.Sprintf("MySQL 数据已就绪（ready），端口 %d 已在监听", port))
				}
				return nil
			}
			if onLine != nil {
				onLine("MySQL 数据已就绪（ready），端口未监听，正在拉起 mysqld ...")
			}
			return startBundledMySql(runtimeDir, mysqldExe, mysqlIni, port, onLine, timeout)
		}
	}

	mysqldSHA256, err := fileSHA256(mysqldExe)
	if err != nil {
		return err
	}

	needInitialize := false
	if okState {
		switch st.Phase {
		case mysqlPhaseInsecure:
			needInitialize = false
		case mysqlPhaseInitializing:
			// 上次初始化中断：备份残留后重来。
			if onLine != nil {
				onLine("检测到上次初始化中断（data-state=initializing），备份残留数据后重新初始化 ...")
			}
			if err := backupInterruptedMySQLData(runtimeDir); err != nil {
				return err
			}
			needInitialize = true
		default:
			return fmt.Errorf("data-state.json phase=%q 无法继续，如需重建请手动清理 runtime\\mysql 后重试", st.Phase)
		}
	} else {
		if DirExists(dataDir) {
			entries, err := os.ReadDir(dataDir)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				return errors.New("runtime\\mysql\\data 非空但没有 data-state.json，为避免误接管已中止；确认可丢弃请手动删除该目录后重新初始化")
			}
		}
		needInitialize = true
	}

	if needInitialize {
		if onLine != nil {
			onLine("初始化便携 MySQL 数据（mysqld --initialize-insecure，仅首次）...")
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return err
		}
		state := mysqlDataState{
			SchemaVersion:  1,
			InstallationID: info.InstallationID,
			Phase:          mysqlPhaseInitializing,
			DataDirectory:  mysqlDataRelative,
			MysqldSHA256:   mysqldSHA256,
			InitializedAt:  time.Now().UTC(),
		}
		if err := writeMySQLDataState(dataStatePath, state); err != nil {
			return err
		}
		iniArg := "--defaults-file=" + mysqlIni
		if err := runProcess(mysqldExe, []string{iniArg, "--initialize-insecure", "--console"},
			filepath.Join(runtimeDir, "mysql", "server"), os.Environ(), onLine, timeout); err != nil {
			return fmt.Errorf("mysqld --initialize-insecure 失败: %w", err)
		}
		if !DirExists(filepath.Join(dataDir, "mysql")) || !FileExists(filepath.Join(dataDir, "mysql.ibd")) || !FileExists(filepath.Join(dataDir, "auto.cnf")) {
			return errors.New("mysqld 初始化未生成完整系统数据（缺 mysql\\mysql.ibd\\auto.cnf）")
		}
		autoSHA, err := fileSHA256(filepath.Join(dataDir, "auto.cnf"))
		if err != nil {
			return err
		}
		state.Phase = mysqlPhaseInsecure
		state.AutoCNFSHA256 = autoSHA
		if err := writeMySQLDataState(dataStatePath, state); err != nil {
			return err
		}
		if onLine != nil {
			onLine("便携 MySQL 系统数据初始化完成")
		}
	}

	// 初始化/重置后重读状态：置为 insecure 以便设密阶段支持空密码回退。

	if st2, ok2 := readMySQLDataState(dataStatePath); ok2 {

		okState = true

		st = st2

	}

	// 4) 拉起 mysqld（端口未监听时）。
	if !ProbePort("127.0.0.1", port, 800*time.Millisecond) {
		if onLine != nil {
			onLine("正在启动便携 mysqld 以完成建库 ...")
		}
		if err := startBundledMySql(runtimeDir, mysqldExe, mysqlIni, port, onLine, timeout); err != nil {
			return err
		}
	}

	// 5) 设 root 密码 + 建库：优先用配置密码，失败且处于 insecure 阶段再用空密码。
	var db *sql.DB
	db, err = openMySQLRootDB("127.0.0.1", port, info.DBUser, info.DBPassword, "")
	if err != nil {
		if okState && st.Phase == mysqlPhaseInsecure {
			db, err = openMySQLRootDB("127.0.0.1", port, info.DBUser, "", "")
		}
		if err != nil {
			return fmt.Errorf("连接便携 MySQL 失败（配置密码: %v）", err)
		}
		if onLine != nil {
			onLine("使用初始空密码完成 root 密码设置与建库 ...")
		}
	} else if onLine != nil {
		onLine("MySQL 已可连接，校验并补齐 root 密码与数据库 ...")
	}
	defer func() { _ = db.Close() }()

	if err := verifyPortableMySQLIdentity(db, port, dataDir); err != nil {
		return err
	}
	if err := provisionPortableMySQLRoot(db, info.DBPassword, info.DBName); err != nil {
		return err
	}

	// 6) 标记 ready。
	st2, ok2 := readMySQLDataState(dataStatePath)
	if !ok2 {
		return errors.New("data-state.json 在初始化过程中丢失")
	}
	st2.Phase = mysqlPhaseReady
	st2.Database = info.DBName
	st2.ReadyAt = time.Now().UTC()
	if err := writeMySQLDataState(dataStatePath, *st2); err != nil {
		return err
	}
	if onLine != nil {
		onLine(fmt.Sprintf("便携 MySQL 初始化完成：库 %s 已建、root 密码已设置（端口 %d）", info.DBName, port))
	}
	return nil
}

// EnsureMySql 确保 MySQL 在 port 端口监听。

// 优先以原版等效方式直启服务端目录自带的便携 MySQL（runtime\mysql\server\bin\mysqld.exe

// + runtime\config\mysql.ini，同 DNF90Control start 的 portable 启动链路，无需 Windows 服务）；

// 目录不携带便携 MySQL 时，才回退到 Windows 上名称含 mysql/maria 的服务。

func EnsureMySql(root string, port int, onLine func(string), timeout time.Duration) error {

	if ProbePort("127.0.0.1", port, 800*time.Millisecond) {

		if onLine != nil {

			onLine(fmt.Sprintf("MySQL 端口 %d 已在监听", port))

		}

		return nil

	}

	mysqldExe := filepath.Join(root, "runtime", "mysql", "server", "bin", "mysqld.exe")

	mysqlIni := filepath.Join(root, "runtime", "config", "mysql.ini")

	if root != "" && FileExists(mysqldExe) {

		if !FileExists(mysqlIni) {

			return fmt.Errorf("服务端内置 MySQL 存在但缺少配置文件: %s", mysqlIni)

		}

		runtimeDir := filepath.Join(root, "runtime")

		if !MySqlDataReady(runtimeDir) {

			return errors.New("服务端内置 MySQL 数据尚未初始化（runtime\\mysql\\data 缺失或未 ready），请先执行「一键初始化」或原版 START.bat 完成首次初始化")

		}

		return startBundledMySql(runtimeDir, mysqldExe, mysqlIni, port, onLine, timeout)

	}

	// 兜底：Windows MySQL 服务（旧的外部 MySQL 部署）。

	return startMySqlWindowsService(port, onLine, timeout)

}

// startBundledMySql 直启服务端自带便携 mysqld（等效 DNF90Control start 的 portable 分支）：

// mysqld --defaults-file=mysql.ini --console --no-monitor，工作目录 runtime\mysql\server。

func startBundledMySql(runtimeDir, mysqldExe, mysqlIni string, port int, onLine func(string), timeout time.Duration) error {

	mysqlServer := filepath.Join(runtimeDir, "mysql", "server")

	logsDir := filepath.Join(runtimeDir, "logs")

	if err := os.MkdirAll(logsDir, 0o700); err != nil {

		return fmt.Errorf("创建 MySQL 日志目录失败: %w", err)

	}

	stamp := time.Now().Format("20060102-150405")

	stdoutPath := filepath.Join(logsDir, "mysql-ops-"+stamp+".stdout.log")

	stderrPath := filepath.Join(logsDir, "mysql-ops-"+stamp+".stderr.log")

	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)

	if err != nil {

		return fmt.Errorf("打开 MySQL stdout 日志失败: %w", err)

	}

	defer stdoutFile.Close()

	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)

	if err != nil {

		return fmt.Errorf("打开 MySQL stderr 日志失败: %w", err)

	}

	defer stderrFile.Close()

	if onLine != nil {

		onLine("端口未监听，发现服务端内置便携 MySQL，正在按原版等效方式直启 mysqld.exe ...")

	}

	cmd := exec.Command(mysqldExe, "--defaults-file="+mysqlIni, "--console", "--no-monitor")

	cmd.Dir = mysqlServer

	cmd.Stdout = stdoutFile

	cmd.Stderr = stderrFile

	cmd.SysProcAttr = hideWindowNewGroupAttr()

	if err := cmd.Start(); err != nil {

		return fmt.Errorf("启动 mysqld.exe 失败: %w", err)

	}

	// 释放句柄：mysqld 独立常驻，等待端口就绪由下方轮询负责。

	if err := cmd.Process.Release(); err != nil {

		if onLine != nil {

			onLine(fmt.Sprintf("警告: 释放 mysqld 进程句柄失败: %v", err))

		}

	}

	if onLine != nil {

		onLine(fmt.Sprintf("mysqld 已启动（pid=%d），等待端口 %d 就绪...", cmd.Process.Pid, port))

	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {

		if ProbePort("127.0.0.1", port, 500*time.Millisecond) {

			if onLine != nil {

				onLine(fmt.Sprintf("MySQL 端口 %d 已就绪（便携 mysqld，日志: %s）", port, stderrPath))

			}

			return nil

		}

		time.Sleep(1 * time.Second)

	}

	// 超时诊断：回显 stderr 日志尾部，便于定位初始化/端口占用等问题。

	if onLine != nil {

		onLine(fmt.Sprintf("等待 MySQL 端口 %d 就绪超时，stderr 日志尾部：", port))

		showLogTail(stderrPath, 25, onLine)

	}

	return fmt.Errorf("mysqld 启动后端口 %d 未在 %s 内就绪，请查看日志: %s", port, timeout, stderrPath)

}

// startMySqlWindowsService 回退方案：查找并启动 Windows 上名称含 mysql/maria 的服务。

func startMySqlWindowsService(port int, onLine func(string), timeout time.Duration) error {

	script := `$svcs = Get-Service -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'mysql' -or $_.DisplayName -match 'mysql' -or $_.Name -match 'maria' }; if (-not $svcs) { Write-Output 'NO_MYSQL_SERVICE'; exit 2 }; foreach ($s in $svcs) { if ($s.Status -ne 'Running') { try { Start-Service -Name $s.Name -ErrorAction Stop; Write-Output ('STARTED ' + $s.Name) } catch { Write-Output ('FAIL ' + $s.Name + ' :: ' + $_.Exception.Message) } } else { Write-Output ('RUNNING ' + $s.Name) } }`

	if onLine != nil {

		onLine("端口未监听且目录未携带内置便携 MySQL，正在查找并启动本机 MySQL 服务...")

	}

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)

	var buf bytes.Buffer

	cmd.Stdout = &buf

	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {

		out := strings.TrimSpace(buf.String())

		if strings.Contains(out, "NO_MYSQL_SERVICE") {

			return errors.New("未找到可用的 MySQL：服务端目录未携带内置便携 MySQL，本机也无 mysql/maria Windows 服务，请确认服务端目录完整（含 runtime\\mysql\\server\\bin\\mysqld.exe）")

		}

		if onLine != nil {

			onLine(out)

		}

		return fmt.Errorf("执行 MySQL 服务启动失败: %v", err)

	}

	out := strings.TrimSpace(buf.String())

	if out != "" && onLine != nil {

		onLine(out)

	}

	// 等待端口就绪。

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {

		if ProbePort("127.0.0.1", port, 500*time.Millisecond) {

			if onLine != nil {

				onLine(fmt.Sprintf("MySQL 端口 %d 已就绪", port))

			}

			return nil

		}

		time.Sleep(1 * time.Second)

	}

	return fmt.Errorf("等待 MySQL 端口 %d 就绪超时（%s），请到服务管理器检查 MySQL 服务状态", port, timeout)

}

// showLogTail 回传文件尾部若干行。

func showLogTail(path string, n int, onLine func(string)) {

	if onLine == nil {

		return

	}

	f, err := os.Open(path)

	if err != nil {

		onLine("（无法读取日志: " + err.Error() + "）")

		return

	}

	defer f.Close()

	var lines []string

	sc := bufio.NewScanner(f)

	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {

		lines = append(lines, sc.Text())

	}

	if len(lines) > n {

		lines = lines[len(lines)-n:]

	}

	for _, line := range lines {

		onLine("  " + line)

	}

}

// ProbePort 探测 addr:port 是否可连。

func ProbePort(host string, port int, timeout time.Duration) bool {

	if port <= 0 {

		return false

	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)

	if err != nil {

		return false

	}

	_ = conn.Close()

	return true

}

// ParseListenPort 从 "0.0.0.0:7001" 解析端口；失败返回 0。

func ParseListenPort(listen string) int {

	if strings.TrimSpace(listen) == "" {

		return 0

	}

	idx := strings.LastIndex(listen, ":")

	if idx < 0 {

		return 0

	}

	p, err := strconv.Atoi(strings.TrimSpace(listen[idx+1:]))

	if err != nil || p <= 0 {

		return 0

	}

	return p

}

// DirExists 判断目录存在。

func DirExists(dir string) bool {

	st, err := os.Stat(dir)

	return err == nil && st.IsDir()

}

// FileExists 判断文件存在（支持多级相对拼接）。

func FileExists(parts ...string) bool {

	if len(parts) == 0 {

		return false

	}

	p := filepath.Join(parts...)

	st, err := os.Stat(p)

	return err == nil && !st.IsDir()

}

// IsPrivateIP 判断 IP 是否为私网/回环/链路本地（用于 public_addr 校验提示）。

func IsPrivateIP(ip string) bool {

	ip = strings.TrimSpace(ip)

	if ip == "" {

		return true

	}

	if strings.EqualFold(ip, "AUTO_DETECT") || strings.EqualFold(ip, "AUTO_GENERATE") {

		return true

	}

	host := ip

	if h, _, err := net.SplitHostPort(ip); err == nil {

		host = h

	}

	parsed := net.ParseIP(host)

	if parsed == nil {

		return true

	}

	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast()

}

// DNFBridgeTomlPath 返回服务端 dnfbridge.toml 绝对路径。

func DNFBridgeTomlPath(root string) string {

	return filepath.Join(root, "runtime", "configs", "dnfbridge.toml")

}

// WritePublicAddr 将 dnfbridge.toml [service].public_addr 的 IP 改写为指定公网 IP（保留原端口）。

// 首次改写前自动备份为 dnfbridge.toml.bak。返回新的 public_addr（含端口）。

func WritePublicAddr(root, ip string) (string, error) {

	ip = strings.TrimSpace(ip)

	if ip == "" {

		return "", errors.New("公网 IP 为空")

	}

	cfgPath := DNFBridgeTomlPath(root)

	raw, err := os.ReadFile(cfgPath)

	if err != nil {

		return "", fmt.Errorf("读取 dnfbridge.toml 失败: %w", err)

	}

	// 从现有 public_addr 提取端口。

	port := ""

	for _, line := range strings.Split(string(raw), "\n") {

		trim := strings.TrimSpace(line)

		if !strings.HasPrefix(trim, "public_addr") {

			continue

		}

		if _, after, ok := strings.Cut(trim, "="); ok {

			v := strings.Trim(strings.TrimSpace(after), `"`)

			if h, p, err := net.SplitHostPort(v); err == nil && h != "" && p != "" {

				port = p

			}

		}

		break

	}

	if port == "" {

		return "", errors.New("dnfbridge.toml 未找到带端口的 public_addr")

	}

	// 首次改写前备份原始文件。

	bakPath := cfgPath + ".bak"

	if !FileExists(bakPath) {

		if err := os.WriteFile(bakPath, raw, 0644); err != nil {

			return "", fmt.Errorf("备份 dnfbridge.toml 失败: %w", err)

		}

	}

	newAddr := net.JoinHostPort(ip, port)

	inService := false

	changed := false

	lines := strings.Split(string(raw), "\n")

	for i, line := range lines {

		trim := strings.TrimSpace(line)

		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {

			inService = strings.EqualFold(strings.TrimSpace(trim), "[service]")

			continue

		}

		if inService && strings.HasPrefix(trim, "public_addr") {

			idx := strings.Index(line, "=")

			if idx < 0 {

				continue

			}

			lines[i] = line[:idx+1] + ` "` + newAddr + `"`

			changed = true

			break

		}

	}

	if !changed {

		return "", errors.New("dnfbridge.toml [service] 段未匹配到 public_addr")

	}

	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {

		return "", fmt.Errorf("写回 dnfbridge.toml 失败: %w", err)

	}

	return newAddr, nil

}

// RewriteChannelListen 将 instance.json 的 server.channelListen 改写为指定监听地址（如 0.0.0.0:7001）。
// 行级替换，保留原 JSON 其余内容与缩进；首次改写前备份为 instance.json.bak。
func RewriteChannelListen(root, listen string) (string, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "", errors.New("监听地址为空")
	}
	instancePath := filepath.Join(root, "runtime", "config", "instance.json")
	raw, err := os.ReadFile(instancePath)
	if err != nil {
		return "", fmt.Errorf("读取 instance.json 失败: %w", err)
	}
	bakPath := instancePath + ".bak"
	if !FileExists(bakPath) {
		if err := os.WriteFile(bakPath, raw, 0644); err != nil {
			return "", fmt.Errorf("备份 instance.json 失败: %w", err)
		}
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		keyIdx := strings.Index(line, "\"channelListen\"")
		if keyIdx < 0 {
			continue
		}
		colonIdx := strings.Index(line[keyIdx:], ":")
		if colonIdx < 0 {
			continue
		}
		sep := keyIdx + colonIdx
		valuePart := strings.TrimSpace(line[sep+1:])
		comma := ""
		if strings.HasSuffix(valuePart, ",") {
			comma = ","
			valuePart = strings.TrimSuffix(strings.TrimSpace(valuePart), ",")
		}
		valuePart = strings.TrimSpace(valuePart)
		if unq, err := strconv.Unquote(valuePart); err == nil && unq == listen {
			// 已为目标值，无需写回。
			return listen, nil
		}
		lines[i] = line[:sep+1] + ` "` + listen + `"` + comma
		changed = true
		break
	}
	if !changed {
		return "", errors.New("instance.json 未匹配到 channelListen 字段")
	}
	if err := os.WriteFile(instancePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return "", fmt.Errorf("写回 instance.json 失败: %w", err)
	}
	return listen, nil
}

// WantedPartyUDPRelayPortCount 队伍 UDP 中继端口池的目标数量：1024。
//
// 为什么统一成 1024：服务端按 instance.json 的 server.partyUdpRelayPortCount 预留
// [partyUdpRelayPortStart, partyUdpRelayPortStart+count) 这段 UDP 端口做组队中继。
// 老的实例文件里是 64（go-server 内置默认 512），组队一多就会把端口池耗光。
// 服务端校验的范围是 12~1024（见 go-server cmd/server/control/instance.go），所以取上限。
//
// 注意：端口段本身要在防火墙/安全组放行（30000~31023/UDP），这是部署侧的事。
const WantedPartyUDPRelayPortCount = 1024

// EnsurePartyUDPRelayPortCount 检查 instance.json 的 server.partyUdpRelayPortCount：
// 不等于 want 就改写为 want，返回是否发生了改写（已经是 want 时不动文件）。
//
// 做法与 RewriteChannelListen 一致：行级替换数值，保留原 JSON 其余内容、缩进与行尾逗号；
// 首次改写前备份为 instance.json.bak。字段缺失（老文件没写这一项）时，按
// partyUdpRelayPortStart 那一行的缩进插一行进去 —— 否则服务端会用内置默认 512。
func EnsurePartyUDPRelayPortCount(root string, want int, onLine func(string)) (bool, error) {
	if want <= 0 {
		return false, errors.New("目标端口数量必须为正数")
	}
	instancePath := filepath.Join(root, "runtime", "config", "instance.json")
	fileRaw, err := os.ReadFile(instancePath)
	if err != nil {
		return false, fmt.Errorf("读取 instance.json 失败: %w", err)
	}
	lines := strings.Split(string(fileRaw), "\n")
	target := strconv.Itoa(want)

	writeBack := func(desc string) (bool, error) {
		bakPath := instancePath + ".bak"
		if !FileExists(bakPath) {
			if err := os.WriteFile(bakPath, fileRaw, 0644); err != nil {
				return false, fmt.Errorf("备份 instance.json 失败: %w", err)
			}
		}
		if err := os.WriteFile(instancePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			return false, fmt.Errorf("写回 instance.json 失败: %w", err)
		}
		if onLine != nil {
			onLine(fmt.Sprintf("instance.json: %s（原文件已备份为 instance.json.bak）", desc))
		}
		return true, nil
	}

	// ① 字段已存在：行级替换数值。
	for i, line := range lines {
		keyIdx := strings.Index(line, "\"partyUdpRelayPortCount\"")
		if keyIdx < 0 {
			continue
		}
		colonIdx := strings.Index(line[keyIdx:], ":")
		if colonIdx < 0 {
			continue
		}
		sep := keyIdx + colonIdx
		after := line[sep+1:]
		cr := ""
		if strings.HasSuffix(after, "\r") {
			after = strings.TrimSuffix(after, "\r")
			cr = "\r"
		}
		valuePart := strings.TrimSpace(after)
		comma := ""
		if strings.HasSuffix(valuePart, ",") {
			comma = ","
			valuePart = strings.TrimSpace(strings.TrimSuffix(valuePart, ","))
		}
		if cur, convErr := strconv.Atoi(valuePart); convErr == nil && cur == want {
			return false, nil // 已经是目标值，不写文件
		}
		// 沿用原冒号后到数值之间的空白，缩进保持不变。
		space := after[:len(after)-len(strings.TrimLeft(after, " \t"))]
		lines[i] = line[:sep+1] + space + target + comma + cr
		if cur, convErr := strconv.Atoi(valuePart); convErr == nil {
			return writeBack(fmt.Sprintf("partyUdpRelayPortCount %d -> %d", cur, want))
		}
		return writeBack(fmt.Sprintf("partyUdpRelayPortCount %q -> %d", valuePart, want))
	}

	// ② 字段缺失：按 partyUdpRelayPortStart 的缩进插一行。
	for i, line := range lines {
		if strings.Index(line, "\"partyUdpRelayPortStart\"") < 0 {
			continue
		}
		cr := ""
		if strings.HasSuffix(line, "\r") {
			line = strings.TrimSuffix(line, "\r")
			cr = "\r"
			lines[i] = line
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		inserted := indent + "\"partyUdpRelayPortCount\": " + target + "," + cr
		lines = append(lines[:i+1], append([]string{inserted}, lines[i+1:]...)...)
		return writeBack(fmt.Sprintf("补写 partyUdpRelayPortCount = %d", want))
	}

	return false, errors.New("instance.json 未匹配到 partyUdpRelayPortCount / partyUdpRelayPortStart 字段")
}
