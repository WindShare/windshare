package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"testing/synctest"
)

func TestSenderStopDuringResponseRetirement(t *testing.T) {
	localFailure := errors.New("independent shutdown failure")
	for _, test := range []struct {
		name      string
		lateFinal bool
		failure   error
	}{
		{name: "started fragment"},
		{name: "late operation error", lateFinal: true},
		{name: "real failure remains visible", failure: localFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				sender.Start()
				t.Cleanup(sender.Close)
				outbound := NewResponses(sender, ResponseConfig{})
				// Keep the operation owner alive through stop so finalization cannot
				// finish before the delayed response settles.
				_, release, err := sender.BeginExternalAdmission(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				id := protocolsession.OperationID(146)
				request, err := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
				if err != nil {
					t.Fatal(err)
				}
				admission, err := sender.operations.ObserveInbound(protocolsession.DirectionReceiverToSender, sender.initial, request)
				if err != nil || admission.op.IsZero() {
					t.Fatalf("admit request: %v", err)
				}
				stop := func() {
					_ = sender.operations.TerminateLocal()
					sender.terminate(TerminationGracefulStop)
					<-sender.Lifetime().Done()
					if !sender.operations.Terminated() {
						t.Fatal("stop did not revoke operation authority")
					}
					if test.failure != nil {
						sender.terminateRuntimeFailed(test.failure)
					}
				}
				stop()
				if test.lateFinal {
					body, encodeErr := protocolsession.EncodeOperationFailure(contentflow.OperationFailure{
						Scope: contentflow.BlockErrorScope, Code: contentflow.BlockCodeTimeout, Message: "Block stopped",
					})
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					var evidence protocolsession.SendEvidence
					evidence, err = outbound.SendControl(context.Background(), admission.op, protocolsession.MessageOperationError, body)
					if evidence != protocolsession.SendNotSent {
						t.Fatalf("stopped control evidence=%d, want SendNotSent", evidence)
					}
				} else {
					fragments, fragmentErr := contentflow.FragmentRecord(id, []byte("record"))
					if fragmentErr != nil {
						t.Fatal(fragmentErr)
					}
					err = outbound.SendFragment(context.Background(), admission.op, fragments[0])
				}
				if err == nil || (!protocolsession.IsNotSent(err) && !errors.Is(err, protocolsession.ErrOperationSuppressed)) {
					t.Fatalf("stopped response error=%v", err)
				}
				if sender.operations.ActiveCount() != 0 || sender.operations.TombstoneCount() != 0 {
					t.Fatal("response cleanup retained operation resources")
				}
				release()
				sender.WaitClosed()
				if err := sender.Err(); !errors.Is(err, test.failure) {
					t.Fatalf("stop error=%v, want %v", err, test.failure)
				}
			})
		})
	}
}
