package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestInboundRetiredPeerCandidateUsesOperationAuthorityWithoutResponseRoute(t *testing.T) {
	for _, terminal := range []protocolsession.MessageKind{
		protocolsession.MessageOperationError,
		protocolsession.MessageCancel,
	} {
		t.Run(map[protocolsession.MessageKind]string{
			protocolsession.MessageOperationError: "rejected",
			protocolsession.MessageCancel:         "cancelled",
		}[terminal], func(t *testing.T) {
			runtime, _ := newUnstartedRuntimeWithContinuations(
				t, protocolsession.RoleSender, protocolsession.OperationLimits{}, nil, continuationReplayClassifier{},
			)
			inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
			id := id16[protocolsession.OperationID](171)
			request := operationMessageForTest(t, protocolsession.MessagePeerOffer, id, []byte{0xf6})
			if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil || disposition != protocolsession.OperationDeliver {
				t.Fatalf("offer admission = %d, %v", disposition, err)
			}
			if _, err := runtime.router.Next(context.Background()); err != nil {
				t.Fatal(err)
			}
			route := runtime.routes.current(id)
			direction := protocolsession.DirectionSenderToReceiver
			if terminal == protocolsession.MessageCancel {
				direction = protocolsession.DirectionReceiverToSender
			}
			body := []byte{0xf6}
			if terminal == protocolsession.MessageOperationError {
				binding := runtime.senderControlBase(runtime.initial)
				binding.Sequence, binding.MessageKind = 1, terminal
				binding.OperationID, binding.HasOperationID = id, true
				var err error
				body, err = protocolsession.SignControlBody(
					ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize)),
					protocolsession.ControlDomainOperation, binding, body,
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			final := operationMessageForTest(t, terminal, id, body)
			if disposition, err := runtime.operations.Observe(direction, final); err != nil || disposition != protocolsession.OperationDeliver {
				t.Fatalf("operation retirement = %d, %v", disposition, err)
			}
			runtime.routes.releaseRoute(id, route)

			// A candidate admitted by the remote writer before the final can arrive
			// after the response route is gone, including on another physical lane.
			for _, lane := range []LaneIdentity{runtime.initial, {ID: runtime.initial.ID + 1, Epoch: 1}} {
				candidate := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xf6})
				disposition, err := (laneInboundRouter{runtime: runtime, identity: lane}).RouteInbound(context.Background(), candidate)
				if err != nil || disposition != protocolsession.OperationDrop {
					t.Fatalf("late candidate on %v = %d, %v", lane, disposition, err)
				}
			}
			if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
				t.Fatal("late traffic revived a response route or operation")
			}

			// Removing a physical-route check must not bypass the retained
			// continuation authority's semantic validation.
			malformed := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id, []byte{0xa0})
			if _, err := inbound.RouteInbound(context.Background(), malformed); !errors.Is(err, protocolsession.ErrInvalidMessage) {
				t.Fatalf("invalid late candidate = %v", err)
			}
			unknown := operationMessageForTest(t, protocolsession.MessagePeerCandidate, id16[protocolsession.OperationID](172), []byte{0xf6})
			if _, err := inbound.RouteInbound(context.Background(), unknown); !errors.Is(err, protocolsession.ErrUnknownOperation) {
				t.Fatalf("unknown operation candidate = %v", err)
			}
		})
	}
}

func TestCompositeRuntimeTransfersAfterInFlightCandidateOutlivesPeerRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		offerBody := []byte{0xf6}
		reject := make(chan struct{})
		fixture.senderFactory.peers = verticalBoundPeerFactory{
			SenderPeerHandlerFactoryFunc: func(session SenderPeerSession) (SenderPeerHandler, error) {
				return &delayedPeerRejectionHandler{
					verticalPeerHandler: &verticalPeerHandler{session: session, rejectedOffer: offerBody},
					reject:              reject,
				}, nil
			},
			rejected: offerBody,
		}
		receiverConfig := fixture.receiverConfig
		receiverConfig.PeerControls = receiverPeerSemanticsForTest(protocolsession.SenderControlSemanticValidatorFunc(
			func(protocolsession.MessageKind, protocolsession.OperationID, []byte) error {
				return protocolsession.ErrControlSemantic
			},
		))
		receiverFactory, err := NewReceiverFactory(receiverConfig)
		if err != nil {
			t.Fatal(err)
		}
		senderChannel, receiverChannel := newMemoryChannelPair()
		delayed := &bufferedContinuationChannel{
			Channel: receiverChannel, pending: make(chan framechannel.Frame, 1),
		}
		accepted := make(chan *SenderRuntime, 1)
		acceptErrors := make(chan error, 1)
		go func() {
			sender, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
			accepted <- sender
			acceptErrors <- err
		}()
		receiver, err := receiverFactory.Connect(context.Background(), delayed, transfer.LaneRouteRelay)
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
			t, operation, termination, ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteOperationRejected, ReceiverPeerTerminalOperationOnly,
			ReceiverPeerProvenanceRemoteOperationRejected, ReceiverPeerDiagnosticRemoteOperationRejected,
		)
		synctest.Wait()
		if sender.routes.current(operation.OperationID()) != nil {
			t.Fatal("final response did not release its route")
		}
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
