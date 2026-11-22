package desktop

import (
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
)

type subscription struct {
	snapshots          chan Snapshot
	releaseAccountView func()
}

type observationLog struct {
	OperationID string            `json:"operation_id"`
	TaskID      string            `json:"task_id"`
	Event       string            `json:"event"`
	Payload     diagnostics.Value `json:"payload"`
	Correlation diagnostics.Value `json:"correlation,omitempty"`
}

func (a *Application) observe(operationID string, task shareTask) {
	defer a.readers.Done()
	events := task.Observations()
	// Arm before querying; a commit racing the query closes this generation.
	changed := task.Changes()
	finished := task.Snapshot().Result != nil
	a.snapshot()
	for !finished || events != nil {
		if finished {
			changed = nil
		}
		select {
		case <-changed:
			changed = task.Changes()
			finished = task.Snapshot().Result != nil
			a.snapshot()
		case record, ok := <-events:
			if !ok {
				events = nil
			} else {
				a.logObservation(operationID, record)
			}
		}
	}
}

// Engine observations stay in local support logs; control sockets only consume
// authoritative snapshots.
func (a *Application) logObservation(operationID string, record engine.Observation) {
	source, ok := record.Event.(diagnostics.Source)
	if !ok {
		return
	}
	entry := source.DiagnosticRecord()
	if !entry.Valid() {
		return
	}
	a.deps.log("engine.observation", observationLog{
		OperationID: operationID,
		TaskID:      string(record.TaskID),
		Event:       entry.Name(),
		Payload:     entry.Payload(),
		Correlation: entry.Correlation(),
	})
}

func (a *Application) subscribe() (*subscription, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	queue := &subscription{snapshots: make(chan Snapshot, 1)}
	a.subscribers[queue] = struct{}{}
	return queue, func() {
		a.mu.Lock()
		delete(a.subscribers, queue)
		queue.hideAccountView()
		a.mu.Unlock()
	}
}
