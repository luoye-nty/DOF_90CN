package main

import (
	"sync"
	"time"
)

type batchTarget struct {
	AccountID   string
	Username    string
	CharacterID string
	Name        string
	Online      bool
}

type batchResult struct {
	OK      bool   `json:"ok"`
	Account string `json:"account"`
	Target  string `json:"target"`
	Message string `json:"message"`
}

type batchJob struct {
	mu        sync.Mutex
	ID        string
	Kind      string
	Total     int
	Done      int
	Succeeded int
	Failed    int
	Running   bool
	Results   []batchResult
	CreatedAt time.Time
}

type batchJobSnapshot struct {
	JobID     string        `json:"job_id"`
	Kind      string        `json:"kind"`
	Total     int           `json:"total"`
	Done      int           `json:"done"`
	Succeeded int           `json:"succeeded"`
	Failed    int           `json:"failed"`
	Running   bool          `json:"running"`
	Results   []batchResult `json:"results"`
	CreatedAt time.Time     `json:"created_at"`
}

func (j *batchJob) snapshot() batchJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	results := make([]batchResult, len(j.Results))
	copy(results, j.Results)
	return batchJobSnapshot{
		JobID:     j.ID,
		Kind:      j.Kind,
		Total:     j.Total,
		Done:      j.Done,
		Succeeded: j.Succeeded,
		Failed:    j.Failed,
		Running:   j.Running,
		Results:   results,
		CreatedAt: j.CreatedAt,
	}
}

func (j *batchJob) record(result batchResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Done++
	if result.OK {
		j.Succeeded++
	} else {
		j.Failed++
	}
	j.Results = append(j.Results, result)
}

func (j *batchJob) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Running = false
}

type batchRegistry struct {
	mu   sync.Mutex
	jobs map[string]*batchJob
	ids  []string
}

func newBatchRegistry() *batchRegistry {
	return &batchRegistry{jobs: make(map[string]*batchJob)}
}

func (r *batchRegistry) create(kind string, total int) *batchJob {
	id, _ := randomHex(8)
	job := &batchJob{
		ID:        id,
		Kind:      kind,
		Total:     total,
		Running:   true,
		Results:   make([]batchResult, 0, total),
		CreatedAt: time.Now(),
	}
	r.mu.Lock()
	r.jobs[id] = job
	r.ids = append(r.ids, id)
	for len(r.ids) > 20 {
		old := r.ids[0]
		r.ids = r.ids[1:]
		delete(r.jobs, old)
	}
	r.mu.Unlock()
	return job
}

func (r *batchRegistry) get(id string) *batchJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

func (r *batchRegistry) list() []batchJobSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]batchJobSnapshot, 0, len(r.ids))
	for i := len(r.ids) - 1; i >= 0; i-- {
		if job := r.jobs[r.ids[i]]; job != nil {
			out = append(out, job.snapshot())
		}
	}
	return out
}
