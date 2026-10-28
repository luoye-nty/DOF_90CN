package main

import (
	"encoding/json"
	"time"
)

type instanceConfig struct {
	SchemaVersion  int             `json:"schemaVersion"`
	InstallationID string          `json:"installationId"`
	Mode           string          `json:"mode"`
	Server         serverConfig    `json:"server"`
	Database       databaseConfig  `json:"database"`
	LegacyRedis    json.RawMessage `json:"redis,omitempty"`
	Game           gameConfig      `json:"game"`
	Protocol       protocolConfig  `json:"protocol"`
	Build          buildConfig     `json:"build"`
	Client         clientConfig    `json:"client"`
	// LauncherBinding pins which launcher may open a game connection. It is a
	// pointer so an installation that never runs keygen keeps an instance.json
	// byte-identical to the one it had before the feature existed; other tools
	// in this project read the same file, some of them strictly. The public keys
	// are the only half of the identity this server ever holds.
	LauncherBinding *launcherBindingConfig `json:"launcherBinding,omitempty"`
}

// launcherBindingConfig mirrors DNFBRIDGE_LAUNCHER_LID_* in dnfbridge.
type launcherBindingConfig struct {
	// Mode is off, audit or enforce. Audit verifies and logs but still serves
	// the legacy paths, which is how a rollout is observed before it rejects.
	Mode string `json:"mode,omitempty"`
	// PublicKeys are base64 DER SubjectPublicKeyInfo blobs (ECDSA P-256). Each
	// entry is one authorized launcher build; several are allowed so a rotation
	// or an edition split does not require a flag day.
	PublicKeys []string `json:"publicKeys,omitempty"`
	// AllowWireGuard keeps the SSH-attested private-source-IP binding reachable
	// while enforcement is on. It is off by default because that path is a
	// second, weaker identity that an enforcing deployment usually means to
	// retire.
	AllowWireGuard bool `json:"allowWireGuard,omitempty"`
}

type serverConfig struct {
	AdvertiseIP            string `json:"advertiseIp"`
	ChannelListen          string `json:"channelListen"`
	AdminListen            string `json:"adminListen"`
	AdminToken             string `json:"adminToken"`
	AccountID              string `json:"accountId"`
	PacketLog              bool   `json:"packetLog"`
	DebugMode              bool   `json:"debugMode"`
	DebugStrikeOnSkillUse  bool   `json:"debugStrikeOnSkillUse"`
	DebugKillRoomHotkey    bool   `json:"debugKillRoomHotkey"`
	RobotEnabled           *bool  `json:"robotEnabled,omitempty"`
	PartyUDPRelayEnabled   bool   `json:"partyUdpRelayEnabled"`
	PartyUDPRelayPortStart int    `json:"partyUdpRelayPortStart"`
	PartyUDPRelayPortCount int    `json:"partyUdpRelayPortCount"`
	// DungeonRoomPartySlotOff disables the dungeon-room op9 party slot table
	// (client host arbitration). Optional; default false keeps the fix on.
	DungeonRoomPartySlotOff bool `json:"dungeonRoomPartySlotOff,omitempty"`
	// RaidLukeWeeklyLimitEnforced turns Luke's [WEEKLY ENTER COUNT] from an
	// observation into a refusal. Optional; default false, because the weekly
	// reset node is still unconfirmed and enforcing a wrong node locks players
	// out of a raid they are entitled to enter.
	RaidLukeWeeklyLimitEnforced bool `json:"raidLukeWeeklyLimitEnforced,omitempty"`
}

func (cfg serverConfig) robotsEnabled() bool {
	return cfg.RobotEnabled != nil && *cfg.RobotEnabled
}

type databaseConfig struct {
	Mode     string `json:"mode"`
	Path     string `json:"path,omitempty"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type gameConfig struct {
	ShardID                   string `json:"shardId"`
	PVFPath                   string `json:"pvfPath"`
	PVFMaxBytes               int64  `json:"pvfMaxBytes"`
	ChannelInfoPath           string `json:"channelInfoPath"`
	DungeonBatchPickupEnabled *bool  `json:"dungeonBatchPickupEnabled,omitempty"`
}

type protocolConfig struct {
	Profile                     string `json:"profile"`
	GameUpperHeader             string `json:"gameUpperHeader"`
	GameUpperBodyCodec          string `json:"gameUpperBodyCodec"`
	GameUpperClientBodyCodec    string `json:"gameUpperClientBodyCodec"`
	GameOuterToken              string `json:"gameOuterToken"`
	ChannelServerIndex          int    `json:"channelServerIndex"`
	ChannelAdvertiseServerIndex int    `json:"channelAdvertiseServerIndex"`
	RegionID                    int    `json:"regionId"`
	RegionName                  string `json:"regionName"`
}

type buildConfig struct {
	GoExecutable string `json:"goExecutable"`
}

type clientConfig struct {
	Directory       string `json:"directory"`
	InitialGamePort int    `json:"initialGamePort"`
	HookCreate      bool   `json:"hookCreate"`
}

type assetManifest struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Files         []assetManifestEntry `json:"files"`
}

type assetManifestEntry struct {
	Path     string `json:"path"`
	Required bool   `json:"required"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type clientManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Profile       string                `json:"profile"`
	Files         []clientManifestEntry `json:"files"`
}

// clientManifestEntry describes one required client file. SHA256 is optional
// here: this controller validates client files by existence only, on purpose,
// because an operator's DNF.exe legitimately varies between regional builds.
// The field still has to be declared and preserved, because the launcher
// (DNF90Hub, another repository) reads the same file and decides "matches" by
// comparing this hash. An entry with no hash is a permanent mismatch there:
// the launcher's repair copies the file, reports success, and the next check
// fails again on the empty expectation. Dropping the field also contradicts
// REBUILD_CLIENT_PATCH.bat, which tells the operator to update the 90CN.dll
// hash here after every client-patch rebuild.
type clientManifestEntry struct {
	Names    []string `json:"names"`
	Required bool     `json:"required"`
	SHA256   string   `json:"sha256"`
}

type processState struct {
	PID                 int       `json:"pid"`
	StartedAt           time.Time `json:"startedAt"`
	ProcessCreatedAt    time.Time `json:"processCreatedAt"`
	Executable          string    `json:"executable"`
	ExecutableSHA256    string    `json:"executableSha256"`
	InstallationID      string    `json:"installationId"`
	ServicePort         int       `json:"servicePort,omitempty"`
	RuntimeConfigSHA256 string    `json:"runtimeConfigSha256,omitempty"`
	LauncherPID         int       `json:"launcherPid,omitempty"`
	LauncherCreatedAt   time.Time `json:"launcherCreatedAt,omitempty"`
	DatabaseHost        string    `json:"databaseHost,omitempty"`
	DatabaseName        string    `json:"databaseName,omitempty"`
	DatabaseUser        string    `json:"databaseUser,omitempty"`
	DatabasePassword    string    `json:"databasePassword,omitempty"`
	Stdout              string    `json:"stdout"`
	Stderr              string    `json:"stderr"`
}

type assetState struct {
	GeneratedAt time.Time          `json:"generatedAt"`
	Files       []assetStateRecord `json:"files"`
}

type assetStateRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type portableMySQLManifest struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Product       string                      `json:"product"`
	Version       string                      `json:"version"`
	Platform      string                      `json:"platform"`
	SourceURL     string                      `json:"sourceUrl"`
	Archive       portableMySQLArchive        `json:"archive"`
	Files         []portableMySQLManifestFile `json:"files"`
}

type portableMySQLArchive struct {
	Path   string `json:"path"`
	Root   string `json:"root"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type portableMySQLManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type appLocalRuntimeManifest struct {
	SchemaVersion int                         `json:"schemaVersion"`
	Product       string                      `json:"product"`
	Version       string                      `json:"version"`
	Platform      string                      `json:"platform"`
	SourceURL     string                      `json:"sourceUrl"`
	Files         []portableMySQLManifestFile `json:"files"`
}

type portableMySQLInstallState struct {
	SchemaVersion int       `json:"schemaVersion"`
	Product       string    `json:"product"`
	Version       string    `json:"version"`
	Platform      string    `json:"platform"`
	ArchiveSHA256 string    `json:"archiveSha256"`
	InstalledAt   time.Time `json:"installedAt"`
}

type portableMySQLDataState struct {
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

type binaryInstallState struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Entries       []binaryInstallStateEntry `json:"entries"`
}

type binaryInstallStateEntry struct {
	Destination     string `json:"destination"`
	Temporary       string `json:"temporary"`
	Backup          string `json:"backup"`
	HadOriginal     bool   `json:"hadOriginal"`
	OriginalSHA256  string `json:"originalSha256,omitempty"`
	CandidateSHA256 string `json:"candidateSha256"`
}

type serverGroupPlan struct {
	Version string             `json:"version"`
	Shards  []serverGroupShard `json:"shards"`
	Groups  []serverGroup      `json:"groups"`
	Routes  []serverGroupRoute `json:"routes"`
}

type serverGroupShard struct {
	ID           string `json:"id"`
	GroupID      string `json:"group_id"`
	State        string `json:"state"`
	OpenAt       string `json:"open_at"`
	PublicOpenAt string `json:"public_open_at"`
}

type serverGroup struct {
	ID       string `json:"id"`
	Service  string `json:"service"`
	MemberID string `json:"member_id"`
	State    string `json:"state"`
}

type serverGroupRoute struct {
	Feature string            `json:"feature"`
	ShardID string            `json:"shard_id"`
	GroupID string            `json:"group_id"`
	State   string            `json:"state"`
	Meta    map[string]string `json:"meta"`
}

type checkOptions struct {
	skipDatabase bool
	skipPorts    bool
	checkClient  bool
}
