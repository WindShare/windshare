package receiversession

import (
	"context"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"testing"
	"time"
)

func TestAttachLaneRetainsAuthenticatedAcceptanceWhenInstallationFails(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	t.Cleanup(sender.Close)
	t.Cleanup(receiver.Close)
	grant := mustRequestLane(t, receiver)
	// Closing content admission after the grant is issued deterministically makes
	// local publication fail after the signed acceptance has been verified.
	receiver.laneSet.Stop()
	senderChannel, receiverBase := newMemoryChannelPair()
	receiverChannel := &countingAdmissionChannel{FrameChannel: receiverBase}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	senderResult := make(chan struct {
		identity sessioncontract.LaneIdentity
		err      error
	}, 1)
	go func() {
		identity, err := fixture.senderFactory.Attach(ctx, senderChannel)
		senderResult <- struct {
			identity sessioncontract.LaneIdentity
			err      error
		}{identity: identity, err: err}
	}()

	admission, err := receiver.AttachLane(ctx, grant, receiverChannel, transferlanes.RouteDirect)
	if !errors.Is(err, transferlanes.ErrClosed) {
		t.Fatalf("receiver installation error = %v", err)
	}
	if admission.GrantOperationID != grant.OperationID ||
		admission.Lane != (sessioncontract.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch}) ||
		admission.Disposition != LaneAdmissionAccepted ||
		admission.LaneInstallation != LaneInstallationFailed {
		t.Fatalf("accepted installation failure = %+v", admission)
	}
	attached := <-senderResult
	if attached.err != nil || attached.identity != admission.Lane {
		t.Fatalf("sender settlement = %+v, %v", attached.identity, attached.err)
	}
	if receiverChannel.closes.Load() != 1 {
		t.Fatalf("failed receiver candidate closes = %d, want 1", receiverChannel.closes.Load())
	}
}
