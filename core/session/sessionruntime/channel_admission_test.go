package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type provisionalTestChannel struct {
	protocolsession.FrameChannel
	confirmations atomic.Int32
	failure       error
}

func (channel *provisionalTestChannel) ConfirmAdmission(context.Context) error {
	channel.confirmations.Add(1)
	return channel.failure
}

func TestProvisionalTransportRequiresAuthenticatedSessionAdmission(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "confirmation failed"}[fail], func(t *testing.T) {
			fixture := newVerticalFixture(t)
			base, peer := newMemoryChannelPair()
			defer base.Close()
			defer peer.Close()
			channel := &provisionalTestChannel{FrameChannel: base}
			if fail {
				channel.failure = errors.New("provisional slot expired")
			}
			type admissionResult struct {
				admission SenderChannelAdmission
				err       error
			}
			result := make(chan admissionResult, 1)
			go func() {
				admission, err := fixture.senderFactory.AdmitChannel(t.Context(), channel)
				result <- admissionResult{admission: admission, err: err}
			}()
			receiver, err := fixture.receiverFactory.Connect(t.Context(), peer, transfer.LaneRouteRelay)
			if receiver != nil {
				defer receiver.Close()
			}
			if !fail && err != nil {
				t.Fatal(err)
			}
			accepted := <-result
			if accepted.admission.Session != nil {
				defer accepted.admission.Session.Close()
			}
			err = accepted.err
			if !errors.Is(err, channel.failure) {
				t.Fatalf("admission error = %v, want %v", err, channel.failure)
			}
			if channel.confirmations.Load() != 1 {
				t.Fatal("authenticated admission did not confirm transport once")
			}
		})
	}
	t.Run("invalid proof never confirms", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		base, peer := newMemoryChannelPair()
		defer base.Close()
		defer peer.Close()
		channel := &provisionalTestChannel{FrameChannel: base}
		if err := peer.Send(t.Context(), []byte("arbitrary traffic")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.senderFactory.AdmitChannel(t.Context(), channel); !errors.Is(err, ErrHandshake) {
			t.Fatal(err)
		}
		if channel.confirmations.Load() != 0 {
			t.Fatal("untrusted bytes retained provisional transport")
		}
	})
}

func TestProvisionalTransportLaneConfirmationAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "confirmation failed"}[fail], func(t *testing.T) {
			fixture := newVerticalFixture(t)
			sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
			defer sender.Close()
			defer receiver.Close()
			grant := mustRequestLane(t, receiver)
			hello := laneHelloForGrant(t, receiver, grant)
			base, peer := newMemoryChannelPair()
			defer base.Close()
			defer peer.Close()
			channel := &provisionalTestChannel{FrameChannel: base}
			if fail {
				channel.failure = errors.New("lane slot expired")
			}
			if err := peer.Send(t.Context(), hello); err != nil {
				t.Fatal(err)
			}
			admission, err := fixture.senderFactory.AdmitChannel(t.Context(), channel)
			if !errors.Is(err, channel.failure) {
				t.Fatal(err)
			}
			if channel.confirmations.Load() != 1 {
				t.Fatal("nested candidate ownership lost confirmation capability")
			}
			want := 2
			if fail {
				want = 1
			} else if admission.Kind != SenderChannelAttachedLane {
				t.Fatal(admission.Kind)
			}
			if sender.AttachedLanes() != want {
				t.Fatalf("lane count = %d, want %d", sender.AttachedLanes(), want)
			}
		})
	}
}
