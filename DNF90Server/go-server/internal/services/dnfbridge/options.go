// options.go 负责 DNF bridge 启动配置和环境变量兼容。
// 这里只归一化协议开关和监听参数，不持有运行期玩家状态。
package dnfbridge

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/platform/config"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	defaultChannelListen   = ":7001"
	defaultChannelInfoFile = "data/dnf/channel_info.etc"
	defaultServerIP        = "127.0.0.1"
	// Bump when the downloaded channel_info.etc semantics change. Keeping the
	// old value makes clients reuse their cached script and continue showing
	// team channels as unavailable even after the server-side schedule fix.
	defaultScriptVersion           = "60"
	channelInfoBodyLatest          = "latest"
	gameInitialModeNotice          = "notice"
	gameInitialModeLogin           = "login_success"
	gameInitialModeUpper           = "upper_endpoint"
	gameInitialModeNone            = "none"
	gamePreBootstrapNone           = "none"
	gamePreBootstrapGuard          = "guard_clear"
	gamePostBootstrapNone          = "none"
	gamePostBootstrapCheck         = "client_check"
	gamePostBootstrapRoster        = "roster_init"
	gameUpperHeaderChannel13       = "channel13"
	gameUpperHeaderServer16        = "server16"
	gameUpperBodyCodecAuto         = "auto"
	gameUpperBodyCodecPlain        = "plaintext"
	gameUpperClientBodyCodecPlain  = "plaintext"
	gameUpperClientBodyCodecProbe  = "probe"
	gameUpperClientBodyCodecNative = "native"
	gameDprotoModeLegacy           = "legacy_patch"
	gameDprotoModeNative           = "native"
	// S2C 回包二次加密模式（见 protocol/s2ccipher.go）。
	//   off    = 明文（默认；客户端 DLL 也是直通，两侧行为与加这个功能之前完全一致）
	//   shadow = 只协商、只记账，**仍然发明文**（用来验证"哪些连接会启用"，零风险）
	//   on     = 协商成功的连接加密 body
	gameS2CCipherOff    = "off"
	gameS2CCipherShadow = "shadow"
	gameS2CCipherOn     = "on"
	// DOVE 明文首包里 UDP 字段是 2311/2312；这里只作为 InitialLoginNotice 的兼容默认值。
	defaultInitialUDPPort1   = 2311
	defaultInitialUDPPort2   = 2312
	defaultCommandCount      = 1382
	defaultNotificationCount = 1326
	defaultMaxPacketBytes    = 1048576
	defaultAccountPrefix     = "dnf:"
	// defaultLauncherAuthVerifyURL is the loopback verifier that the external
	// game connection auth gate calls after it receives a launcher ticket.
	// The URL stays internal to the host: launcher.exe owns the endpoint and
	// dnfbridge only talks to it over 127.0.0.1.
	defaultLauncherAuthVerifyURL        = "http://127.0.0.1:19091/internal/verify-game-ticket"
	defaultLauncherAuthTimeout          = 3 * time.Second
	defaultLauncherOnlineReportInterval = 30 * time.Second
	defaultPVFPath                      = "data/dnf/Script.pvf"
	defaultPartyUDPRelayPortStart       = 30000
	// defaultPartyUDPRelayPortCount 是队伍 UDP 中继的端口池大小。
	//
	// 一个四人队需要 4×3 = 12 条有向腿（每对成员两个方向各占一个端口），所以池子
	// 直接决定"同时能有多少个队伍拿到中继"：64 ≈ 5 个四人队，512 ≈ 42 个。
	// 拿不到中继的队伍会回退成"直连打洞"，两边都在 NAT 后面时就连不上 —— 玩家看到
	// 的就是红电脑；而旧值 64 太小的直接证据是线上实测已占用 40/64（≈3 个四人队）。
	// 参照实现的池子是 1000（队伍侧约 500 ≈ 40 个并发四人队），这里对齐到同一量级。
	// 注意：线上 instance.json 若显式写了 partyUdpRelayPortCount，需要一并改大。
	defaultPartyUDPRelayPortCount = 512
)

type options struct {
	channelListen       string
	gameListenHost      string
	channelInfoFile     string
	channelServerID     int
	channelAdvertiseID  int
	channelInfoBodyMode string
	// channelTableStrict 恢复"频道表必须含默认城镇频道 11 / 引导频道 19"的旧硬校验。
	//
	// 默认关闭：运维会主动只开少数频道（例如只留一个特殊区域频道），此时
	//   - 缺少引导频道 19 不该让服务端起不来（Build90CNOnlineScript 的化石校验，见其注释）；
	//   - 缺少默认城镇频道 11 不该让热重载被拒（原来会让运维工具"关不掉"）。
	// 关掉后这两条降级为启动/热重载日志里的告警；"表里一个频道都没有""一个游戏端口都没有"
	// 这类真正致命的状态仍然是硬错误。
	channelTableStrict       bool
	serverIP                 string
	partyUDPBindIP           string
	scriptVersion            string
	gameOuterToken           uint32
	gameInitialMode          string
	gamePreBootstrap         string
	gamePostBootstrap        string
	gameUpperHeader          string
	gameUpperBodyCodec       string
	gameUpperClientBodyCodec string
	gameDprotoMode           string
	// gameS2CCipher 控制 S2C 回包二次加密（off / shadow / on，默认 off）。
	// gameS2CMasterKeyBase64 覆盖固化主密钥，仅用于本地联调与灰度。
	gameS2CCipher          string
	gameS2CMasterKeyBase64 string
	initialUDPPort1        int
	initialUDPPort2        int
	commandCount           int
	notificationCount      int
	maxPacketBytes         int
	packetLogEnabled       bool
	packetLogPath          string
	// recordDir 三类审计记录（点券消费/对话/副本）的落盘目录。
	// 默认取 packet_log 的同级 records/，让"日志放哪、记录就放哪"；
	// DNFBRIDGE_RECORD_DIR 可覆盖。
	recordDir               string
	accountID               string
	accountPrefix           string
	pvfPath                 string
	pvfMaxBytes             int64
	partyUDPRelayEnabled    bool
	partyUDPRelayPortStart  int
	partyUDPRelayPortCount  int
	raidCreatedRecoverySeed string
	regionID                int
	// dungeonRoomPartySlotOff drops the dungeon-room op9 party slot table that
	// arms the current client's host arbitration (sceneActorManager+0x48/+0x68).
	// Off by default; it exists for attribution runs, at the cost of every party
	// member answering "not host" and nobody simulating the room.
	dungeonRoomPartySlotOff bool
	// raidLukeWeeklyLimitEnforced turns Luke's [WEEKLY ENTER COUNT] from an
	// observation into a refusal. OFF by default: the reset weekday the document
	// does not declare is still unconfirmed, and enforcing a wrongly guessed node
	// locks players out of a raid they are entitled to enter. Turn it on only
	// after the node has been read off the observation lines in
	// raid_luke_weekly_observation.go. Turning it on requires a durable weekly
	// counter to exist first -- see that file's header.
	raidLukeWeeklyLimitEnforced bool
	// launcherAuth* gate external game connections through the loopback
	// launcher verifier before any DNF protocol flow starts. All four options
	// default to disabled/empty so local PID / WireGuard / Wine fast paths are
	// untouched unless an operator explicitly enables the external mode.
	launcherAuthEnabled       bool
	launcherAuthVerifyURL     string
	launcherAuthInternalToken string
	launcherAuthTimeout       time.Duration
	// launcherIdentity* bind every game connection to the deployment's own
	// launcher. launcherIdentityMode is off/audit/enforce; the keys are the
	// pinned ECDSA P-256 public keys (PEM or base64 DER). An operator who has
	// not run keygen gets mode "off", which leaves every legacy path intact.
	launcherIdentityMode           string
	launcherIdentityPublicKeys     []string
	launcherIdentityAllowWireGuard bool
	// launcherOnlineReport* push the real online account count to the ops
	// tool so its /launcher/v1/health can show/limit capacity. Enabled by
	// default; harmless on the same host as the ops tool.
	launcherOnlineReportEnabled  bool
	launcherOnlineReportInterval time.Duration
}

func optionsFromConfig(cfg config.ServiceConfig) options {
	opts := options{
		channelListen:   envString("DNFBRIDGE_CHANNEL_LISTEN", defaultChannelListen),
		gameListenHost:  strings.TrimSpace(os.Getenv("DNFBRIDGE_GAME_LISTEN_HOST")),
		channelInfoFile: firstEnv([]string{"DNFBRIDGE_CHANNEL_INFO_PATH", "DNF_CHANNEL_INFO_FILE"}, defaultChannelInfoFile),
		channelServerID: envIntAny([]string{"DNFBRIDGE_CHANNEL_SERVER_INDEX", "DNF_CHANNEL_SOURCE_SERVER_INDEX"}, 1),
		channelAdvertiseID: envIntAny(
			[]string{"DNFBRIDGE_CHANNEL_ADVERTISE_SERVER_INDEX", "DFO_CHANNEL_SERVER_INDEX"},
			-1,
		),
		channelInfoBodyMode: normalizeChannelInfoBodyMode(envString(
			"DNFBRIDGE_CHANNEL_INFO_BODY_MODE",
			channelInfoBodyLatest,
		)),
		channelTableStrict: envBool("DNFBRIDGE_CHANNEL_TABLE_STRICT", false),
		serverIP:           chooseServerIP(cfg),
		partyUDPBindIP:     choosePartyUDPBindIP(cfg),
		scriptVersion:      envString("DNF_SCRIPT_VERSION", defaultScriptVersion),
		gameOuterToken:     envUint32Any([]string{"DNFBRIDGE_GAME_OUTER_TOKEN", "DFO_GAME_OUTER_TOKEN"}, 0),
		gameInitialMode:    normalizeGameInitialMode(envString("DNFBRIDGE_GAME_INITIAL_MODE", gameInitialModeNotice)),
		gamePreBootstrap:   normalizeGamePreBootstrap(envString("DNFBRIDGE_GAME_PRE_BOOTSTRAP", gamePreBootstrapNone)),
		gamePostBootstrap:  normalizeGamePostBootstrap(envString("DNFBRIDGE_GAME_POST_BOOTSTRAP", gamePostBootstrapNone)),
		gameUpperHeader:    normalizeGameUpperHeader(envString("DNFBRIDGE_GAME_UPPER_HEADER", gameUpperHeaderChannel13)),
		gameUpperBodyCodec: normalizeGameUpperBodyCodec(envString(
			"DNFBRIDGE_GAME_UPPER_BODY_CODEC",
			gameUpperBodyCodecAuto,
		)),
		gameUpperClientBodyCodec: normalizeGameUpperClientBodyCodec(envString(
			"DNFBRIDGE_GAME_UPPER_CLIENT_BODY_CODEC",
			gameUpperClientBodyCodecPlain,
		)),
		gameDprotoMode:              normalizeGameDprotoMode(envString("DNFBRIDGE_DPROTO_MODE", gameDprotoModeLegacy)),
		gameS2CCipher:               normalizeGameS2CCipher(envString("DNFBRIDGE_GAME_S2C_CIPHER", gameS2CCipherOff)),
		gameS2CMasterKeyBase64:      strings.TrimSpace(os.Getenv("DNFBRIDGE_GAME_S2C_KEY_BASE64")),
		initialUDPPort1:             envInt("DNF_INITIAL_UDP_PORT1", defaultInitialUDPPort1),
		initialUDPPort2:             envInt("DNF_INITIAL_UDP_PORT2", defaultInitialUDPPort2),
		commandCount:                envInt("DNF_COMMAND_PACKET_COUNT", defaultCommandCount),
		notificationCount:           envInt("DNF_NOTIFICATION_PACKET_COUNT", defaultNotificationCount),
		maxPacketBytes:              envInt("DNF_MAX_PACKET_BYTES", defaultMaxPacketBytes),
		packetLogEnabled:            envBool("DNFBRIDGE_PACKET_LOG", false),
		packetLogPath:               envString("DNFBRIDGE_PACKET_LOG_PATH", "logs/packet_log.txt"),
		recordDir:                   envString("DNFBRIDGE_RECORD_DIR", defaultRecordDirFor(envString("DNFBRIDGE_PACKET_LOG_PATH", "logs/packet_log.txt"))),
		accountID:                   envString("DNFBRIDGE_ACCOUNT_ID", ""),
		accountPrefix:               envString("DNFBRIDGE_ACCOUNT_PREFIX", defaultAccountPrefix),
		pvfPath:                     choosePVFPath(cfg),
		pvfMaxBytes:                 choosePVFMaxBytes(cfg),
		partyUDPRelayEnabled:        envBool("DNFBRIDGE_PARTY_UDP_RELAY_ENABLED", false),
		partyUDPRelayPortStart:      envInt("DNFBRIDGE_PARTY_UDP_RELAY_PORT_START", defaultPartyUDPRelayPortStart),
		partyUDPRelayPortCount:      envInt("DNFBRIDGE_PARTY_UDP_RELAY_PORT_COUNT", defaultPartyUDPRelayPortCount),
		dungeonRoomPartySlotOff:     envBool("DNFBRIDGE_DUNGEON_ROOM_PARTY_SLOT_OFF", false),
		raidLukeWeeklyLimitEnforced: envBool("DNFBRIDGE_RAID_LUKE_WEEKLY_LIMIT_ENFORCED", false),
		raidCreatedRecoverySeed: strings.TrimSpace(os.Getenv(
			"DNFBRIDGE_RAID_CREATED_RECOVERY_SEED_BASE64",
		)),
		regionID: envInt("DNFBRIDGE_REGION_ID", 1),
		launcherAuthEnabled: envBool(
			"DNFBRIDGE_LAUNCHER_AUTH_ENABLED",
			false,
		),
		launcherAuthVerifyURL: envString(
			"DNFBRIDGE_LAUNCHER_AUTH_VERIFY_URL",
			defaultLauncherAuthVerifyURL,
		),
		launcherAuthInternalToken: envString(
			"DNFBRIDGE_LAUNCHER_AUTH_INTERNAL_TOKEN",
			"",
		),
		launcherAuthTimeout: envDuration(
			"DNFBRIDGE_LAUNCHER_AUTH_TIMEOUT",
			defaultLauncherAuthTimeout,
		),
		launcherIdentityMode: envString(
			"DNFBRIDGE_LAUNCHER_LID_MODE",
			LauncherIdentityModeOff,
		),
		launcherIdentityPublicKeys: envStringList(
			"DNFBRIDGE_LAUNCHER_LID_PUBLIC_KEYS",
		),
		launcherIdentityAllowWireGuard: envBool(
			"DNFBRIDGE_LAUNCHER_LID_ALLOW_WIREGUARD",
			false,
		),
		launcherOnlineReportEnabled: envBool(
			"DNFBRIDGE_LAUNCHER_ONLINE_REPORT_ENABLED",
			true,
		),
		launcherOnlineReportInterval: envDuration(
			"DNFBRIDGE_LAUNCHER_ONLINE_REPORT_INTERVAL",
			defaultLauncherOnlineReportInterval,
		),
	}
	if opts.maxPacketBytes <= 0 {
		opts.maxPacketBytes = defaultMaxPacketBytes
	}
	if opts.channelServerID <= 0 {
		opts.channelServerID = 1
	}
	if opts.channelAdvertiseID < 0 {
		opts.channelAdvertiseID = opts.channelServerID
	}
	// Current NoPack's class-0/op273 friend-row reader rejects region values
	// >= 250. Zero is also kept out of operator configuration because it is a
	// client-reserved resource-table entry rather than a normal region.
	if opts.regionID < 1 || opts.regionID >= 250 {
		opts.regionID = 1
	}
	if opts.partyUDPRelayPortStart < 1 || opts.partyUDPRelayPortStart > 65535 {
		opts.partyUDPRelayPortStart = defaultPartyUDPRelayPortStart
	}
	if opts.partyUDPRelayPortCount < 1 ||
		opts.partyUDPRelayPortCount > 1024 ||
		opts.partyUDPRelayPortStart+opts.partyUDPRelayPortCount-1 > 65535 {
		opts.partyUDPRelayPortCount = defaultPartyUDPRelayPortCount
	}
	return opts
}

func choosePVFPath(cfg config.ServiceConfig) string {
	for _, key := range []string{"DNFBRIDGE_PVF_PATH", "DNF_SCRIPT_PVF_PATH", "DNF_PVF_PATH"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if value := strings.TrimSpace(cfg.PVF.Path); value != "" {
		return value
	}
	return defaultPVFPath
}

func choosePVFMaxBytes(cfg config.ServiceConfig) int64 {
	for _, key := range []string{"DNFBRIDGE_PVF_MAX_BYTES", "DNF_SCRIPT_PVF_MAX_BYTES", "DNF_PVF_MAX_BYTES"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err == nil && (parsed > 0 || parsed == platformpvf.NoMaxBytes) {
				return parsed
			}
		}
	}
	if cfg.PVF.MaxBytes > 0 || cfg.PVF.MaxBytes == platformpvf.NoMaxBytes {
		return cfg.PVF.MaxBytes
	}
	return platformpvf.DefaultMaxBytes
}

func normalizeChannelInfoBodyMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", channelInfoBodyLatest, "csharp", "legacy", "legacy_csharp", "old_csharp":
		// IDA/MCP 与 86JP_protocol.log 复核后，最新客户端 0x12 明文必须带 server_count/server_id/count。
		// 旧 C# 的 2 字节保留位前缀会导致客户端把 0x000B0000 当 server_count 反复解析。
		return channelInfoBodyLatest
	default:
		return channelInfoBodyLatest
	}
}

func normalizeGameInitialMode(value string) string {
	// DOVE/C# 对齐：首包模式不再可配置，game 建连后固定主动发送 class=0 op=1。
	return gameInitialModeNotice
}

func normalizeGamePreBootstrap(value string) string {
	// C# 端 game 连接后只主动发首包；其余初始化包都由客户端 C2S 触发。
	// 旧探针开关保留为兼容配置名，但正常链路统一关闭，避免提前推送请求触发包。
	return gamePreBootstrapNone
}

func normalizeGamePostBootstrap(value string) string {
	// C# 行为证据：GET_USERINFO/SELECT_CHARACTER 后续包必须等客户端请求后再回。
	// 因此 post bootstrap 探针不再通过环境变量开启。
	return gamePostBootstrapNone
}

func normalizeGameUpperHeader(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", gameUpperHeaderChannel13, "13", "channel", "raw13":
		return gameUpperHeaderChannel13
	case gameUpperHeaderServer16, "16", "raw16":
		return gameUpperHeaderServer16
	default:
		return gameUpperHeaderChannel13
	}
}

func normalizeGameUpperBodyCodec(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", gameUpperBodyCodecAuto, "codec", "encoded", "latest":
		return gameUpperBodyCodecAuto
	case gameUpperBodyCodecPlain, "plain", "none", "off", "decodecopy":
		return gameUpperBodyCodecPlain
	default:
		return gameUpperBodyCodecAuto
	}
}

func normalizeGameUpperClientBodyCodec(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", gameUpperClientBodyCodecPlain, "plain", "none", "off", "disabled", "passthrough":
		return gameUpperClientBodyCodecPlain
	case gameUpperClientBodyCodecProbe, "detect", "log", "log_only", "log-only":
		return gameUpperClientBodyCodecProbe
	case gameUpperClientBodyCodecNative, "decode", "decoded", "server", "server_native":
		return gameUpperClientBodyCodecNative
	default:
		return gameUpperClientBodyCodecPlain
	}
}

func normalizeGameDprotoMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case gameDprotoModeNative, "native_dproto", "protected":
		return gameDprotoModeNative
	case "", gameDprotoModeLegacy, "legacy", "patch", "compat":
		return gameDprotoModeLegacy
	default:
		return gameDprotoModeLegacy
	}
}

// normalizeGameS2CCipher 归一化 S2C 回包加密模式。默认 off：
// 任何拼写错误都不许悄悄打开加密——那会让客户端收到乱码并表现为"卡住"。
func normalizeGameS2CCipher(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case gameS2CCipherOn, "enable", "enabled", "true", "1":
		return gameS2CCipherOn
	case gameS2CCipherShadow, "observe", "dry_run", "dry-run":
		return gameS2CCipherShadow
	case "", gameS2CCipherOff, "disable", "disabled", "false", "0":
		return gameS2CCipherOff
	default:
		return gameS2CCipherOff
	}
}

func chooseServerIP(cfg config.ServiceConfig) string {
	for _, key := range []string{"DNFBRIDGE_SERVER_IP", "DNFBRIDGE_PUBLIC_HOST", "DFO_PUBLIC_SERVER_IP", "SERVER_IP"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if host := hostFromAddr(cfg.Service.PublicAddr); host != "" {
		return host
	}
	return defaultServerIP
}

// choosePartyUDPBindIP selects the local address the party UDP reachability
// probe binds. The legacy (pre-merge) behaviour binds 0.0.0.0 so the probe
// receives datagrams on every interface. Public-cloud NAT rewrites inbound
// datagrams to the private interface address, so a listener pinned to the
// advertised public IP never sees them; binding a single interface IP is only
// useful when the operator wants to restrict the probe to one NIC. Set
// DNFBRIDGE_UDP_BIND_IP=<ip> to override the wildcard default.
func choosePartyUDPBindIP(cfg config.ServiceConfig) string {
	if value := strings.TrimSpace(os.Getenv("DNFBRIDGE_UDP_BIND_IP")); value != "" {
		return value
	}
	return "0.0.0.0"
}

func hostFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(addr, "[]")
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func firstEnv(keys []string, fallback string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return fallback
}

// envStringList reads a comma or semicolon separated list. Pinned launcher
// identity keys travel this way because each one is a single-line base64 blob
// and an operator may pin more than one launcher build at a time.
func envStringList(key string) []string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envIntAny(keys []string, fallback int) int {
	for _, key := range keys {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			continue
		}
		return envInt(key, fallback)
	}
	return fallback
}

func envUint32(key string, fallback uint32) uint32 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		parsed, err = parseFlexibleUint32(value)
	}
	if err != nil {
		return fallback
	}
	return uint32(parsed)
}

func envUint32Any(keys []string, fallback uint32) uint32 {
	for _, key := range keys {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			continue
		}
		return envUint32(key, fallback)
	}
	return fallback
}

func parseFlexibleUint32(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	trimmed := strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if len(trimmed) > 8 {
		// 启动参数里的 32 位 hex token 不是 uint32；调试时传入该值时只取前 4 字节作为 outer 校验候选。
		trimmed = trimmed[:8]
	}
	return strconv.ParseUint(trimmed, 16, 32)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
