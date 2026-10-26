package receiversession

import (
	"context"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
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
	if _, err := firstReceiver.AttachLane(wrongContext, grant, wrongReceiverChannel, transferlanes.RouteDirect); err == nil {
		t.Fatal("receiver attached a peer lane through a sibling ProtocolSession")
	}
	if err := <-wrongResult; !errors.Is(err, sessioncontract.ErrHandshake) {
		t.Fatalf("sibling peer admission error = %v", err)
	}

	rightSenderChannel, rightReceiverChannel := newMemoryChannelPair()
	rightResult := make(chan struct {
		admission sendersession.PeerAdmissionResult
		err       error
	}, 1)
	go func() {
		admission, admitErr := firstSender.AdmitPeerChannel(
			context.Background(), rightSenderChannel, allowSenderPeerSettlement,
		)
		rightResult <- struct {
			admission sendersession.PeerAdmissionResult
			err       error
		}{admission: admission, err: admitErr}
	}()
	receiverAdmission, err := firstReceiver.AttachLane(context.Background(), grant, rightReceiverChannel, transferlanes.RouteDirect)
	if err != nil {
		t.Fatalf("attach exact-session peer lane after sibling rejection: %v", err)
	}
	right := <-rightResult
	if right.err != nil || right.admission.Lane != receiverAdmission.Lane ||
		!right.admission.SettlementBegan ||
		right.admission.GrantOperationID != grant.OperationID ||
		right.admission.Disposition != sendersession.PeerAdmissionAccepted ||
		right.admission.ResponseDelivery != sendersession.PeerResponseDelivered ||
		right.admission.LaneAttachment != sendersession.PeerLaneAttached ||
		receiverAdmission.Disposition != LaneAdmissionAccepted ||
		receiverAdmission.LaneInstallation != LaneInstalled {
		t.Fatalf("exact peer admission = %#v, %v; receiver=%#v", right.admission, right.err, receiverAdmission)
	}
	firstSender.DetachLane(runtimeIdentity(firstSender))
	firstReceiver.DetachLane(runtimeIdentity(firstReceiver))
	waitSessionCondition(t, "exact-session peer survives relay loss", func() bool {
		return firstSender.AttachedLanes() == 1 && firstReceiver.AttachedLanes() == 1
	})
	select {
	case <-firstReceiver.Done():
		t.Fatal("relay loss ended the receiver despite its admitted peer lane")
	default:
	}
}
