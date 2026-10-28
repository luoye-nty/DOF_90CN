// Package guild owns the persistent guild aggregate and its lifecycle rules.
// Wire decoding and current-client packet projection stay in dnfbridge.
package guild

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	RegistryScope      = "global:guild_registry:v1"
	registryStateKey   = "state"
	DefaultCreateGold  = int64(300000)
	DefaultMemberLimit = 1000
	DefaultMasterGrade = byte(1)
	DefaultMemberGrade = byte(4)
	// 公会金库 capacity. DefaultCargoLimit is the row count a guild starts with
	// and MaxCargoLimit the ceiling [building upgrade] / 建设 can reach.
	DefaultCargoLimit = 8
	MaxCargoLimit     = 56
	// 公会徽章 purchase ladder. sub_1305E20 creates 16 selectors and sub_26AEA50
	// renders indices below 16; sub_12F8F20 applies 0..4 directly, while the
	// remaining marks go through the native purchase confirmation whose
	// sub_12FED40 text specifies 2000 guild points.
	MaxGuildMark     = byte(15)
	MaxFreeGuildMark = byte(4)
	GuildMarkPrice   = uint32(2000)
)

var (
	ErrRepositoryUnavailable = errors.New("guild repository is unavailable")
	ErrCharacterNotFound     = errors.New("guild character was not found")
	ErrInvalidName           = errors.New("guild name is invalid")
	ErrInvalidPromotion      = errors.New("guild promotion is invalid")
	ErrNameExists            = errors.New("guild name already exists")
	ErrAlreadyMember         = errors.New("character is already a guild member")
	ErrGuildNotFound         = errors.New("guild was not found")
	ErrNotMember             = errors.New("character is not a guild member")
	ErrPermissionDenied      = errors.New("guild permission denied")
	ErrApplicationExists     = errors.New("guild application already exists")
	ErrApplicationNotFound   = errors.New("guild application was not found")
	ErrMemberLimit           = errors.New("guild member limit reached")
	ErrInsufficientGold      = errors.New("insufficient gold for guild creation")
	ErrInsufficientGuardian  = errors.New("insufficient guardian resources")
)

type Registry struct {
	Version int               `json:"version"`
	NextID  uint32            `json:"next_id"`
	Guilds  map[uint32]*Guild `json:"guilds,omitempty"`
	// BaseDefaultsSeeded records that the one-off 公会基地 backfill already ran.
	// Without it the backfill would resurrect the base every time the registry is
	// saved, so 会长 deleting the base (BaseStage 0) would never stick.
	BaseDefaultsSeeded bool `json:"base_defaults_seeded,omitempty"`
}

type Guild struct {
	ID                  uint32           `json:"id"`
	Name                string           `json:"name"`
	MasterCharacterID   string           `json:"master_character_id"`
	MasterName          string           `json:"master_name"`
	Level               uint16           `json:"level"`
	Experience          uint32           `json:"experience"`
	Mileage             uint32           `json:"mileage"`
	Notice              string           `json:"notice,omitempty"`
	Promotion           string           `json:"promotion,omitempty"`
	Mark                byte             `json:"mark,omitempty"`
	AuraMark            uint32           `json:"aura_mark,omitempty"`
	AuraFrame           uint32           `json:"aura_frame,omitempty"`
	Benefits            map[byte]Benefit `json:"benefits,omitempty"`
	RecommendedRegion   uint32           `json:"recommended_region,omitempty"`
	RecommendedChannel  uint32           `json:"recommended_channel,omitempty"`
	AllianceRecruitment bool             `json:"alliance_recruitment,omitempty"`
	Tags                []byte           `json:"tags,omitempty"`
	JoinMode            byte             `json:"join_mode"`
	MemberLimit         int              `json:"member_limit"`
	GradeNames          map[byte]string  `json:"grade_names,omitempty"`
	// GradePermissions is the 18-bit mask op70's grade table carries per
	// grade. The client's editor (00F9C8A2..00F9CA8A) folds 18 checkboxes
	// into one u32 and sends it with op748; absent entries fall back to
	// DefaultGradePermissions.
	GradePermissions map[byte]uint32        `json:"grade_permissions,omitempty"`
	Members          map[string]Member      `json:"members,omitempty"`
	Applications     map[string]Application `json:"applications,omitempty"`
	Invitations      map[string]Invitation  `json:"invitations,omitempty"`
	MileageHistory   []MileageHistory       `json:"mileage_history,omitempty"`
	// BaseStage is 公会基地's stage: 0 means the guild has no base, 1..MaxBaseStage
	// means it exists at that stage. op227/op228/op229/op231 own this field, and
	// the client's 公会基地 window renders 当前公会基地 : %d阶段 from it.
	BaseStage byte      `json:"base_stage,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// DungeonGauges is the 公会地下城 purification gauge per stage id, 0 up to
	// that stage's [gauge limit]. See dungeon.go for the progression rules.
	// The three fields below sit at the end of the struct on purpose: adding a
	// wider type to the middle of the aligned block above would make gofmt
	// reflow every pre-existing field in it.
	DungeonGauges map[uint32]uint32 `json:"dungeon_gauges,omitempty"`
	// DungeonRewards marks "<stage>:<gauge ratio>" pairs already paid this
	// week, and DungeonWeekID is the week the two above belong to; a newer week
	// wipes both.
	DungeonRewards map[string]bool `json:"dungeon_rewards,omitempty"`
	DungeonWeekID  int64           `json:"dungeon_week_id,omitempty"`

	// --- 守护神 (guardian) / 公会战 (agit war) / 金库 (cargo) state. ---
	// These come from the upstream guild branch. They are appended after the
	// Dungeon* block rather than woven into the aligned run above so that
	// adding them does not reflow every pre-existing field in the struct.
	//
	// Guardians is the 守护神 roster (at most 5) and ActiveGuardian the one
	// currently fielded; GuardianAP / GuardianGP are the guild's shared
	// 守护神成长 resource pools. See guardian.go and guardian_growth.go.
	Guardians      []Guardian `json:"guardians,omitempty"`
	ActiveGuardian uint32     `json:"active_guardian,omitempty"`
	GuardianAP     uint32     `json:"guardian_ap,omitempty"`
	GuardianGP     uint32     `json:"guardian_gp,omitempty"`
	// AreaLevel is the 公会战 area (agit) expansion level, WarShopLevels the
	// per-kind [building upgrade] level of the two 公会战 shops, and War the
	// current 公会战 pairing/score state. See war.go and war_shop.go.
	AreaLevel     byte                        `json:"area_level,omitempty"`
	WarShopLevels [2]uint16                   `json:"war_shop_levels,omitempty"`
	War           WarState                    `json:"war,omitempty"`
	CargoLimit    int                         `json:"cargo_limit"`
	Cargo         map[int16]dnfrepo.ItemStack `json:"cargo,omitempty"`
	CargoHistory  []CargoHistory              `json:"cargo_history,omitempty"`
}

// MaxBaseStage caps 公会基地 upgrades. The client shows 已经升级至最高阶段。 for
// stages beyond its own table, so the server keeps the same small ladder.
const MaxBaseStage = byte(5)

// Guardian is one 守护神 owned by the guild. Level is the 守护神 level, the five
// Skills entries are the per-slot skill levels, and APExp / GPExp are the
// accumulated 守护神成长 experience feeding GuardianAP / GuardianGP.
type Guardian struct {
	ID     uint32  `json:"id"`
	Level  byte    `json:"level"`
	Skills [5]byte `json:"skills"`
	APExp  uint32  `json:"ap_exp"`
	GPExp  uint32  `json:"gp_exp"`
}

type Member struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Job         string `json:"job,omitempty"`
	// GrowType is the advancement byte the member-list row needs next to Job.
	// sub_1D21FD0 reads it right after the job byte and splits it into a low
	// nibble and bits 4..6, exactly like the role-select card does, so a row
	// without it renders the base class instead of the advanced one.
	GrowType string `json:"grow_type,omitempty"`
	Level    int    `json:"level"`
	Grade    byte   `json:"grade"`
	// Representative marks the one member per account the member list groups
	// under; normalizeRegistry keeps exactly one live representative per
	// account and SetRepresentative moves it. The op1454/op126 row carries it
	// as a single byte after the account group id.
	Representative bool   `json:"representative,omitempty"`
	Memo           string `json:"memo,omitempty"`
	Mileage        uint32 `json:"mileage"`
	// ContributionPoints is the 公会贡献度 total the guild keeps for this
	// member (红包 / 贡献币发放 read it). It is separate from Mileage, which is
	// the guild's own 里程 pool.
	ContributionPoints uint32    `json:"contribution_points,omitempty"`
	JoinedAt           time.Time `json:"joined_at"`
	AttendedAt         time.Time `json:"attended_at,omitempty"`
	// ChatAt is the last 公会聊天 contribution grant; the rule is once per
	// local day (help text 76515 进行公会聊天， 可以获得2点公会贡献。).
	ChatAt time.Time `json:"chat_at,omitempty"`
	// LastOnlineAt is refreshed whenever the member is seen with a live game
	// session. The member row carries the elapsed seconds since then, which is
	// how the client tells an offline member from an online one -- AttendedAt
	// cannot serve here because it only advances once per local day.
	LastOnlineAt time.Time `json:"last_online_at,omitempty"`
}

type Application struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Job         string `json:"job,omitempty"`
	// GrowType is the advancement byte the application row carries, the same
	// packed value Member.GrowType holds: sub_1D30B10 feeds this byte to
	// sub_3A51780 (01D30C8D) and splits it into a low nibble and bits 4..6,
	// exactly like the member row and the role-select card do.
	GrowType  string    `json:"grow_type,omitempty"`
	Level     int       `json:"level"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Invitation struct {
	CharacterID string    `json:"character_id"`
	InviterID   string    `json:"inviter_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type MileageHistory struct {
	CharacterID string    `json:"character_id"`
	Name        string    `json:"name"`
	Amount      uint32    `json:"amount"`
	CreatedAt   time.Time `json:"created_at"`
	// Reason is the op747 row reason byte recorded when the points were granted.
	// Zero keeps the reader's per-role fallback (2 公会成员每日签到 for members,
	// 0x0B 公会会长每日签到 for the master), which is what every pre-2026-09-12
	// row is; the newer 公会贡献 rules carry their own byte.
	Reason byte `json:"reason,omitempty"`
}

func NewRegistry() Registry {
	return Registry{Version: 1, NextID: 1, Guilds: make(map[uint32]*Guild)}
}

func Load(ctx context.Context, repo dnfrepo.SettingsRepository) (Registry, error) {
	if repo == nil {
		return Registry{}, ErrRepositoryUnavailable
	}
	record, found, err := repo.Load(ctx, RegistryScope)
	if err != nil {
		return Registry{}, err
	}
	if !found || strings.TrimSpace(record.Values[registryStateKey]) == "" {
		return NewRegistry(), nil
	}
	var registry Registry
	if err := json.Unmarshal([]byte(record.Values[registryStateKey]), &registry); err != nil {
		return Registry{}, err
	}
	normalizeRegistry(&registry)
	return registry, nil
}

func Save(ctx context.Context, repo dnfrepo.SettingsRepository, registry Registry, now time.Time) error {
	if repo == nil {
		return ErrRepositoryUnavailable
	}
	normalizeRegistry(&registry)
	encoded, err := json.Marshal(registry)
	if err != nil {
		return err
	}
	return repo.Save(ctx, dnfrepo.SettingsRecord{
		Scope:     RegistryScope,
		Values:    map[string]string{registryStateKey: string(encoded)},
		UpdatedAt: now,
	})
}

func normalizeRegistry(registry *Registry) {
	if registry.Version == 0 {
		registry.Version = 1
	}
	if registry.NextID == 0 {
		registry.NextID = 1
	}
	if registry.Guilds == nil {
		registry.Guilds = make(map[uint32]*Guild)
	}
	for id, guild := range registry.Guilds {
		if guild == nil {
			delete(registry.Guilds, id)
			continue
		}
		guild.ID = id
		if guild.Level == 0 {
			guild.Level = 1
		}
		if guild.MemberLimit <= 0 {
			guild.MemberLimit = DefaultMemberLimit
		}
		// 公会基地: 该客户端没有创建基地按钮, 公会内容商店与从赛丽亚房间进基地
		// 都以基地已存在为前提 ⇒ 早于 BaseStage 字段的公会一次性补齐到 1 阶段。
		// 只在迁移那一轮补, 否则会长删掉基地(stage=0)后每次存档都会被补回来。
		if !registry.BaseDefaultsSeeded && guild.BaseStage == 0 {
			guild.BaseStage = 1
		}
		if guild.CargoLimit <= 0 {
			guild.CargoLimit = DefaultCargoLimit
		}
		if guild.Cargo == nil {
			guild.Cargo = make(map[int16]dnfrepo.ItemStack)
		}
		if guild.Members == nil {
			guild.Members = make(map[string]Member)
		}
		if guild.Applications == nil {
			guild.Applications = make(map[string]Application)
		}
		if guild.Invitations == nil {
			guild.Invitations = make(map[string]Invitation)
		}
		if guild.GradeNames == nil {
			guild.GradeNames = defaultGradeNames()
		}
		// Older registries had no representative. Keep one real member per
		// account, and select a remaining member when the previous
		// representative leaves.
		representatives := make(map[string]string)
		for _, member := range SortedMembers(guild) {
			if member.AccountID != "" && (representatives[member.AccountID] == "" || member.Representative) {
				representatives[member.AccountID] = member.CharacterID
			}
		}
		for memberID, member := range guild.Members {
			member.Representative = member.AccountID != "" && representatives[member.AccountID] == member.CharacterID
			guild.Members[memberID] = member
		}
		if id >= registry.NextID {
			registry.NextID = id + 1
		}
	}
	registry.BaseDefaultsSeeded = true
}

// memberGrowType is the advancement stat a member row carries next to Job. The
// member list needs both: sub_1D21FD0 splits this byte into a low nibble and
// bits 4..6, so a row without it renders the base class.
func memberGrowType(character dnfrepo.CharacterRecord) string {
	if character.Stats == nil {
		return ""
	}
	value, found := character.Stats["grow_type"]
	if !found {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func defaultGradeNames() map[byte]string {
	return map[byte]string{1: "会长", 2: "副会长", 3: "优秀会员", 4: "普通会员"}
}

func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	count := utf8.RuneCountInString(name)
	if count < 2 || count > 12 || strings.ContainsAny(name, "\r\n\t") {
		return ErrInvalidName
	}
	return nil
}

// ValidateNotice bounds the guild notice by the field the current client
// actually reads it back through: op70's second DSTR is read into a 256-unit
// buffer (NoPack sub_1D8B560 -> sub_3457D20 with capacity 0x100) before being
// assigned to guild+0x18. One reserved unit keeps the terminator.
func ValidateNotice(notice string) error {
	notice = strings.TrimSpace(notice)
	if len(utf16.Encode([]rune(notice))) > 255 || strings.ContainsRune(notice, 0) {
		return ErrInvalidPromotion
	}
	return nil
}

func ValidatePromotion(promotion string) error {
	promotion = strings.TrimSpace(promotion)
	if utf8.RuneCountInString(promotion) > 80 || strings.ContainsRune(promotion, '\x00') {
		return ErrInvalidPromotion
	}
	return nil
}

func (r *Registry) NameAvailable(name string) bool {
	name = strings.TrimSpace(name)
	if ValidateName(name) != nil {
		return false
	}
	for _, guild := range r.Guilds {
		if strings.EqualFold(strings.TrimSpace(guild.Name), name) {
			return false
		}
	}
	return true
}

func (r *Registry) GuildForCharacter(characterID string) (*Guild, Member, bool) {
	characterID = strings.TrimSpace(characterID)
	for _, guild := range r.Guilds {
		if member, ok := guild.Members[characterID]; ok {
			return guild, member, true
		}
	}
	return nil, Member{}, false
}

func (r *Registry) removeApplicationsForCharacter(characterID string, now time.Time) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return
	}
	for _, guild := range r.Guilds {
		if guild == nil || guild.Applications == nil {
			continue
		}
		if _, found := guild.Applications[characterID]; !found {
			continue
		}
		delete(guild.Applications, characterID)
		guild.UpdatedAt = now
	}
}

func (r *Registry) GuildByID(id uint32) (*Guild, bool) {
	guild, ok := r.Guilds[id]
	return guild, ok && guild != nil
}

func (r *Registry) GuildByName(name string) (*Guild, bool) {
	name = strings.TrimSpace(name)
	for _, value := range r.Guilds {
		if value != nil && strings.EqualFold(strings.TrimSpace(value.Name), name) {
			return value, true
		}
	}
	return nil, false
}

func (r *Registry) Create(character dnfrepo.CharacterRecord, name string, now time.Time) (*Guild, error) {
	name = strings.TrimSpace(name)
	if ValidateName(name) != nil {
		return nil, ErrInvalidName
	}
	if !r.NameAvailable(name) {
		return nil, ErrNameExists
	}
	if _, _, found := r.GuildForCharacter(character.CharacterID); found {
		return nil, ErrAlreadyMember
	}
	id := r.NextID
	if id == 0 {
		id = 1
	}
	r.NextID = id + 1
	member := Member{
		CharacterID: character.CharacterID,
		AccountID:   character.AccountID,
		Name:        character.Name,
		Job:         character.Job,
		GrowType:    memberGrowType(character),
		Level:       character.Level,
		Grade:       DefaultMasterGrade,
		JoinedAt:    now,
		AttendedAt:  now,
	}
	guild := &Guild{
		ID:                id,
		Name:              name,
		MasterCharacterID: character.CharacterID,
		MasterName:        character.Name,
		Level:             1,
		JoinMode:          1,

		// 公会基地: this client has no 创建公会基地 button and the 公会内容 shop as well
		// as 从赛丽亚房间进入公会基地 assume the base exists, so a new guild owns stage 1.
		BaseStage:    1,
		MemberLimit:  DefaultMemberLimit,
		CargoLimit:   DefaultCargoLimit,
		Cargo:        make(map[int16]dnfrepo.ItemStack),
		GradeNames:   defaultGradeNames(),
		Members:      map[string]Member{character.CharacterID: member},
		Applications: make(map[string]Application),
		Invitations:  make(map[string]Invitation),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	r.removeApplicationsForCharacter(character.CharacterID, now)
	r.Guilds[id] = guild
	return guild, nil
}

func (r *Registry) Search(query string, page, pageSize int) []*Guild {
	query = strings.TrimSpace(query)
	if page < 0 {
		page = 0
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	values := make([]*Guild, 0, len(r.Guilds))
	for _, guild := range r.Guilds {
		if query == "" || strings.Contains(strings.ToLower(guild.Name), strings.ToLower(query)) ||
			strings.Contains(strings.ToLower(guild.MasterName), strings.ToLower(query)) {
			values = append(values, guild)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Level != values[j].Level {
			return values[i].Level > values[j].Level
		}
		return values[i].ID < values[j].ID
	})
	start := page * pageSize
	if start >= len(values) {
		return nil
	}
	end := start + pageSize
	if end > len(values) {
		end = len(values)
	}
	return values[start:end]
}

func (r *Registry) Apply(guildID uint32, character dnfrepo.CharacterRecord, message string, now time.Time) error {
	if _, _, found := r.GuildForCharacter(character.CharacterID); found {
		return ErrAlreadyMember
	}
	guild, found := r.GuildByID(guildID)
	if !found {
		return ErrGuildNotFound
	}
	if _, exists := guild.Applications[character.CharacterID]; exists {
		return ErrApplicationExists
	}
	guild.Applications[character.CharacterID] = Application{
		CharacterID: character.CharacterID,
		AccountID:   character.AccountID,
		Name:        character.Name,
		Job:         character.Job,
		GrowType:    memberGrowType(character),
		Level:       character.Level,
		Message:     strings.TrimSpace(message),
		CreatedAt:   now,
	}
	guild.UpdatedAt = now
	return nil
}

// JoinDirect adds character to guildID as a member without an application.
//
// The guild's 加入方式 is op740's mode byte: the settings window renders
// [record+0x3c] == 0 as 31591 接受后加入 and == 1 as 31592 立即加入
// (00FC903A..00FC90C3). The native client only sends op348 (申请加入) once, and
// it expects the server to honour that mode: with 立即加入 no approval step
// exists, so there is nothing to approve. Before this method the handler always
// created an application, which is why both modes behaved as 接受后加入.
func (r *Registry) JoinDirect(guildID uint32, character dnfrepo.CharacterRecord, now time.Time) (Member, error) {
	if _, _, found := r.GuildForCharacter(character.CharacterID); found {
		return Member{}, ErrAlreadyMember
	}
	guild, found := r.GuildByID(guildID)
	if !found {
		return Member{}, ErrGuildNotFound
	}
	if len(guild.Members) >= guild.MemberLimit {
		return Member{}, ErrMemberLimit
	}
	joined := Member{
		CharacterID: character.CharacterID,
		AccountID:   character.AccountID,
		Name:        character.Name,
		Job:         character.Job,
		GrowType:    memberGrowType(character),
		Level:       character.Level,
		Grade:       DefaultMemberGrade,
		JoinedAt:    now,
		AttendedAt:  now,
	}
	guild.Members[character.CharacterID] = joined
	r.removeApplicationsForCharacter(character.CharacterID, now)
	guild.UpdatedAt = now
	return joined, nil
}

func (r *Registry) CancelApplication(guildID uint32, characterID string, now time.Time) error {
	guild, found := r.GuildByID(guildID)
	if !found {
		return ErrGuildNotFound
	}
	if _, exists := guild.Applications[characterID]; !exists {
		return ErrApplicationNotFound
	}
	delete(guild.Applications, characterID)
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) DecideApplication(guildID uint32, actorID, characterID string, approve bool, now time.Time) (Member, error) {
	guild, found := r.GuildByID(guildID)
	if !found {
		return Member{}, ErrGuildNotFound
	}
	actor, member := guild.Members[actorID]
	if !member || actor.Grade > 2 {
		return Member{}, ErrPermissionDenied
	}
	application, found := guild.Applications[characterID]
	if !found {
		return Member{}, ErrApplicationNotFound
	}
	delete(guild.Applications, characterID)
	if !approve {
		guild.UpdatedAt = now
		return Member{}, nil
	}
	if len(guild.Members) >= guild.MemberLimit {
		return Member{}, ErrMemberLimit
	}
	if _, _, exists := r.GuildForCharacter(characterID); exists {
		return Member{}, ErrAlreadyMember
	}
	joined := Member{
		CharacterID: application.CharacterID,
		AccountID:   application.AccountID,
		Name:        application.Name,
		Job:         application.Job,
		// Carry the advancement byte over from the application; without it a
		// member who joined this way renders as their base class until the
		// next member-list read reconciles it from the character repository.
		GrowType:   application.GrowType,
		Level:      application.Level,
		Grade:      DefaultMemberGrade,
		JoinedAt:   now,
		AttendedAt: now,
	}
	guild.Members[characterID] = joined
	r.removeApplicationsForCharacter(characterID, now)
	guild.UpdatedAt = now
	return joined, nil
}

func (r *Registry) Invite(actorID string, character dnfrepo.CharacterRecord, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade > 2 {
		return ErrPermissionDenied
	}
	if _, _, found := r.GuildForCharacter(character.CharacterID); found {
		return ErrAlreadyMember
	}
	guild.Invitations[character.CharacterID] = Invitation{
		CharacterID: character.CharacterID,
		InviterID:   actorID,
		CreatedAt:   now,
	}
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) ReplyInvitation(character dnfrepo.CharacterRecord, accept bool, now time.Time) (*Guild, error) {
	for _, guild := range r.Guilds {
		if guild == nil {
			continue
		}
		if _, found := guild.Invitations[character.CharacterID]; !found {
			continue
		}
		delete(guild.Invitations, character.CharacterID)
		guild.UpdatedAt = now
		if !accept {
			return guild, nil
		}
		if _, _, found := r.GuildForCharacter(character.CharacterID); found {
			return nil, ErrAlreadyMember
		}
		if len(guild.Members) >= guild.MemberLimit {
			return nil, ErrMemberLimit
		}
		guild.Members[character.CharacterID] = Member{
			CharacterID: character.CharacterID,
			AccountID:   character.AccountID,
			Name:        character.Name,
			Job:         character.Job,
			GrowType:    memberGrowType(character),
			Level:       character.Level,
			Grade:       DefaultMemberGrade,
			JoinedAt:    now,
			AttendedAt:  now,
		}
		r.removeApplicationsForCharacter(character.CharacterID, now)
		guild.UpdatedAt = now
		return guild, nil
	}
	return nil, ErrApplicationNotFound
}

func (r *Registry) RemoveMember(actorID, targetID string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade > 2 {
		return ErrPermissionDenied
	}
	target, found := guild.Members[targetID]
	if !found || target.Grade == DefaultMasterGrade || target.Grade <= actor.Grade {
		return ErrPermissionDenied
	}
	delete(guild.Members, targetID)
	delete(guild.Invitations, targetID)
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) Leave(characterID string, now time.Time) error {
	guild, _, found := r.GuildForCharacter(characterID)
	if !found {
		return ErrNotMember
	}
	if guild.MasterCharacterID == characterID {
		return ErrPermissionDenied
	}
	delete(guild.Members, characterID)
	delete(guild.Invitations, characterID)
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) Disband(characterID string) error {
	guild, _, found := r.GuildForCharacter(characterID)
	if !found {
		return ErrNotMember
	}
	if guild.MasterCharacterID != characterID {
		return ErrPermissionDenied
	}
	delete(r.Guilds, guild.ID)
	return nil
}

func (r *Registry) Delegate(actorID, targetID string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	target, found := guild.Members[targetID]
	if !found {
		return ErrNotMember
	}
	actor.Grade = 2
	target.Grade = DefaultMasterGrade
	guild.Members[actorID] = actor
	guild.Members[targetID] = target
	guild.MasterCharacterID = target.CharacterID
	guild.MasterName = target.Name
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) ChangeGrade(actorID, targetID string, grade byte, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade || grade < 2 || grade > 4 {
		return ErrPermissionDenied
	}
	target, found := guild.Members[targetID]
	if !found || target.Grade == DefaultMasterGrade {
		return ErrNotMember
	}
	target.Grade = grade
	guild.Members[targetID] = target
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) SetGradeName(actorID string, grade byte, name string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade || grade < 1 || grade > 4 {
		return ErrPermissionDenied
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 12 {
		return ErrInvalidName
	}
	guild.GradeNames[grade] = name
	guild.UpdatedAt = now
	return nil
}

// GradePermissionMask is the width of op70's per-grade permission field. The
// editor window folds exactly 18 checkboxes into it (00FB4A87 cmp esi,0x12).
const GradePermissionMask = uint32(0x3ffff)

// DefaultGradePermissions is what op70 carried before the masks were stored:
// everything for the master, and the fixed subset grade 2 always had.
func DefaultGradePermissions(grade byte) uint32 {
	switch grade {
	case DefaultMasterGrade:
		return GradePermissionMask
	case 2:
		return 0x4400 | 1<<4
	default:
		return 0
	}
}

// GradePermissionsFor resolves the mask op70 should carry for grade.
func GradePermissionsFor(guild *Guild, grade byte) uint32 {
	if guild != nil {
		if mask, found := guild.GradePermissions[grade]; found {
			return mask & GradePermissionMask
		}
	}
	return DefaultGradePermissions(grade)
}

// Permissions resolves the 18-bit permission mask for one grade. It is the
// method form the 公会战 / 守护神 rules use; GradePermissionsFor is the package
// function the op70 projection uses. The two agree by construction -- the
// master is GradePermissionMask, an untouched grade 2 is 0x4400|1<<4, and every
// other untouched grade is zero.
func (g *Guild) Permissions(grade byte) uint32 {
	return GradePermissionsFor(g, grade)
}

// SetGradePermissions applies the mask op748 carries. The master's own row is
// not editable: ChangeGrade, Delegate and RemoveMember all key off grade 1
// being the full-authority grade, so letting it drop bits would lock the guild
// out of its own management. The client's editor does not offer it either.
// The ladder runs 2..5 to match the native grade table; an out-of-range bit is
// masked off rather than rejected so a client that sends the wider editor word
// still lands a usable mask.
func (r *Registry) SetGradePermissions(actorID string, grade byte, permissions uint32, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade || grade < 2 || grade > 5 {
		return ErrPermissionDenied
	}
	if guild.GradePermissions == nil {
		guild.GradePermissions = make(map[byte]uint32)
	}
	guild.GradePermissions[grade] = permissions & GradePermissionMask
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) SetMemo(actorID, characterID, memo string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found {
		return ErrNotMember
	}
	member, found := guild.Members[characterID]
	if !found {
		return ErrNotMember
	}
	if actorID != characterID && (actor.Grade > 2 || member.Grade <= actor.Grade) {
		return ErrPermissionDenied
	}
	member.Memo = strings.TrimSpace(memo)
	guild.Members[characterID] = member
	guild.UpdatedAt = now
	return nil
}

func (r *Registry) SetPromotion(actorID, promotion string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || guild.Permissions(actor.Grade)&(1<<5) == 0 {
		return ErrPermissionDenied
	}
	if ValidatePromotion(promotion) != nil {
		return ErrInvalidPromotion
	}
	guild.Promotion = strings.TrimSpace(promotion)
	guild.UpdatedAt = now
	return nil
}

// SetMark applies the 公会徽章 the master picked. Marks 0..MaxFreeGuildMark are
// free; the ones above go through the native purchase confirmation, whose
// sub_12FED40 text specifies GuildMarkPrice guild points (里程).
func (r *Registry) SetMark(actorID string, mark byte, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found {
		return ErrGuildNotFound
	}
	if actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	if mark > MaxGuildMark {
		return ErrInvalidPromotion
	}
	if guild.Mark == mark {
		return nil
	}
	if mark > MaxFreeGuildMark {
		if guild.Mileage < GuildMarkPrice {
			return ErrInsufficientMileage
		}
		guild.Mileage -= GuildMarkPrice
	}
	guild.Mark, guild.UpdatedAt = mark, now
	return nil
}

// SetRepresentative moves the per-account 代表角色 flag onto targetID. Both the
// actor and the target must be on the same account, which is what the client's
// 代表角色 selector enforces.
func (r *Registry) SetRepresentative(actorID, accountID, targetID string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found {
		return ErrNotMember
	}
	target, found := guild.Members[targetID]
	if !found {
		return ErrCharacterNotFound
	}
	if accountID == "" || actor.AccountID != accountID || target.AccountID != accountID {
		return ErrPermissionDenied
	}
	for memberID, member := range guild.Members {
		if member.AccountID == accountID {
			member.Representative = memberID == targetID
			guild.Members[memberID] = member
		}
	}
	guild.UpdatedAt = now
	return nil
}

// SetGuardian applies one 守护神 action from the 公会战/守护神 window: action 8
// recruits the guardian from costs (spending GuardianAP/GuardianGP), action 7
// fields an already-recruited one. Only the guild master may do either, and the
// roster is capped at five.
func (r *Registry) SetGuardian(characterID string, action byte, guardianID uint32, costs map[uint32]GuardianCost, now time.Time) error {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return ErrNotMember
	}
	if member.Grade != DefaultMasterGrade || guild.MasterCharacterID != characterID {
		return ErrPermissionDenied
	}
	cost, valid := costs[guardianID]
	if !valid || guardianID == 0 || (action != 7 && action != 8) {
		return ErrInvalidContent
	}
	for _, guardian := range guild.Guardians {
		if guardian.ID == guardianID {
			if action != 7 {
				return ErrContentActive
			}
			guild.ActiveGuardian, guild.UpdatedAt = guardianID, now
			return nil
		}
	}
	if action != 8 || len(guild.Guardians) >= 5 {
		return ErrInvalidContent
	}
	if guild.GuardianAP < cost.AP || guild.GuardianGP < cost.GP {
		return ErrInsufficientGuardian
	}
	guild.GuardianAP -= cost.AP
	guild.GuardianGP -= cost.GP
	guild.Guardians = append(guild.Guardians, Guardian{ID: guardianID, Level: 1})
	guild.ActiveGuardian, guild.UpdatedAt = guardianID, now
	return nil
}

// 公会基地 (op227/op228/op229/op231) state errors. The service layer maps each
// one to the code byte the client's toast tables expect.
var (
	ErrBaseExists  = errors.New("guild base already exists")
	ErrBaseMissing = errors.New("guild base does not exist")
	ErrBaseStage   = errors.New("guild base stage is invalid")
)

// CreateBase places 公会基地 for the actor's guild. Only the master may do it and
// the base must not exist yet; the client then unlocks 从赛丽亚房间进入公会基地.
func (r *Registry) CreateBase(actorID string, now time.Time) error {
	return r.setBaseStage(actorID, 1, false, now)
}

// DeleteBase removes the guild's base, dropping its stage back to zero.
func (r *Registry) DeleteBase(actorID string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	if guild.BaseStage == 0 {
		return ErrBaseMissing
	}
	guild.BaseStage, guild.UpdatedAt = 0, now
	return nil
}

// UpgradeBase moves the base to stage, which must sit inside 1..MaxBaseStage and
// be strictly higher than the current one.
func (r *Registry) UpgradeBase(actorID string, stage byte, now time.Time) error {
	return r.setBaseStage(actorID, stage, true, now)
}

func (r *Registry) setBaseStage(actorID string, stage byte, upgrade bool, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	if stage == 0 || stage > MaxBaseStage {
		return ErrBaseStage
	}
	if upgrade {
		if guild.BaseStage == 0 {
			return ErrBaseMissing
		}
		if stage <= guild.BaseStage {
			return ErrBaseStage
		}
	} else if guild.BaseStage != 0 {
		return ErrBaseExists
	}
	guild.BaseStage, guild.UpdatedAt = stage, now
	return nil
}

func (r *Registry) SetRecommendedChannel(actorID string, region, channel uint32, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	if region > 249 || channel == 0 || channel > 255 {
		return ErrInvalidPromotion
	}
	guild.RecommendedRegion, guild.RecommendedChannel, guild.UpdatedAt = region, channel, now
	return nil
}

// SetNotice stores the guild notice (公会公告). The current client sends it
// through op154 NOTIFY_MESSAGE_TO_GUILD and reads it back from op70's second
// DSTR, which it also pops once per session through a template string.
func (r *Registry) SetNotice(actorID, notice string, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade > 2 {
		return ErrPermissionDenied
	}
	if ValidateNotice(notice) != nil {
		return ErrInvalidPromotion
	}
	guild.Notice, guild.UpdatedAt = strings.TrimSpace(notice), now
	return nil
}

func (r *Registry) SetRecruitment(actorID string, mask uint32, promotion string, alliance bool, tags uint32, mode byte, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || mask&^guild.Permissions(actor.Grade) != 0 {
		return ErrPermissionDenied
	}
	if mask & ^uint32(0x20022) != 0 || mode > 2 || tags > 127 || ValidatePromotion(promotion) != nil {
		return ErrInvalidPromotion
	}
	var selectedTags []byte
	for tag := byte(0); tag < 7; tag++ {
		if tags&(1<<tag) != 0 {
			selectedTags = append(selectedTags, tag)
		}
	}
	if len(selectedTags) > 3 {
		return ErrInvalidPromotion
	}
	if mask&0x20 != 0 {
		guild.Promotion, guild.AllianceRecruitment = strings.TrimSpace(promotion), alliance
	}
	if mask&2 != 0 {
		guild.JoinMode = mode
	}
	if mask&0x20000 != 0 {
		guild.Tags = selectedTags
	}
	guild.UpdatedAt = now
	return nil
}

// 公会贡献 (公会积分) sources, from the client's own help texts (76511..76515):
//
//	76514 完成公会每日签到， 可以获得3点公会贡献。      -> MileageSignInPoints
//	76515 进行公会聊天， 可以获得2点公会贡献。          -> MileageChatPoints
//	76513 同公会成员组队通关地下城， 可以获得1点公会贡献。 -> MileagePartyPoints
//	76511 通关5次公会地下城， 可以获得10点公会贡献。     -> not wired (公会地下城
//	                                                       needs 公会基地 first)
//
// The row reason byte is what the client turns into the row label with
// id = 21000 - byte, so a source without a byte of its own would print another
// source's wording. 0 means "use the per-role 签到 fallback".
const (
	MileageSignInPoints = uint32(3)
	MileageChatPoints   = uint32(2)
	MileagePartyPoints  = uint32(1)

	// MileageReasonChat and MileageReasonParty are the op747 row bytes for the two
	// newer rules. 2 is 公会成员每日签到 and 0x0B is 公会会长每日签到, which is why
	// the sign-in rows leave Reason zero and let the reader pick per role.
	MileageReasonChat  = byte(10)
	MileageReasonParty = byte(12)
)

// Attend grants the once-per-local-day 公会签到 contribution, which is worth 3
// points (help text 76514) rather than the 1 this used to add.
func (r *Registry) Attend(characterID string, now time.Time) (bool, error) {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return false, ErrNotMember
	}
	if sameLocalDay(member.AttendedAt, now) {
		return false, nil
	}
	member.AttendedAt = now
	r.grantMileage(guild, characterID, member, MileageSignInPoints, 0, now)
	return true, nil
}

// GrantChatMileage grants the once-per-local-day 公会聊天 contribution (2 points).
func (r *Registry) GrantChatMileage(characterID string, now time.Time) (bool, error) {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return false, ErrNotMember
	}
	if sameLocalDay(member.ChatAt, now) {
		return false, nil
	}
	member.ChatAt = now
	r.grantMileage(guild, characterID, member, MileageChatPoints, MileageReasonChat, now)
	return true, nil
}

// GrantPartyMileage grants the 同公会成员组队通关地下城 contribution (1 point).
// partyCharacterIDs is the clearing party, actor included; the rule needs at
// least two of them inside the actor's guild (help text 76513), so a solo run or
// a party without a guildmate grants nothing. It is not a daily rule: every
// qualifying clear pays once.
func (r *Registry) GrantPartyMileage(characterID string, partyCharacterIDs []string, now time.Time) (bool, error) {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return false, ErrNotMember
	}
	guildmates := 0
	for _, partyMember := range partyCharacterIDs {
		if strings.TrimSpace(partyMember) == "" {
			continue
		}
		if _, isMember := guild.Members[partyMember]; isMember {
			guildmates++
		}
	}
	if guildmates < 2 {
		return false, nil
	}
	r.grantMileage(guild, characterID, member, MileagePartyPoints, MileageReasonParty, now)
	return true, nil
}

// grantMileage is the single writer for 公会贡献: it moves the member's own
// counter, the guild total and one history row together, and keeps the history
// bounded to the 200 rows the window renders.
func (r *Registry) grantMileage(guild *Guild, characterID string, member Member, amount uint32, reason byte, now time.Time) {
	member.Mileage += amount
	guild.Mileage += amount
	member.CharacterID = characterID
	guild.Members[characterID] = member
	guild.MileageHistory = append(guild.MileageHistory, MileageHistory{
		CharacterID: characterID,
		Name:        member.Name,
		Amount:      amount,
		CreatedAt:   now,
		Reason:      reason,
	})
	if len(guild.MileageHistory) > 200 {
		guild.MileageHistory = append([]MileageHistory(nil), guild.MileageHistory[len(guild.MileageHistory)-200:]...)
	}
	guild.UpdatedAt = now
}

func sameLocalDay(left, right time.Time) bool {
	if left.IsZero() || right.IsZero() {
		return false
	}
	ly, lm, ld := left.In(right.Location()).Date()
	ry, rm, rd := right.Date()
	return ly == ry && lm == rm && ld == rd
}

func SortedMembers(guild *Guild) []Member {
	if guild == nil {
		return nil
	}
	values := make([]Member, 0, len(guild.Members))
	for _, member := range guild.Members {
		values = append(values, member)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Grade != values[j].Grade {
			return values[i].Grade < values[j].Grade
		}
		return values[i].CharacterID < values[j].CharacterID
	})
	return values
}

func SortedApplications(guild *Guild) []Application {
	if guild == nil {
		return nil
	}
	values := make([]Application, 0, len(guild.Applications))
	for _, application := range guild.Applications {
		values = append(values, application)
	}
	sort.Slice(values, func(i, j int) bool {
		if !values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].CreatedAt.Before(values[j].CreatedAt)
		}
		return values[i].CharacterID < values[j].CharacterID
	})
	return values
}
