package outputruntime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ImmutableSessionGettersDuringSettlement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		settle     func(*Session) (transfer.JobSettlement, error)
		settlement transfer.JobSettlementKind
	}{
		{
			name: "pause",
			settle: func(session *Session) (transfer.JobSettlement, error) {
				return session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
			},
			settlement: transfer.JobPaused,
		},
		{
			name: "complete",
			settle: func(session *Session) (transfer.JobSettlement, error) {
				return session.CompleteJob(context.Background(), transfer.JobSucceeded)
			},
			settlement: transfer.JobClosed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			session := opened.Session
			wantSession := session.SessionID()
			wantBackend := session.BackendID()
			wantCapabilities := session.Capabilities()

			const getterWorkers = 16
			stop := make(chan struct{})
			active := make(chan struct{}, getterWorkers)
			errorsSeen := make(chan error, getterWorkers)
			var workers sync.WaitGroup
			workers.Add(getterWorkers)
			for range getterWorkers {
				go func() {
					defer workers.Done()
					reportedActive := false
					defer func() {
						if !reportedActive {
							active <- struct{}{}
						}
					}()
					for {
						if actual := session.SessionID(); actual != wantSession {
							errorsSeen <- fmt.Errorf("session ID changed from %s to %s", wantSession, actual)
							return
						}
						if actual := session.BackendID(); actual != wantBackend {
							errorsSeen <- fmt.Errorf("backend changed from %s to %s", wantBackend, actual)
							return
						}
						if actual := session.Capabilities(); actual != wantCapabilities {
							errorsSeen <- fmt.Errorf("capabilities changed from %+v to %+v", wantCapabilities, actual)
							return
						}
						if !reportedActive {
							active <- struct{}{}
							reportedActive = true
						}
						select {
						case <-stop:
							return
						default:
						}
					}
				}()
			}
			for range getterWorkers {
				<-active
			}
			settlement, settleErr := test.settle(session)
			close(stop)
			workers.Wait()
			close(errorsSeen)
			for getterErr := range errorsSeen {
				t.Error(getterErr)
			}
			if settleErr != nil || settlement.Kind() != test.settlement {
				t.Fatalf("settlement = (%+v, %v), want %v", settlement, settleErr, test.settlement)
			}
		})
	}
}
