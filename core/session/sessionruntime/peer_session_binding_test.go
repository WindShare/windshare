package sessionruntime

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/transfer"
	"testing"
	"time"
)

func TestAdmitPeerChannelCannotRouteAProofIntoSiblingProtocolSession(t *testing.T) {
	fixture := newVerticalFixture(t)
	firstSender, firstReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	secondSender, secondReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer firstSender.Close()
	defer firstReceiver.Close()
	defer secondSender.Close()
	defer secondReceiver.Close()
	grant, err := firstReceiver.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	wrongSenderChannel, wrongReceiverChannel := newMemoryChannelPair()
	wrongResult := make(chan error, 1)
	wrongContext, cancelWrong := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelWrong()
	go func() {
		_, admitErr := secondSender.AdmitPeerChannel(wrongContext, wrongSenderChannel, allowSenderPeerSettlement)
		wrongResult <- admitErr
	}()
	if _, err := firstReceiver.AttachLane(wrongContext, grant, wrongReceiverChannel, transfer.LaneRouteDirect); err == nil {
		t.Fatal("receiver attached a peer lane through a sibling ProtocolSession")
	}
	if err := <-wrongResult; !errors.Is(err, ErrHandshake) {
		t.Fatalf("sibling peer admission error = %v", err)
	}

	rightSenderChannel, rightReceiverChannel := newMemoryChannelPair()
	rightResult := make(chan struct {
		admission SenderPeerAdmissionResult
		err       error
	}, 1)
	go func() {
		admission, admitErr := firstSender.AdmitPeerChannel(
			context.Background(), rightSenderChannel, allowSenderPeerSettlement,
		)
		rightResult <- struct {
			admission SenderPeerAdmissionResult
			err       error
		}{admission: admission, err: admitErr}
	}()
	receiverAdmission, err := firstReceiver.AttachLane(context.Background(), grant, rightReceiverChannel, transfer.LaneRouteDirect)
	if err != nil {
		t.Fatalf("attach exact-session peer lane after sibling rejection: %v", err)
	}
	right := <-rightResult
	if right.err != nil || right.admission.Lane != receiverAdmission.Lane ||
		!right.admission.SettlementBegan ||
		right.admission.GrantOperationID != grant.OperationID ||
		right.admission.Disposition != SenderPeerAdmissionAccepted ||
		right.admission.ResponseDelivery != SenderPeerResponseDelivered ||
		right.admission.LaneAttachment != SenderPeerLaneAttached ||
		receiverAdmission.Disposition != ReceiverLaneAdmissionAccepted ||
		receiverAdmission.LaneInstallation != ReceiverLaneInstalled {
		t.Fatalf("exact peer admission = %#v, %v; receiver=%#v", right.admission, right.err, receiverAdmission)
	}
	senderRelay, err := firstSender.lanes.selectLane(&firstSender.initial)
	if err != nil {
		t.Fatal(err)
	}
	receiverRelay, err := firstReceiver.lanes.selectLane(&firstReceiver.initial)
	if err != nil {
		t.Fatal(err)
	}
	_ = senderRelay.channel.Close()
	_ = receiverRelay.channel.Close()
	waitSessionCondition(t, "exact-session peer survives relay loss", func() bool {
		return firstSender.AttachedLanes() == 1 && firstReceiver.AttachedLanes() == 1
	})
	select {
	case <-firstReceiver.Done():
		t.Fatal("relay loss ended the receiver despite its admitted peer lane")
	default:
	}
}
