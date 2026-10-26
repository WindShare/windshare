package sessioncore

import (
	"context"
	"errors"
	"fmt"
	sessioncontract "github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

func TestProtocolOperationCauseRetainsJoinedFaults(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want sessionobs.ProtocolOperationCause
	}{
		{"canceled cleanup", errors.Join(context.Canceled, errors.New("cleanup failed")), sessionobs.ProtocolOperationCauseProtocolFailure},
		{"wrapped canceled cleanup", fmt.Errorf("operation: %w", errors.Join(errors.New("cleanup failed"), context.Canceled)), sessionobs.ProtocolOperationCauseProtocolFailure},
		{"canceled runtime", errors.Join(context.Canceled, sessioncontract.ErrRuntimeClosed), sessionobs.ProtocolOperationCauseRuntimeClosed},
		{"canceled deadline", errors.Join(context.Canceled, context.DeadlineExceeded), sessionobs.ProtocolOperationCauseDeadline},
		{"unproven missing authority", sessioncontract.ErrOperationMissing, sessionobs.ProtocolOperationCauseOperationClosed},
		{"opaque cancellation match", cancellationMatchingBlockFault{}, sessionobs.ProtocolOperationCauseProtocolFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := newOperationCall(id16[protocolsession.OperationID](65), protocolsession.MessageRequestBlocks, time.Now(), 0, false, true)
			owner := call.claimProtocolTermination()
			call.recordProtocolTraceFailure(test.err)
			event, ok := call.protocolOperationTerminationTrace(time.Now(), owner,
				OperationOutcome{sessionobs.ProtocolOperationReceiverEnded, sessionobs.ProtocolOperationCauseSuperseded})
			if !ok || event.Stage != sessionobs.ProtocolOperationReceiverFailed || event.Cause != test.want {
				t.Fatalf("joined cancellation terminal = %+v present=%v", event, ok)
			}
		})
	}
}

type cancellationMatchingBlockFault struct{}

func (cancellationMatchingBlockFault) Error() string { return "fault with a cancellation match" }

func (cancellationMatchingBlockFault) Is(target error) bool { return target == context.Canceled }
