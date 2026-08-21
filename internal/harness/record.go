package harness

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RunRecord is what gets written when a run starts. Everything here is what
// D12.5 wants on the row: which image, which CLI, which model, which session,
// and the caps it ran under.
type RunRecord struct {
	RunID       string
	Runtime     Runtime
	ImageDigest string
	CLIVersion  string
	Model       string
	SessionID   string
	Network     NetworkMode
	Limits      Limits
	Deadline    time.Duration
	StartedAt   time.Time
}

// RunStore persists runs and their output.
//
// This is the seam for the schema Megan owns. [MemoryStore] satisfies it today
// so the supervisor's wiring is finished and swapping in Postgres is one
// constructor call, not a refactor.
//
// Implementations must tolerate AppendEvent being called from the supervisor's
// drain path: it is on the critical path for a child process's pipe, so a slow
// store slows the agent and a blocking one hangs it.
type RunStore interface {
	CreateRun(ctx context.Context, rec RunRecord) error
	AppendEvent(ctx context.Context, runID string, ev Event) error
	FinishRun(ctx context.Context, runID string, res Result) error
}

// ErrRunNotFound is returned when a store is asked about a run it never saw.
var ErrRunNotFound = errors.New("harness: run not found")

// StoredRun is one run's full state in a [MemoryStore].
type StoredRun struct {
	Record RunRecord
	Events []Event
	Result Result
	Done   bool
}

// MemoryStore keeps runs in memory. It is the development and test
// implementation; it is not durable and makes no attempt to be.
type MemoryStore struct {
	mu   sync.Mutex
	runs map[string]*StoredRun
	// order preserves insertion order so tests can assert on it without
	// sorting map keys.
	order []string
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: make(map[string]*StoredRun)}
}

func (m *MemoryStore) CreateRun(_ context.Context, rec RunRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.runs[rec.RunID]; exists {
		return errors.New("harness: run already exists: " + rec.RunID)
	}
	m.runs[rec.RunID] = &StoredRun{Record: rec}
	m.order = append(m.order, rec.RunID)
	return nil
}

func (m *MemoryStore) AppendEvent(_ context.Context, runID string, ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	run.Events = append(run.Events, ev)
	return nil
}

func (m *MemoryStore) FinishRun(_ context.Context, runID string, res Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	run, ok := m.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	run.Result = res
	run.Done = true
	return nil
}

// Run returns a copy of one run's state.
func (m *MemoryStore) Run(runID string) (StoredRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	run, ok := m.runs[runID]
	if !ok {
		return StoredRun{}, ErrRunNotFound
	}
	events := make([]Event, len(run.Events))
	copy(events, run.Events)
	return StoredRun{
		Record: run.Record,
		Events: events,
		Result: run.Result,
		Done:   run.Done,
	}, nil
}

// RunIDs returns every run id in creation order.
func (m *MemoryStore) RunIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, len(m.order))
	copy(ids, m.order)
	return ids
}
