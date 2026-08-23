package commandprojection

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

func TestProjectTransferLifecycleAcceptsEveryCapacityStageWithExactCorrelation(t *testing.T) {
	stages := []struct {
		core     transfer.TransferLifecycleStage
		cli      clievent.TransferLifecycleStage
		terminal bool
	}{
		{transfer.TransferCapacityRetryScheduled, clievent.TransferCapacityRetryScheduled, false},
		{transfer.TransferCapacityRetryReady, clievent.TransferCapacityRetryReady, false},
		{transfer.TransferCapacityRetrySucceeded, clievent.TransferCapacityRetrySucceeded, false},
		{transfer.TransferCapacityBudgetPaused, clievent.TransferCapacityBudgetPaused, true},
		{transfer.TransferCapacityWaitCanceled, clievent.TransferCapacityWaitCanceled, true},
		{transfer.TransferCapacityGenerationEnded, clievent.TransferCapacityGenerationEnded, true},
	}
	for _, test := range stages {
		t.Run(stageName(t, test.cli), func(t *testing.T) {
			trace := transfer.TransferLifecycleTrace{
				Stage:               test.core,
				ReceiveOperationID:  receivecontract.OperationID{0x11},
				ProtocolSessionID:   protocolsession.ProtocolSessionID{0x12},
				TransferJobID:       transfer.TransferJobID{0x13},
				Progress:            transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryOpen, CountersExact: true},
				CapacityWaitID:      revisionwait.WaitID{0x14},
				CapacityGeneration:  revisionwait.GenerationToken{0x15},
				CapacityOperationID: protocolsession.OperationID{0x16},
				CapacityAttempt:     3, CapacityHint: 250 * time.Millisecond,
				CapacityAccumulated: 700 * time.Millisecond, CapacityActiveWaiters: 2,
				Failed: test.terminal,
			}
			if test.core == transfer.TransferCapacityRetryScheduled {
				trace.CapacityJitter = 7 * time.Millisecond
				trace.CapacityDelay = 257 * time.Millisecond
			}
			event, err := ProjectTransferLifecycle(trace)
			if err != nil {
				t.Fatal(err)
			}
			capacity, ok := event.CapacityLifecycle()
			if !ok || event.Stage() != test.cli || capacity.WaitID().Hex() != clieventIDHex(0x14) ||
				capacity.GenerationID().Hex() != clieventIDHex(0x15) ||
				capacity.OperationID().Hex() != clieventIDHex(0x16) ||
				capacity.Attempt() != 3 || capacity.Hint() != 250*time.Millisecond ||
				capacity.Jitter() != trace.CapacityJitter || capacity.Delay() != trace.CapacityDelay ||
				capacity.AccumulatedWait() != 700*time.Millisecond || capacity.ActiveWaiters() != 2 {
				t.Fatalf("capacity projection = %#v, present=%t", capacity, ok)
			}
			_, failed := event.Failure()
			if failed != test.terminal {
				t.Fatalf("failure present=%t, want %t", failed, test.terminal)
			}
		})
	}
}

func TestProjectTransferLifecycleRejectsCapacityFactsOutsideCapacityStages(t *testing.T) {
	_, err := ProjectTransferLifecycle(transfer.TransferLifecycleTrace{
		Stage:              transfer.TransferDiscoveryStarted,
		ReceiveOperationID: receivecontract.OperationID{1},
		ProtocolSessionID:  protocolsession.ProtocolSessionID{2},
		TransferJobID:      transfer.TransferJobID{3},
		Progress:           transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryOpen, CountersExact: true},
		CapacityWaitID:     revisionwait.WaitID{4},
	})
	if err != ErrInvalidProjection {
		t.Fatalf("non-capacity stage error = %v", err)
	}
}

func stageName(t *testing.T, stage clievent.TransferLifecycleStage) string {
	t.Helper()
	name, ok := stage.Name()
	if !ok {
		t.Fatalf("invalid test stage %v", stage)
	}
	return name
}

func clieventIDHex(first byte) string {
	const digits = "0123456789abcdef"
	result := make([]byte, clievent.IdentityBytes*2)
	result[0] = digits[first>>4]
	result[1] = digits[first&0xf]
	for index := 2; index < len(result); index++ {
		result[index] = '0'
	}
	return string(result)
}
