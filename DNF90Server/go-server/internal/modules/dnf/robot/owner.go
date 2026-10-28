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

var (
	ErrUnavailable = errors.New("dnf robot settings repository is unavailable")
	ErrInvalid     = errors.New("dnf robot configuration is invalid")
)

// Profile identifies one real character that is driven through the current
// game protocol. Secrets are process configuration and are never persisted.
type Profile struct {
	ID                     string `json:"id"`
	AccountID              string `json:"account_id"`
	CharacterID            uint16 `json:"character_id"`
	ChannelID              int    `json:"channel_id"`
	AutoStart              bool   `json:"auto_start"`
	Village                uint8  `json:"village"`
	Area                   uint8  `json:"area"`
	X                      uint16 `json:"x"`
	Y                      uint16 `json:"y"`
	MoveType               uint8  `json:"move_type,omitempty"`
	MoveBoundsSet          bool   `json:"move_bounds_set,omitempty"`
	MoveMinX               uint16 `json:"move_min_x,omitempty"`
	MoveMaxX               uint16 `json:"move_max_x,omitempty"`
	MoveMinY               uint16 `json:"move_min_y,omitempty"`
	MoveMaxY               uint16 `json:"move_max_y,omitempty"`
	AutomaticStoreDisabled bool   `json:"automatic_store_disabled,omitempty"`
}

type Config struct {
	Profiles              []Profile        `json:"profiles"`
	Automation            AutomationPolicy `json:"automation,omitempty"`
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
	StoreIntervalMinSeconds     int      `json:"store_interval_min_seconds,omitempty"`
	StoreIntervalMaxSeconds     int      `json:"store_interval_max_seconds,omitempty"`
	StoreDurationSeconds        int      `json:"store_duration_seconds,omitempty"`
	StoreConcurrent             int      `json:"store_concurrent,omitempty"`
	StoreMaximumRows            int      `json:"store_maximum_rows,omitempty"`
	StoreUnitPrice              uint32   `json:"store_unit_price,omitempty"`
	StoreTitle                  string   `json:"store_title,omitempty"`
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
	if config.AutomaticFleetVersion < 0 || config.AutomaticFleetVersion > 7 {
		return Config{}, fmt.Errorf("%w: unsupported automatic fleet version %d", ErrInvalid, config.AutomaticFleetVersion)
	}
	result := Config{
		Profiles: make([]Profile, 0, len(config.Profiles)), Automation: automation,
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

func CloneConfig(config Config) Config {
	result := Config{
		Profiles: append([]Profile(nil), config.Profiles...), Automation: config.Automation,
		AutomaticFleetVersion: config.AutomaticFleetVersion,
	}
	result.Automation.ChatMessages = append([]string(nil), config.Automation.ChatMessages...)
	return result
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
	if policy.MegaphoneIntervalMinSeconds < 0 || policy.MegaphoneIntervalMaxSeconds < 0 || policy.MegaphoneProfileModulo < 0 {
		return AutomationPolicy{}, fmt.Errorf("%w: automation megaphone values must not be negative", ErrInvalid)
	}
	if (policy.MegaphoneIntervalMinSeconds == 0) != (policy.MegaphoneIntervalMaxSeconds == 0) ||
		policy.MegaphoneIntervalMinSeconds > policy.MegaphoneIntervalMaxSeconds || policy.MegaphoneIntervalMaxSeconds > 86400 {
		return AutomationPolicy{}, fmt.Errorf("%w: invalid automation megaphone interval", ErrInvalid)
	}
	if policy.MegaphoneIntervalMinSeconds > 0 {
		if len(messages) == 0 || policy.MegaphoneProfileModulo < 1 || policy.MegaphoneProfileModulo > MaxProfiles {
			return AutomationPolicy{}, fmt.Errorf("%w: invalid automation megaphone policy", ErrInvalid)
		}
	}
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
