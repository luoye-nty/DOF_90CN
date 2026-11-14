// Package robot owns the durable configuration of server-managed DNF robots.
package robot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	SettingsScope      = "dnf_robot_manager"
	settingsVersionKey = "version"
	settingsConfigKey  = "config"
	settingsVersion    = "1"
	MaxProfiles        = 1024
)

type RobotType string

const (
	RobotTypeProfessional  RobotType = "professional"
	RobotTypePersonalStore RobotType = "personal_store"
	RobotTypeNormal        RobotType = "normal"

	SpeechModeNormal    = "normal"
	SpeechModeMegaphone = "megaphone"

	DefaultPersonalStoreTitle     = "机器人个人商店"
	DefaultPersonalStoreItemID    = uint32(3037)
	DefaultPersonalStoreItemCount = uint32(10000)
	DefaultPersonalStoreItemPrice = uint32(200)
)

var (
	ErrUnavailable = errors.New("dnf robot settings repository is unavailable")
	ErrInvalid     = errors.New("dnf robot configuration is invalid")
)

// Profile identifies one real character that is driven through the current
// game protocol. Secrets are process configuration and are never persisted.
type Profile struct {
	ID                       string             `json:"id"`
	AccountID                string             `json:"account_id"`
	CharacterID              uint16             `json:"character_id"`
	RobotType                RobotType          `json:"robot_type,omitempty"`
	ChannelID                int                `json:"channel_id"`
	AutoStart                bool               `json:"auto_start"`
	Village                  uint8              `json:"village"`
	Area                     uint8              `json:"area"`
	X                        uint16             `json:"x"`
	Y                        uint16             `json:"y"`
	MoveType                 uint8              `json:"move_type,omitempty"`
	MoveBoundsSet            bool               `json:"move_bounds_set,omitempty"`
	MoveMinX                 uint16             `json:"move_min_x,omitempty"`
	MoveMaxX                 uint16             `json:"move_max_x,omitempty"`
	MoveMinY                 uint16             `json:"move_min_y,omitempty"`
	MoveMaxY                 uint16             `json:"move_max_y,omitempty"`
	AutomaticStoreDisabled   bool               `json:"automatic_store_disabled,omitempty"`
	ExpertStoreKind          string             `json:"expert_store_kind,omitempty"`
	ExpertStoreCost          *uint32            `json:"expert_store_cost,omitempty"`
	PrivateStoreTitle        string             `json:"private_store_title,omitempty"`
	PrivateStoreItems        []PrivateStoreItem `json:"private_store_items,omitempty"`
	MegaphoneEnabled         bool               `json:"megaphone_enabled,omitempty"`
	MoveIntervalMinSeconds   int                `json:"move_interval_min_seconds,omitempty"`
	MoveIntervalMaxSeconds   int                `json:"move_interval_max_seconds,omitempty"`
	MoveRadius               uint16             `json:"move_radius,omitempty"`
	SpeechMode               string             `json:"speech_mode,omitempty"`
	SpeechIntervalMinSeconds int                `json:"speech_interval_min_seconds,omitempty"`
	SpeechIntervalMaxSeconds int                `json:"speech_interval_max_seconds,omitempty"`
	SpeechMessages           []string           `json:"speech_messages,omitempty"`
	Recycle                  RecyclePolicy      `json:"recycle,omitempty"`
}

type PrivateStoreItem struct {
	ItemID uint32 `json:"item_id"`
	Count  uint32 `json:"count"`
	Price  uint32 `json:"price"`
}

type Config struct {
	Profiles              []Profile        `json:"profiles"`
	Automation            AutomationPolicy `json:"automation,omitempty"`
	Recycle               RecyclePolicy    `json:"recycle,omitempty"`
	AutomaticFleetVersion int              `json:"automatic_fleet_version,omitempty"`
}

// AutomationPolicy is the bounded town-only behavior policy recovered from
// the 2026-08-25 robot runtime. Private stores use the current client's
// op85/op87 acknowledgement chain; dungeon actions remain disabled until their
// current-client request/ack contracts are complete.
type AutomationPolicy struct {
	Enabled                     bool     `json:"enabled"`
	TargetOnline                int      `json:"target_online,omitempty"`
	ScaleIntervalSeconds        int      `json:"scale_interval_seconds,omitempty"`
	ScaleUpBatch                int      `json:"scale_up_batch,omitempty"`
	ScaleDownBatch              int      `json:"scale_down_batch,omitempty"`
	MoveIntervalMinSeconds      int      `json:"move_interval_min_seconds,omitempty"`
	MoveIntervalMaxSeconds      int      `json:"move_interval_max_seconds,omitempty"`
	MoveRadius                  uint16   `json:"move_radius,omitempty"`
	ChatIntervalMinSeconds      int      `json:"chat_interval_min_seconds,omitempty"`
	ChatIntervalMaxSeconds      int      `json:"chat_interval_max_seconds,omitempty"`
	ChatMode                    uint8    `json:"chat_mode,omitempty"`
	ChatMessages                []string `json:"chat_messages,omitempty"`
	MegaphoneIntervalMinSeconds int      `json:"megaphone_interval_min_seconds,omitempty"`
	MegaphoneIntervalMaxSeconds int      `json:"megaphone_interval_max_seconds,omitempty"`
	MegaphoneProfileModulo      int      `json:"megaphone_profile_modulo,omitempty"`
	MegaphoneMessages           []string `json:"megaphone_messages,omitempty"`
	StoreIntervalMinSeconds     int      `json:"store_interval_min_seconds,omitempty"`
	StoreIntervalMaxSeconds     int      `json:"store_interval_max_seconds,omitempty"`
	StoreDurationSeconds        int      `json:"store_duration_seconds,omitempty"`
	StoreConcurrent             int      `json:"store_concurrent,omitempty"`
	StoreMaximumRows            int      `json:"store_maximum_rows,omitempty"`
	StoreUnitPrice              uint32   `json:"store_unit_price,omitempty"`
	StoreTitle                  string   `json:"store_title,omitempty"`
}

// RecyclePolicy is the global price list used when a player opens a normal
// trade with an ambient robot. Only listed item IDs receive gold. Unlisted
// equipment may enter the configured equipment exchange flow; unlisted
// materials are accepted only as part of a transaction that already has a
// gold hit or an accepted equipment exchange.
type RecyclePolicy struct {
	Enabled  bool             `json:"enabled"`
	BuyItems []RecycleBuyItem `json:"buy_items,omitempty"`
}

type RecycleBuyItem struct {
	ItemID uint32 `json:"item_id"`
	Price  uint64 `json:"price"`
}

type Owner struct {
	provider func() (dnfrepo.Group, bool)
	now      func() time.Time
}

func NewOwner(provider func() (dnfrepo.Group, bool)) *Owner {
	return &Owner{provider: provider, now: time.Now}
}

func (o *Owner) Load(ctx context.Context) (Config, error) {
	repo, err := o.settings()
	if err != nil {
		return Config{}, err
	}
	record, ok, err := repo.Load(ctx, SettingsScope)
	if err != nil {
		return Config{}, err
	}
	if !ok {
		return Config{}, nil
	}
	if version := strings.TrimSpace(record.Values[settingsVersionKey]); version != "" && version != settingsVersion {
		return Config{}, fmt.Errorf("%w: unsupported version %q", ErrInvalid, version)
	}
	raw := strings.TrimSpace(record.Values[settingsConfigKey])
	if raw == "" {
		return Config{}, nil
	}
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return Config{}, fmt.Errorf("%w: decode: %v", ErrInvalid, err)
	}
	return Normalize(config)
}

func (o *Owner) Save(ctx context.Context, config Config) (Config, error) {
	normalized, err := Normalize(config)
	if err != nil {
		return Config{}, err
	}
	repo, err := o.settings()
	if err != nil {
		return Config{}, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC()
	if o != nil && o.now != nil {
		now = o.now().UTC()
	}
	record := dnfrepo.SettingsRecord{
		Scope: SettingsScope,
		Values: map[string]string{
			settingsVersionKey: settingsVersion,
			settingsConfigKey:  string(raw),
		},
		UpdatedAt: now,
	}
	if err := repo.Save(ctx, record); err != nil {
		return Config{}, err
	}
	return CloneConfig(normalized), nil
}

func Normalize(config Config) (Config, error) {
	if len(config.Profiles) > MaxProfiles {
		return Config{}, fmt.Errorf("%w: profile count %d exceeds %d", ErrInvalid, len(config.Profiles), MaxProfiles)
	}
	automation, err := normalizeAutomation(config.Automation)
	if err != nil {
		return Config{}, err
	}
	recycle, err := normalizeRecycle(config.Recycle)
	if err != nil {
		return Config{}, err
	}
	config.Profiles = migrateLegacyProfiles(config.Profiles, automation, recycle)
	if config.AutomaticFleetVersion < 0 || config.AutomaticFleetVersion > 7 {
		return Config{}, fmt.Errorf("%w: unsupported automatic fleet version %d", ErrInvalid, config.AutomaticFleetVersion)
	}
	result := Config{
		Profiles: make([]Profile, 0, len(config.Profiles)), Automation: automation, Recycle: recycle,
		AutomaticFleetVersion: config.AutomaticFleetVersion,
	}
	seenIDs := make(map[string]struct{}, len(config.Profiles))
	seenCharacters := make(map[uint16]struct{}, len(config.Profiles))
	for _, profile := range config.Profiles {
		profile.ID = strings.TrimSpace(profile.ID)
		profile.AccountID = strings.TrimSpace(profile.AccountID)
		if profile.ID == "" || len(profile.ID) > 64 || strings.IndexByte(profile.ID, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: invalid profile id", ErrInvalid)
		}
		if profile.AccountID == "" || len(profile.AccountID) > 96 || strings.IndexByte(profile.AccountID, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: profile %q has invalid account", ErrInvalid, profile.ID)
		}
		if profile.CharacterID == 0 || profile.ChannelID <= 0 || profile.ChannelID > 65535 {
			return Config{}, fmt.Errorf("%w: profile %q has invalid character/channel", ErrInvalid, profile.ID)
		}
		profile, err = normalizeProfileType(profile)
		if err != nil {
			return Config{}, err
		}
		profile.ExpertStoreKind = strings.ToLower(strings.TrimSpace(profile.ExpertStoreKind))
		if profile.ExpertStoreKind != "" && profile.ExpertStoreKind != "disjoint" && profile.ExpertStoreKind != "enchant" {
			return Config{}, fmt.Errorf("%w: profile %q has invalid expert store kind", ErrInvalid, profile.ID)
		}
		profile.PrivateStoreTitle = strings.TrimSpace(profile.PrivateStoreTitle)
		if len(profile.PrivateStoreItems) > 14 {
			return Config{}, fmt.Errorf("%w: profile %q has too many private store items", ErrInvalid, profile.ID)
		}
		if len(profile.PrivateStoreItems) > 0 && profile.PrivateStoreTitle == "" {
			return Config{}, fmt.Errorf("%w: profile %q has private store items without a title", ErrInvalid, profile.ID)
		}
		privateStoreItems := make(map[uint32]struct{}, len(profile.PrivateStoreItems))
		for _, item := range profile.PrivateStoreItems {
			if item.ItemID == 0 || item.Count == 0 || item.Price == 0 {
				return Config{}, fmt.Errorf("%w: profile %q has invalid private store item", ErrInvalid, profile.ID)
			}
			if _, duplicate := privateStoreItems[item.ItemID]; duplicate {
				return Config{}, fmt.Errorf("%w: profile %q has duplicate private store item %d", ErrInvalid, profile.ID, item.ItemID)
			}
			privateStoreItems[item.ItemID] = struct{}{}
		}
		if err := validateProfileTypeFields(profile); err != nil {
			return Config{}, err
		}
		if profile.MoveBoundsSet {
			if profile.MoveMinX > profile.MoveMaxX || profile.MoveMinY > profile.MoveMaxY ||
				profile.X < profile.MoveMinX || profile.X > profile.MoveMaxX ||
				profile.Y < profile.MoveMinY || profile.Y > profile.MoveMaxY {
				return Config{}, fmt.Errorf("%w: profile %q has invalid movement bounds", ErrInvalid, profile.ID)
			}
		} else if profile.MoveMinX != 0 || profile.MoveMaxX != 0 || profile.MoveMinY != 0 || profile.MoveMaxY != 0 {
			return Config{}, fmt.Errorf("%w: profile %q has movement bounds without ownership", ErrInvalid, profile.ID)
		}
		key := strings.ToLower(profile.ID)
		if _, ok := seenIDs[key]; ok {
			return Config{}, fmt.Errorf("%w: duplicate profile id %q", ErrInvalid, profile.ID)
		}
		if _, ok := seenCharacters[profile.CharacterID]; ok {
			return Config{}, fmt.Errorf("%w: duplicate character %d", ErrInvalid, profile.CharacterID)
		}
		seenIDs[key] = struct{}{}
		seenCharacters[profile.CharacterID] = struct{}{}
		result.Profiles = append(result.Profiles, profile)
	}
	sort.SliceStable(result.Profiles, func(i, j int) bool {
		return strings.ToLower(result.Profiles[i].ID) < strings.ToLower(result.Profiles[j].ID)
	})
	return result, nil
}

func migrateLegacyProfiles(
	profiles []Profile,
	automation AutomationPolicy,
	recycle RecyclePolicy,
) []Profile {
	result := append([]Profile(nil), profiles...)
	for index := range result {
		profile := &result[index]
		if profile.RobotType != "" {
			continue
		}
		switch {
		case strings.TrimSpace(profile.ExpertStoreKind) != "":
			profile.RobotType = RobotTypeProfessional
		case len(profile.PrivateStoreItems) > 0:
			profile.RobotType = RobotTypePersonalStore
		default:
			profile.RobotType = RobotTypeNormal
			if profile.MoveIntervalMinSeconds == 0 && profile.MoveIntervalMaxSeconds == 0 {
				profile.MoveIntervalMinSeconds = automation.MoveIntervalMinSeconds
				profile.MoveIntervalMaxSeconds = automation.MoveIntervalMaxSeconds
				profile.MoveRadius = automation.MoveRadius
			}
			if len(profile.SpeechMessages) == 0 {
				if profile.MegaphoneEnabled {
					profile.SpeechMode = SpeechModeMegaphone
					profile.SpeechMessages = append([]string(nil), automation.MegaphoneMessages...)
					profile.SpeechIntervalMinSeconds = automation.MegaphoneIntervalMinSeconds
					profile.SpeechIntervalMaxSeconds = automation.MegaphoneIntervalMaxSeconds
				} else {
					profile.SpeechMode = SpeechModeNormal
					profile.SpeechMessages = append([]string(nil), automation.ChatMessages...)
					profile.SpeechIntervalMinSeconds = automation.ChatIntervalMinSeconds
					profile.SpeechIntervalMaxSeconds = automation.ChatIntervalMaxSeconds
				}
			}
			if !profile.Recycle.Enabled && len(profile.Recycle.BuyItems) == 0 {
				profile.Recycle = cloneRecyclePolicy(recycle)
			}
		}
		profile.AutomaticStoreDisabled = true
	}
	return result
}

func normalizeProfileType(profile Profile) (Profile, error) {
	switch profile.RobotType {
	case RobotTypeProfessional, RobotTypePersonalStore, RobotTypeNormal:
	default:
		return Profile{}, fmt.Errorf("%w: profile %q has invalid robot type %q", ErrInvalid, profile.ID, profile.RobotType)
	}
	if profile.RobotType != RobotTypeNormal {
		profile.MoveIntervalMinSeconds = 0
		profile.MoveIntervalMaxSeconds = 0
		profile.MoveRadius = 0
		profile.Recycle = RecyclePolicy{}
	}
	profile.MoveIntervalMinSeconds = maxIntValue(profile.MoveIntervalMinSeconds, 0)
	profile.MoveIntervalMaxSeconds = maxIntValue(profile.MoveIntervalMaxSeconds, 0)
	if (profile.MoveIntervalMinSeconds == 0) != (profile.MoveIntervalMaxSeconds == 0) ||
		profile.MoveIntervalMinSeconds > profile.MoveIntervalMaxSeconds ||
		profile.MoveIntervalMaxSeconds > 3600 {
		return Profile{}, fmt.Errorf("%w: profile %q has invalid movement interval", ErrInvalid, profile.ID)
	}
	if profile.MoveIntervalMinSeconds > 0 {
		if profile.MoveRadius == 0 {
			profile.MoveRadius = 80
		}
		if profile.MoveRadius > 2000 {
			return Profile{}, fmt.Errorf("%w: profile %q movement radius exceeds 2000", ErrInvalid, profile.ID)
		}
	} else {
		profile.MoveRadius = 0
	}
	profile.SpeechMode = strings.ToLower(strings.TrimSpace(profile.SpeechMode))
	if len(profile.SpeechMessages) > 64 {
		return Profile{}, fmt.Errorf("%w: profile %q speech message count exceeds 64", ErrInvalid, profile.ID)
	}
	messages := make([]string, 0, len(profile.SpeechMessages))
	for _, message := range profile.SpeechMessages {
		message = strings.TrimSpace(message)
		if message == "" || len(message) > 512 || strings.IndexByte(message, 0) >= 0 {
			return Profile{}, fmt.Errorf("%w: profile %q has invalid speech message", ErrInvalid, profile.ID)
		}
		messages = append(messages, message)
	}
	profile.SpeechMessages = messages
	if len(messages) == 0 {
		if profile.SpeechMode == "" {
			profile.SpeechMode = SpeechModeNormal
		}
		if profile.SpeechMode != SpeechModeNormal && profile.SpeechMode != SpeechModeMegaphone {
			return Profile{}, fmt.Errorf("%w: profile %q has invalid speech mode", ErrInvalid, profile.ID)
		}
		profile.SpeechIntervalMinSeconds = 0
		profile.SpeechIntervalMaxSeconds = 0
		profile.MegaphoneEnabled = profile.SpeechMode == SpeechModeMegaphone
	} else {
		if profile.SpeechMode == "" {
			profile.SpeechMode = SpeechModeNormal
		}
		if profile.SpeechMode != SpeechModeNormal && profile.SpeechMode != SpeechModeMegaphone {
			return Profile{}, fmt.Errorf("%w: profile %q has invalid speech mode", ErrInvalid, profile.ID)
		}
		if profile.SpeechIntervalMinSeconds == 0 && profile.SpeechIntervalMaxSeconds == 0 {
			profile.SpeechIntervalMinSeconds = 60
			profile.SpeechIntervalMaxSeconds = 180
		}
		if profile.SpeechIntervalMinSeconds < 1 || profile.SpeechIntervalMaxSeconds < 1 ||
			profile.SpeechIntervalMinSeconds > profile.SpeechIntervalMaxSeconds ||
			profile.SpeechIntervalMaxSeconds > 86400 {
			return Profile{}, fmt.Errorf("%w: profile %q has invalid speech interval", ErrInvalid, profile.ID)
		}
		profile.MegaphoneEnabled = profile.SpeechMode == SpeechModeMegaphone
	}
	recycle, err := normalizeRecycle(profile.Recycle)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q recycle: %w", profile.ID, err)
	}
	profile.Recycle = recycle
	return profile, nil
}

func validateProfileTypeFields(profile Profile) error {
	switch profile.RobotType {
	case RobotTypeProfessional:
		if profile.ExpertStoreKind == "" {
			return fmt.Errorf("%w: professional robot %q requires an expert store kind", ErrInvalid, profile.ID)
		}
		if len(profile.PrivateStoreItems) > 0 {
			return fmt.Errorf("%w: professional robot %q cannot own a personal store", ErrInvalid, profile.ID)
		}
	case RobotTypePersonalStore:
		if profile.ExpertStoreKind != "" {
			return fmt.Errorf("%w: personal-store robot %q cannot own an expert store", ErrInvalid, profile.ID)
		}
		if profile.PrivateStoreTitle == "" || len(profile.PrivateStoreItems) == 0 {
			return fmt.Errorf("%w: personal-store robot %q requires a title and items", ErrInvalid, profile.ID)
		}
	case RobotTypeNormal:
		if profile.ExpertStoreKind != "" || len(profile.PrivateStoreItems) > 0 || profile.PrivateStoreTitle != "" {
			return fmt.Errorf("%w: normal robot %q cannot own a personal or expert store", ErrInvalid, profile.ID)
		}
	}
	return nil
}

func cloneRecyclePolicy(policy RecyclePolicy) RecyclePolicy {
	policy.BuyItems = append([]RecycleBuyItem(nil), policy.BuyItems...)
	return policy
}

func maxIntValue(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func CloneConfig(config Config) Config {
	result := Config{
		Profiles: append([]Profile(nil), config.Profiles...), Automation: config.Automation,
		Recycle:               config.Recycle,
		AutomaticFleetVersion: config.AutomaticFleetVersion,
	}
	result.Automation.ChatMessages = append([]string(nil), config.Automation.ChatMessages...)
	result.Automation.MegaphoneMessages = append([]string(nil), config.Automation.MegaphoneMessages...)
	result.Recycle.BuyItems = append([]RecycleBuyItem(nil), config.Recycle.BuyItems...)
	for index := range result.Profiles {
		result.Profiles[index].PrivateStoreItems = append([]PrivateStoreItem(nil), config.Profiles[index].PrivateStoreItems...)
		result.Profiles[index].SpeechMessages = append([]string(nil), config.Profiles[index].SpeechMessages...)
		result.Profiles[index].Recycle = cloneRecyclePolicy(config.Profiles[index].Recycle)
	}
	return result
}

func normalizeRecycle(policy RecyclePolicy) (RecyclePolicy, error) {
	if len(policy.BuyItems) > 2000 {
		return RecyclePolicy{}, fmt.Errorf("%w: robot recycle item count exceeds 2000", ErrInvalid)
	}
	items := make([]RecycleBuyItem, 0, len(policy.BuyItems))
	seen := make(map[uint32]struct{}, len(policy.BuyItems))
	for _, item := range policy.BuyItems {
		if item.ItemID == 0 || item.Price == 0 {
			return RecyclePolicy{}, fmt.Errorf("%w: robot recycle item and price must be positive", ErrInvalid)
		}
		if _, duplicate := seen[item.ItemID]; duplicate {
			return RecyclePolicy{}, fmt.Errorf("%w: duplicate robot recycle item %d", ErrInvalid, item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ItemID < items[j].ItemID })
	return RecyclePolicy{Enabled: policy.Enabled, BuyItems: items}, nil
}

func normalizeAutomation(policy AutomationPolicy) (AutomationPolicy, error) {
	if policy.TargetOnline < 0 || policy.TargetOnline > MaxProfiles {
		return AutomationPolicy{}, fmt.Errorf("%w: automation target online exceeds %d", ErrInvalid, MaxProfiles)
	}
	if policy.TargetOnline > 0 {
		if policy.ScaleIntervalSeconds == 0 {
			policy.ScaleIntervalSeconds = 5
		}
		if policy.ScaleUpBatch == 0 {
			policy.ScaleUpBatch = 2
		}
		if policy.ScaleDownBatch == 0 {
			policy.ScaleDownBatch = 2
		}
		if policy.ScaleIntervalSeconds < 1 || policy.ScaleIntervalSeconds > 300 ||
			policy.ScaleUpBatch < 1 || policy.ScaleUpBatch > 64 || policy.ScaleDownBatch < 1 || policy.ScaleDownBatch > 64 {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation scaling policy", ErrInvalid)
		}
	}
	if policy.MoveIntervalMinSeconds < 0 || policy.MoveIntervalMaxSeconds < 0 ||
		policy.ChatIntervalMinSeconds < 0 || policy.ChatIntervalMaxSeconds < 0 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation intervals must not be negative", ErrInvalid)
	}
	if (policy.MoveIntervalMinSeconds == 0) != (policy.MoveIntervalMaxSeconds == 0) ||
		policy.MoveIntervalMinSeconds > policy.MoveIntervalMaxSeconds || policy.MoveIntervalMaxSeconds > 3600 {
		return AutomationPolicy{}, fmt.Errorf("%w: invalid automation move interval", ErrInvalid)
	}
	if policy.MoveIntervalMinSeconds > 0 {
		if policy.MoveRadius == 0 {
			policy.MoveRadius = 80
		}
		if policy.MoveRadius > 2000 {
			return AutomationPolicy{}, fmt.Errorf("%w: automation move radius exceeds 2000", ErrInvalid)
		}
	}
	if (policy.ChatIntervalMinSeconds == 0) != (policy.ChatIntervalMaxSeconds == 0) ||
		policy.ChatIntervalMinSeconds > policy.ChatIntervalMaxSeconds || policy.ChatIntervalMaxSeconds > 86400 {
		return AutomationPolicy{}, fmt.Errorf("%w: invalid automation chat interval", ErrInvalid)
	}
	if policy.ChatMode == 0 {
		policy.ChatMode = 3
	}
	if len(policy.ChatMessages) > 64 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation chat message count exceeds 64", ErrInvalid)
	}
	messages := make([]string, 0, len(policy.ChatMessages))
	for _, message := range policy.ChatMessages {
		message = strings.TrimSpace(message)
		if message == "" || len(message) > 512 || strings.IndexByte(message, 0) >= 0 {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation chat message", ErrInvalid)
		}
		messages = append(messages, message)
	}
	policy.ChatMessages = messages
	if policy.ChatIntervalMinSeconds > 0 && len(messages) == 0 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation chat interval requires messages", ErrInvalid)
	}
	if len(policy.MegaphoneMessages) > 64 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation megaphone message count exceeds 64", ErrInvalid)
	}
	megaphoneMessages := make([]string, 0, len(policy.MegaphoneMessages))
	for _, message := range policy.MegaphoneMessages {
		message = strings.TrimSpace(message)
		if message == "" || len(message) > 512 || strings.IndexByte(message, 0) >= 0 {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation megaphone message", ErrInvalid)
		}
		megaphoneMessages = append(megaphoneMessages, message)
	}
	if policy.MegaphoneIntervalMinSeconds < 0 || policy.MegaphoneIntervalMaxSeconds < 0 || policy.MegaphoneProfileModulo < 0 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation megaphone values must not be negative", ErrInvalid)
	}
	if (policy.MegaphoneIntervalMinSeconds == 0) != (policy.MegaphoneIntervalMaxSeconds == 0) ||
		policy.MegaphoneIntervalMinSeconds > policy.MegaphoneIntervalMaxSeconds || policy.MegaphoneIntervalMaxSeconds > 86400 {
		return AutomationPolicy{}, fmt.Errorf("%w: invalid automation megaphone interval", ErrInvalid)
	}
	if policy.MegaphoneIntervalMinSeconds > 0 {
		if len(megaphoneMessages) == 0 {
			// Backward compatibility for configurations created before the
			// dedicated megaphone message pool existed.
			megaphoneMessages = append([]string(nil), messages...)
		}
		if policy.MegaphoneProfileModulo == 0 {
			policy.MegaphoneProfileModulo = 1
		}
		if len(megaphoneMessages) == 0 || policy.MegaphoneProfileModulo < 1 || policy.MegaphoneProfileModulo > MaxProfiles {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation megaphone policy", ErrInvalid)
		}
	}
	policy.MegaphoneMessages = megaphoneMessages
	if policy.StoreIntervalMinSeconds < 0 || policy.StoreIntervalMaxSeconds < 0 || policy.StoreDurationSeconds < 0 || policy.StoreConcurrent < 0 || policy.StoreMaximumRows < 0 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation store values must not be negative", ErrInvalid)
	}
	if (policy.StoreIntervalMinSeconds == 0) != (policy.StoreIntervalMaxSeconds == 0) ||
		policy.StoreIntervalMinSeconds > policy.StoreIntervalMaxSeconds || policy.StoreIntervalMaxSeconds > 86400 {
		return AutomationPolicy{}, fmt.Errorf("%w: invalid automation store interval", ErrInvalid)
	}
	policy.StoreTitle = strings.TrimSpace(policy.StoreTitle)
	if policy.StoreIntervalMinSeconds > 0 {
		if policy.StoreDurationSeconds <= 0 || policy.StoreDurationSeconds > 86400 || policy.StoreUnitPrice == 0 ||
			policy.StoreTitle == "" || len(policy.StoreTitle) > 80 || strings.IndexByte(policy.StoreTitle, 0) >= 0 {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation private store", ErrInvalid)
		}
		if policy.StoreConcurrent == 0 {
			policy.StoreConcurrent = 1
		}
		if policy.StoreConcurrent > 64 {
			return AutomationPolicy{}, fmt.Errorf("%w: automation store concurrency exceeds 64", ErrInvalid)
		}
		if policy.StoreMaximumRows == 0 {
			policy.StoreMaximumRows = 8
		}
		if policy.StoreMaximumRows > 14 {
			return AutomationPolicy{}, fmt.Errorf("%w: automation store rows exceed 14", ErrInvalid)
		}
	}
	return policy, nil
}

func (o *Owner) settings() (dnfrepo.SettingsRepository, error) {
	if o == nil || o.provider == nil {
		return nil, ErrUnavailable
	}
	group, ok := o.provider()
	if !ok || group.Settings == nil {
		return nil, ErrUnavailable
	}
	return group.Settings, nil
}
