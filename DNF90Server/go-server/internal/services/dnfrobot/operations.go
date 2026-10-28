package dnfrobot

import (
	"strings"
	"time"
)

const maximumOperationHistory = 20

type OperationStatus struct {
	ID         int64     `json:"id"`
	Type       string    `json:"type"`
	Scope      string    `json:"scope,omitempty"`
	State      string    `json:"state"`
	Summary    string    `json:"summary,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (m *Manager) BeginOperation(operationType, scope string) OperationStatus {
	if m == nil {
		return OperationStatus{}
	}
	now := time.Now().UTC()
	m.historyMu.Lock()
	m.nextOperationID++
	operation := OperationStatus{
		ID: m.nextOperationID, Type: strings.TrimSpace(operationType), Scope: strings.TrimSpace(scope),
		State: "running", StartedAt: now, UpdatedAt: now,
	}
	m.operations = append([]OperationStatus{operation}, m.operations...)
	if len(m.operations) > maximumOperationHistory {
		m.operations = m.operations[:maximumOperationHistory]
	}
	m.historyMu.Unlock()
	return operation
}

func (m *Manager) CompleteOperation(id int64, summary string, operationErr error) OperationStatus {
	if m == nil {
		return OperationStatus{}
	}
	now := time.Now().UTC()
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	for index := range m.operations {
		if m.operations[index].ID != id {
			continue
		}
		m.operations[index].Summary = strings.TrimSpace(summary)
		m.operations[index].UpdatedAt = now
		m.operations[index].FinishedAt = now
		if operationErr != nil {
			m.operations[index].State = "failed"
			m.operations[index].Error = operationErr.Error()
		} else {
			m.operations[index].State = "done"
		}
		return m.operations[index]
	}
	return OperationStatus{ID: id, State: "unknown", Summary: strings.TrimSpace(summary), UpdatedAt: now}
}

func (m *Manager) Operations() []OperationStatus {
	if m == nil {
		return nil
	}
	m.historyMu.Lock()
	result := append([]OperationStatus(nil), m.operations...)
	m.historyMu.Unlock()
	return result
}
