package requestlane

import (
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestResponseEstimatesSeparateKindsAndExpire(t *testing.T) {
	now := time.Unix(100, 0)
	open := protocolsession.MessageOpenRevisions
	release := protocolsession.MessageReleaseLease
	lane := New(5 * time.Millisecond)
	for _, elapsed := range []time.Duration{100 * time.Millisecond, 20 * time.Millisecond} {
		reservation := lane.Reserve(open, now)
		now = now.Add(elapsed)
		reservation.Complete(now)
		reservation.Complete(now.Add(time.Second))
		reservation.Abandon()
	}
	if got := lane.Estimate(open, now, 0); got.Response != 80*time.Millisecond || got.Pending != 0 {
		t.Fatalf("smoothed response = %+v", got)
	}
	if got := lane.Estimate(release, now, 0); got.Response != 5*time.Millisecond {
		t.Fatalf("unrelated request kind inherited response = %+v", got)
	}
	if got := lane.Estimate(open, now.Add(sampleLifetime), 0); got.Response != 5*time.Millisecond {
		t.Fatalf("stale sample did not return to handshake evidence = %+v", got)
	}
	lane.Reserve(open, now).Complete(now.Add(time.Second))
	if got := lane.Estimate(open, now.Add(time.Second), 0); got.Response != time.Second {
		t.Fatalf("congestion was smoothed away = %+v", got)
	}
}

func TestPendingRequestsAndContentQueueContributeToCost(t *testing.T) {
	now := time.Unix(100, 0)
	kind := protocolsession.MessageOpenRevisions
	lane := New(10 * time.Millisecond)
	first := lane.Reserve(kind, now)
	second := lane.Reserve(kind, now)
	got := lane.Estimate(kind, now.Add(50*time.Millisecond), 200*time.Millisecond)
	if got.Pending != 2 || got.Response != 50*time.Millisecond || got.Expected != 350*time.Millisecond {
		t.Fatalf("pending cost = %+v", got)
	}
	first.Abandon()
	first.Complete(now.Add(time.Second))
	second.Abandon()
	if got := lane.Estimate(kind, now, 0); got.Pending != 0 || got.Response != 10*time.Millisecond {
		t.Fatalf("cancellation became a response sample = %+v", got)
	}
}

func TestEstimatesAreBoundedAndUnsupportedOperationsStaySeparate(t *testing.T) {
	now := time.Unix(100, 0)
	kind := protocolsession.MessageListChildren
	for _, test := range []struct{ initial, expected time.Duration }{
		{0, MinimumResponse}, {-time.Second, MinimumResponse},
		{time.Nanosecond, MinimumResponse}, {24 * time.Hour, MaximumEstimate},
	} {
		lane := New(test.initial)
		if got := lane.Estimate(kind, now, -time.Hour); got.Expected != test.expected {
			t.Fatalf("initial %v: %+v", test.initial, got)
		}
		reservation := lane.Reserve(kind, now)
		if got := lane.Estimate(kind, now.Add(48*time.Hour), 48*time.Hour); got.Expected != MaximumEstimate {
			t.Fatalf("overflowing estimate = %+v", got)
		}
		reservation.Complete(now.Add(-time.Second))
		if got := lane.Estimate(kind, now, 0); got.Response != MinimumResponse {
			t.Fatalf("backward clock = %+v", got)
		}
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageListChildren, protocolsession.MessageOpenRevisions,
		protocolsession.MessageRenewLease, protocolsession.MessageReleaseLease,
	} {
		if !Managed(kind) {
			t.Fatalf("control kind %v not scheduled", kind)
		}
	}
	for _, kind := range []protocolsession.MessageKind{
		protocolsession.MessageRequestBlocks, protocolsession.MessagePeerOffer, protocolsession.MessageLaneAttach,
	} {
		if Managed(kind) {
			t.Fatalf("stream/negotiation %v treated as RTT", kind)
		}
	}
	var absent *Reservation
	absent.Complete(now)
	absent.Abandon()
}
