package v2peer_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testloopback"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	receiverAttemptStartFailureReason     = "attempt_start_failed"
	receiverAttemptPrematureFailureReason = "attempt_finished_before_ready"
	receiverAttemptReadyTimeoutReason     = "attempt_ready_timeout"
	receiverLaneIdentityFailureReason     = "lane_identity_mismatch"
)

type receiverIntegrationControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control receiverIntegrationControl) Kind() protocolsession.MessageKind { return control.kind }
func (control receiverIntegrationControl) Body() []byte                      { return bytes.Clone(control.body) }

type receiverIntegrationResult struct {
	control v2peer.ReceiverControl
	err     error
}

type receiverIntegrationOperation struct {
	id      protocolsession.OperationID
	ingress *integrationPeerIngress
	results chan receiverIntegrationResult

	terminalOnce sync.Once
	terminalDone chan struct{}
	terminal     v2peer.ReceiverSignalingTermination
	binding      v2peer.ReceiverSignalingOperationBinding
	wireBinding  v2signal.Binding
}

func newReceiverIntegrationOperation(id protocolsession.OperationID) *receiverIntegrationOperation {
	return &receiverIntegrationOperation{
		id: id, results: make(chan receiverIntegrationResult, 64),
		terminalDone: make(chan struct{}),
	}
}

func (operation *receiverIntegrationOperation) OperationID() protocolsession.OperationID {
	return operation.id
}

func (operation *receiverIntegrationOperation) MaximumContinuations() (int, bool) {
	return operation.ingress.MaximumContinuations(operation.id)
}

func (operation *receiverIntegrationOperation) SendCandidate(
	ctx context.Context,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	message, err := protocolsession.NewMessage(protocolsession.MessagePeerCandidate, &operation.id, body)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	return operation.ingress.Route(ctx, message)
}

func (operation *receiverIntegrationOperation) Receive(
	ctx context.Context,
) v2peer.ReceiverSignalingReceiveResult {
	select {
	case <-operation.terminalDone:
		return receiverIntegrationTerminalResult(operation.terminal)
	case <-ctx.Done():
		// The receive context is already exhausted; exact in-memory router cleanup
		// still needs an independent authority so the fixture cannot leak its call.
		terminal := operation.finish(
			context.Background(),
			ctx.Err(),
			true,
			v2peer.NewReceiverSignalingLocalTermination,
		)
		return receiverIntegrationTerminalResult(terminal)
	case result := <-operation.results:
		if result.err != nil {
			terminal := operation.finish(
				ctx,
				result.err,
				false,
				v2peer.NewReceiverSignalingRemoteTermination,
			)
			return receiverIntegrationTerminalResult(terminal)
		}
		select {
		case <-operation.terminalDone:
			return receiverIntegrationTerminalResult(operation.terminal)
		default:
			return v2peer.NewReceiverSignalingControlResult(result.control)
		}
	}
}

func (operation *receiverIntegrationOperation) Terminate(
	ctx context.Context,
) v2peer.ReceiverSignalingTermination {
	return operation.finish(
		ctx,
		nil,
		true,
		v2peer.NewReceiverSignalingLocalTermination,
	)
}

func (operation *receiverIntegrationOperation) finish(
	ctx context.Context,
	cause error,
	cancelExact bool,
	newTermination func(
		v2peer.ReceiverSignalingOperationBinding,
		error,
	) v2peer.ReceiverSignalingTermination,
) v2peer.ReceiverSignalingTermination {
	operation.terminalOnce.Do(func() {
		if cancelExact {
			cause = errors.Join(cause, operation.ingress.Cancel(ctx, operation.id))
		}
		operation.terminal = newTermination(operation.binding, cause)
		close(operation.terminalDone)
	})
	<-operation.terminalDone
	return operation.terminal
}

func receiverIntegrationTerminalResult(
	terminal v2peer.ReceiverSignalingTermination,
) v2peer.ReceiverSignalingReceiveResult {
	return v2peer.NewReceiverSignalingTerminationResult(terminal)
}

type receiverIntegrationSignaling struct {
	operation *receiverIntegrationOperation
}

func (signaling receiverIntegrationSignaling) OpenPeerOperation(
	ctx context.Context,
	binding v2peer.ReceiverSignalingOperationBinding,
	offer []byte,
) (v2peer.ReceiverSignalingOperation, error) {
	signaling.operation.binding = binding
	decodedOffer, err := v2signal.DecodeOffer(offer)
	if err != nil {
		return nil, err
	}
	signaling.operation.wireBinding = decodedOffer.Binding
	message, err := protocolsession.NewMessage(
		protocolsession.MessagePeerOffer,
		&signaling.operation.id,
		offer,
	)
	if err != nil {
		return nil, err
	}
	if err := signaling.operation.ingress.Deliver(ctx, message); err != nil {
		return nil, err
	}
	return signaling.operation, nil
}

type receiverIntegrationAdmissionOutcome struct {
	result sessionruntime.SenderPeerAdmissionResult
	err    error
}

type receiverIntegrationAdmissionScript struct {
	lane               sessionruntime.LaneIdentity
	grantOperation     protocolsession.OperationID
	laneHello          framechannel.Frame
	response           framechannel.Frame
	rejection          *protocolsession.LaneRejection
	settlementBegan    chan struct{}
	contextCanceled    chan struct{}
	settlementRelease  <-chan struct{}
	laneOutcome        chan<- receiverIntegrationAdmissionOutcome
	observationOutcome chan<- receiverIntegrationAdmissionOutcome
}

type receiverIntegrationSenderSession struct {
	share     catalog.ShareInstance
	sessionID protocolsession.ProtocolSessionID
	operation *receiverIntegrationOperation
	lane      sessionruntime.LaneIdentity
	channels  chan protocolsession.FrameChannel

	operationMu      sync.RWMutex
	operations       map[protocolsession.OperationID]*receiverIntegrationOperation
	admissionScripts chan receiverIntegrationAdmissionScript
}

func (session *receiverIntegrationSenderSession) ShareInstance() catalog.ShareInstance {
	return session.share
}

func (session *receiverIntegrationSenderSession) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return session.sessionID
}

func (session *receiverIntegrationSenderSession) receiverOperation(
	operationID protocolsession.OperationID,
) *receiverIntegrationOperation {
	session.operationMu.RLock()
	defer session.operationMu.RUnlock()
	if session.operation != nil && session.operation.id == operationID {
		return session.operation
	}
	return session.operations[operationID]
}

func (session *receiverIntegrationSenderSession) SendPeerControl(
	ctx context.Context,
	kind protocolsession.MessageKind,
	operation protocolsession.OperationID,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	target := session.receiverOperation(operation)
	if target == nil {
		return protocolsession.OperationDrop, errors.New("sender changed the receiver peer operation identity")
	}
	select {
	case <-ctx.Done():
		return protocolsession.OperationDrop, ctx.Err()
	case target.results <- receiverIntegrationResult{
		control: receiverIntegrationControl{kind: kind, body: bytes.Clone(body)},
	}:
		return protocolsession.OperationDeliver, nil
	}
}

func (session *receiverIntegrationSenderSession) FailPeerOperation(
	ctx context.Context,
	operation protocolsession.OperationID,
	_ uint16,
	message string,
) error {
	target := session.receiverOperation(operation)
	if target == nil {
		return errors.New("sender failed a different peer operation")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case target.results <- receiverIntegrationResult{err: errors.New(message)}:
		return nil
	}
}

func (session *receiverIntegrationSenderSession) AdmitPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control sessionruntime.SenderPeerAdmissionControl,
) (result sessionruntime.SenderPeerAdmissionResult, err error) {
	if session.admissionScripts != nil {
		select {
		case <-ctx.Done():
			return sessionruntime.SenderPeerAdmissionResult{}, ctx.Err()
		case script := <-session.admissionScripts:
			return session.admitScriptedPeerChannel(ctx, channel, control, script)
		}
	}
	result = sessionruntime.SenderPeerAdmissionResult{
		Disposition:      sessionruntime.SenderPeerAdmissionSilentClose,
		ResponseDelivery: sessionruntime.SenderPeerResponseNotAttempted,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = channel.Close()
		}
	}()
	opened, ok := channel.(interface{ Opened() <-chan struct{} })
	if !ok {
		return result, errors.New("sender peer channel omitted its open signal")
	}
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-opened.Opened():
	}
	grantOperation := integrationOperationID(0x7d)
	if !control.BeginAuthenticatedSettlement(grantOperation, session.lane) {
		return result, context.Canceled
	}
	result = sessionruntime.SenderPeerAdmissionResult{
		SettlementBegan: true, GrantOperationID: grantOperation, Lane: session.lane,
		Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
		ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttached,
	}
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case session.channels <- channel:
		transferred = true
		return result, nil
	}
}

func (session *receiverIntegrationSenderSession) admitScriptedPeerChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	control sessionruntime.SenderPeerAdmissionControl,
	script receiverIntegrationAdmissionScript,
) (result sessionruntime.SenderPeerAdmissionResult, err error) {
	result = sessionruntime.SenderPeerAdmissionResult{
		Disposition:      sessionruntime.SenderPeerAdmissionSilentClose,
		ResponseDelivery: sessionruntime.SenderPeerResponseNotAttempted,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = channel.Close()
		}
	}()
	opened, ok := channel.(interface{ Opened() <-chan struct{} })
	if !ok {
		return result, errors.New("sender peer channel omitted its open signal")
	}
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-opened.Opened():
	}
	if len(script.laneHello) != 0 {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case frame, ok := <-channel.Recv():
			if !ok || !bytes.Equal(frame, script.laneHello) {
				return result, errors.New("queued LaneHello did not cross the real Pion DataChannel")
			}
		}
	}
	if !control.BeginAuthenticatedSettlement(script.grantOperation, script.lane) {
		return result, context.Canceled
	}
	if script.settlementBegan != nil {
		close(script.settlementBegan)
	}
	if script.contextCanceled != nil {
		go func() {
			<-ctx.Done()
			close(script.contextCanceled)
		}()
	}
	// After authentication, the lane service owns its typed settlement even if
	// the phase timer cancels I/O; waiting here makes that ordering observable.
	if script.settlementRelease != nil {
		<-script.settlementRelease
	}
	publishOutcome := func(outcome receiverIntegrationAdmissionOutcome) {
		if script.laneOutcome != nil {
			script.laneOutcome <- outcome
		}
		if script.observationOutcome != nil {
			script.observationOutcome <- outcome
		}
	}
	if script.rejection != nil {
		result = sessionruntime.SenderPeerAdmissionResult{
			SettlementBegan: true, GrantOperationID: script.grantOperation, Lane: script.lane,
			Disposition:      sessionruntime.SenderPeerAdmissionRejected,
			Rejection:        *script.rejection,
			ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
			LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
		}
		err = &sessionruntime.LaneRejectedError{Rejection: *script.rejection}
		publishOutcome(receiverIntegrationAdmissionOutcome{result: result, err: err})
		return result, err
	}
	if len(script.response) != 0 {
		sendContext, cancel := context.WithTimeout(context.Background(), v2PeerIntegrationTimeout)
		err = channel.Send(sendContext, script.response)
		cancel()
		if err != nil {
			result = sessionruntime.SenderPeerAdmissionResult{
				SettlementBegan: true, GrantOperationID: script.grantOperation, Lane: script.lane,
				Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
				ResponseDelivery: sessionruntime.SenderPeerResponseDeliveryFailed,
				LaneAttachment:   sessionruntime.SenderPeerLaneAttachmentNotAttempted,
			}
			publishOutcome(receiverIntegrationAdmissionOutcome{result: result, err: err})
			return result, err
		}
	}
	result = sessionruntime.SenderPeerAdmissionResult{
		SettlementBegan: true, GrantOperationID: script.grantOperation, Lane: script.lane,
		Disposition:      sessionruntime.SenderPeerAdmissionAccepted,
		ResponseDelivery: sessionruntime.SenderPeerResponseDelivered,
		LaneAttachment:   sessionruntime.SenderPeerLaneAttached,
	}
	session.channels <- channel
	transferred = true
	publishOutcome(receiverIntegrationAdmissionOutcome{result: result})
	return result, nil
}

type receiverIntegrationLanes struct {
	lane     sessionruntime.LaneIdentity
	channels chan protocolsession.FrameChannel
}

func (lanes receiverIntegrationLanes) RequestLane(
	context.Context,
	uint32,
) (sessionruntime.LaneAttachmentGrant, error) {
	return sessionruntime.LaneAttachmentGrant{
		LaneID: lanes.lane.ID, LaneEpoch: lanes.lane.Epoch,
		OperationID: integrationOperationID(0x42),
	}, nil
}

func (lanes receiverIntegrationLanes) AttachLane(
	ctx context.Context,
	grant sessionruntime.LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
	_ transfer.LaneRoute,
) (sessionruntime.ReceiverLaneAdmissionResult, error) {
	select {
	case <-ctx.Done():
		return receiverUnverifiedLaneAdmission(), ctx.Err()
	case lanes.channels <- channel:
		return receiverInstalledLaneAdmission(grant.OperationID, lanes.lane), nil
	}
}

type receiverControlledLanes struct {
	lane           sessionruntime.LaneIdentity
	grantOperation protocolsession.OperationID
	grantRelease   <-chan struct{}
	requestStarted chan struct{}
	laneHello      framechannel.Frame
	helloSent      chan struct{}
	response       framechannel.Frame
	laneOutcome    <-chan receiverIntegrationAdmissionOutcome
	channels       chan protocolsession.FrameChannel
}

func (lanes receiverControlledLanes) RequestLane(
	ctx context.Context,
	_ uint32,
) (sessionruntime.LaneAttachmentGrant, error) {
	if lanes.requestStarted != nil {
		close(lanes.requestStarted)
	}
	if lanes.grantRelease != nil {
		select {
		case <-ctx.Done():
			return sessionruntime.LaneAttachmentGrant{}, ctx.Err()
		case <-lanes.grantRelease:
		}
	}
	return sessionruntime.LaneAttachmentGrant{
		LaneID:      lanes.lane.ID,
		LaneEpoch:   lanes.lane.Epoch,
		OperationID: lanes.grantOperation,
	}, nil
}

func (lanes receiverControlledLanes) AttachLane(
	ctx context.Context,
	grant sessionruntime.LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
	_ transfer.LaneRoute,
) (admission sessionruntime.ReceiverLaneAdmissionResult, err error) {
	transferred := false
	defer func() {
		if !transferred {
			_ = channel.Close()
		}
	}()
	if len(lanes.laneHello) != 0 {
		if err := channel.Send(ctx, lanes.laneHello); err != nil {
			return receiverUnverifiedLaneAdmission(), err
		}
		if lanes.helloSent != nil {
			close(lanes.helloSent)
		}
	}
	if lanes.laneOutcome != nil {
		select {
		case <-ctx.Done():
			return receiverUnverifiedLaneAdmission(), ctx.Err()
		case outcome := <-lanes.laneOutcome:
			if outcome.err != nil {
				if outcome.result.Disposition == sessionruntime.SenderPeerAdmissionRejected {
					return sessionruntime.ReceiverLaneAdmissionResult{
						GrantOperationID: outcome.result.GrantOperationID,
						Lane:             outcome.result.Lane,
						Disposition:      sessionruntime.ReceiverLaneAdmissionRejected,
						Rejection:        outcome.result.Rejection,
						LaneInstallation: sessionruntime.ReceiverLaneInstallationNotAttempted,
					}, outcome.err
				}
				return receiverUnverifiedLaneAdmission(), outcome.err
			}
		}
	}
	if len(lanes.response) != 0 {
		select {
		case <-ctx.Done():
			return receiverUnverifiedLaneAdmission(), ctx.Err()
		case frame, ok := <-channel.Recv():
			if !ok || !bytes.Equal(frame, lanes.response) {
				return receiverUnverifiedLaneAdmission(), errors.New(
					"lane response did not cross the real Pion DataChannel",
				)
			}
		}
	}
	if lanes.channels != nil {
		select {
		case <-ctx.Done():
			return receiverUnverifiedLaneAdmission(), ctx.Err()
		case lanes.channels <- channel:
		}
	}
	transferred = true
	return receiverInstalledLaneAdmission(grant.OperationID, lanes.lane), nil
}

func receiverUnverifiedLaneAdmission() sessionruntime.ReceiverLaneAdmissionResult {
	return sessionruntime.ReceiverLaneAdmissionResult{
		Disposition:      sessionruntime.ReceiverLaneAdmissionUnverified,
		LaneInstallation: sessionruntime.ReceiverLaneInstallationNotAttempted,
	}
}

func receiverInstalledLaneAdmission(
	operationID protocolsession.OperationID,
	lane sessionruntime.LaneIdentity,
) sessionruntime.ReceiverLaneAdmissionResult {
	return sessionruntime.ReceiverLaneAdmissionResult{
		GrantOperationID: operationID,
		Lane:             lane,
		Disposition:      sessionruntime.ReceiverLaneAdmissionAccepted,
		LaneInstallation: sessionruntime.ReceiverLaneInstalled,
	}
}

func integrationOperationID(seed byte) protocolsession.OperationID {
	operation, _ := protocolsession.OperationIDFromBytes(
		bytes.Repeat([]byte{seed}, protocolsession.IdentityBytes),
	)
	return operation
}

func TestV2PeerReceiverAndSenderFactoriesInteroperateOverRealPion(t *testing.T) {
	requireLongPionIntegration(t)
	loopback := testloopback.New(t)
	trace := startIntegrationScenario(t, receiverRealPionScenario, receiverRealPionComponent)
	trace.RequireCleanup(t, "loopback fixture", func(context.Context) error {
		return loopback.Close()
	})
	senderAPI := loopback.NewPionAPI()
	receiverAPI := loopback.NewPionAPI()
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{0x31}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{0x32}, protocolsession.IdentityBytes),
	)
	lane := sessionruntime.LaneIdentity{ID: 23, Epoch: 5}
	signalingOperation := newReceiverIntegrationOperation(integrationOperationID(0x33))
	senderSession := &receiverIntegrationSenderSession{
		share: share, sessionID: sessionID, operation: signalingOperation, lane: lane,
		channels: make(chan protocolsession.FrameChannel, 1),
	}
	senderFactory, err := v2peer.NewFactory(v2peer.Config{
		Configuration: pion.Configuration{}, NegotiationBudget: v2PeerIntegrationTimeout, AdmissionBudget: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
		PeerConnections: v2peer.PeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.PeerConnection, error) {
				return senderAPI.NewPeerConnection(configuration)
			},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := senderFactory.NewSenderPeerHandler(senderSession)
	if err != nil {
		t.Fatal(err)
	}
	ingress, err := newIntegrationPeerIngress(senderFactory, handler)
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "receiver sender ingress", func(context.Context) error {
		ingress.Close()
		return nil
	})
	signalingOperation.ingress = ingress

	ctx, cancel := context.WithTimeout(context.Background(), v2PeerIntegrationTimeout)
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler.Run(ctx) }()
	trace.RequireCleanup(t, "receiver sender peer handler", func(context.Context) error {
		cancel()
		return joinIntegrationRoutine(
			"receiver sender peer handler",
			handlerDone,
			v2PeerIntegrationTimeout,
			func(err error) bool { return errors.Is(err, context.Canceled) },
		)
	})

	receiverFactory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Configuration: pion.Configuration{}, NegotiationBudget: v2PeerIntegrationTimeout, AdmissionBudget: v2PeerIntegrationTimeout,
		MaxCandidates: v2PeerIntegrationMaxCandidates,
		PeerConnections: v2peer.ReceiverPeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.ReceiverPeerConnection, error) {
				return receiverAPI.NewPeerConnection(configuration)
			},
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	receiverChannels := make(chan protocolsession.FrameChannel, 1)
	peerReadiness := startIntegrationPhase(
		t,
		trace,
		testrun.PeerReadyMilestone,
		nil,
		peerReadinessEvidenceStartFailureReason,
	)
	attempt, err := receiverFactory.Start(
		ctx,
		receiverIntegrationSignaling{operation: signalingOperation},
		receiverIntegrationLanes{lane: lane, channels: receiverChannels},
	)
	if err != nil {
		failIntegrationPhase(
			t,
			peerReadiness,
			receiverAttemptStartFailureReason,
			"start receiver Pion attempt: %v",
			err,
		)
	}
	trace.RequireCleanup(t, "receiver Pion attempt", func(context.Context) error {
		return attempt.Close()
	})

	select {
	case <-attempt.Ready():
	case <-attempt.Done():
		failIntegrationPhase(
			t,
			peerReadiness,
			receiverAttemptPrematureFailureReason,
			"receiver Pion attempt failed before admission: %v",
			attempt.Err(),
		)
	case <-ctx.Done():
		failIntegrationPhase(
			t,
			peerReadiness,
			receiverAttemptReadyTimeoutReason,
			"receiver Pion attempt did not become ready: %v",
			ctx.Err(),
		)
	}
	if err := peerReadiness.Succeed(nil); err != nil {
		t.Fatalf("record receiver peer readiness: %v", err)
	}
	laneAdoption := startIntegrationPhase(
		t,
		trace,
		testrun.LaneAdoptedMilestone,
		nil,
		laneAdoptionEvidenceStartFailureReason,
	)
	if attached, ok := attempt.Lane(); !ok || attached != lane {
		failIntegrationPhase(
			t,
			laneAdoption,
			receiverLaneIdentityFailureReason,
			"receiver attached lane = %+v, %v",
			attached,
			ok,
		)
	}
	if err := laneAdoption.Succeed(integrationLaneContext{
		LaneID: lane.ID, LaneEpoch: lane.Epoch,
	}); err != nil {
		t.Fatalf("record receiver lane adoption: %v", err)
	}

	var senderChannel, receiverChannel protocolsession.FrameChannel
	select {
	case senderChannel = <-senderSession.channels:
	case <-ctx.Done():
		t.Fatal("sender did not admit its real Pion channel")
	}
	select {
	case receiverChannel = <-receiverChannels:
	case <-ctx.Done():
		t.Fatal("receiver did not attach its real Pion channel")
	}
	if err := receiverChannel.Send(ctx, framechannel.Frame("receiver-to-sender")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-senderChannel.Recv():
		if !bytes.Equal(frame, []byte("receiver-to-sender")) {
			t.Fatalf("sender frame = %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("sender did not receive the receiver frame")
	}
	if err := senderChannel.Send(ctx, framechannel.Frame("sender-to-receiver")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-receiverChannel.Recv():
		if !bytes.Equal(frame, []byte("sender-to-receiver")) {
			t.Fatalf("receiver frame = %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not receive the sender frame")
	}
	if err := receiverChannel.Close(); err != nil {
		t.Fatal(err)
	}
	if err := senderChannel.Close(); err != nil {
		t.Fatal(err)
	}

	trace.RequireSuccess(t)
}

func TestV2PeerRealPionPhaseGatesPreserveExpiryAndReplacement(t *testing.T) {
	requireLongPionIntegration(t)
	loopback := testloopback.New(t)
	t.Cleanup(func() {
		if err := loopback.Close(); err != nil {
			t.Errorf("close loopback fixture: %v", err)
		}
	})

	senderAPI := loopback.NewPionAPI()
	receiverAPI := loopback.NewPionAPI()
	senderPeers := make(chan *integrationTrackedPeerConnection, 4)
	receiverPeers := make(chan *integrationTrackedPeerConnection, 4)
	senderDataChannels := newIntegrationTrackedDataChannelAdapter()
	receiverDataChannels := newIntegrationTrackedDataChannelAdapter()
	senderTimers := newIntegrationPhaseTimerSource()
	receiverTimers := newIntegrationPhaseTimerSource()

	firstSenderOpen := make(chan struct{})
	firstSenderPhysicalOpen := make(chan struct{})
	senderDataChannels.scripts <- integrationDataChannelScript{
		openGate: firstSenderOpen, physicalOpened: firstSenderPhysicalOpen,
	}
	receiverDataChannels.scripts <- integrationDataChannelScript{}

	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{0x51}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(
		bytes.Repeat([]byte{0x52}, protocolsession.IdentityBytes),
	)
	firstOperation := newReceiverIntegrationOperation(integrationOperationID(0x53))
	secondOperation := newReceiverIntegrationOperation(integrationOperationID(0x54))
	senderChannels := make(chan protocolsession.FrameChannel, 2)
	senderSession := &receiverIntegrationSenderSession{
		share:     share,
		sessionID: sessionID,
		channels:  senderChannels,
		operations: map[protocolsession.OperationID]*receiverIntegrationOperation{
			firstOperation.id:  firstOperation,
			secondOperation.id: secondOperation,
		},
		admissionScripts: make(chan receiverIntegrationAdmissionScript, 2),
	}

	senderFactory, err := v2peer.NewFactory(v2peer.Config{
		Configuration:     pion.Configuration{},
		NegotiationBudget: v2PeerIntegrationTimeout,
		AdmissionBudget:   v2PeerIntegrationTimeout,
		PhaseTimers:       senderTimers,
		MaxCandidates:     v2PeerIntegrationMaxCandidates,
		DataChannels:      senderDataChannels,
		PeerConnections: v2peer.PeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.PeerConnection, error) {
				connection, createErr := senderAPI.NewPeerConnection(configuration)
				if createErr != nil {
					return nil, createErr
				}
				tracked := newIntegrationTrackedPeerConnection(connection)
				senderPeers <- tracked
				return tracked, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("create controlled sender factory: %v", err)
	}
	handler, err := senderFactory.NewSenderPeerHandler(senderSession)
	if err != nil {
		t.Fatalf("create controlled sender handler: %v", err)
	}
	ingress, err := newIntegrationPeerIngress(senderFactory, handler)
	if err != nil {
		t.Fatalf("create controlled sender ingress: %v", err)
	}
	t.Cleanup(ingress.Close)
	firstOperation.ingress = ingress
	secondOperation.ingress = ingress

	ctx, cancel := context.WithCancel(context.Background())
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := joinIntegrationRoutine(
			"controlled sender handler",
			handlerDone,
			v2PeerIntegrationTimeout,
			func(err error) bool { return err == nil || errors.Is(err, context.Canceled) },
		); err != nil {
			t.Errorf("stop controlled sender handler: %v", err)
		}
	})

	receiverFactory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{
		Configuration:     pion.Configuration{},
		NegotiationBudget: v2PeerIntegrationTimeout,
		AdmissionBudget:   v2PeerIntegrationTimeout,
		PhaseTimers:       receiverTimers,
		MaxCandidates:     v2PeerIntegrationMaxCandidates,
		DataChannels:      receiverDataChannels,
		PeerConnections: v2peer.ReceiverPeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.ReceiverPeerConnection, error) {
				connection, createErr := receiverAPI.NewPeerConnection(configuration)
				if createErr != nil {
					return nil, createErr
				}
				tracked := newIntegrationTrackedPeerConnection(connection)
				receiverPeers <- tracked
				return tracked, nil
			},
		),
	})
	if err != nil {
		t.Fatalf("create controlled receiver factory: %v", err)
	}

	firstLane := sessionruntime.LaneIdentity{ID: 41, Epoch: 1}
	firstGrantOperation := integrationOperationID(0x55)
	firstGrantRelease := make(chan struct{})
	firstGrantRequested := make(chan struct{})
	firstLaneHello := framechannel.Frame("controlled-lane-hello")
	firstLaneHelloSent := make(chan struct{})
	firstSettlementBegan := make(chan struct{})
	firstSettlementContextCanceled := make(chan struct{})
	firstSettlementRelease := make(chan struct{})
	firstLaneOutcome := make(chan receiverIntegrationAdmissionOutcome, 1)
	firstObservedOutcome := make(chan receiverIntegrationAdmissionOutcome, 1)
	grantExpired := protocolsession.LaneRejection{Code: protocolsession.LaneRejectGrantExpired}
	senderSession.admissionScripts <- receiverIntegrationAdmissionScript{
		lane:               firstLane,
		grantOperation:     firstGrantOperation,
		laneHello:          firstLaneHello,
		rejection:          &grantExpired,
		settlementBegan:    firstSettlementBegan,
		contextCanceled:    firstSettlementContextCanceled,
		settlementRelease:  firstSettlementRelease,
		laneOutcome:        firstLaneOutcome,
		observationOutcome: firstObservedOutcome,
	}
	firstLanes := receiverControlledLanes{
		lane:           firstLane,
		grantOperation: firstGrantOperation,
		grantRelease:   firstGrantRelease,
		requestStarted: firstGrantRequested,
		laneHello:      firstLaneHello,
		helloSent:      firstLaneHelloSent,
		laneOutcome:    firstLaneOutcome,
	}
	firstAttempt, err := receiverFactory.Start(
		ctx,
		receiverIntegrationSignaling{operation: firstOperation},
		firstLanes,
	)
	if err != nil {
		t.Fatalf("start first controlled Pion attempt: %v", err)
	}
	t.Cleanup(func() { _ = firstAttempt.Close() })

	firstReceiverNegotiation := receiveIntegrationPhaseTimer(
		t,
		receiverTimers,
		v2peer.PeerAttemptPhaseNegotiation,
	)
	firstReceiverPeer := receiveIntegrationPeer(t, receiverPeers)
	firstReceiverChannel := receiveIntegrationDataChannel(t, receiverDataChannels)
	firstSenderNegotiation := receiveIntegrationPhaseTimer(
		t,
		senderTimers,
		v2peer.PeerAttemptPhaseNegotiation,
	)
	firstSenderPeer := receiveIntegrationPeer(t, senderPeers)
	firstSenderChannel := receiveIntegrationDataChannel(t, senderDataChannels)

	receiveIntegrationSignal(t, "sender physical DataChannel Open", firstSenderPhysicalOpen)
	firstReceiverAdmission := receiveIntegrationPhaseTimer(
		t,
		receiverTimers,
		v2peer.PeerAttemptPhaseAdmission,
	)
	receiveIntegrationSignal(t, "receiver lane-grant request", firstGrantRequested)
	if !firstReceiverNegotiation.timer.stopped.Load() {
		t.Fatal("receiver negotiation timer remained armed after its local Open")
	}
	if firstSenderNegotiation.timer.stopped.Load() {
		t.Fatal("sender negotiation ended before its independently observed Open")
	}
	requireNoIntegrationPhaseTimer(t, senderTimers)

	close(firstGrantRelease)
	receiveIntegrationSignal(t, "queued LaneHello send", firstLaneHelloSent)
	select {
	case <-firstSettlementBegan:
		t.Fatal("sender authenticated LaneHello before its gated local Open")
	default:
	}

	close(firstSenderOpen)
	firstSenderAdmission := receiveIntegrationPhaseTimer(
		t,
		senderTimers,
		v2peer.PeerAttemptPhaseAdmission,
	)
	if !firstSenderNegotiation.timer.stopped.Load() {
		t.Fatal("sender negotiation timer remained armed after its local Open")
	}
	receiveIntegrationSignal(t, "authenticated LaneHello settlement", firstSettlementBegan)
	if !firstSenderAdmission.timer.Fire() {
		t.Fatal("sender admission timer could not be expired at the settlement boundary")
	}
	receiveIntegrationSignal(
		t,
		"sender admission context cancellation",
		firstSettlementContextCanceled,
	)
	close(firstSettlementRelease)

	firstOutcome := receiveIntegrationAdmissionOutcome(t, firstObservedOutcome)
	var senderRejected *sessionruntime.LaneRejectedError
	if !firstOutcome.result.SettlementBegan ||
		firstOutcome.result.Disposition != sessionruntime.SenderPeerAdmissionRejected ||
		firstOutcome.result.Rejection != grantExpired ||
		!errors.As(firstOutcome.err, &senderRejected) ||
		senderRejected.Rejection != grantExpired {
		t.Fatalf("sender authenticated expiry outcome = %#v, error=%v", firstOutcome.result, firstOutcome.err)
	}
	if errors.Is(firstOutcome.err, v2peer.ErrPeerAdmissionTimeout) {
		t.Fatalf("sender phase timeout hid authenticated grant expiry: %v", firstOutcome.err)
	}

	receiveIntegrationSignal(t, "first receiver attempt completion", firstAttempt.Done())
	if firstAttempt.Err() == nil {
		t.Fatal("rejected first admission completed without a receiver failure")
	}
	if errors.Is(firstAttempt.Err(), v2peer.ErrPeerAdmissionTimeout) {
		t.Fatalf("receiver manufactured its own phase timeout after sender settlement: %v", firstAttempt.Err())
	}
	if _, ok := firstAttempt.Lane(); ok {
		t.Fatal("expired first attempt published a lane")
	}
	if !firstReceiverAdmission.timer.stopped.Load() ||
		!firstSenderAdmission.timer.stopped.Load() {
		t.Fatal("terminal first attempt retained an armed admission timer")
	}
	requireIntegrationTransportClosedOnce(
		t,
		"first receiver",
		firstReceiverPeer,
		firstReceiverChannel,
	)
	requireIntegrationTransportClosedOnce(
		t,
		"first sender",
		firstSenderPeer,
		firstSenderChannel,
	)
	_ = firstAttempt.Close()
	if firstReceiverPeer.closeCalls.Load() != 1 ||
		firstReceiverChannel.closeCalls.Load() != 1 ||
		firstSenderPeer.closeCalls.Load() != 1 ||
		firstSenderChannel.closeCalls.Load() != 1 {
		t.Fatal("idempotent attempt close repeated real Pion transport teardown")
	}

	senderDataChannels.scripts <- integrationDataChannelScript{}
	receiverDataChannels.scripts <- integrationDataChannelScript{}
	secondLane := sessionruntime.LaneIdentity{ID: 42, Epoch: 2}
	secondGrantOperation := integrationOperationID(0x56)
	secondLaneHello := framechannel.Frame("replacement-lane-hello")
	secondResponse := framechannel.Frame("replacement-lane-accepted")
	secondSettlementBegan := make(chan struct{})
	secondLaneOutcome := make(chan receiverIntegrationAdmissionOutcome, 1)
	secondObservedOutcome := make(chan receiverIntegrationAdmissionOutcome, 1)
	secondReceiverChannels := make(chan protocolsession.FrameChannel, 1)
	senderSession.admissionScripts <- receiverIntegrationAdmissionScript{
		lane:               secondLane,
		grantOperation:     secondGrantOperation,
		laneHello:          secondLaneHello,
		response:           secondResponse,
		settlementBegan:    secondSettlementBegan,
		laneOutcome:        secondLaneOutcome,
		observationOutcome: secondObservedOutcome,
	}
	secondLanes := receiverControlledLanes{
		lane:           secondLane,
		grantOperation: secondGrantOperation,
		laneHello:      secondLaneHello,
		response:       secondResponse,
		laneOutcome:    secondLaneOutcome,
		channels:       secondReceiverChannels,
	}
	secondAttempt, err := receiverFactory.Start(
		ctx,
		receiverIntegrationSignaling{operation: secondOperation},
		secondLanes,
	)
	if err != nil {
		t.Fatalf("start replacement Pion attempt: %v", err)
	}
	t.Cleanup(func() { _ = secondAttempt.Close() })

	secondReceiverNegotiation := receiveIntegrationPhaseTimer(
		t,
		receiverTimers,
		v2peer.PeerAttemptPhaseNegotiation,
	)
	secondReceiverPeer := receiveIntegrationPeer(t, receiverPeers)
	secondReceiverChannel := receiveIntegrationDataChannel(t, receiverDataChannels)
	secondSenderNegotiation := receiveIntegrationPhaseTimer(
		t,
		senderTimers,
		v2peer.PeerAttemptPhaseNegotiation,
	)
	secondSenderPeer := receiveIntegrationPeer(t, senderPeers)
	secondSenderChannel := receiveIntegrationDataChannel(t, senderDataChannels)
	secondReceiverAdmission := receiveIntegrationPhaseTimer(
		t,
		receiverTimers,
		v2peer.PeerAttemptPhaseAdmission,
	)
	secondSenderAdmission := receiveIntegrationPhaseTimer(
		t,
		senderTimers,
		v2peer.PeerAttemptPhaseAdmission,
	)
	receiveIntegrationSignal(t, "replacement authenticated settlement", secondSettlementBegan)
	secondOutcome := receiveIntegrationAdmissionOutcome(t, secondObservedOutcome)
	if secondOutcome.err != nil ||
		secondOutcome.result.Disposition != sessionruntime.SenderPeerAdmissionAccepted ||
		secondOutcome.result.LaneAttachment != sessionruntime.SenderPeerLaneAttached {
		t.Fatalf("replacement sender admission = %#v, error=%v", secondOutcome.result, secondOutcome.err)
	}
	receiveIntegrationSignal(t, "replacement receiver readiness", secondAttempt.Ready())
	if attached, ok := secondAttempt.Lane(); !ok || attached != secondLane {
		t.Fatalf("replacement lane = %#v, %t, want %#v", attached, ok, secondLane)
	}
	if firstOperation.binding == secondOperation.binding ||
		firstOperation.wireBinding == secondOperation.wireBinding ||
		firstOperation.wireBinding.AttemptID == secondOperation.wireBinding.AttemptID {
		t.Fatalf(
			"replacement reused signaling identity first=%#v/%#v second=%#v/%#v",
			firstOperation.binding,
			firstOperation.wireBinding,
			secondOperation.binding,
			secondOperation.wireBinding,
		)
	}
	for name, timer := range map[string]*integrationManualPhaseTimer{
		"receiver negotiation": secondReceiverNegotiation.timer,
		"sender negotiation":   secondSenderNegotiation.timer,
		"receiver admission":   secondReceiverAdmission.timer,
		"sender admission":     secondSenderAdmission.timer,
	} {
		if !timer.stopped.Load() {
			t.Fatalf("replacement %s timer remained armed", name)
		}
	}

	replacementReceiverChannel := receiveIntegrationFrameChannel(
		t,
		"replacement receiver",
		secondReceiverChannels,
	)
	replacementSenderChannel := receiveIntegrationFrameChannel(
		t,
		"replacement sender",
		senderChannels,
	)
	replacementFrame := framechannel.Frame("replacement-real-pion-frame")
	if err := replacementReceiverChannel.Send(ctx, replacementFrame); err != nil {
		t.Fatalf("send replacement frame: %v", err)
	}
	select {
	case frame := <-replacementSenderChannel.Recv():
		if !bytes.Equal(frame, replacementFrame) {
			t.Fatalf("replacement frame = %q, want %q", frame, replacementFrame)
		}
	case <-time.After(v2PeerIntegrationTimeout):
		t.Fatal("replacement frame did not cross the real Pion DataChannel")
	}

	if err := replacementReceiverChannel.Close(); err != nil {
		t.Fatalf("close replacement receiver channel: %v", err)
	}
	if err := replacementSenderChannel.Close(); err != nil {
		t.Fatalf("close replacement sender channel: %v", err)
	}
	_ = secondAttempt.Close()
	requireIntegrationTransportClosedOnce(
		t,
		"replacement receiver",
		secondReceiverPeer,
		secondReceiverChannel,
	)
	requireIntegrationTransportClosedOnce(
		t,
		"replacement sender",
		secondSenderPeer,
		secondSenderChannel,
	)
}
