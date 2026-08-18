package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRuntimeCloseCancelsAndJoinsExternalLaneHandshakes(t *testing.T) {
	t.Run("sender peer channel", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer receiver.Close()
		base, peer := newMemoryChannelPair()
		defer base.Close()
		defer peer.Close()
		channel := &admissionBarrierChannel{FrameChannel: base, entered: make(chan struct{})}
		admissionResult := make(chan error, 1)
		go func() {
			_, err := sender.AdmitPeerChannel(
				context.Background(), channel, allowSenderPeerSettlement,
			)
			admissionResult <- err
		}()
		<-channel.entered
		closeDone := make(chan struct{})
		go func() {
			sender.Close()
			close(closeDone)
		}()
		select {
		case err := <-admissionResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal-canceled sender admission error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("sender close did not cancel its external handshake")
		}
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("sender close published no completion after external handshake drained")
		}
	})

	t.Run("authenticated sender settlement", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer receiver.Close()
		grant := mustRequestLane(t, receiver)
		hello := laneHelloForGrant(t, receiver, grant)
		base, peer := newObservedChannelPair()
		defer peer.Close()
		responseGate := base.gateNextSend()
		candidate := &countingAdmissionChannel{FrameChannel: base}
		settlementEntered := make(chan struct{})
		releaseSettlement := make(chan struct{})
		admissionResult := make(chan struct {
			admission SenderPeerAdmissionResult
			err       error
		}, 1)
		go func() {
			admission, err := sender.AdmitPeerChannel(
				context.Background(),
				candidate,
				SenderPeerAdmissionControlFunc(func(protocolsession.OperationID, LaneIdentity) bool {
					close(settlementEntered)
					<-releaseSettlement
					return true
				}),
			)
			admissionResult <- struct {
				admission SenderPeerAdmissionResult
				err       error
			}{admission: admission, err: err}
		}()
		if err := peer.Send(context.Background(), framechannel.Frame(hello)); err != nil {
			t.Fatal(err)
		}
		<-settlementEntered
		closeDone := make(chan struct{})
		go func() {
			sender.Close()
			close(closeDone)
		}()
		<-sender.ctx.Done()
		select {
		case <-closeDone:
			t.Fatal("runtime close did not join authenticated settlement")
		default:
		}
		close(releaseSettlement)
		<-responseGate.started
		settled := <-admissionResult
		var rejected *LaneRejectedError
		if !errors.As(settled.err, &rejected) ||
			rejected.Rejection.Code != protocolsession.LaneRejectStopping ||
			!settled.admission.SettlementBegan ||
			settled.admission.GrantOperationID != grant.OperationID ||
			settled.admission.Disposition != SenderPeerAdmissionRejected ||
			settled.admission.Rejection != rejected.Rejection ||
			settled.admission.ResponseDelivery != SenderPeerResponseDeliveryFailed {
			t.Fatalf("runtime-close settlement = %+v, %v", settled.admission, settled.err)
		}
		<-closeDone
		if candidate.closes.Load() != 1 {
			t.Fatalf("runtime-close candidate closes=%d", candidate.closes.Load())
		}
	})

	t.Run("receiver lane attachment", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer sender.Close()
		grant := mustRequestLane(t, receiver)
		candidate, peer := newMemoryChannelPair()
		defer candidate.Close()
		defer peer.Close()
		admissionResult := make(chan error, 1)
		go func() {
			_, err := receiver.AttachLane(context.Background(), grant, candidate)
			admissionResult <- err
		}()
		select {
		case <-peer.Recv():
		case <-time.After(time.Second):
			t.Fatal("receiver attachment did not reach the response boundary")
		}
		closeDone := make(chan struct{})
		go func() {
			receiver.Close()
			close(closeDone)
		}()
		select {
		case err := <-admissionResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("terminal-canceled receiver attachment error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("receiver close did not cancel its external handshake")
		}
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("receiver close published no completion after external handshake drained")
		}
	})
}
