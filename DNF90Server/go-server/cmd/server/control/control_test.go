package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureClientPersistsAbsoluteDirectory(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}
	clientRoot := filepath.Join(root, "client")
	if err := os.MkdirAll(clientRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientRoot, "DNF.exe"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	controller := newController(paths, &output, &output)
	if err := controller.configureClient(`"` + clientRoot + `"`); err != nil {
		t.Fatal(err)
	}
	updated, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Client.Directory != clientRoot {
		t.Fatalf("client directory = %q, want %q", updated.Client.Directory, clientRoot)
	}
	if !strings.Contains(output.String(), "Client directory configured") {
		t.Fatalf("configure output = %q", output.String())
	}
}

func TestLoadInstanceMigratesMissingDungeonBatchPickupToEnabled(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	if cfg.Game.DungeonBatchPickupEnabled != nil {
		t.Fatal("legacy fixture unexpectedly contains dungeon batch pickup setting")
	}
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Game.DungeonBatchPickupEnabled == nil || !*loaded.Game.DungeonBatchPickupEnabled {
		t.Fatalf("loaded dungeon batch pickup setting=%v, want true", loaded.Game.DungeonBatchPickupEnabled)
	}
	persisted, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Game.DungeonBatchPickupEnabled == nil || !*persisted.Game.DungeonBatchPickupEnabled {
		t.Fatalf("persisted dungeon batch pickup setting=%v, want true", persisted.Game.DungeonBatchPickupEnabled)
	}
}

func TestLoadInstanceMigratesMissingRegionIdentityToNativeDefault(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Protocol.RegionID = 0
	cfg.Protocol.ChannelAdvertiseServerIndex = 0
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Protocol.RegionID != 1 || loaded.Protocol.RegionName != "" || loaded.Protocol.ChannelAdvertiseServerIndex != 1 {
		t.Fatalf(
			"loaded region identity=(%d,%q,advertise=%d), want native default (1,empty,advertise=1)",
			loaded.Protocol.RegionID,
			loaded.Protocol.RegionName,
			loaded.Protocol.ChannelAdvertiseServerIndex,
		)
	}
	persisted, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Protocol.RegionID != 1 || persisted.Protocol.ChannelAdvertiseServerIndex != 1 {
		t.Fatalf(
			"persisted region identity=(%d,advertise=%d), want (1,advertise=1)",
			persisted.Protocol.RegionID,
			persisted.Protocol.ChannelAdvertiseServerIndex,
		)
	}
}

func TestLoadInstanceMigratesMissingRobotSwitchToDisabled(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Server.RobotEnabled = nil
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"robotEnabled"`)) {
		t.Fatal("legacy fixture unexpectedly contains robotEnabled")
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadInstance(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server.RobotEnabled == nil || *loaded.Server.RobotEnabled {
		t.Fatal("missing robot switch was not migrated to disabled")
	}
	persisted, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Server.RobotEnabled == nil || *persisted.Server.RobotEnabled {
		t.Fatal("disabled robot switch was not persisted")
	}
}

func TestValidateInstance(t *testing.T) {
	cfg := validTestInstance()
	if err := validateInstance(cfg); err != nil {
		t.Fatalf("validateInstance(valid) error = %v", err)
	}
	configuringCustomRegion := cfg
	configuringCustomRegion.Protocol.RegionID = 42
	configuringCustomRegion.Protocol.ChannelAdvertiseServerIndex = 42
	configuringCustomRegion.Protocol.RegionName = "测试区"
	if err := validateInstance(configuringCustomRegion); err != nil {
		t.Fatalf("validateInstance(custom region) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*instanceConfig)
		want   string
	}{
		{
			name: "profile",
			mutate: func(cfg *instanceConfig) {
				cfg.Protocol.Profile = "unknown"
			},
			want: "protocol.profile",
		},
		{
			name: "absolute asset",
			mutate: func(cfg *instanceConfig) {
				cfg.Game.PVFPath = `C:\outside\Script.pvf`
			},
			want: "relative to runtime",
		},
		{
			name: "unsafe password",
			mutate: func(cfg *instanceConfig) {
				cfg.Database.Password = "bad password"
			},
			want: "database.password",
		},
		{
			name: "non-root portable database",
			mutate: func(cfg *instanceConfig) {
				cfg.Database.User = "dnf"
			},
			want: "must be root",
		},
		{
			name: "non-loopback channel",
			mutate: func(cfg *instanceConfig) {
				cfg.Server.ChannelListen = "0.0.0.0:7001"
			},
			want: "127.0.0.1",
		},
		{
			name: "public advertise address",
			mutate: func(cfg *instanceConfig) {
				cfg.Server.AdvertiseIP = "8.8.8.8"
			},
			want: "private LAN",
		},
		{
			name: "non-standard loopback advertise address",
			mutate: func(cfg *instanceConfig) {
				cfg.Server.AdvertiseIP = "127.0.0.2"
			},
			want: "loopback must be 127.0.0.1",
		},
		{
			name: "wrong outer token",
			mutate: func(cfg *instanceConfig) {
				cfg.Protocol.GameOuterToken = "00112233445566778899aabbccddeeff"
			},
			want: "compatibility profile",
		},
		{
			name: "mismatched channel advertise region",
			mutate: func(cfg *instanceConfig) {
				cfg.Protocol.ChannelAdvertiseServerIndex = 2
			},
			want: "must match protocol.regionId",
		},
		{
			name: "invalid region id",
			mutate: func(cfg *instanceConfig) {
				cfg.Protocol.RegionID = 250
			},
			want: "protocol.regionId",
		},
		{
			name: "region name too long",
			mutate: func(cfg *instanceConfig) {
				cfg.Protocol.RegionName = strings.Repeat("区", 25)
			},
			want: "protocol.regionName",
		},
		{
			name: "wrong initial game port",
			mutate: func(cfg *instanceConfig) {
				cfg.Client.InitialGamePort = 10011
			},
			want: "must be 0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := cfg
			test.mutate(&invalid)
			err := validateInstance(invalid)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInstance() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestGenerateRuntimeConfigs(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := os.MkdirAll(filepath.Dir(paths.channelAsset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.channelAsset, []byte("channel fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	if err := generateRuntimeConfigs(paths, cfg); err != nil {
		t.Fatalf("generateRuntimeConfigs() error = %v", err)
	}

	required := []string{
		filepath.Join(paths.runtimeConfigs, "dnfbridge.toml"),
		filepath.Join(paths.runtimeConfigs, "dnf", "logic.toml"),
		filepath.Join(paths.runtimeConfigs, "servergroup", "plan.json"),
		paths.mysqlConfig,
		filepath.Join(paths.runtimeData, "channel_info.etc"),
	}
	for _, path := range required {
		if !isRegularFile(path) {
			t.Errorf("generated file missing: %s", path)
		}
	}
	if isRegularFile(filepath.Join(paths.runtimeConfig, "environment.ps1")) {
		t.Fatal("native controller must not generate PowerShell")
	}
	if isRegularFile(filepath.Join(paths.runtimeConfig, "docker.env")) {
		t.Fatal("portable controller must not generate Docker state")
	}

	serviceData, err := os.ReadFile(filepath.Join(paths.runtimeConfigs, "dnfbridge.toml"))
	if err != nil {
		t.Fatal(err)
	}
	service := string(serviceData)
	for _, want := range []string{
		`public_addr = "192.168.1.113:7001"`,
		`private_addr = "127.0.0.1:18111"`,
		`shard_id = 9999`,
		`path = "data/dnf/Script.pvf"`,
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service config missing %q", want)
		}
	}

	var plan serverGroupPlan
	planData, err := os.ReadFile(filepath.Join(paths.runtimeConfigs, "servergroup", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(planData, &plan); err != nil {
		t.Fatalf("decode generated plan: %v", err)
	}
	if got := plan.Routes[0].Meta["dnf.repository.write_database"]; got != cfg.Database.Name {
		t.Fatalf("write database = %q, want %q", got, cfg.Database.Name)
	}
	if got := plan.Routes[0].Meta["dnf.repository.read_database"]; got != cfg.Database.Name {
		t.Fatalf("read database = %q, want %q", got, cfg.Database.Name)
	}
}

func TestGenerateSQLiteRuntimeConfigsDoesNotRequireMySQL(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := os.MkdirAll(filepath.Dir(paths.channelAsset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.channelAsset, []byte("channel fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Database = databaseConfig{Mode: "sqlite", Path: "data/dnf90.db", Name: "dnf_local"}
	if err := validateInstance(cfg); err != nil {
		t.Fatalf("validate SQLite instance: %v", err)
	}
	if err := generateRuntimeConfigs(paths, cfg); err != nil {
		t.Fatalf("generate SQLite configs: %v", err)
	}
	logicPath := filepath.Join(paths.runtimeConfigs, "dnf", "logic.toml")
	logicData, err := os.ReadFile(logicPath)
	if err != nil {
		t.Fatal(err)
	}
	logic := string(logicData)
	for _, want := range []string{`driver = "sqlite"`, `sqlite_path = "data/dnf90.db"`} {
		if !strings.Contains(logic, want) {
			t.Errorf("SQLite logic config missing %q: %s", want, logic)
		}
	}
	if strings.Contains(logic, "mysql_dsn") {
		t.Fatalf("SQLite logic config still contains MySQL DSN: %s", logic)
	}
	if isRegularFile(paths.mysqlConfig) {
		t.Fatal("SQLite config generation created mysql.ini")
	}
}

func TestChoosePrivateLANIPv4(t *testing.T) {
	candidates := []lanIPv4Candidate{
		{Address: "192.168.56.1", InterfaceName: "VirtualBox Host-Only", InterfaceIndex: 2},
		{Address: "10.12.0.5", InterfaceName: "Ethernet", InterfaceIndex: 8},
		{Address: "127.0.0.1", InterfaceName: "Loopback", InterfaceIndex: 1},
		{Address: "169.254.1.2", InterfaceName: "Disconnected", InterfaceIndex: 9},
	}

	got, err := choosePrivateLANIPv4(candidates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.12.0.5" {
		t.Fatalf("fallback LAN IPv4 = %q, want physical interface 10.12.0.5", got)
	}

	got, err = choosePrivateLANIPv4(candidates, net.ParseIP("192.168.56.1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.12.0.5" {
		t.Fatalf("virtual default-route LAN IPv4 = %q, want physical interface 10.12.0.5", got)
	}

	got, err = choosePrivateLANIPv4(candidates, net.ParseIP("10.12.0.5"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.12.0.5" {
		t.Fatalf("physical default-route LAN IPv4 = %q, want 10.12.0.5", got)
	}

	if _, err := choosePrivateLANIPv4([]lanIPv4Candidate{
		{Address: "127.0.0.1", InterfaceName: "Loopback", InterfaceIndex: 1},
		{Address: "8.8.8.8", InterfaceName: "Invalid", InterfaceIndex: 2},
	}, nil); err == nil {
		t.Fatal("choosePrivateLANIPv4 accepted a candidate set without a private LAN IPv4")
	}
}

func TestAppendWineRouteCandidateUsesOnlyPrivateRoute(t *testing.T) {
	original := []lanIPv4Candidate{{
		Address:        "192.168.56.1",
		InterfaceName:  "VirtualBox Host-Only",
		InterfaceIndex: 2,
	}}
	withoutWine := appendWineRouteCandidate(
		append([]lanIPv4Candidate(nil), original...),
		net.ParseIP("10.20.30.40"),
		false,
	)
	if len(withoutWine) != len(original) {
		t.Fatalf("native candidate count=%d, want %d", len(withoutWine), len(original))
	}
	withWine := appendWineRouteCandidate(
		append([]lanIPv4Candidate(nil), original...),
		net.ParseIP("10.20.30.40"),
		true,
	)
	got, err := choosePrivateLANIPv4(withWine, net.ParseIP("10.20.30.40"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.20.30.40" {
		t.Fatalf("Wine route candidate=%q, want 10.20.30.40", got)
	}
	public := appendWineRouteCandidate(nil, net.ParseIP("8.8.8.8"), true)
	if len(public) != 0 {
		t.Fatalf("public Wine route was accepted: %+v", public)
	}
}

// AGENTS.md documents one exception to the private-LAN rule: a host with no
// outbound route falls back to loopback so the local client can still reach the
// channels. The private LAN address must still win whenever one exists.
func TestChooseAdvertiseIPv4FallsBackToLoopbackOnlyWithoutCandidates(t *testing.T) {
	address, fallback := chooseAdvertiseIPv4(nil, nil)
	if address != "127.0.0.1" || !fallback {
		t.Fatalf("empty candidate set = (%q, %v), want (\"127.0.0.1\", true)", address, fallback)
	}
	address, fallback = chooseAdvertiseIPv4(
		[]lanIPv4Candidate{{Address: "8.8.8.8", InterfaceName: "WAN", InterfaceIndex: 1}},
		net.ParseIP("8.8.8.8"),
	)
	if address != "127.0.0.1" || !fallback {
		t.Fatalf("public-only candidate set = (%q, %v), want (\"127.0.0.1\", true)", address, fallback)
	}
	address, fallback = chooseAdvertiseIPv4([]lanIPv4Candidate{
		{Address: "192.168.56.1", InterfaceName: "VirtualBox Host-Only", InterfaceIndex: 1},
	}, nil)
	if address != "192.168.56.1" || fallback {
		t.Fatalf("virtual-only candidate set = (%q, %v), want (\"192.168.56.1\", false)", address, fallback)
	}
	address, fallback = chooseAdvertiseIPv4([]lanIPv4Candidate{
		{Address: "192.168.56.1", InterfaceName: "VirtualBox Host-Only", InterfaceIndex: 1},
		{Address: "192.168.1.113", InterfaceName: "Ethernet", InterfaceIndex: 2},
	}, net.ParseIP("192.168.1.113"))
	if address != "192.168.1.113" || fallback {
		t.Fatalf("LAN candidate = (%q, %v), want (\"192.168.1.113\", false)", address, fallback)
	}
}

// An operator or the hub may pin the loopback advertisement explicitly; the
// controller must honour it instead of rewriting it back to AUTO_DETECT.
func TestExplicitLoopbackAdvertiseIPIsHonoured(t *testing.T) {
	cfg := validTestInstance()
	cfg.Server.AdvertiseIP = "127.0.0.1"
	if err := validateInstance(cfg); err != nil {
		t.Fatalf("validateInstance rejected an explicit 127.0.0.1: %v", err)
	}
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, data, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := loadInstance(paths)
	if err != nil {
		t.Fatalf("loadInstance() error = %v", err)
	}
	if resolved.Server.AdvertiseIP != "127.0.0.1" {
		t.Fatalf("resolved advertiseIp = %q, want 127.0.0.1", resolved.Server.AdvertiseIP)
	}
	stored, err := decodeInstance(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Server.AdvertiseIP != "127.0.0.1" {
		t.Fatalf("stored advertiseIp was rewritten to %q", stored.Server.AdvertiseIP)
	}
}

// An installation carried over from the previous one-click tree has keys this
// release removed. runtime/config/ is outside the launcher update whitelist, so
// rejecting the file would leave the installation unrepairable by any update.
func TestLoadInstanceDropsLegacyFieldsAndKeepsRejectingTypos(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	if err := paths.ensureDirectories(); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	data, err := marshalInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["game"].(map[string]any)["mainQuestContinuityExperience"] = true
	document["game"].(map[string]any)["deleteExpiredItems"] = false
	document["server"].(map[string]any)["partyUdpRelay"] = true
	legacy, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := loadInstance(paths)
	if err != nil {
		t.Fatalf("loadInstance() rejected a legacy instance.json: %v", err)
	}
	if resolved.Server.AdvertiseIP != cfg.Server.AdvertiseIP {
		t.Fatalf("advertiseIp = %q, want %q", resolved.Server.AdvertiseIP, cfg.Server.AdvertiseIP)
	}
	cleaned, err := os.ReadFile(paths.instance)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"mainQuestContinuityExperience", "deleteExpiredItems", "partyUdpRelay\"",
	} {
		if strings.Contains(string(cleaned), key) {
			t.Fatalf("legacy key %q survived the rewrite:\n%s", key, cleaned)
		}
	}

	document["game"].(map[string]any)["shardIdd"] = "9999"
	typo, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.instance, typo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstance(paths); err == nil {
		t.Fatal("loadInstance accepted an unknown field that is not a known legacy key")
	}
}

func TestExternalBinaryReleaseIsAcceptedAsProjectRoot(t *testing.T) {
	root := t.TempDir()
	for _, relative := range []string{
		"runtime/templates/instance.example.json",
		"runtime/bin/DNF90Control.exe",
		"RELEASE_INFO.json",
	} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !isProjectRoot(root) {
		t.Fatal("binary-only external release root was rejected")
	}
}

// The hub ships the server as runtime/ plus changlog.md and nothing else: its
// update whitelist cannot write RELEASE_INFO.json into the root, and the Go
// module is not distributed. Such a tree must still be a valid project root.
func TestRuntimeOnlyInstallationIsAcceptedAsProjectRoot(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root,
		"changlog.md",
		"runtime/templates/instance.example.json",
		"runtime/assets/manifest.json",
		"runtime/assets/client-compatibility.json",
		"runtime/bin/DNF90Control.exe",
	)
	if !isProjectRoot(root) {
		t.Fatal("runtime-only hub installation root was rejected")
	}
	if hasGoServerSource(newProjectPaths(root)) {
		t.Fatal("runtime-only installation was reported as carrying go-server source")
	}
}

func TestUnrelatedDirectoriesAreNotProjectRoots(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []string
	}{
		{
			name:  "runtime directory without the instance template",
			files: []string{"runtime/assets/manifest.json", "runtime/bin/DNF90Control.exe"},
		},
		{
			name:  "instance template without any second anchor",
			files: []string{"runtime/templates/instance.example.json"},
		},
		{
			name:  "release marker without the instance template",
			files: []string{"RELEASE_INFO.json", "runtime/bin/DNF90Control.exe"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeProjectFixture(t, root, test.files...)
			if isProjectRoot(root) {
				t.Fatalf("%s was accepted as a project root", test.name)
			}
		})
	}
}

func TestBuildBinariesRefusesRuntimeOnlyInstallation(t *testing.T) {
	root := t.TempDir()
	writeProjectFixture(t, root,
		"runtime/templates/instance.example.json",
		"runtime/assets/manifest.json",
	)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	c := &controller{paths: newProjectPaths(root), stdout: stdout, stderr: stderr}
	err := c.buildBinaries(context.Background(), validTestInstance(), false)
	if err == nil {
		t.Fatal("buildBinaries succeeded without a go-server source tree")
	}
	if !strings.Contains(err.Error(), "go-server source is not part of this installation") {
		t.Fatalf("buildBinaries error = %v, want the runtime-only explanation", err)
	}
}

func writeProjectFixture(t *testing.T, root string, relatives ...string) {
	t.Helper()
	for _, relative := range relatives {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateAssetsAndClient(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	runtimeAsset := filepath.Join(paths.runtimeData, "fixture.bin")
	if err := os.MkdirAll(filepath.Dir(runtimeAsset), 0o755); err != nil {
		t.Fatal(err)
	}
	assetData := []byte("asset")
	if err := os.WriteFile(runtimeAsset, assetData, 0o644); err != nil {
		t.Fatal(err)
	}
	assetDigest, err := fileSHA256(runtimeAsset)
	if err != nil {
		t.Fatal(err)
	}
	assetManifestData, err := json.Marshal(assetManifest{
		SchemaVersion: 1,
		Files: []assetManifestEntry{{
			Path:     "data/dnf/fixture.bin",
			Required: true,
			Size:     int64(len(assetData)),
			SHA256:   assetDigest,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.assetManifest, assetManifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := validateAssets(paths, &output); err != nil {
		t.Fatalf("validateAssets() error = %v", err)
	}

	clientRoot := filepath.Join(root, "client")
	if err := os.MkdirAll(clientRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	clientFiles := []string{"DNF.exe", "ijl15.dll", "ijl15_real.dll", "90CN.dll", "90CNLua.dll"}
	manifest := clientManifest{SchemaVersion: 1, Profile: "90cn-decode-bypass-v1"}
	for index, name := range clientFiles {
		path := filepath.Join(clientRoot, name)
		data := []byte{byte(index + 1), byte(index + 9)}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, clientManifestEntry{
			Names:    []string{name},
			Required: true,
		})
	}
	clientManifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.clientManifest, clientManifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Client.Directory = clientRoot
	gotRoot, executable, err := validateClient(paths, cfg, "", &output)
	if err != nil {
		t.Fatalf("validateClient() error = %v", err)
	}
	if gotRoot != clientRoot {
		t.Fatalf("client root = %q, want %q", gotRoot, clientRoot)
	}
	if filepath.Base(executable) != "DNF.exe" {
		t.Fatalf("client executable = %q", executable)
	}
}

func TestValidateAssetsAcceptsModifiedScriptPVF(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	pvfPath := filepath.Join(paths.runtimeData, "Script.pvf")
	if err := os.MkdirAll(filepath.Dir(pvfPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pvfPath, []byte("operator-provided-pvf"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(assetManifest{
		SchemaVersion: 1,
		Files: []assetManifestEntry{{
			Path:     "data/dnf/Script.pvf",
			Required: true,
			Size:     1,
			SHA256:   strings.Repeat("0", 64),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.assetManifest, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := validateAssets(paths, &output); err != nil {
		t.Fatalf("validateAssets() custom PVF error = %v", err)
	}
	if !strings.Contains(output.String(), "custom PVF accepted") {
		t.Fatalf("validateAssets() output = %q", output.String())
	}
}

func TestValidateClientChecksExistenceWithoutHashingClientFiles(t *testing.T) {
	root := t.TempDir()
	paths := newProjectPaths(root)
	clientRoot := filepath.Join(root, "client")
	if err := os.MkdirAll(clientRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	executablePath := filepath.Join(clientRoot, "DNF.exe")
	dllPath := filepath.Join(clientRoot, "90CN.dll")
	if err := os.WriteFile(executablePath, []byte("expected-exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dllPath, []byte("replaceable-dll"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := clientManifest{
		SchemaVersion: 1,
		Profile:       "90cn-decode-bypass-v1",
		Files: []clientManifestEntry{
			{Names: []string{"DNF.exe"}, Required: true},
			{Names: []string{"90CN.dll"}, Required: true},
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(paths.clientManifest, manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validTestInstance()
	cfg.Client.Directory = clientRoot
	var output strings.Builder
	if _, _, err := validateClient(paths, cfg, "", &output); err != nil {
		t.Fatalf("validateClient() rejected unpinned DLL: %v", err)
	}
	if !strings.Contains(output.String(), "existence only") {
		t.Fatalf("validation output does not identify existence-only DLL check: %q", output.String())
	}

	if err := os.WriteFile(dllPath, []byte("replacement-dll"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateClient(paths, cfg, "", &output); err != nil {
		t.Fatalf("validateClient() rejected replacement DLL: %v", err)
	}
	if err := os.WriteFile(executablePath, []byte("different-exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateClient(paths, cfg, "", &output); err != nil {
		t.Fatalf("validateClient() rejected replacement executable: %v", err)
	}
}

func TestMergeEnvironmentOverridesCaseInsensitively(t *testing.T) {
	got := mergeEnvironment(
		[]string{"Path=old", "KEEP=value"},
		map[string]string{"PATH": "new", "ADDED": "yes"},
	)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "Path=old") {
		t.Fatalf("old environment value survived: %q", joined)
	}
	for _, want := range []string{"PATH=new", "KEEP=value", "ADDED=yes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment missing %q: %q", want, joined)
		}
	}
}

func TestRuntimeEnvironmentScrubsInheritedProtocolOverrides(t *testing.T) {
	t.Setenv("DNFBRIDGE_GAME_INITIAL_MODE", "inherited-bad-mode")
	t.Setenv("DFO_GAME_OUTER_TOKEN", "inherited-bad-token")
	t.Setenv("DNF_UNRELATED_STALE_VALUE", "inherited")

	cfg := validTestInstance()
	cfg.Server.AdvertiseIP = "192.168.1.113"
	joined := strings.Join(runtimeEnvironment(cfg), "\n")
	for _, forbidden := range []string{
		"inherited-bad-mode",
		"inherited-bad-token",
		"DNF_UNRELATED_STALE_VALUE",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("runtime environment retained inherited DNF override %q", forbidden)
		}
	}
	for _, required := range []string{
		"DNFBRIDGE_GAME_INITIAL_MODE=notice",
		"DNFBRIDGE_GAME_PRE_BOOTSTRAP=none",
		"DNFBRIDGE_GAME_POST_BOOTSTRAP=none",
		"DNFBRIDGE_DPROTO_MODE=legacy_patch",
		"DNFBRIDGE_CHANNEL_SERVER_INDEX=1",
		"DNFBRIDGE_CHANNEL_ADVERTISE_SERVER_INDEX=1",
		"DNFBRIDGE_REGION_ID=1",
		"DNFBRIDGE_SERVER_IP=192.168.1.113",
		"DNFBRIDGE_GAME_LISTEN_HOST=192.168.1.113",
		"DNFBRIDGE_PARTY_UDP_RELAY_ENABLED=false",
		"DNFBRIDGE_DUNGEON_BATCH_PICKUP_ENABLED=true",
		"DNFBRIDGE_ROBOT_ENABLED=false",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("runtime environment missing locked value %q", required)
		}
	}
}

func TestRuntimeEnvironmentCanDisableDungeonBatchPickup(t *testing.T) {
	cfg := validTestInstance()
	disabled := false
	cfg.Game.DungeonBatchPickupEnabled = &disabled
	joined := strings.Join(runtimeEnvironment(cfg), "\n")
	if !strings.Contains(joined, "DNFBRIDGE_DUNGEON_BATCH_PICKUP_ENABLED=false") {
		t.Fatalf("disabled dungeon batch pickup was not exported: %q", joined)
	}
}

func TestRuntimeEnvironmentExportsDisabledRobotSwitch(t *testing.T) {
	cfg := validTestInstance()
	disabled := false
	cfg.Server.RobotEnabled = &disabled
	joined := strings.Join(runtimeEnvironment(cfg), "\n")
	if !strings.Contains(joined, "DNFBRIDGE_ROBOT_ENABLED=false") {
		t.Fatalf("disabled robot switch was not exported: %q", joined)
	}
}

func TestRuntimeEnvironmentKeepsPartyRelayExplicitlyOptIn(t *testing.T) {
	cfg := validTestInstance()
	cfg.Server.PartyUDPRelayEnabled = true
	cfg.Server.PartyUDPRelayPortStart = 30000
	cfg.Server.PartyUDPRelayPortCount = 64
	if err := validateInstance(cfg); err != nil {
		t.Fatalf("validate explicit relay: %v", err)
	}
	joined := strings.Join(runtimeEnvironment(cfg), "\n")
	if !strings.Contains(joined, "DNFBRIDGE_PARTY_UDP_RELAY_ENABLED=true") {
		t.Fatalf("explicit relay opt-in was not exported: %q", joined)
	}
}

func TestRuntimeEnvironmentPassesCreatedRaidRecoverySeedOnlyInLocalDebug(t *testing.T) {
	t.Setenv("DNFBRIDGE_RAID_CREATED_RECOVERY_SEED_BASE64", "exact-seed")
	cfg := validTestInstance()
	if joined := strings.Join(runtimeEnvironment(cfg), "\n"); strings.Contains(joined, "exact-seed") {
		t.Fatalf("non-debug runtime inherited raid recovery seed: %q", joined)
	}
	cfg.Server.DebugMode = true
	if joined := strings.Join(runtimeEnvironment(cfg), "\n"); !strings.Contains(joined, "DNFBRIDGE_RAID_CREATED_RECOVERY_SEED_BASE64=exact-seed") {
		t.Fatalf("local debug runtime did not inherit raid recovery seed: %q", joined)
	}
}

func TestRuntimeEnvironmentKeepsDebugSkillStrikeSeparatelyOptIn(t *testing.T) {
	cfg := validTestInstance()
	cfg.Server.DebugMode = true
	joined := strings.Join(runtimeEnvironment(cfg), "\n")
	if !strings.Contains(joined, "DNFBRIDGE_DEBUG_MODE=true") ||
		!strings.Contains(joined, "DNFBRIDGE_DEBUG_STRIKE_ON_SKILL_USE=false") {
		t.Fatalf("debug mode unexpectedly enabled normal skill interception: %q", joined)
	}

	cfg.Server.DebugStrikeOnSkillUse = true
	joined = strings.Join(runtimeEnvironment(cfg), "\n")
	if !strings.Contains(joined, "DNFBRIDGE_DEBUG_STRIKE_ON_SKILL_USE=true") {
		t.Fatalf("explicit debug skill-strike opt-in was not exported: %q", joined)
	}
}

func TestClientLaunchArgumentUsesBootstrapListenHost(t *testing.T) {
	cfg := validTestInstance()
	cfg.Server.AdvertiseIP = "192.168.1.113"
	cfg.Server.ChannelListen = "127.0.0.1:7001"

	got, err := clientLaunchArgument(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "99?127.0.0.1?7001?0?de509f65e9ccaae621cb7278fc2b8e6c?01?1?0?0?0?0?1?9n2b1c8r3w7y?0?0?19847"
	if got != want {
		t.Fatalf("client launch argument = %q, want %q", got, want)
	}
}

func TestAdditionalClientStartupWarningAcceptsBootstrapHandoff(t *testing.T) {
	warning := additionalClientStartupWarning(
		4321,
		`C:\\DNF\\NoPack.exe`,
		liveProcess{},
		nil,
	)
	if !strings.Contains(warning, "handed off to a child process") {
		t.Fatalf("bootstrap handoff warning = %q", warning)
	}

	if warning := additionalClientStartupWarning(
		4321,
		`C:\\DNF\\NoPack.exe`,
		liveProcess{running: true, executable: `C:\\DNF\\NoPack.exe`},
		nil,
	); warning != "" {
		t.Fatalf("surviving client warning = %q", warning)
	}
}

func validTestInstance() instanceConfig {
	robotsEnabled := false
	return instanceConfig{
		SchemaVersion:  1,
		InstallationID: "inst_test",
		Mode:           "local-single-account",
		Server: serverConfig{
			AdvertiseIP:   "192.168.1.113",
			ChannelListen: "127.0.0.1:7001",
			AdminListen:   "127.0.0.1:18111",
			AdminToken:    "adm_test",
			AccountID:     "dnf:1",
			RobotEnabled:  &robotsEnabled,
		},
		Database: databaseConfig{
			Mode:     "portable",
			Host:     "127.0.0.1",
			Port:     13306,
			Name:     "dnf_local",
			User:     "root",
			Password: "db_test",
		},
		Game: gameConfig{
			ShardID:         "9999",
			PVFPath:         "data/dnf/Script.pvf",
			PVFMaxBytes:     1073741824,
			ChannelInfoPath: "data/dnf/channel_info.etc",
		},
		Protocol: protocolConfig{
			Profile:                     "90cn-decode-bypass-v1",
			GameUpperHeader:             "server16",
			GameUpperBodyCodec:          "plaintext",
			GameUpperClientBodyCodec:    "plaintext",
			GameOuterToken:              "de509f65e9ccaae621cb7278fc2b8e6c",
			ChannelServerIndex:          1,
			ChannelAdvertiseServerIndex: 1,
			RegionID:                    1,
		},
		Build:  buildConfig{GoExecutable: "go"},
		Client: clientConfig{InitialGamePort: 0, HookCreate: true},
	}
}
