package desktop

import (
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

const notificationCapacity = 64

func (a *Application) observe(operationID string, task shareTask) {
	defer a.readers.Done()
	for record := range task.Observations() {
		a.publishObservation(operationID, record)
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
		case subscriber <- Message{Version: ProtocolVersion, Observation: notice}:
		default:
			// State never depends on delivery: periodic snapshots repair even silent loss.
		}
	}
}

func (a *Application) subscribe() (chan Message, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	queue := make(chan Message, notificationCapacity)
	a.subscribers[queue] = struct{}{}
	return queue, func() {
		a.mu.Lock()
		delete(a.subscribers, queue)
		a.mu.Unlock()
	}
}
