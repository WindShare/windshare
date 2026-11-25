package peerset

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/throughput"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type laneSetReceiverLanes struct {
	*fakeReceiverLanes
	laneSet *transferlanes.Set
}

func (l *laneSetReceiverLanes) LaneSet() *transferlanes.Set { return l.laneSet }

type contentOwnerReceiverLanes struct {
	*laneSetReceiverLanes
}

func (*contentOwnerReceiverLanes) DetachLane(session.LaneIdentity) bool { return false }

// immediateSampleClock fires throughput sample timers synchronously so any
// erroneously launched runContentControl(nil) goroutine panics deterministically.
type immediateSampleClock struct {
	*testClock
	sampleTimers atomic.Int32
}

func (c *immediateSampleClock) NewTimer(d time.Duration) Timer {
	timer := c.testClock.NewTimer(d)
	if d == throughput.SampleInterval {
		c.sampleTimers.Add(1)
		timer.(*testTimer).fire(c.Now())
	}
	return timer
}

func newTestReceiverFactory(t *testing.T, clock Clock) *v2peer.ReceiverFactory {
	t.Helper()
	native := newTestNative(t, nativepeer.Config{
		Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Now:          clock.Now,
		Reachability: reachability.New(reachability.Config{Now: clock.Now}),
	})
	t.Cleanup(func() { _ = native.Close(context.Background()) })
	factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Native: native,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestOpenReceiverSessionRejectsNilParent(t *testing.T) {
	factory := newTestReceiverFactory(t, newClock())
	var nilCtx context.Context
	if _, err := OpenReceiverSession(nilCtx, Config{}, ReceiverConfig{Factory: factory}); !errors.Is(err, ErrConfig) {
		t.Fatalf("OpenReceiverSession(nil) err = %v, want %v", err, ErrConfig)
	}
}

func TestOpenReceiverSessionSkipsNilLaneSet(t *testing.T) {
	cases := []struct {
		name  string
		lanes func(*fakePathControls) v2peer.ReceiverLaneSession
	}{
		{
			name: "lane set owner",
			lanes: func(controls *fakePathControls) v2peer.ReceiverLaneSession {
				return &laneSetReceiverLanes{
					fakeReceiverLanes: &fakeReceiverLanes{
						fakePathControls: controls,
						sessionID:        protocolsession.ProtocolSessionID{11},
					},
				}
			},
		},
		{
			name: "content lane owner",
			lanes: func(controls *fakePathControls) v2peer.ReceiverLaneSession {
				return &contentOwnerReceiverLanes{
					laneSetReceiverLanes: &laneSetReceiverLanes{
						fakeReceiverLanes: &fakeReceiverLanes{
							fakePathControls: controls,
							sessionID:        protocolsession.ProtocolSessionID{12},
						},
					},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &immediateSampleClock{testClock: newClock()}
			factory := newTestReceiverFactory(t, clock)
			controls := &fakePathControls{sent: make(chan []byte, 32)}
			receiverSession, err := OpenReceiverSession(t.Context(), Config{Clock: clock}, ReceiverConfig{
				Factory:   factory,
				Signaling: blockingReceiverSignaling{},
				Lanes:     tc.lanes(controls),
				Demand:    ContentDemand,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Wait for the path maintenance goroutine to emit initial demand before closing.
			receive(t, controls.sent)
			if err := receiverSession.Close(); err != nil {
				t.Fatalf("Close() err = %v", err)
			}
			if got := clock.sampleTimers.Load(); got != 0 {
				t.Fatalf("content control started with nil LaneSet: sampleTimers=%d", got)
			}
		})
	}
}

func TestOpenReceiverSessionEndsWithoutTURNRuntimeWhenLaneSetAllowsTURN(t *testing.T) {
	clock := newClock()
	factory := newTestReceiverFactory(t, clock)
	sessionID := protocolsession.ProtocolSessionID{13}
	laneSet, err := transferlanes.New(transferlanes.Config{
		ProtocolSessionID:  sessionID,
		ContentRoutePolicy: transferlanes.ContentRouteAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer laneSet.Close()

	controls := &fakePathControls{sent: make(chan []byte, 32)}
	lanes := &contentOwnerReceiverLanes{
		laneSetReceiverLanes: &laneSetReceiverLanes{
			fakeReceiverLanes: &fakeReceiverLanes{
				fakePathControls: controls,
				sessionID:        sessionID,
			},
			laneSet: laneSet,
		},
	}
	receiverSession, err := OpenReceiverSession(t.Context(), Config{Clock: clock}, ReceiverConfig{
		Factory:   factory,
		Signaling: blockingReceiverSignaling{},
		Lanes:     lanes,
		Demand:    ContentDemand,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiverSession.Close()
	if receiverSession.turn != nil {
		t.Fatal("session created TURN manager without turnRuntime")
	}

	for {
		control, err := protocolsession.DecodePeerPathControl(receive(t, controls.sent))
		if err != nil {
			t.Fatal(err)
		}
		if control.Kind == protocolsession.PeerPathDemand && control.PeerPathID == [16]byte(receiverSession.direct.key.path) {
			break
		}
	}

	revokeBody, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID:          [16]byte(receiverSession.direct.key.path),
		NetworkGenerationID: [16]byte{2},
		ControlSequence:     1,
		Kind:                protocolsession.PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controls.receive(revokeBody); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-receiverSession.direct.Done():
			goto directDone
		case timer := <-clock.timers:
			if timer.duration == time.Second {
				timer.fire(clock.Now())
			}
		case <-deadline:
			t.Fatal("direct path did not terminate after revoke")
		}
	}
directDone:
	receive(t, receiverSession.Done())
	if receiverSession.Result().Cause != ErrPathTerminal {
		t.Fatalf("session result = %+v, want ErrPathTerminal", receiverSession.Result())
	}
}
