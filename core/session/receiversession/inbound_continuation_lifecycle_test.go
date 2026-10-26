package receiversession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCompositeRuntimeTransfersAfterInFlightCandidateOutlivesPeerRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		offerBody := []byte{0xf6}
		reject := make(chan struct{})
		fixture.senderConfig.Peers = verticalBoundPeerFactory{
			PeerHandlerFactoryFunc: func(session sendersession.PeerSession) (sendersession.PeerHandler, error) {
				return &delayedPeerRejectionHandler{
					verticalPeerHandler: &verticalPeerHandler{session: session, rejectedOffer: offerBody},
					reject:              reject,
				}, nil
			},
			rejected: offerBody,
		}
		fixture.rebuildSender(t)
		receiverConfig := fixture.receiverConfig
		receiverConfig.PeerControls = receiverPeerSemanticsForTest(protocolsession.SenderControlSemanticValidatorFunc(
			func(protocolsession.MessageKind, protocolsession.OperationID, []byte) error {
				return protocolsession.ErrControlSemantic
			},
		))
		receiverFactory, err := NewFactory(receiverConfig)
		if err == nil {
			trackReceiverFactory(receiverFactory)
		}
		if err != nil {
			t.Fatal(err)
		}
		senderChannel, receiverChannel := newMemoryChannelPair()
		delayed := &bufferedContinuationChannel{
			Channel: receiverChannel, pending: make(chan framechannel.Frame, 1),
		}
		accepted := make(chan *sendersession.Runtime, 1)
		acceptErrors := make(chan error, 1)
		go func() {
			sender, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
			accepted <- sender
			acceptErrors <- err
		}()
		receiver, err := receiverFactory.Connect(context.Background(), delayed, transferlanes.RouteRelay)
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		sender := <-accepted
		if err := <-acceptErrors; err != nil {
			t.Fatal(err)
		}
		defer sender.Close()

		operation, err := receiver.OpenPeerOperation(context.Background(), offerBody)
		if err != nil {
			t.Fatal(err)
		}
		// The transport accepts this authenticated frame before the rejection,
		// but delivers it after both endpoints have retired the operation.
		delayed.bufferNext.Store(true)
		if _, err := operation.SendCandidate(context.Background(), []byte{0xf6}); err != nil {
			t.Fatal(err)
		}
		candidate := <-delayed.pending
		close(reject)
		termination := requireReceiverPeerTermination(t, operation.Receive(context.Background()))
		assertReceiverPeerTermination(
			t, operation, termination, PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteOperationRejected, PeerTerminalOperationOnly,
			PeerProvenanceRemoteOperationRejected, PeerDiagnosticRemoteOperationRejected,
		)
		synctest.Wait()
		assertRequestsRetired(t, receiver.rpc)
		if err := receiverChannel.Send(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}

		// A fresh lease and block read require the sender's actual pump and
		// dispatch workers; cached metadata cannot hide a terminated session.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		opened, err := receiver.OpenRevision(ctx, fixture.fileID)
		if err != nil {
			t.Fatalf("open revision after late candidate (sender: %v): %v", sender.Err(), err)
		}
		output := make([]byte, len(fixture.fileData))
		err = receiver.BlockBroker().ReadRange(
			ctx, opened.LeaseID, opened.Descriptor,
			content.Range{Offset: 0, End: uint64(len(output))},
			transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
				copy(output[offset:], data)
				return nil
			}),
		)
		if err != nil || !bytes.Equal(output, fixture.fileData) {
			t.Fatalf("file transfer after late candidate = %v, contents match = %v", err, bytes.Equal(output, fixture.fileData))
		}
		if err := receiver.ReleaseRevision(ctx, opened.LeaseID); err != nil {
			t.Fatal(err)
		}
		if sender.Err() != nil {
			t.Fatalf("late candidate terminated the sender: %v", sender.Err())
		}
	})
}

type bufferedContinuationChannel struct {
	framechannel.Channel
	bufferNext atomic.Bool
	pending    chan framechannel.Frame
}

func (channel *bufferedContinuationChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	if channel.bufferNext.CompareAndSwap(true, false) {
		channel.pending <- bytes.Clone(frame)
		return nil
	}
	return channel.Channel.Send(ctx, frame)
}

type delayedPeerRejectionHandler struct {
	*verticalPeerHandler
	reject <-chan struct{}
}

func (handler *delayedPeerRejectionHandler) HandleMessage(ctx context.Context, message protocolsession.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-handler.reject:
		return handler.verticalPeerHandler.HandleMessage(ctx, message)
	}
}
