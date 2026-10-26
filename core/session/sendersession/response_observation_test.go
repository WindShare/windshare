package sendersession

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestResponseObservationsDrainSourceEvidenceBeforeClosing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		output, consumer, _ := observationstream.New[sessionobs.ProtocolObservation](1)
		observations := newResponseObservations(output)
		receipt, correlation := settledObservationReceipt(t)
		if !receipt.ObserveSettlement(correlation, observations.producer) {
			t.Fatal("settled receipt rejected observation")
		}
		const upstreamLoss = 3
		observations.producer.RecordDropped(upstreamLoss)
		observations.close()
		event, ok := (<-consumer).(sessionobs.SendAttemptSettled)
		if !ok {
			t.Fatal("settlement was not drained into the output stream")
		}
		got := event.Correlation()
		if got.Role != protocolsession.RoleSender || got.ProtocolSessionID != correlation.ProtocolSessionID ||
			got.OperationID != correlation.OperationID || got.RequestKind != correlation.RequestKind ||
			event.ResponseKind() != correlation.ResponseKind || event.Attempt().Identity() != correlation.Identity ||
			!event.Attempt().Settled() || event.Attempt().Outcome() != protocolsession.SendOutcomeDropped ||
			event.ObservedAt().IsZero() {
			t.Fatalf("settlement lost source evidence: %+v", event)
		}
		if observations.producer.TryPublish(protocolsession.SendAttemptSettlement{}) {
			t.Fatal("closed response owner admitted another settlement")
		}
		completion := output.Complete()
		if completion.Enqueued != 1 || completion.CapacityDropped != upstreamLoss {
			t.Fatalf("output completion=%+v", completion)
		}
	})
}

func TestResponseObservationOwnershipFollowsSenderConstruction(t *testing.T) {
	if observations := newResponseObservations(observationstream.Producer[sessionobs.ProtocolObservation]{}); observations != nil {
		t.Fatal("disabled observations acquired a worker")
	}
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		output, _, _ := observationstream.New[sessionobs.ProtocolObservation](8)
		fixture.senderFactory.protocolObservations = output
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer receiver.Close()
		sender.Close()
		// Both normal finalization and failed attachment must join their worker;
		// synctest detects a constructor leaving either worker behind.
		if outbound, err := newResponses(sender.core, fixture.senderFactory.privateKey, nil, output); !errors.Is(err, session.ErrRuntimeClosed) || outbound != nil {
			t.Fatalf("closed runtime accepted response ownership: %v, %v", outbound, err)
		}
		output.Complete()
	})
}

// A real writer supplies an immutable settled receipt without exposing protocol
// internals or manufacturing diagnostic values that production cannot create.
func settledObservationReceipt(t *testing.T) (protocolsession.SendReceipt, protocolsession.SendAttemptCorrelation) {
	t.Helper()
	correlation := protocolsession.SendAttemptCorrelation{
		ProtocolSessionID: id16[protocolsession.ProtocolSessionID](21),
		OperationID:       id16[protocolsession.OperationID](22),
		RequestKind:       protocolsession.MessageReleaseLease,
		ResponseKind:      protocolsession.MessageOperationComplete,
		Identity:          protocolsession.SendAttemptIdentity{ResponseSequence: 7, AttemptSequence: 2, LaneID: 3, LaneEpoch: 1},
	}
	key, err := protocolsession.TrafficKeyFromBytes(make([]byte, protocolsession.TrafficKeyBytes), protocolsession.DirectionReceiverToSender)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := protocolsession.NewEnvelopeSealer(key, protocolsession.EnvelopeBinding{
		ShareInstance: id16[catalog.ShareInstance](23), ProtocolSessionID: correlation.ProtocolSessionID,
		LaneID: correlation.Identity.LaneID, Direction: protocolsession.DirectionReceiverToSender,
	}, bytes.NewReader(make([]byte, protocolsession.EnvelopeNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	defer sealer.Destroy()
	operations, err := protocolsession.NewOperationTable(protocolsession.OperationLimits{MaxActive: 1, MaxTracked: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	channel, peer := newMemoryChannelPair()
	defer channel.Close()
	defer peer.Close()
	writer, err := protocolsession.NewSessionWriter(channel, sealer, router)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(correlation.RequestKind, &correlation.OperationID, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := writer.TryControl(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("writer cancellation=%v", err)
	}
	return receipt, correlation
}
