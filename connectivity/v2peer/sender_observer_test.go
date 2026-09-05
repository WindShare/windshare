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

type senderObservationCollector struct {
	mu           sync.Mutex
	source       <-chan SenderAttemptObservation
	observations []SenderAttemptObservation
}

var successfulSenderAttemptStages = []SenderAttemptStage{
	SenderAttemptStarted,
	SenderAttemptNegotiationDeadlineArmed,
	SenderAttemptOfferReceived,
	SenderAttemptAnswerCreated,
	SenderAttemptAnswerSent,
	SenderAttemptDataChannelOpen,
	SenderAttemptAdmissionDeadlineArmed,
	SenderAttemptLaneHelloAuthenticated,
	SenderAttemptAdmissionResponseSettled,
	SenderAttemptAdmitted,
}

func senderAttemptReachedTerminal(
	observations []SenderAttemptObservation,
	terminal SenderAttemptStage,
) bool {
	return len(observations) != 0 && observations[len(observations)-1].Stage == terminal
}

func assertSenderAttemptStages(
	t *testing.T,
	observations []SenderAttemptObservation,
	want []SenderAttemptStage,
) {
	t.Helper()
	if len(observations) != len(want) {
		t.Fatalf("sender attempt stage count = %d, want %d: %#v", len(observations), len(want), observations)
	}
	for index, observation := range observations {
		if observation.Stage != want[index] {
			t.Fatalf("sender attempt stage[%d] = %q, want %q", index, observation.Stage, want[index])
		}
	}
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
	control sessionruntime.SenderPeerAdmissionControl,
) (sessionruntime.SenderPeerAdmissionResult, error) {
	session.admissions <- receiverObservedChannel(channel)
	grantOperation := testOperationID(249)
	if !control.BeginAuthenticatedSettlement(grantOperation, session.lane) {
		_ = channel.Close()
		return sessionruntime.SenderPeerAdmissionResult{
			Disposition:      sessionruntime.SenderPeerAdmissionSilentClose,
			ResponseDelivery: sessionruntime.SenderPeerResponseNotAttempted,
			LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
		}, context.Canceled
	}
	if err := channel.Close(); err != nil {
		return sessionruntime.SenderPeerAdmissionResult{
			SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
			Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
			ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
			LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentFailed,
		}, err
	}
	return sessionruntime.SenderPeerAdmissionResult{
		SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
		Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
		ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttached,
	}, nil
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

func (collector *senderObservationCollector) bind(source <-chan SenderAttemptObservation) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.source = source
}

func (collector *senderObservationCollector) forAttempt(
	attemptID v2signal.AttemptID,
) []SenderAttemptObservation {
	collector.mu.Lock()
	defer collector.mu.Unlock()
draining:
	for collector.source != nil {
		select {
		case observation, open := <-collector.source:
			if !open {
				collector.source = nil
				continue
			}
			collector.observations = append(collector.observations, observation)
		default:
			break draining
		}
	}
	var observations []SenderAttemptObservation
	for _, observation := range collector.observations {
		if observation.AttemptID == attemptID {
			observations = append(observations, observation)
		}
	}
	return observations
}

func mustTestFactoryWithSenderCollector(
	t *testing.T,
	collector *senderObservationCollector,
	config Config,
) *Factory {
	t.Helper()
	if config.SenderAttemptObservationCapacity == 0 {
		config.SenderAttemptObservationCapacity = DefaultSenderAttemptObservationCapacity
	}
	factory := mustTestFactory(t, config)
	collector.bind(factory.SenderAttemptObservations())
	return factory
}

func TestSenderAttemptObservationEmitsExactSuccessfulLifecycle(t *testing.T) {
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	channel.opened = make(chan struct{})
	collector := &senderObservationCollector{}
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
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
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) >= 5 })
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 5 {
		t.Fatalf("pre-open observations = %#v", observations)
	}
	close(channel.opened)
	if admitted := receiveTest(t, session.admissions); admitted != channel {
		t.Fatalf("admitted channel = %T, want injected channel", admitted)
	}
	admittedOwner := receiveTest(t, session.ownedAdmissions)
	waitForTest(t, func() bool {
		observed := collector.forAttempt(binding.AttemptID)
		return len(observed) != 0 && (observed[len(observed)-1].Stage == SenderAttemptAdmitted ||
			observed[len(observed)-1].Stage == SenderAttemptFailed)
	})

	observations := collector.forAttempt(binding.AttemptID)
	if len(observations) != 10 {
		t.Fatalf("successful lifecycle stages = %v", attemptObservationStages(observations))
	}
	wantStages := []SenderAttemptStage{
		SenderAttemptStarted,
		SenderAttemptNegotiationDeadlineArmed,
		SenderAttemptOfferReceived,
		SenderAttemptAnswerCreated,
		SenderAttemptAnswerSent,
		SenderAttemptDataChannelOpen,
		SenderAttemptAdmissionDeadlineArmed,
		SenderAttemptLaneHelloAuthenticated,
		SenderAttemptAdmissionResponseSettled,
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
		if index < 3 && observation.CandidateCounts != nil {
			t.Fatalf("early observation[%d] carried counts %#v", index, observation.CandidateCounts)
		}
		if index >= 3 && observation.CandidateCounts == nil {
			t.Fatalf("observation[%d] omitted candidate counts", index)
		}
		if index >= 5 && *observation.CandidateCounts != (SenderCandidateCounts{LocalEmitted: 1, RemoteAccepted: 1}) {
			t.Fatalf("candidate counts[%d] = %#v", index, observation.CandidateCounts)
		}
		if index < 7 && observation.Lane != nil {
			t.Fatalf("observation[%d] carried premature lane %#v", index, observation.Lane)
		}
		if index >= 7 && (observation.Lane == nil || *observation.Lane != session.lane) {
			t.Fatalf("lane[%d] = %#v", index, observation.Lane)
		}
	}
	if observations[0].OfferOperationID != operation || observations[7].GrantOperationID.IsZero() {
		t.Fatalf("operation correlation = %#v", observations)
	}
	if observations[1].Phase != SenderAttemptPhaseNegotiation || observations[1].DeadlineMillis == 0 ||
		observations[6].Phase != SenderAttemptPhaseAdmission || observations[6].DeadlineMillis == 0 {
		t.Fatalf("phase deadlines = %#v", observations)
	}
	if observations[8].AdmissionDisposition != SenderAdmissionAccepted ||
		observations[8].ResponseDelivery != SenderResponseDelivered {
		t.Fatalf("admission settlement = %#v", observations[8])
	}

	if err := admittedOwner.Close(); err != nil {
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
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 10 })
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
	if observations := collector.forAttempt(binding.AttemptID); len(observations) != 10 {
		t.Fatalf("binding replay restarted evidence stream: %#v", observations)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderAttemptObservationImmediateCloseHasOneSafeTerminal(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
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
	waitForTest(t, func() bool {
		observed := collector.forAttempt(binding.AttemptID)
		return len(observed) != 0 && (observed[len(observed)-1].Stage == SenderAttemptAdmitted ||
			observed[len(observed)-1].Stage == SenderAttemptFailed)
	})
	observed := collector.forAttempt(binding.AttemptID)
	terminal := observed[len(observed)-1]
	if len(observed) != 8 && len(observed) != 10 {
		t.Fatalf("immediate-close lifecycle stages = %v", attemptObservationStages(observed))
	}
	if terminal.Stage != SenderAttemptAdmitted && terminal.Stage != SenderAttemptFailed {
		t.Fatalf("immediate-close terminal = %#v", terminal)
	}
	if terminal.Failure != nil && (terminal.Failure.Message != "" ||
		(terminal.Failure.Operation != nil && terminal.Failure.Operation.Message != "")) {
		t.Fatalf("immediate-close failure retained private text = %#v", terminal.Failure)
	}
	select {
	case failure := <-baseSession.failures:
		t.Fatalf("immediate normal close emitted operation failure %#v", failure)
	default:
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func attemptObservationStages(observations []SenderAttemptObservation) []SenderAttemptStage {
	stages := make([]SenderAttemptStage, len(observations))
	for index := range observations {
		stages[index] = observations[index].Stage
	}
	return stages
}

func TestSenderAttemptObservationRedactsNegotiationFailureText(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{
		SenderAttemptObservationCapacity: DefaultSenderAttemptObservationCapacity,
		PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) {
			return peer, errors.New("synthetic peer creation failure")
		}),
	})
	observations := factory.SenderAttemptObservations()
	session := newTestPeerSession(71)
	handler, ctx, cancel, runDone := startSenderTestRuntime(t, factory, session)
	operation, binding, _ := sendSenderTestOffer(t, handler, ctx, 72)
	started := receiveTest(t, observations)
	deadlineArmed := receiveTest(t, observations)
	offerReceived := receiveTest(t, observations)
	failed := receiveTest(t, observations)
	if started.Stage != SenderAttemptStarted || deadlineArmed.Stage != SenderAttemptNegotiationDeadlineArmed ||
		offerReceived.Stage != SenderAttemptOfferReceived ||
		failed.Stage != SenderAttemptFailed {
		t.Fatalf("failure lifecycle = %q, %q, %q, %q", started.Stage, deadlineArmed.Stage, offerReceived.Stage, failed.Stage)
	}
	if failed.AttemptID != binding.AttemptID || failed.SideSequence != 4 || failed.CandidateCounts != nil {
		t.Fatalf("failure envelope = %#v", failed)
	}
	wantFailure := SenderAttemptFailure{
		FailedAtStage:      SenderAttemptAnswerCreated,
		Scope:              AttemptFailureScopeAttempt,
		TypedPeerErrorCode: TypedPeerErrorNegotiation,
	}
	if failed.Failure == nil || failed.Failure.FailedAtStage != wantFailure.FailedAtStage ||
		failed.Failure.Scope != wantFailure.Scope ||
		failed.Failure.TypedPeerErrorCode != wantFailure.TypedPeerErrorCode ||
		failed.Failure.Message != "" || failed.Failure.Operation == nil ||
		failed.Failure.Operation.Code != protocolsession.PeerOperationCodeNegotiation ||
		failed.Failure.Operation.Message != "" {
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

func TestSenderAttemptObservationPreservesTypedAdmissionFailure(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
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
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 8 })
	observed := collector.forAttempt(binding.AttemptID)
	terminal := observed[len(observed)-1]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptLaneHelloAuthenticated ||
		terminal.Failure.Scope != AttemptFailureScopeAttempt ||
		terminal.Failure.TypedPeerErrorCode != TypedPeerErrorAdmission ||
		terminal.Failure.Message != "" ||
		terminal.Failure.Operation == nil ||
		terminal.Failure.Operation.Code != protocolsession.PeerOperationCodeAdmission ||
		terminal.CandidateCounts == nil {
		t.Fatalf("admission terminal = %#v", terminal)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderAttemptObservationClassifiesCancellationAndRuntimeStop(t *testing.T) {
	for _, test := range []struct {
		name      string
		cancelRun bool
		scope     AttemptFailureScope
		code      TypedPeerErrorCode
	}{
		{
			name: "operation cancellation", scope: AttemptFailureScopeAttempt,
			code: TypedPeerErrorCancelled,
		},
		{
			name: "runtime stop", cancelRun: true, scope: AttemptFailureScopeSession,
			code: TypedPeerErrorStopped,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := &senderObservationCollector{}
			peer := newTestPeerConnection()
			factory := mustTestFactoryWithSenderCollector(t, collector, Config{
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
			waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 6 })
			observed := collector.forAttempt(binding.AttemptID)
			failed := observed[len(observed)-1]
			if failed.Stage != SenderAttemptFailed || failed.Failure == nil ||
				failed.Failure.FailedAtStage != SenderAttemptDataChannelOpen ||
				failed.Failure.Scope != test.scope || failed.Failure.TypedPeerErrorCode != test.code ||
				failed.Failure.Message != "" || failed.Failure.Operation != nil {
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

func TestSenderAttemptObservationRejectsCapacityWithCompleteAttemptStream(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	clock := newManualTestClock(time.Unix(8_000, 0))
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
		MaxRetiredBindings: 1,
		RetiredBindingTTL:  time.Minute,
		Now:                clock.Now,
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
	waitForTest(t, func() bool { return len(collector.forAttempt(rejectedBinding.AttemptID)) == 2 })
	rejected := collector.forAttempt(rejectedBinding.AttemptID)
	if rejected[0].Stage != SenderAttemptStarted || rejected[1].Stage != SenderAttemptFailed ||
		rejected[1].Failure == nil ||
		rejected[1].Failure.FailedAtStage != SenderAttemptNegotiationDeadlineArmed ||
		rejected[1].Failure.Operation == nil ||
		rejected[1].Failure.Operation.Code != protocolsession.PeerOperationCodeNegotiation {
		t.Fatalf("rejected attempt stream = %#v", rejected)
	}
	repeatedBody, err := v2signal.EncodeOffer(v2signal.Offer{Binding: rejectedBinding, SDP: "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	// The first active binding saturates replay retention. Evidence still owns
	// the rejected identity, and that claim survives the retention window.
	clock.Advance(2 * time.Minute)
	if err := handler.HandleMessage(
		rejectedContext,
		testMessage(t, protocolsession.MessagePeerOffer, rejectedOperation, repeatedBody),
	); err != nil {
		t.Fatalf("repeat rejected offer: %v", err)
	}
	receiveTest(t, session.failures)
	if repeated := collector.forAttempt(rejectedBinding.AttemptID); len(repeated) != 2 {
		t.Fatalf("repeated rejection restarted evidence stream: %#v", repeated)
	}
	if err := handler.Cancel(firstContext, firstOperation); err != nil {
		t.Fatalf("cancel first operation: %v", err)
	}
	stopSenderTestRuntime(t, cancel, runDone)
}

func TestSenderLocalCandidatePruningPreservesAttempt(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{MaxCandidates: 1, PeerConnections: PeerConnectionFactoryFunc(func(pion.Configuration) (PeerConnection, error) { return peer, nil })})
	session := newTestPeerSession(101)
	handler, ctx, cancel, done := startSenderTestRuntime(t, factory, session)
	_, _, _ = sendSenderTestOffer(t, handler, ctx, 102)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	candidate := &pion.ICECandidate{Foundation: "1", Priority: 1, Address: "10.0.0.8", Port: 43000, Protocol: pion.ICEProtocolUDP, Typ: pion.ICECandidateTypeHost, Component: 1}
	peer.emitCandidate(candidate)
	receiveTest(t, session.controls)
	for i := range 1000 {
		peer.emitCandidate(candidate)
		next := *candidate
		next.Port += uint16(i + 1)
		peer.emitCandidate(&next)
	}
	select {
	case failure := <-session.failures:
		t.Fatal(failure)
	case extra := <-session.controls:
		t.Fatal(extra)
	default:
	}
	stopSenderTestRuntime(t, cancel, done)
}

func TestSenderAttemptObservationTreatsRetiredCandidateAsAttemptCancellation(t *testing.T) {
	collector := &senderObservationCollector{}
	peer := newTestPeerConnection()
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{
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
	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 6 })
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

func TestSenderAttemptObservationProjectsAuthenticatedRejectionSettlement(t *testing.T) {
	collector := &senderObservationCollector{}
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{})
	session := newTestPeerSession(121)
	binding := testBinding(122)
	offerOperation := testOperationID(123)
	grantOperation := testOperationID(124)
	recorder := newSenderAttemptRecorder(
		factory, session.sessionID, binding, offerOperation,
	)
	recorder.begin()
	recorder.negotiationDeadlineArmed()
	recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, SenderAttemptObservation{})
	recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, SenderAttemptObservation{})
	recorder.dataChannelOpened(SenderCandidateCounts{})
	recorder.laneHelloAuthenticated(grantOperation, session.lane)
	recorder.admissionSettled(sessionruntime.SenderPeerAdmissionResult{
		SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
		Disposition:      sessionruntime.SenderPeerAdmissionRejected,
		ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
		Rejection: protocolsession.LaneRejection{
			Code: protocolsession.LaneRejectAdmissionLimited, RetryAfter: 7 * time.Second,
		},
	}, SenderCandidateCounts{})
	recorder.fail(SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorAdmission,
		Message: "private provider text",
	})

	waitForTest(t, func() bool { return len(collector.forAttempt(binding.AttemptID)) == 10 })
	observed := collector.forAttempt(binding.AttemptID)
	settled := observed[8]
	if settled.Stage != SenderAttemptAdmissionResponseSettled ||
		settled.AdmissionDisposition != SenderAdmissionRejected ||
		settled.ResponseDelivery != SenderResponseDelivered || settled.Rejection == nil ||
		settled.Rejection.Code != protocolsession.LaneRejectAdmissionLimited ||
		settled.Rejection.RetryAfterMillis != 7_000 || settled.OfferOperationID != offerOperation ||
		settled.GrantOperationID != grantOperation || settled.Lane == nil || *settled.Lane != session.lane {
		t.Fatalf("authenticated rejection settlement = %#v", settled)
	}
	terminal := observed[9]
	if terminal.Stage != SenderAttemptFailed || terminal.Failure == nil ||
		terminal.Failure.FailedAtStage != SenderAttemptAdmitted || terminal.Failure.Message != "" {
		t.Fatalf("rejected terminal = %#v", terminal)
	}
}

func TestSenderAttemptRecorderAllowsOnlyOneTerminalAtEveryBoundary(t *testing.T) {
	stages := []SenderAttemptStage{
		SenderAttemptStarted,
		SenderAttemptNegotiationDeadlineArmed,
		SenderAttemptOfferReceived,
		SenderAttemptAnswerCreated,
		SenderAttemptAnswerSent,
		SenderAttemptDataChannelOpen,
		SenderAttemptAdmissionDeadlineArmed,
		SenderAttemptLaneHelloAuthenticated,
		SenderAttemptAdmissionResponseSettled,
		SenderAttemptAdmitted,
	}
	for failedIndex := 1; failedIndex < len(stages); failedIndex++ {
		t.Run(string(stages[failedIndex]), func(t *testing.T) {
			collector := &senderObservationCollector{}
			factory := mustTestFactoryWithSenderCollector(t, collector, Config{})
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
