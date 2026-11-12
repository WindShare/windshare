package desktop

import (
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

const notificationCapacity = 64

type subscription struct {
	snapshots chan Snapshot
	evidence  chan Message
	dropped   uint64
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
				a.publishObservation(operationID, record)
			}
		}
	}
}

func (a *Application) publishObservation(operationID string, record engine.Observation) {
	event, ok := record.Event.(appobs.Event)
	if !ok {
		return
	}
	encoded, err := wire.Encode(event)
	if err != nil {
		return
	}
	notice := &Observation{OperationID: operationID, TaskID: string(record.TaskID), Event: encoded}
	a.deps.log("engine.observation", notice)
	a.mu.Lock()
	defer a.mu.Unlock()
	for subscriber := range a.subscribers {
		select {
		case subscriber.evidence <- Message{Version: ProtocolVersion, Observation: notice}:
		default:
			subscriber.dropped++
		}
	}
}

func (a *Application) subscribe() (*subscription, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	queue := &subscription{snapshots: make(chan Snapshot, 1), evidence: make(chan Message, notificationCapacity)}
	a.subscribers[queue] = struct{}{}
	return queue, func() {
		a.mu.Lock()
		delete(a.subscribers, queue)
		dropped := queue.dropped
		a.mu.Unlock()
		if dropped != 0 {
			a.deps.log("host.evidence_dropped", map[string]uint64{"count": dropped})
		}
	}
}
