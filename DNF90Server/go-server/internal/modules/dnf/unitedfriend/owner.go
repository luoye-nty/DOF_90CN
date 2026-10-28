package unitedfriend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

const MaximumFriends = 100

// UnknownCharacterID is the current client's op290 "target id not known"
// sentinel. The friend window adds by typed name and has no actor to read an
// id from, so it fills 0xffff; only a scene target (right-click a nearby
// player) carries a real character id. Comparing the sentinel against the
// resolved record's id rejected every add made from the friend window.
const UnknownCharacterID = uint16(0xffff)

var (
	ErrRepositoryUnavailable = errors.New("united friend repository is unavailable")
	ErrCharacterNotFound     = errors.New("united friend character was not found")
	ErrSelf                  = errors.New("cannot add self as united friend")
	ErrFull                  = errors.New("united friend list is full")
	ErrRelationNotFound      = errors.New("united friend relation was not found")
)

type Owner struct {
	mu         sync.Mutex
	friends    repository.UnitedFriendRepository
	characters repository.CharacterRepository
	now        func() time.Time
}

type Friend struct {
	Character repository.CharacterRecord
}

func NewOwner(group repository.Group, now func() time.Time) (*Owner, error) {
	if group.UnitedFriend == nil || group.Character == nil {
		return nil, ErrRepositoryUnavailable
	}
	if now == nil {
		now = time.Now
	}
	return &Owner{friends: group.UnitedFriend, characters: group.Character, now: now}, nil
}

func (o *Owner) List(ctx context.Context, ownerName string) ([]Friend, error) {
	record, found, err := o.friends.Load(ctx, strings.TrimSpace(ownerName))
	if err != nil || !found {
		return nil, err
	}
	out := make([]Friend, 0, len(record.FriendNames))
	for _, name := range record.FriendNames {
		character, found, err := o.characterByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, Friend{Character: character})
		}
	}
	return out, nil
}

func (o *Owner) Add(ctx context.Context, ownerName string, ownerCharacterID uint16, targetName string, targetCharacterID uint16) (Friend, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ownerName = strings.TrimSpace(ownerName)
	targetName = strings.TrimSpace(targetName)
	target, found, err := o.characterByName(ctx, targetName)
	if err != nil {
		return Friend{}, err
	}
	if !found {
		return Friend{}, ErrCharacterNotFound
	}
	resolvedID := numericCharacterID(target.CharacterID)
	if targetCharacterID != 0 && targetCharacterID != UnknownCharacterID && resolvedID != targetCharacterID {
		return Friend{}, ErrCharacterNotFound
	}
	if targetName == ownerName || (ownerCharacterID != 0 && ownerCharacterID == resolvedID) {
		return Friend{}, ErrSelf
	}
	record, found, err := o.friends.Load(ctx, ownerName)
	if err != nil {
		return Friend{}, err
	}
	if !found {
		record = repository.UnitedFriendRecord{OwnerName: ownerName}
	}
	if slices.Contains(record.FriendNames, targetName) {
		return Friend{Character: target}, nil
	}
	if len(record.FriendNames) >= MaximumFriends {
		return Friend{}, ErrFull
	}
	record.FriendNames = append(record.FriendNames, targetName)
	if record.FriendSince == nil {
		record.FriendSince = make(map[string]time.Time, len(record.FriendNames))
	}
	// 记录这条关系**建立**的时间：商城赠送要求"互为好友满 7 天"。
	// 注意不能覆盖已存在的时间（重复 Add 已在上面 return）。
	now := o.now().UTC()
	if _, exists := record.FriendSince[targetName]; !exists {
		record.FriendSince[targetName] = now
	}
	record.UpdatedAt = now
	if err := o.friends.Save(ctx, record); err != nil {
		return Friend{}, err
	}
	return Friend{Character: target}, nil
}

// FriendshipSince 返回 ownerName→friendName 这条关系建立了多久，以及这个时间是否可信。
//
// trusted=false 表示这条关系早于 friend_since 字段上线（老数据）：调用方必须按
// "已满足 7 天"处理，否则新规则会追溯封锁所有老好友。
func (o *Owner) FriendshipSince(ctx context.Context, ownerName, friendName string) (time.Time, bool, bool, error) {
	record, found, err := o.friends.Load(ctx, strings.TrimSpace(ownerName))
	if err != nil || !found {
		return time.Time{}, false, found, err
	}
	if !slices.Contains(record.FriendNames, strings.TrimSpace(friendName)) {
		return time.Time{}, false, false, nil
	}
	at, trusted := repository.UnitedFriendSince(record, friendName)
	return at, trusted, true, nil
}

func (o *Owner) Remove(ctx context.Context, ownerName, targetName string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	ownerName = strings.TrimSpace(ownerName)
	targetName = strings.TrimSpace(targetName)
	record, found, err := o.friends.Load(ctx, ownerName)
	if err != nil {
		return err
	}
	if !found {
		return ErrRelationNotFound
	}
	index := slices.Index(record.FriendNames, targetName)
	if index < 0 {
		return ErrRelationNotFound
	}
	record.FriendNames = append(record.FriendNames[:index], record.FriendNames[index+1:]...)
	record.UpdatedAt = o.now().UTC()
	return o.friends.Save(ctx, record)
}

func (o *Owner) Observers(ctx context.Context, friendName string) ([]string, error) {
	return o.friends.ListObservers(ctx, strings.TrimSpace(friendName))
}

// DeleteCharacter removes both outgoing and incoming one-way edges and
// returns the owners whose visible list must be refreshed.
func (o *Owner) DeleteCharacter(ctx context.Context, name string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	name = strings.TrimSpace(name)
	observers, err := o.friends.ListObservers(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, ownerName := range observers {
		record, found, err := o.friends.Load(ctx, ownerName)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		record.FriendNames = slices.DeleteFunc(record.FriendNames, func(value string) bool { return value == name })
		record.UpdatedAt = o.now().UTC()
		if err := o.friends.Save(ctx, record); err != nil {
			return nil, err
		}
	}
	if err := o.friends.Save(ctx, repository.UnitedFriendRecord{OwnerName: name, UpdatedAt: o.now().UTC()}); err != nil {
		return nil, err
	}
	return observers, nil
}

func (o *Owner) characterByName(ctx context.Context, name string) (repository.CharacterRecord, bool, error) {
	id, found, err := o.characters.FindIDByName(ctx, name)
	if err != nil || !found {
		return repository.CharacterRecord{}, found, err
	}
	record, found, err := o.characters.Load(ctx, id)
	if err != nil || !found {
		return repository.CharacterRecord{}, found, err
	}
	if record.Stats["delete_flag"] != 0 || record.Name != name {
		return repository.CharacterRecord{}, false, nil
	}
	return record, true, nil
}

func numericCharacterID(value string) uint16 {
	var id uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &id); err != nil || id > 0xffff {
		return 0
	}
	return uint16(id)
}
