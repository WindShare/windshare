package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer"
)

type fakeReceiverContentSuspension struct {
	mu          sync.Mutex
	resumeCount int
	resumeError error
	resumeEvent chan struct{}
	resumeGate  <-chan struct{}
	resumeOnce  sync.Once
}

type receiverContentSuspensionFunc func() error

func (resume receiverContentSuspensionFunc) Resume() error { return resume() }

func newFakeReceiverContentSuspension() *fakeReceiverContentSuspension {
	return &fakeReceiverContentSuspension{resumeEvent: make(chan struct{})}
}

func (suspension *fakeReceiverContentSuspension) Resume() error {
	suspension.mu.Lock()
	suspension.resumeCount++
	err := suspension.resumeError
	gate := suspension.resumeGate
	suspension.mu.Unlock()
	suspension.resumeOnce.Do(func() { close(suspension.resumeEvent) })
	if gate != nil {
		<-gate
	}
	return err
}

func (suspension *fakeReceiverContentSuspension) count() int {
	suspension.mu.Lock()
	defer suspension.mu.Unlock()
	return suspension.resumeCount
}

func receiveReceiverAdmissionDecision(
	t *testing.T,
	admission *relayContentAdmission,
) receiverAdmissionDecision {
	t.Helper()
	select {
	case decision, ok := <-admission.Decision():
		if !ok {
			t.Fatal("admission closed before publishing its decision")
		}
		return decision
	case <-time.After(time.Second):
		t.Fatal("admission did not publish its decision")
		return receiverAdmissionDecision{}
	}
}

type inertReceiverBlockLane struct{}

func (inertReceiverBlockLane) FetchBlock(
	context.Context,
	transfer.BlockDemand,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, errors.New("admission race must not fetch content")
}
