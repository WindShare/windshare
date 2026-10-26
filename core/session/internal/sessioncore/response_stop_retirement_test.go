package sessioncore

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/session/contentflow"

	sessionobs "github.com/windshare/windshare/core/session/observation"
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
				sender.start()
				t.Cleanup(sender.close)
				outbound := NewResponses(sender, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)), nil)
				// Keep the operation owner alive through stop so finalization cannot
				// clear its route before the delayed response retires it.
				_, release, err := sender.beginExternalAdmission(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				id := id16[protocolsession.OperationID](146)
				request, err := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &id, []byte{0xa0})
				if err != nil {
					t.Fatal(err)
				}
				ctx, _ := testOutboundOperationContext(t, sender, sender.initial, request)
				stop := func() {
					_ = sender.router.TerminateLocal()
					sender.terminate(RuntimeTerminationGracefulStop)
					<-sender.Lifetime().Done()
					if !sender.operations.Terminated() {
						t.Fatal("stop did not revoke operation authority")
					}
					if test.failure != nil {
						sender.terminateRuntimeFailed(test.failure)
					}
				}
				var result protocolsession.ResponseSendResult
				if test.lateFinal {
					stop()
					body, encodeErr := protocolsession.EncodeOperationFailure(contentflow.OperationFailure{
						Scope: contentflow.BlockErrorScope, Code: contentflow.BlockCodeTimeout, Message: "Block stopped",
					})
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					result, err = outbound.SendControl(ctx, protocolsession.MessageOperationError, id, body)
				} else {
					fragments, fragmentErr := contentflow.FragmentRecord(id, []byte("record"))
					if fragmentErr != nil {
						t.Fatal(fragmentErr)
					}
					result, err = outbound.executeResponse(ctx, fragments[0].Kind(), id, sessionobs.ProtocolErrorContent{}, func(transaction *outboundTransaction) (outboundLaneAttempt, error) {
						// Place stop after transaction admission and before its send
						// returns, without relying on transport timing or a sleep.
						stop()
						return func(lane selectedLane, _ protocolsession.OutboundReplayPermit) (protocolsession.SendReceipt, error) {
							return lane.writer.TryAuthorizedData(fragments[0], transaction.authority)
						}, nil
					})
				}
				if err == nil || result.Evidence() != protocolsession.ResponseSendEvidenceDefinitelyNotSent ||
					result.Cleanup() != protocolsession.SendCleanupOperationRetired {
					t.Fatalf("stopped response result=%+v error=%v", result, err)
				}
				if sender.routes.len() != 0 || sender.operations.ActiveCount() != 0 || sender.operations.TombstoneCount() != 0 {
					t.Fatal("response cleanup retained operation resources")
				}
				release()
				sender.waitClosed()
				if err := sender.Err(); !errors.Is(err, test.failure) {
					t.Fatalf("stop error=%v, want %v", err, test.failure)
				}
			})
		})
	}
}
