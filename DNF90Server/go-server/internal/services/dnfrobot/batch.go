package dnfrobot

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	BatchActionStart      = "start"
	BatchActionStop       = "stop"
	BatchActionMove       = "move"
	BatchActionChat       = "chat"
	BatchActionStoreOpen  = "store_open"
	BatchActionStoreClose = "store_close"
)

type BatchRequest struct {
	Action     string   `json:"action"`
	IDs        []string `json:"ids,omitempty"`
	Village    uint8    `json:"village,omitempty"`
	Area       uint8    `json:"area,omitempty"`
	X          uint16   `json:"x,omitempty"`
	Y          uint16   `json:"y,omitempty"`
	MoveType   uint8    `json:"move_type,omitempty"`
	ChatMode   uint8    `json:"chat_mode,omitempty"`
	Message    string   `json:"message,omitempty"`
	StoreTitle string   `json:"store_title,omitempty"`
	StoreRows  int      `json:"store_rows,omitempty"`
	StorePrice uint32   `json:"store_price,omitempty"`
}

type BatchActionResult struct {
	ID       string          `json:"id"`
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Snapshot RuntimeSnapshot `json:"snapshot"`
}

type BatchResult struct {
	Action    string              `json:"action"`
	Requested int                 `json:"requested"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	Results   []BatchActionResult `json:"results"`
}

type ManagerStatus struct {
	Configured       int `json:"configured"`
	Running          int `json:"running"`
	Connecting       int `json:"connecting"`
	TownReady        int `json:"town_ready"`
	StoreReady       int `json:"store_ready"`
	Pending          int `json:"pending"`
	Errors           int `json:"errors"`
	Reconnects       int `json:"reconnects"`
	TargetOnline     int `json:"target_online"`
	SchedulerManaged int `json:"scheduler_managed"`
}

func (m *Manager) Status() ManagerStatus {
	status := ManagerStatus{TargetOnline: m.Automation().TargetOnline}
	for _, snapshot := range m.List() {
		status.Configured++
		status.Reconnects += snapshot.Reconnects
		if snapshot.Running {
			status.Running++
		}
		if snapshot.SchedulerManaged {
			status.SchedulerManaged++
		}
		switch snapshot.State {
		case StateConnecting, StateBinding, StateSelecting:
			status.Connecting++
		case StateTownReady:
			status.TownReady++
		case StateStoreReady:
			status.StoreReady++
		case StateStoreOpening, StateStoreClosing:
			status.Pending++
		case StateError:
			status.Errors++
		}
		if snapshot.LastError != "" && snapshot.State != StateError {
			status.Errors++
		}
	}
	return status
}

func (m *Manager) Batch(ctx context.Context, request BatchRequest) (BatchResult, error) {
	if m == nil {
		return BatchResult{}, ErrManagerStopped
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	switch action {
	case BatchActionStart, BatchActionStop, BatchActionMove, BatchActionChat, BatchActionStoreOpen, BatchActionStoreClose:
	default:
		return BatchResult{}, errors.New("invalid dnf robot batch action")
	}
	ids := m.batchProfileIDs(request.IDs)
	if len(ids) == 0 {
		return BatchResult{}, ErrProfileNotFound
	}
	operation := m.BeginOperation("batch", action)
	result := BatchResult{Action: action, Requested: len(ids), Results: make([]BatchActionResult, 0, len(ids))}
	for _, id := range ids {
		var (
			snapshot RuntimeSnapshot
			err      error
		)
		switch action {
		case BatchActionStart:
			snapshot, err = m.StartProfile(id)
		case BatchActionStop:
			snapshot, err = m.StopProfile(id)
		case BatchActionMove:
			err = m.Move(id, request.Village, request.Area, request.X, request.Y, request.MoveType)
			snapshot = m.snapshotByID(id)
		case BatchActionChat:
			err = m.Chat(id, request.ChatMode, request.Message)
			snapshot = m.snapshotByID(id)
		case BatchActionStoreOpen:
			err = m.OpenPrivateStore(ctx, id, request.StoreTitle, request.StoreRows, request.StorePrice)
			snapshot = m.snapshotByID(id)
		case BatchActionStoreClose:
			err = m.ClosePrivateStore(id)
			snapshot = m.snapshotByID(id)
		}
		row := BatchActionResult{ID: id, OK: err == nil, Snapshot: snapshot}
		if err != nil {
			row.Error = err.Error()
			result.Failed++
		} else {
			result.Succeeded++
		}
		result.Results = append(result.Results, row)
	}
	m.CompleteOperation(operation.ID, fmt.Sprintf("requested=%d succeeded=%d failed=%d", result.Requested, result.Succeeded, result.Failed), nil)
	return result, nil
}

func (m *Manager) batchProfileIDs(requested []string) []string {
	if len(requested) == 0 {
		m.mu.RLock()
		ids := make([]string, 0, len(m.config.Profiles))
		for _, profile := range m.config.Profiles {
			ids = append(ids, profile.ID)
		}
		m.mu.RUnlock()
		return ids
	}
	seen := make(map[string]struct{}, len(requested))
	ids := make([]string, 0, len(requested))
	for _, id := range requested {
		id = strings.TrimSpace(id)
		key := profileKey(id)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) snapshotByID(id string) RuntimeSnapshot {
	for _, snapshot := range m.List() {
		if profileKey(snapshot.Profile.ID) == profileKey(id) {
			return snapshot
		}
	}
	return RuntimeSnapshot{}
}
