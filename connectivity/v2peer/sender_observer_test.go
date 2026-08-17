package v2peer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type selectedPairTestPeer struct {
	*testPeerConnection
	pair *pion.ICECandidatePair
	err  error
}

func (peer *selectedPairTestPeer) SelectedCandidatePair() (*pion.ICECandidatePair, error) {
	return peer.pair, peer.err
}

type senderObservationCollector struct {
	mu           sync.Mutex
	observations []SenderAttemptObservation
}

type candidateDropTestSession struct {
	*testPeerSession
	candidateCalls chan struct{}
}

type closingAdmissionTestSession struct {
	*testPeerSession
}

func (session *closingAdmissionTestSession) AdmitPeerChannel(
	_ context.Context,
	channel protocolsession.FrameChannel,
) (sessionruntime.LaneIdentity, error) {
	session.admissions <- channel
	if err := channel.Close(); err != nil {
		return sessionruntime.LaneIdentity{}, err
	}
	return session.lane, nil
}

func (session *candidateDropTestSession) SendPeerControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	if kind == protocolsession.MessagePeerCandidate {
		session.candidateCalls <- struct{}{}
		return protocolsession.OperationDrop, nil
	}
	return session.testPeerSession.SendPeerControl(ctx, kind, operation, body)
}

func (collector *senderObservationCollector) observe(observation SenderAttemptObservation) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.observations = append(collector.observations, observation)
}

func (collector *senderObservationCollector) forAttempt(
	attemptID v2signal.AttemptID,
) []SenderAttemptObservation {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	var observations []SenderAttemptObservation
	for _, observation := range collector.observations {
		if observation.AttemptID == attemptID {
			observations = append(observations, observation)
		}
	}
	return observations
}

func TestSenderObserverEmitsExactSuccessfulLifecycle(t *testing.T) {
	basePeer := newTestPeerConnection()
	peer := &selectedPairTestPeer{
		testPeerConnection: basePeer,
		pair: &pion.ICECandidatePair{
			Local: &pion.ICECandidate{
				Address: "10.0.0.5", Port: 41000, Protocol: pion.ICEProtocolUDP,
				Typ: pion.ICECandidateTypeHost,
			},
			Remote: &pion.ICECandidate{
				Address: "10.0.0.6", Port: 42000, Protocol: pion.ICEProtocolUDP,
				Typ: pion.ICECandidateTypePrflx,
			},
		},
	}
	channel := newTestPeerChannel()
	channel.opened = make(chan struct{})
	collector := &senderObservationCollector{}
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	session := newTestPeerSession(51)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	operation, binding, operationContext := sendSenderTestOffer(t, handler, ctx, 52)

	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	remoteCandidate := v2signal.Candidate{Binding: binding, Candidate: "candidate:remote"}
	remoteBody, err := v2signal.EncodeCandidate(remoteCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleMessage(
		operationContext,
		testMessage(t, protocolsession.MessagePeerCandidate, operation, remoteBody),
	); err != nil {
		t.Fatalf("remote candidate: %v", err)
	}
	receiveTest(t, peer.added)
	peer.emitCandidate(&pion.ICECandidate{
		Address: "10.0.0.5", Port: 41000, Protocol: pion.ICEProtocolUDP,
		Typ: pion.ICECandidateTypeHost,
	})
	receiveTest(t, session.controls)

	peer.emitDataChannel(&pion.DataChannel{})
	select {
	case admission := <-session.admissions:
		t.Fatalf("channel admitted before Opened completed: %T", admission)
	case <-time.After(25 * time.Millisecond):
	}
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) >= 4 })
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 4 {
		t.Fatalf("pre-open observations = %#v", observations)
	}
	close(channel.opened)
	if admitted := receiveTest(t, session.admissions); admitted != channel {
		t.Fatalf("admitted channel = %T, want injected channel", admitted)
	}
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 7 })

	observations := collector.forAttempt(binding.AttemptID)
	wantStages := []SenderAttemptStage{
		SenderAttemptStarted,
		SenderAttemptOfferReceived,
		SenderAttemptAnswerCreated,
		SenderAttemptAnswerSent,
		SenderAttemptDataChannelOpen,
		SenderAttemptLaneAdmissionStarted,
		SenderAttemptAdmitted,
	}
	for index, observation := range observations {
		if observation.Stage != wantStages[index] {
			t.Fatalf("stage[%d] = %q, want %q", index, observation.Stage, wantStages[index])
		}
		if observation.SessionID != session.sessionID || observation.PeerPathID != binding.PeerPathID ||
			observation.AttemptID != binding.AttemptID {
			t.Fatalf("identity[%d] = %#v", index, observation)
		}
		if observation.SideSequence != uint64(index+1) {
			t.Fatalf("sequence[%d] = %d", index, observation.SideSequence)
		}
		if index > 0 && observation.AttemptElapsedMillis < observations[index-1].AttemptElapsedMillis {
			t.Fatalf("elapsed time regressed at index %d", index)
		}
		if observation.Failure != nil {
			t.Fatalf("success observation[%d] carried failure %#v", index, observation.Failure)
		}
		if index < 2 && observation.CandidateCounts != nil {
			t.Fatalf("early observation[%d] carried counts %#v", index, observation.CandidateCounts)
		}
		if index >= 2 && observation.CandidateCounts == nil {
			t.Fatalf("observation[%d] omitted candidate counts", index)
		}
		if index >= 4 && *observation.CandidateCounts != (SenderCandidateCounts{LocalEmitted: 1, RemoteAccepted: 1}) {
			t.Fatalf("candidate counts[%d] = %#v", index, observation.CandidateCounts)
		}
		if index < 5 && observation.Lane != nil {
			t.Fatalf("observation[%d] carried premature lane %#v", index, observation.Lane)
		}
		if index >= 5 && (observation.Lane == nil || *observation.Lane != session.lane) {
			t.Fatalf("lane[%d] = %#v", index, observation.Lane)
		}
		if index < 6 && observation.SelectedPair != nil {
			t.Fatalf("observation[%d] carried premature selected pair", index)
		}
	}
	selected := observations[6].SelectedPair
	if selected == nil || selected.Local.AddressFamily != "ipv4" || selected.Remote.AddressFamily != "ipv4" ||
		selected.Local.CandidateType != "host" || selected.Remote.CandidateType != "prflx" {
		t.Fatalf("selected pair = %#v", selected)
	}

	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	receiveTest(t, peer.closed)
	operationKey := testPeerOperationFromContext(t, operationContext, operation)
	waitForTest(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		_, retired := handler.retiredOperations[operationKey]
		return retired
	})
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 7 })
	select {
	case failure := <-session.failures:
		t.Fatalf("normal post-admission close emitted failure %#v", failure)
	default:
	}
	replayOperation := testOperationID(59)
	replayBody, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	replayMessage := testMessage(t, protocolsession.MessagePeerOffer, replayOperation, replayBody)
	if err := handler.HandleMessage(testPeerMessageContext(t, ctx, replayMessage), replayMessage); err != nil {
		t.Fatalf("replay offer: %v", err)
	}
	replayFailure := receiveTest(t, session.failures)
	if replayFailure.operation != replayOperation {
		t.Fatalf("replay failure = %#v", replayFailure)
	}
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 7 {
		t.Fatalf("binding replay restarted evidence stream: %#v", observations)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverPanicCannotChangeConnectivityOutcome(t *testing.T) {
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	diagnostics := &peerDiagnosticCollector{}
	factory := mustTestFactory(t, Config{
		Observer:           SenderAttemptObserverFunc(func(SenderAttemptObservation) { panic("observer failure") }),
		DiagnosticObserver: PeerDiagnosticObserverFunc(diagnostics.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	session := newTestPeerSession(61)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	_, _, _ = sendSenderTestOffer(t, handler, ctx, 62)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	peer.emitDataChannel(&pion.DataChannel{})
	receiveTest(t, session.admissions)
	waitForTest(t, func() bool {
		observation, ok := diagnostics.latest(
			PeerDiagnosticSenderAttempt,
			PeerDiagnosticObserverPanic,
		)
		return ok && observation.Count == 7
	})
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	receiveTest(t, peer.closed)
	select {
	case failure := <-session.failures:
		t.Fatalf("observer panic emitted operation failure %#v", failure)
	default:
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverAdmissionWinsImmediateNormalClose(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	baseSession := newTestPeerSession(66)
	session := &closingAdmissionTestSession{testPeerSession: baseSession}
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	_, binding, _ := sendSenderTestOffer(t, handler, ctx, 67)
	receiveTest(t, peer.remote)
	receiveTest(t, baseSession.controls)
	peer.emitDataChannel(&pion.DataChannel{})
	receiveTest(t, baseSession.admissions)
	receiveTest(t, peer.closed)
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 7 })
	observed := collector.forAttempt(binding.AttemptID)
	if terminal := observed[len(observed)-1]; terminal.Stage != SenderAttemptAdmitted || terminal.Failure != nil {
		t.Fatalf("immediate-close terminal = %#v", terminal)
	}
	select {
	case failure := <-baseSession.failures:
		t.Fatalf("immediate normal close emitted operation failure %#v", failure)
	default:
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverPreservesNegotiationFailure(t *testing.T) {
	observations := make(chan SenderAttemptObservation, 8)
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(func(observation SenderAttemptObservation) {
			observations <- observation
		}),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, errors.New("synthetic peer creation failure")
		}),
	})
	session := newTestPeerSession(71)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	operation, binding, _ := sendSenderTestOffer(t, handler, ctx, 72)
	started := receiveTest(t, observations)
	offerReceived := receiveTest(t, observations)
	failed := receiveTest(t, observations)
	if started.Stage != SenderAttemptStarted || offerReceived.Stage != SenderAttemptOfferReceived ||
		failed.Stage != SenderAttemptFailed {
		t.Fatalf("failure lifecycle = %q, %q, %q", started.Stage, offerReceived.Stage, failed.Stage)
	}
	if failed.AttemptID != binding.AttemptID || failed.SideSequence != 3 || failed.CandidateCounts != nil {
		t.Fatalf("failure envelope = %#v", failed)
	}
	wantFailure := SenderAttemptFailure{
		FailedAtStage:      SenderAttemptAnswerCreated,
		Scope:              AttemptFailureScopeAttempt,
		TypedPeerErrorCode: TypedPeerErrorNegotiation,
		Message:            peerNegotiationFailureMessage,
	}
	if failed.Failure == nil || failed.Failure.FailedAtStage != wantFailure.FailedAtStage ||
		failed.Failure.Scope != wantFailure.Scope ||
		failed.Failure.TypedPeerErrorCode != wantFailure.TypedPeerErrorCode ||
		failed.Failure.Message != wantFailure.Message || failed.Failure.Operation == nil ||
		failed.Failure.Operation.Code != protocolsession.PeerOperationCodeNegotiation ||
		failed.Failure.Operation.Message != peerNegotiationFailureMessage {
		t.Fatalf("failure details = %#v", failed.Failure)
	}
	wireFailure := receiveTest(t, session.failures)
	if wireFailure.operation != operation || wireFailure.code != protocolsession.PeerOperationCodeNegotiation ||
		wireFailure.message != peerNegotiationFailureMessage {
		t.Fatalf("wire failure = %#v", wireFailure)
	}
	receiveTest(t, peer.closed)
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverPreservesAdmissionFailure(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return channel, nil
		}),
	})
	session := newTestPeerSession(76)
	session.admitErr = errors.New("synthetic admission failure")
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	operation, binding, _ := sendSenderTestOffer(t, handler, ctx, 77)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	peer.emitDataChannel(&pion.DataChannel{})
	receiveTest(t, session.admissions)
	wireFailure := receiveTest(t, session.failures)
	if wireFailure.operation != operation || wireFailure.code != protocolsession.PeerOperationCodeAdmission ||
		wireFailure.message != peerAdmissionFailureMessage {
		t.Fatalf("admission wire failure = %#v", wireFailure)
	}
	receiveTest(t, peer.closed)
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 6 })
	observed := collector.forAttempt(binding.AttemptID)
	terminal := observed[len(observed)-1]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptLaneAdmissionStarted ||
		terminal.Failure.Scope != AttemptFailureScopeAttempt ||
		terminal.Failure.TypedPeerErrorCode != TypedPeerErrorAdmission ||
		terminal.Failure.Message != peerAdmissionFailureMessage ||
		terminal.Failure.Operation == nil ||
		terminal.Failure.Operation.Code != protocolsession.PeerOperationCodeAdmission ||
		terminal.CandidateCounts == nil {
		t.Fatalf("admission terminal = %#v", terminal)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverClassifiesCancellationAndRuntimeStop(t *testing.T) {
	for _, test := range []struct {
		name      string
		cancelRun bool
		scope     AttemptFailureScope
		code      TypedPeerErrorCode
		message   string
	}{
		{
			name: "operation cancellation", scope: AttemptFailureScopeAttempt,
			code: TypedPeerErrorCancelled, message: peerAttemptCancelledMessage,
		},
		{
			name: "runtime stop", cancelRun: true, scope: AttemptFailureScopeSession,
			code: TypedPeerErrorStopped, message: peerRuntimeStoppedMessage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := &senderObservationCollector{}
			peer := newTestPeerConnection()
			factory := mustTestFactory(t, Config{
				Observer: SenderAttemptObserverFunc(collector.observe),
				PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
					return peer, nil
				}),
			})
			session := newTestPeerSession(81)
			handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
			operation, binding, operationContext := sendSenderTestOffer(t, handler, ctx, 82)
			receiveTest(t, peer.remote)
			receiveTest(t, session.controls)
			if test.cancelRun {
				cancel()
				if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
					t.Fatalf("runtime stop = %v", err)
				}
			} else {
				if err := handler.Cancel(operationContext, operation); err != nil {
					t.Fatalf("cancel operation: %v", err)
				}
				stopSenderTestRuntime(t, cancel, runDone)
			}
			waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 5 })
			observed := collector.forAttempt(binding.AttemptID)
			failed := observed[len(observed)-1]
			if failed.Stage != SenderAttemptFailed || failed.Failure == nil ||
				failed.Failure.FailedAtStage != SenderAttemptDataChannelOpen ||
				failed.Failure.Scope != test.scope || failed.Failure.TypedPeerErrorCode != test.code ||
				failed.Failure.Message != test.message || failed.Failure.Operation != nil {
				t.Fatalf("classified failure = %#v", failed)
			}
			select {
			case failure := <-session.failures:
				t.Fatalf("cancellation emitted operation failure %#v", failure)
			default:
			}
		})
	}
}

func TestSenderObserverRejectsCapacityWithCompleteAttemptStream(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	now := time.Unix(8_000, 0)
	factory := mustTestFactory(t, Config{
		MaxActiveAttempts:  1,
		MaxRetiredBindings: 1,
		RetiredBindingTTL:  time.Minute,
		Now:                func() time.Time { return now },
		Observer:           SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := newTestPeerSession(91)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	firstOperation, _, firstContext := sendSenderTestOffer(t, handler, ctx, 92)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	rejectedOperation, rejectedBinding, rejectedContext := sendSenderTestOffer(t, handler, ctx, 94)
	receiveTest(t, session.failures)
	waitForTest(t, func() bool { return len(collector.forAttempt(rejectedBinding.AttemptID)) == 3 })
	rejected := collector.forAttempt(rejectedBinding.AttemptID)
	if rejected[0].Stage != SenderAttemptStarted || rejected[1].Stage != SenderAttemptOfferReceived ||
		rejected[2].Stage != SenderAttemptFailed || rejected[2].Failure == nil ||
		rejected[2].Failure.FailedAtStage != SenderAttemptAnswerCreated ||
		rejected[2].Failure.Operation == nil ||
		rejected[2].Failure.Operation.Code != protocolsession.PeerOperationCodeNegotiation {
		t.Fatalf("rejected attempt stream = %#v", rejected)
	}
	repeatedBody, err := v2signal.EncodeOffer(v2signal.Offer{Binding: rejectedBinding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	// The first active binding saturates replay retention. Evidence still owns
	// the rejected identity, and that claim survives the retention window.
	now = now.Add(2 * time.Minute)
	if err := handler.HandleMessage(
		rejectedContext,
		testMessage(t, protocolsession.MessagePeerOffer, rejectedOperation, repeatedBody),
	); err != nil {
		t.Fatalf("repeat rejected offer: %v", err)
	}
	receiveTest(t, session.failures)
	if repeated := collector.forAttempt(rejectedBinding.AttemptID); len(repeated) != 3 {
		t.Fatalf("repeated rejection restarted evidence stream: %#v", repeated)
	}
	if err := handler.Cancel(firstContext, firstOperation); err != nil {
		t.Fatalf("cancel first operation: %v", err)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverCountsOnlyDeliveredCandidatesBeforeLimitFailure(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		MaxCandidates: 1,
		Observer:      SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	session := newTestPeerSession(101)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	_, binding, _ := sendSenderTestOffer(t, handler, ctx, 102)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	candidate := &pion.ICECandidate{
		Address: "10.0.0.8", Port: 43000, Protocol: pion.ICEProtocolUDP,
		Typ: pion.ICECandidateTypeHost,
	}
	peer.emitCandidate(candidate)
	if control := receiveTest(t, session.controls); control.kind != protocolsession.MessagePeerCandidate {
		t.Fatalf("first candidate control = %#v", control)
	}
	peer.emitCandidate(candidate)
	failure := receiveTest(t, session.failures)
	if failure.code != protocolsession.PeerOperationCodeCandidates || failure.message != peerCandidateLimitMessage {
		t.Fatalf("candidate limit failure = %#v", failure)
	}
	receiveTest(t, peer.closed)
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 5 })
	observed := collector.forAttempt(binding.AttemptID)
	terminal := observed[len(observed)-1]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptDataChannelOpen ||
		terminal.Failure.TypedPeerErrorCode != TypedPeerErrorCandidates || terminal.CandidateCounts == nil ||
		*terminal.CandidateCounts != (SenderCandidateCounts{LocalEmitted: 1}) {
		t.Fatalf("candidate limit terminal = %#v", terminal)
	}
	select {
	case extra := <-session.controls:
		t.Fatalf("over-limit candidate was emitted: %#v", extra)
	default:
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderObserverTreatsRetiredCandidateAsAttemptCancellation(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		Observer: SenderAttemptObserverFunc(collector.observe),
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, nil
		}),
	})
	baseSession := newTestPeerSession(111)
	session := &candidateDropTestSession{
		testPeerSession: baseSession,
		candidateCalls:  make(chan struct{}, 1),
	}
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	_, binding, _ := sendSenderTestOffer(t, handler, ctx, 112)
	receiveTest(t, peer.remote)
	receiveTest(t, baseSession.controls)
	peer.emitCandidate(&pion.ICECandidate{
		Address: "10.0.0.9", Port: 44000, Protocol: pion.ICEProtocolUDP,
		Typ: pion.ICECandidateTypeHost,
	})
	receiveTest(t, session.candidateCalls)
	receiveTest(t, peer.closed)
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 5 })
	observed := collector.forAttempt(binding.AttemptID)
	terminal := observed[len(observed)-1]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptDataChannelOpen ||
		terminal.Failure.Scope != AttemptFailureScopeAttempt ||
		terminal.Failure.TypedPeerErrorCode != TypedPeerErrorCancelled ||
		terminal.Failure.Operation != nil || terminal.CandidateCounts == nil ||
		*terminal.CandidateCounts != (SenderCandidateCounts{}) {
		t.Fatalf("dropped candidate terminal = %#v", terminal)
	}
	select {
	case failure := <-baseSession.failures:
		t.Fatalf("retired candidate emitted operation failure %#v", failure)
	default:
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderAttemptRecorderAllowsOnlyOneTerminalAtEveryBoundary(t *testing.T) {
	stages := []SenderAttemptStage{
		SenderAttemptStarted,
		SenderAttemptOfferReceived,
		SenderAttemptAnswerCreated,
		SenderAttemptAnswerSent,
		SenderAttemptDataChannelOpen,
		SenderAttemptLaneAdmissionStarted,
		SenderAttemptAdmitted,
	}
	for failedIndex := 1; failedIndex < len(stages); failedIndex++ {
		t.Run(string(stages[failedIndex]), func(t *testing.T) {
			collector := &senderObservationCollector{}
			factory := mustTestFactory(t, Config{Observer: SenderAttemptObserverFunc(collector.observe)})
			session := newTestPeerSession(byte(100 + failedIndex))
			binding := testBinding(byte(110 + failedIndex))
			recorder := newSenderAttemptRecorder(factory, session.sessionID, binding)
			for index := 0; index < failedIndex; index++ {
				recorder.complete(stages[index], SenderCandidateCounts{}, nil, nil)
			}
			recorder.fail(SenderAttemptFailure{
				Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorUnexpected,
				Message: peerUnexpectedFailureMessage,
			})
			recorder.fail(SenderAttemptFailure{
				Scope: AttemptFailureScopeSession, TypedPeerErrorCode: TypedPeerErrorStopped,
				Message: peerRuntimeStoppedMessage,
			})
			recorder.complete(stages[failedIndex], SenderCandidateCounts{}, nil, nil)
			waitForTest(t, func() bool {
				return len(collector.forAttempt(binding.AttemptID)) == failedIndex+1
			})
			observed := collector.forAttempt(binding.AttemptID)
			if len(observed) != failedIndex+1 || observed[len(observed)-1].Stage != SenderAttemptFailed ||
				observed[len(observed)-1].Failure == nil ||
				observed[len(observed)-1].Failure.FailedAtStage != stages[failedIndex] {
				t.Fatalf("terminal observations = %#v", observed)
			}
		})
	}
}

func TestSelectedPairEvidenceRejectsNonOperationalIPv4Ranges(t *testing.T) {
	for _, address := range []string{"0.1.2.3", "127.0.0.2", "169.254.1.2", "224.0.0.1", "240.0.0.1"} {
		t.Run(address, func(t *testing.T) {
			_, err := pionCandidateEvidence(&pion.ICECandidate{
				Address: address, Port: 40000, Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeHost,
			})
			if err == nil {
				t.Fatalf("address %s was accepted as operational", address)
			}
		})
	}
}

func startSenderTestRuntime(
	t *testing.T,
	factory *Factory,
	session sessionruntime.SenderPeerSession,
) (*senderHandler, context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	interfaceHandler, err := factory.NewSenderPeerHandler(session)
	if err != nil {
		t.Fatalf("create sender handler: %v", err)
	}
	handler := interfaceHandler.(*senderHandler)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- handler.Run(ctx) }()
	return handler, ctx, cancel, runDone
}

func sendSenderTestOffer(
	t *testing.T,
	handler *senderHandler,
	ctx context.Context,
	seed byte,
) (protocolsession.OperationID, v2signal.Binding, context.Context) {
	t.Helper()
	operation := testOperationID(seed)
	binding := testBinding(seed + 1)
	body, err := v2signal.EncodeOffer(v2signal.Offer{Binding: binding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, protocolsession.MessagePeerOffer, operation, body)
	operationContext := testPeerMessageContext(t, ctx, message)
	if err := handler.HandleMessage(operationContext, message); err != nil {
		t.Fatalf("send offer: %v", err)
	}
	return operation, binding, operationContext
}

func stopSenderTestRuntime(t *testing.T, cancel context.CancelFunc, runDone <-chan error) {
	t.Helper()
	cancel()
	if err := receiveTest(t, runDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop sender runtime: %v", err)
	}
}
