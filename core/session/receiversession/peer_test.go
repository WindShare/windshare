package receiversession

import (
	"bytes"
	"context"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	"testing"
)

func TestReceiverPeerOperationClosedStateIsInert(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(func() { stopTestCore(runtime) })
	rpc := newTestRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{1}, protocolsession.IdentityBytes)))
	receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.OpenPeerOperation(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled peer open error = %v", err)
	}

	call, _, err := rpc.PrepareCall(context.Background(), protocolsession.MessagePeerOffer, []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	closed := &PeerOperation{
		rpc: rpc, call: call, token: new(receiverPeerOperationToken), closed: true,
	}
	if closed.OperationID() != call.ID() {
		t.Fatal("closed peer operation changed its exact identity")
	}
	if _, err := closed.SendCandidate(context.Background(), nil); !errors.Is(err, sessioncontract.ErrOperationMissing) {
		t.Fatalf("closed peer candidate error = %v", err)
	}
	result := closed.Receive(context.Background())
	received := requireReceiverPeerTermination(t, result)
	assertReceiverPeerTermination(
		t,
		closed,
		received,
		PeerTerminalAuthorityLocal,
		PeerProvenanceLocalOperationContract,
		PeerTerminalOperationOnly,
		PeerProvenanceLocalOperationContract,
		PeerDiagnosticOperationMissing,
	)
	termination := closed.Terminate(context.Background())
	assertReceiverPeerTerminationsEqual(t, received, termination)
}

func TestReceiverPeerCancelReconcilesTombstonedWireSuppressionLocally(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 86)
	generation, _ := fixture.call.OperationAuthority()
	cancelPendingCall(t, fixture.operation.rpc, fixture.call)
	assertReceiverPeerTermination(t, fixture.operation, fixture.operation.Terminate(context.Background()),
		PeerTerminalAuthorityLocal, PeerProvenanceLocalExplicitStop, PeerTerminalOperationOnly, PeerProvenanceLocalExplicitStop)
	if generation.IsActive() {
		t.Fatal("tombstoned peer cancel retained operation authority")
	}
	assertRequestsRetired(t, fixture.operation.rpc)
}

type inertSenderPeerHandler struct{}

func (inertSenderPeerHandler) HandleMessage(context.Context, protocolsession.Message) error {
	return nil
}

func (inertSenderPeerHandler) Cancel(context.Context, protocolsession.OperationID) error {
	return nil
}

func (inertSenderPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func inertSenderPeerFactory() sendersession.PeerHandlerFactory {
	return sendersession.PeerHandlerFactoryFunc(func(sendersession.PeerSession) (sendersession.PeerHandler, error) {
		return inertSenderPeerHandler{}, nil
	})
}
