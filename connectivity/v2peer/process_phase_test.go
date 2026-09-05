package v2peer

import (
	"context"
	"net/netip"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type processPhaseLanes struct{ *receiverTestLanes }

func (processPhaseLanes) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return protocolsession.ProtocolSessionID{9}
}

type processPhaseNetwork struct{}

func (processPhaseNetwork) Snapshot(context.Context) (networkstate.State, error) {
	return networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr("127.0.0.1"), InterfaceIndex: 1}}}, nil
}
func TestNativeReceiverProcessWaitPrecedesPreparationAndFullCheckingPhases(t *testing.T) {
	gate := nativepeer.NewProcessAdmission(nativepeer.AdmissionClock{})
	pool, _ := icepolicy.NewICEEndpointPool(nil)
	native := nativepeer.New(nativepeer.Config{Admission: gate, Pool: &pool, Monitor: networkstate.NewMonitor(processPhaseNetwork{}, time.Nanosecond), Reachability: reachability.New(reachability.Config{}), ObservationCapacity: 64})
	defer native.Close(context.Background())
	binding := v2signal.Binding{PeerPathID: v2signal.PeerPathID{1}, AttemptID: v2signal.AttemptID{1}, AttemptSequence: 1}
	var blockers []*provider.Connection
	for i := byte(1); i <= nativepeer.ProcessConcurrentAttempts; i++ {
		pc, err := native.NewPeerConnection(context.Background(), nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{i}, Binding: binding})
		if err != nil {
			t.Fatal(err)
		}
		blockers = append(blockers, pc)
	}
	timers := newRecordingReceiverPhaseTimerSource()
	factory, err := NewReceiverFactory(ReceiverFactoryConfig{Native: native, PhaseTimers: timers})
	if err != nil {
		t.Fatal(err)
	}
	signaling := &receiverTestSignaling{operation: newReceiverTestOperation(), offers: make(chan []byte, 2)}
	attempts := make(chan *ReceiverAttempt, 1)
	failures := make(chan error, 1)
	go func() {
		a, err := factory.StartBinding(context.Background(), signaling, processPhaseLanes{newReceiverTestLanes()}, binding)
		if err != nil {
			failures <- err
			return
		}
		attempts <- a
	}()
	for {
		event := receiveTest(t, native.Observations())
		if event.Admission != nil && event.Admission.Kind == nativepeer.AdmissionQueued && event.Subject.ProtocolSessionID == [16]byte{9} {
			break
		}
	}
	select {
	case phase := <-timers.created:
		t.Fatal("queued work armed phase", phase)
	default:
	}
	_ = blockers[0].Close()
	preparation := receiveTest(t, timers.created)
	if preparation.phase != PeerAttemptPhasePreparation || preparation.duration != PeerSignalingPreparationBudget {
		t.Fatal(preparation)
	}
	var attempt *ReceiverAttempt
	select {
	case attempt = <-attempts:
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("native attempt did not start")
	}
	defer attempt.Close()
	attempt.phases.observeICE(pion.ICEConnectionStateChecking)
	checking := receiveTest(t, timers.created)
	if checking.phase != PeerAttemptPhaseChecking || checking.duration != 40*time.Second {
		t.Fatal(checking)
	}
	preparation.timer.Fire()
	if attempt.phaseContext.Err() != nil {
		t.Fatal("obsolete preparation timer cut checking")
	}
	for _, pc := range blockers {
		_ = pc.Close()
	}
}
