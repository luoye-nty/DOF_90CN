package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

func (m *Manager) ConfigurePrivateStore(ctx context.Context, id, title string, items []dnfrobotdomain.PrivateStoreItem) (RuntimeSnapshot, error) {
	title = strings.TrimSpace(title)
	encodedTitle, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(title))
	if err != nil || len(encodedTitle) == 0 || len(encodedTitle) > 80 {
		return RuntimeSnapshot{}, errors.New("private store title must be 1..80 bytes in GB18030")
	}
	if len(items) == 0 || len(items) > privateStoreMaximumRows {
		return RuntimeSnapshot{}, fmt.Errorf("private store requires 1..%d items", privateStoreMaximumRows)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	id = strings.TrimSpace(id)
	m.mu.RLock()
	config := dnfrobotdomain.CloneConfig(m.config)
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	found := false
	for index := range config.Profiles {
		if profileKey(config.Profiles[index].ID) != profileKey(id) {
			continue
		}
		config.Profiles[index].PrivateStoreTitle = title
		config.Profiles[index].PrivateStoreItems = append([]dnfrobotdomain.PrivateStoreItem(nil), items...)
		found = true
		break
	}
	if !found {
		return RuntimeSnapshot{}, ErrProfileNotFound
	}
	saved, err := m.owner.Save(ctx, config)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	profile, _ := findProfile(saved, id)
	m.mu.Lock()
	m.config = saved
	m.mu.Unlock()
	if runtime != nil {
		runtime.mu.Lock()
		runtime.profile.PrivateStoreTitle = title
		runtime.profile.PrivateStoreItems = append([]dnfrobotdomain.PrivateStoreItem(nil), items...)
		runtime.mu.Unlock()
	}
	return snapshotRuntime(profile, runtime), nil
}

func (m *Manager) OpenConfiguredPrivateStore(ctx context.Context, id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	client, profile, err := m.profileClient(id, StateTownReady)
	if err != nil {
		return err
	}
	if strings.TrimSpace(profile.PrivateStoreTitle) == "" || len(profile.PrivateStoreItems) == 0 {
		return errors.New("robot private store configuration is empty")
	}
	planner, ok := m.endpoint.(ConfiguredPrivateStorePlanner)
	if !ok {
		return errors.New("configured private store planner is unavailable")
	}
	m.mu.RLock()
	runtime := m.running[profileKey(id)]
	m.mu.RUnlock()
	if runtime != nil {
		runtime.mu.Lock()
		runtime.privateStoreTransition = true
		runtime.mu.Unlock()
		defer func() {
			runtime.mu.Lock()
			runtime.privateStoreTransition = false
			runtime.mu.Unlock()
		}()
	}
	snapshot := client.Snapshot()
	if snapshot.Village != profile.Village || snapshot.Area != profile.Area ||
		snapshot.X != profile.X || snapshot.Y != profile.Y {
		if err := client.Move(profile.Village, profile.Area, profile.X, profile.Y, profile.MoveType); err != nil {
			return fmt.Errorf("move robot to configured private store position: %w", err)
		}
	}
	rows, err := planner.PrepareRobotPrivateStoreRows(ctx, profile.AccountID, profile.CharacterID, profile.PrivateStoreItems)
	if err != nil {
		return err
	}
	return client.OpenPrivateStore(profile.PrivateStoreTitle, rows)
}
