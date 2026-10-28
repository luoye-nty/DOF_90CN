package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"longheng.io/server/internal/platform/wineenv"
)

const (
	autoDetectAdvertiseIP         = "AUTO_DETECT"
	defaultAccountIDPrefix        = "dnf:"
	defaultPartyUDPRelayPortStart = 30000
	defaultPartyUDPRelayPortCount = 512
)

var (
	safeValuePattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	sqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	digitsPattern        = regexp.MustCompile(`^[0-9]+$`)
	hexTokenPattern      = regexp.MustCompile(`^[0-9A-Fa-f]{8,32}$`)
)

func loadInstance(paths projectPaths) (instanceConfig, error) {
	if err := paths.ensureDirectories(); err != nil {
		return instanceConfig{}, err
	}
	if !isRegularFile(paths.instance) {
		data, err := os.ReadFile(paths.instanceExample)
		if err != nil {
			return instanceConfig{}, fmt.Errorf("read instance template: %w", err)
		}
		if err := writeFile(paths.instance, data, 0o600); err != nil {
			return instanceConfig{}, err
		}
		fmt.Println("Created runtime config:", paths.instance)
	}

	cfg, droppedLegacy, err := decodeInstanceMigratingLegacyFields(paths.instance)
	if err != nil {
		return instanceConfig{}, err
	}
	changed := false
	if len(droppedLegacy) > 0 {
		fmt.Println(
			"Removing instance.json settings this release does not implement:",
			strings.Join(droppedLegacy, ", "),
		)
		changed = true
	}
	if cfg.InstallationID == "" || cfg.InstallationID == "AUTO_GENERATE" {
		installationID, err := randomCredential("inst_")
		if err != nil {
			return instanceConfig{}, err
		}
		cfg.InstallationID = installationID
		changed = true
	}
	if cfg.Server.AdminToken == "AUTO_GENERATE" {
		token, err := randomCredential("adm_")
		if err != nil {
			return instanceConfig{}, err
		}
		cfg.Server.AdminToken = token
		changed = true
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Server.AccountID), "AUTO_GENERATE") {
		accountID, err := randomCredential(defaultAccountIDPrefix)
		if err != nil {
			return instanceConfig{}, err
		}
		cfg.Server.AccountID = accountID
		changed = true
	}
	if cfg.Database.Password == "AUTO_GENERATE" {
		password, err := randomCredential("db_")
		if err != nil {
			return instanceConfig{}, err
		}
		cfg.Database.Password = password
		changed = true
	}
	if cfg.Database.Mode == "docker" {
		cfg.Database.Mode = "portable"
		changed = true
	}
	if cfg.Database.Mode == "sqlite" && strings.TrimSpace(cfg.Database.Path) == "" {
		cfg.Database.Path = "data/dnf90.db"
		changed = true
	}
	if len(cfg.LegacyRedis) != 0 {
		cfg.LegacyRedis = nil
		changed = true
	}
	if cfg.Server.RobotEnabled == nil {
		enabled := false
		cfg.Server.RobotEnabled = &enabled
		changed = true
	}
	if cfg.Server.PartyUDPRelayPortStart == 0 {
		cfg.Server.PartyUDPRelayPortStart = defaultPartyUDPRelayPortStart
		changed = true
	}
	if cfg.Server.PartyUDPRelayPortCount == 0 {
		cfg.Server.PartyUDPRelayPortCount = defaultPartyUDPRelayPortCount
		changed = true
	}
	// Preserve the pre-switch behavior for existing installations. A pointer is
	// used in the JSON model so an omitted legacy field is distinct from the
	// operator explicitly choosing false.
	if cfg.Game.DungeonBatchPickupEnabled == nil {
		enabled := true
		cfg.Game.DungeonBatchPickupEnabled = &enabled
		changed = true
	}
	if cfg.Protocol.RegionID == 0 {
		// Preserve the stock current-client mapping (1 = 骞夸笢1) for existing
		// installations. Operators can select any normal region id from 1..249
		// and optionally override its client-side display name.
		cfg.Protocol.RegionID = 1
		changed = true
	}
	if cfg.Protocol.ChannelAdvertiseServerIndex == 0 {
		// The current client's guild-create region selector is populated from
		// the channel-directory server-group ids and only enumerates 1..249.
		// Legacy server group 0 therefore produced an empty selector. Keep the
		// advertised group and all social region packets on one identity.
		cfg.Protocol.ChannelAdvertiseServerIndex = cfg.Protocol.RegionID
		changed = true
	}
	resolved := cfg
	configuredAdvertiseIP := strings.TrimSpace(cfg.Server.AdvertiseIP)
	autoDetect := strings.EqualFold(configuredAdvertiseIP, autoDetectAdvertiseIP)
	if autoDetect {
		// An explicit 127.0.0.1 is a deliberate operator/hub choice and is left
		// alone; only AUTO_DETECT reaches the detector.
		advertiseIP, loopbackFallback, err := detectPrivateLANIPv4()
		if err != nil {
			return instanceConfig{}, fmt.Errorf(
				"auto-detect server.advertiseIp: %w; set it explicitly to an IPv4 assigned to this computer",
				err,
			)
		}
		resolved.Server.AdvertiseIP = advertiseIP
		if loopbackFallback {
			fmt.Println(
				"No private LAN IPv4 was found on this computer; advertising the 90cn game channels on",
				advertiseIP+".",
			)
			fmt.Println(
				"Only this computer can reach the server in that state: other computers on the LAN and",
				"party P2P cannot connect until a private LAN adapter is available. Connect a LAN or",
				"Wi-Fi adapter and run start again, or set server.advertiseIp explicitly.",
			)
		} else {
			fmt.Println("Using local LAN IPv4 for 90cn game channels:", advertiseIP)
		}
	} else {
		resolved.Server.AdvertiseIP = configuredAdvertiseIP
		if cfg.Server.AdvertiseIP != configuredAdvertiseIP {
			cfg.Server.AdvertiseIP = configuredAdvertiseIP
			changed = true
		}
	}
	if err := validateInstance(resolved); err != nil {
		return instanceConfig{}, err
	}
	if changed {
		data, err := marshalInstance(cfg)
		if err != nil {
			return instanceConfig{}, err
		}
		if err := writeFile(paths.instance, data, 0o600); err != nil {
			return instanceConfig{}, err
		}
		fmt.Println("Generated private local credentials in runtime/config/instance.json")
	}
	return resolved, nil
}

// legacyInstanceFields are instance.json keys earlier one-click trees wrote for
// features this release does not implement. They are dropped on load with an
// explicit note instead of being rejected by the strict decoder: runtime/config/
// is outside the launcher's update whitelist, so an installation carrying one
// could never be repaired by an update, only by hand-deleting instance.json.
// Every other unknown field still fails decoding, so a typo in a hand-edited
// file is never silently ignored.
var legacyInstanceFields = []struct {
	section string
	keys    []string
}{
	{section: "server", keys: []string{"partyUdpRelay"}},
	{section: "game", keys: []string{
		"enabledChannelIds",
		"mainQuestContinuityExperience",
		"deleteExpiredItems",
		"pvfItemExpirationEnabled",
	}},
}

// dropLegacyInstanceFields removes exactly the known legacy keys and reports
// which ones were present, as "section.key". Input it cannot parse is returned
// untouched so the strict decoder reports the real syntax problem.
func dropLegacyInstanceFields(data []byte) ([]byte, []string) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return data, nil
	}
	var dropped []string
	for _, legacy := range legacyInstanceFields {
		raw, found := document[legacy.section]
		if !found {
			continue
		}
		var section map[string]json.RawMessage
		if err := json.Unmarshal(raw, &section); err != nil {
			continue
		}
		sectionChanged := false
		for _, key := range legacy.keys {
			if _, present := section[key]; !present {
				continue
			}
			delete(section, key)
			dropped = append(dropped, legacy.section+"."+key)
			sectionChanged = true
		}
		if !sectionChanged {
			continue
		}
		encoded, err := json.Marshal(section)
		if err != nil {
			return data, nil
		}
		document[legacy.section] = encoded
	}
	if len(dropped) == 0 {
		return data, nil
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return data, nil
	}
	return encoded, dropped
}

func decodeInstance(path string) (instanceConfig, error) {
	cfg, _, err := decodeInstanceMigratingLegacyFields(path)
	return cfg, err
}

func decodeInstanceMigratingLegacyFields(path string) (instanceConfig, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return instanceConfig{}, nil, fmt.Errorf("read instance %s: %w", path, err)
	}
	data, dropped := dropLegacyInstanceFields(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg instanceConfig
	if err := decoder.Decode(&cfg); err != nil {
		return instanceConfig{}, nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return instanceConfig{}, nil, fmt.Errorf("invalid JSON in %s: multiple values", path)
		}
		return instanceConfig{}, nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	return cfg, dropped, nil
}

func randomCredential(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate private credential: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func marshalInstance(cfg instanceConfig) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode instance configuration: %w", err)
	}
	return append(data, '\n'), nil
}

func validateInstance(cfg instanceConfig) error {
	if cfg.SchemaVersion != 1 {
		return errorsf("instance.json schemaVersion must be 1")
	}
	if err := validateSafeValue("installationId", cfg.InstallationID); err != nil {
		return err
	}
	if cfg.Mode != "local-single-account" {
		return errorsf("this release supports only mode=local-single-account")
	}
	ip := net.ParseIP(strings.TrimSpace(cfg.Server.AdvertiseIP))
	if ip == nil || ip.To4() == nil {
		return errorsf("server.advertiseIp must be an IPv4 address")
	}
	// A private LAN IPv4 is the required shape: party P2P and any second
	// computer depend on it. 127.0.0.1 is the single documented exception for a
	// host with no LAN adapter at all, where the local client is the only peer.
	if ip.IsLoopback() {
		if !ip.Equal(net.IPv4(127, 0, 0, 1)) {
			return errorsf("server.advertiseIp loopback must be 127.0.0.1")
		}
	} else if !ip.IsPrivate() {
		return errorsf("90cn game channels require server.advertiseIp to be a private LAN IPv4 address or 127.0.0.1, not a public address")
	}
	if cfg.Server.PartyUDPRelayEnabled {
		relayPortStart := cfg.Server.PartyUDPRelayPortStart
		if relayPortStart == 0 {
			relayPortStart = defaultPartyUDPRelayPortStart
		}
		relayPortCount := cfg.Server.PartyUDPRelayPortCount
		if relayPortCount == 0 {
			relayPortCount = defaultPartyUDPRelayPortCount
		}
		if relayPortStart < 1 || relayPortStart > 65535 {
			return errorsf("server.partyUdpRelayPortStart must be between 1 and 65535")
		}
		if relayPortCount < 12 || relayPortCount > 1024 ||
			relayPortStart+relayPortCount-1 > 65535 {
			return errorsf("server.partyUdpRelayPortCount must reserve 12-1024 UDP ports within the valid port range")
		}
	}
	if err := validateListenAddress("server.channelListen", cfg.Server.ChannelListen, false); err != nil {
		return err
	}
	if err := validateLoopbackListenAddress("server.channelListen", cfg.Server.ChannelListen); err != nil {
		return err
	}
	if err := validateListenAddress("server.adminListen", cfg.Server.AdminListen, false); err != nil {
		return err
	}
	if err := validateLoopbackListenAddress("server.adminListen", cfg.Server.AdminListen); err != nil {
		return err
	}
	if err := validateSafeValue("server.adminToken", cfg.Server.AdminToken); err != nil {
		return err
	}
	accountID := strings.TrimSpace(cfg.Server.AccountID)
	if accountID == "" {
		return errorsf("server.accountId is required")
	}
	if accountID == "AUTO_GENERATE" ||
		len(accountID) > 128 ||
		!safeValuePattern.MatchString(accountID) {
		return errorsf("server.accountId must be a safe account id with at most 128 bytes")
	}

	switch cfg.Database.Mode {
	case "sqlite":
		path := strings.TrimSpace(cfg.Database.Path)
		if path == "" || filepath.IsAbs(path) || escapesRoot(path) {
			return errorsf("database.path must be a non-empty path inside runtime")
		}
	case "portable", "mysql":
		if strings.TrimSpace(cfg.Database.Host) == "" {
			return errorsf("database.host is required")
		}
		if cfg.Database.Port < 1 || cfg.Database.Port > 65535 {
			return errorsf("database.port must be between 1 and 65535")
		}
		if !sqlIdentifierPattern.MatchString(cfg.Database.Name) {
			return errorsf("database.name must be a safe SQL identifier")
		}
		if err := validateSafeValue("database.user", cfg.Database.User); err != nil {
			return err
		}
		if cfg.Database.User != "root" {
			return errorsf("database.user must be root, as required by this local profile")
		}
		if err := validateSafeValue("database.password", cfg.Database.Password); err != nil {
			return err
		}
		if cfg.Database.Host != "127.0.0.1" && cfg.Database.Host != "localhost" {
			return errorsf("local-single-account requires database.host=127.0.0.1 or localhost")
		}
	default:
		return errorsf("database.mode must be sqlite, mysql, or portable")
	}
	if !digitsPattern.MatchString(cfg.Game.ShardID) {
		return errorsf("game.shardId must contain only digits")
	}
	if cfg.Game.PVFMaxBytes == 0 || cfg.Game.PVFMaxBytes < -1 {
		return errorsf("game.pvfMaxBytes must be positive or -1 to disable the size limit")
	}
	for _, value := range []string{cfg.Game.PVFPath, cfg.Game.ChannelInfoPath} {
		if strings.TrimSpace(value) == "" || filepath.IsAbs(value) {
			return errorsf("game asset paths must be non-empty paths relative to runtime")
		}
		if escapesRoot(value) {
			return errorsf("game asset paths must stay inside runtime")
		}
	}
	if filepath.ToSlash(cfg.Game.PVFPath) != "data/dnf/Script.pvf" ||
		filepath.ToSlash(cfg.Game.ChannelInfoPath) != "data/dnf/channel_info.etc" {
		return errorsf("the 90cn profile locks game asset paths to data/dnf/Script.pvf and data/dnf/channel_info.etc")
	}
	if cfg.Protocol.Profile != "90cn-decode-bypass-v1" {
		return errorsf("protocol.profile must be 90cn-decode-bypass-v1")
	}
	if cfg.Protocol.GameUpperHeader != "server16" {
		return errorsf("protocol.gameUpperHeader must be server16 for this client profile")
	}
	if cfg.Protocol.GameUpperBodyCodec != "plaintext" ||
		cfg.Protocol.GameUpperClientBodyCodec != "plaintext" {
		return errorsf("protocol body codecs must both be plaintext for this client profile")
	}
	if !hexTokenPattern.MatchString(cfg.Protocol.GameOuterToken) ||
		!strings.EqualFold(cfg.Protocol.GameOuterToken, "de509f65e9ccaae621cb7278fc2b8e6c") {
		return errorsf("protocol.gameOuterToken does not match the 90cn compatibility profile")
	}
	if cfg.Protocol.ChannelServerIndex != 1 {
		return errorsf("protocol.channelServerIndex must remain 1 for the current client profile")
	}
	if cfg.Protocol.RegionID < 1 || cfg.Protocol.RegionID >= 250 {
		return errorsf("protocol.regionId must be between 1 and 249 for the current client")
	}
	if cfg.Protocol.ChannelAdvertiseServerIndex != cfg.Protocol.RegionID {
		return errorsf("protocol.channelAdvertiseServerIndex must match protocol.regionId so guild and social region identities stay aligned")
	}
	regionName := strings.TrimSpace(cfg.Protocol.RegionName)
	if utf8.RuneCountInString(regionName) > 24 {
		return errorsf("protocol.regionName must contain at most 24 characters")
	}
	for _, character := range regionName {
		if unicode.IsControl(character) {
			return errorsf("protocol.regionName must not contain control characters")
		}
	}
	if cfg.Client.InitialGamePort != 0 {
		return errorsf("client.initialGamePort must be 0 for the 90cn dynamic channel profile")
	}
	if !cfg.Client.HookCreate {
		return errorsf("client.hookCreate must be true for this client profile")
	}
	if strings.TrimSpace(cfg.Build.GoExecutable) == "" {
		return errorsf("build.goExecutable is required")
	}
	return nil
}

type lanIPv4Candidate struct {
	Address        string
	InterfaceName  string
	InterfaceIndex int
}

// loopbackAdvertiseIP is the sole documented exception to the private-LAN rule.
// A host with no outbound route at all (fully offline, or carrying only virtual
// host-only adapters) still has to be playable by the client running on it.
const loopbackAdvertiseIP = "127.0.0.1"

// detectPrivateLANIPv4 resolves AUTO_DETECT. It returns the chosen address and
// whether the loopback fallback had to be used; callers must surface that
// second return value to the operator, because loopback silently rules out
// LAN play and party P2P.
func detectPrivateLANIPv4() (string, bool, error) {
	preferred := preferredOutboundIPv4()
	interfaces, err := net.Interfaces()
	if err != nil {
		if wineenv.Detected() && isPrivateLANIPv4(preferred) {
			return preferred.To4().String(), false, nil
		}
		return "", false, fmt.Errorf("enumerate network interfaces: %w", err)
	}
	candidates := make([]lanIPv4Candidate, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 ||
			networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			default:
				host, _, splitErr := net.SplitHostPort(address.String())
				if splitErr == nil {
					ip = net.ParseIP(host)
				}
			}
			if !isPrivateLANIPv4(ip) {
				continue
			}
			candidates = append(candidates, lanIPv4Candidate{
				Address:        ip.To4().String(),
				InterfaceName:  networkInterface.Name,
				InterfaceIndex: networkInterface.Index,
			})
		}
	}
	candidates = appendWineRouteCandidate(candidates, preferred, wineenv.Detected())
	address, loopbackFallback := chooseAdvertiseIPv4(candidates, preferred)
	return address, loopbackFallback, nil
}

// chooseAdvertiseIPv4 keeps the private LAN IPv4 as the strongly preferred
// answer and only degrades to loopback when there is not a single usable
// candidate, so a machine that is merely offline still starts instead of
// failing hard.
func chooseAdvertiseIPv4(candidates []lanIPv4Candidate, preferred net.IP) (string, bool) {
	address, err := choosePrivateLANIPv4(candidates, preferred)
	if err != nil {
		return loopbackAdvertiseIP, true
	}
	return address, false
}

func appendWineRouteCandidate(
	candidates []lanIPv4Candidate,
	preferred net.IP,
	underWine bool,
) []lanIPv4Candidate {
	if !underWine || !isPrivateLANIPv4(preferred) {
		return candidates
	}
	// Wine's GetAdaptersAddresses view can omit the Unix host interface even
	// though a socket route lookup returns a bindable private source address.
	// Feed that source address through the normal private-IP selector instead
	// of weakening validation or falling back to wildcard/loopback binding.
	return append(candidates, lanIPv4Candidate{
		Address:        preferred.To4().String(),
		InterfaceName:  "Wine default route",
		InterfaceIndex: int(^uint(0) >> 1),
	})
}

func preferredOutboundIPv4() net.IP {
	// UDP connect performs only a local route lookup; it sends no application data.
	connection, err := net.DialUDP(
		"udp4",
		nil,
		&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9},
	)
	if err != nil {
		return nil
	}
	defer connection.Close()
	local, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return local.IP.To4()
}

func choosePrivateLANIPv4(candidates []lanIPv4Candidate, preferred net.IP) (string, error) {
	unique := make(map[string]lanIPv4Candidate, len(candidates))
	for _, candidate := range candidates {
		ip := net.ParseIP(strings.TrimSpace(candidate.Address))
		if !isPrivateLANIPv4(ip) {
			continue
		}
		candidate.Address = ip.To4().String()
		existing, found := unique[candidate.Address]
		if !found || candidate.InterfaceIndex < existing.InterfaceIndex {
			unique[candidate.Address] = candidate
		}
	}
	if preferred = preferred.To4(); isPrivateLANIPv4(preferred) {
		if candidate, found := unique[preferred.String()]; found &&
			!likelyVirtualInterface(candidate.InterfaceName) {
			return preferred.String(), nil
		}
	}
	valid := make([]lanIPv4Candidate, 0, len(unique))
	for _, candidate := range unique {
		valid = append(valid, candidate)
	}
	sort.Slice(valid, func(left, right int) bool {
		leftVirtual := likelyVirtualInterface(valid[left].InterfaceName)
		rightVirtual := likelyVirtualInterface(valid[right].InterfaceName)
		if leftVirtual != rightVirtual {
			return !leftVirtual
		}
		leftRank := privateLANSubnetRank(net.ParseIP(valid[left].Address))
		rightRank := privateLANSubnetRank(net.ParseIP(valid[right].Address))
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if valid[left].InterfaceIndex != valid[right].InterfaceIndex {
			return valid[left].InterfaceIndex < valid[right].InterfaceIndex
		}
		return valid[left].Address < valid[right].Address
	})
	if len(valid) == 0 {
		return "", errors.New("no active private LAN IPv4 address was found")
	}
	return valid[0].Address, nil
}

func isPrivateLANIPv4(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsPrivate()
}

func likelyVirtualInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{
		"virtual",
		"vethernet",
		"hyper-v",
		"vmware",
		"virtualbox",
		"vbox",
		"docker",
		"wsl",
		"tailscale",
		"zerotier",
		"wireguard",
		"wintun",
		"openvpn",
		"hamachi",
		"clash",
		"sing-box",
		"loopback",
		"npcap",
		"tap",
		"tun",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func privateLANSubnetRank(ip net.IP) int {
	ip = ip.To4()
	if ip == nil {
		return 3
	}
	switch {
	case ip[0] == 192 && ip[1] == 168:
		return 0
	case ip[0] == 10:
		return 1
	default:
		return 2
	}
}

func validateListenAddress(name, value string, requireHost bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errorsf("%s is required", name)
	}
	separator := strings.LastIndex(value, ":")
	if separator < 0 {
		return errorsf("%s must be host:port or :port", name)
	}
	if requireHost && separator == 0 {
		return errorsf("%s must include a host", name)
	}
	port, err := strconv.Atoi(value[separator+1:])
	if err != nil || port < 1 || port > 65535 {
		return errorsf("%s has an invalid TCP port", name)
	}
	return nil
}

func validateLoopbackListenAddress(name, value string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return errorsf("%s must use the form 127.0.0.1:port", name)
	}
	if host != "127.0.0.1" {
		return errorsf("local-single-account requires %s to bind 127.0.0.1", name)
	}
	return nil
}

func validateSafeValue(name, value string) error {
	if strings.TrimSpace(value) == "" || !safeValuePattern.MatchString(value) {
		return errorsf("%s may contain only letters, digits, dot, underscore, colon, and dash", name)
	}
	return nil
}

func listenPort(address string) (int, error) {
	separator := strings.LastIndex(strings.TrimSpace(address), ":")
	if separator < 0 {
		return 0, fmt.Errorf("address %q has no port", address)
	}
	port, err := strconv.Atoi(address[separator+1:])
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("address %q has an invalid port", address)
	}
	return port, nil
}

func escapesRoot(path string) bool {
	cleaned := filepath.Clean(path)
	return cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

func errorsf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	temp, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	installed := false
	defer func() {
		_ = temp.Close()
		if !installed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode for %s: %w", path, err)
	}
	written, err := temp.Write(data)
	if err != nil {
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	if written != len(data) {
		return fmt.Errorf(
			"write temporary file for %s: wrote %d of %d bytes",
			path,
			written,
			len(data),
		)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("flush temporary file for %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err := replaceFileAtomic(tempPath, path); err != nil {
		return fmt.Errorf("replace %s atomically: %w", path, err)
	}
	installed = true
	return nil
}
