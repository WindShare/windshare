package commandprojection

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestRelayLifecycleProjectionEnforcesSourceFieldMatrix(t *testing.T) {
	session := v2.RelaySessionID{1}
	send := func(stage relayv2.LifecycleStage) relayv2.LifecycleTrace {
		return relayv2.LifecycleTrace{LinkID: 1, RelaySessionID: session, OperationID: 2, Stage: stage,
			RetirementSource: relayv2.LifecycleRetirementNone, Cause: relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone}
	}
	terminalReserved := send(relayv2.LifecycleTerminalReserved)
	terminalReserved.Terminal = true
	sendAdmitted := send(relayv2.LifecycleSendAdmitted)
	sendAdmitted.Terminal = true
	sendAdmitted.Disposition = framechannel.SendAccepted
	providerFailure := send(relayv2.LifecycleSendAdmitted)
	providerFailure.Disposition = framechannel.SendAccepted
	providerFailure.Cause = relayv2.LifecycleCauseTransport
	sendRejected := send(relayv2.LifecycleSendRejected)
	sendRejected.Disposition = framechannel.SendRejected
	sendRejected.Cause = relayv2.LifecycleCauseCanceled
	rolledBack := send(relayv2.LifecycleSendRolledBack)
	rolledBack.Disposition = framechannel.SendRejected
	rolledBack.Cause = relayv2.LifecycleCauseCanceled
	deferred := send(relayv2.LifecycleRetirementDeferred)
	deferred.RetirementSource = relayv2.LifecycleRetirementTerminal
	deferred.DrainCause = relayv2.LifecycleCauseTransport
	retired := send(relayv2.LifecycleRetired)
	retired.RetirementSource = relayv2.LifecycleRetirementTerminal
	retired.DrainCause = relayv2.LifecycleCauseClosed
	settled := send(relayv2.LifecycleTerminalSettled)
	settled.Terminal = true
	settled.Disposition = framechannel.SendAccepted
	settled.Cause = relayv2.LifecycleCauseTransport
	retiring := relayv2.LifecycleTrace{LinkID: 1, OperationID: 2, Stage: relayv2.LifecycleLinkRetiring, RetirementSource: relayv2.LifecycleRetirementLinkClose, Cause: relayv2.LifecycleCauseClosed, DrainCause: relayv2.LifecycleCauseTransport}
	closed := retiring
	closed.Stage = relayv2.LifecycleLinkClosed
	closed.DrainCause = relayv2.LifecycleCauseNone
	dropped := relayv2.LifecycleTrace{LinkID: 1, Stage: relayv2.LifecycleTraceDropped, RetirementSource: relayv2.LifecycleRetirementNone, Cause: relayv2.LifecycleCauseNone, DrainCause: relayv2.LifecycleCauseNone, Dropped: 3}

	valid := []relayv2.LifecycleTrace{terminalReserved, sendAdmitted, providerFailure, sendRejected, rolledBack, deferred, retired, settled, retiring, closed, dropped}
	for _, source := range valid {
		if violation := relayv2.ValidateLifecycleTrace(source); violation != relayv2.LifecycleContractValid {
			t.Fatalf("source contract rejected %s: %v", source.Stage, violation)
		}
		if _, err := ProjectRelayLifecycle(clievent.CommandGet, source); err != nil {
			t.Fatalf("valid %s rejected: %v", source.Stage, err)
		}
	}

	missingSession := sendAdmitted
	missingSession.RelaySessionID = v2.RelaySessionID{}
	linkWithSession := closed
	linkWithSession.RelaySessionID = session
	dropWithCause := dropped
	dropWithCause.Cause = relayv2.LifecycleCauseTransport
	wrongDisposition := sendRejected
	wrongDisposition.Disposition = framechannel.SendAccepted
	for _, source := range []relayv2.LifecycleTrace{missingSession, linkWithSession} {
		_, err := ProjectRelayLifecycle(clievent.CommandGet, source)
		var projection ProjectionError
		if !errors.As(err, &projection) || projection.Reason() != ProjectionInvalidIdentity {
			t.Fatalf("invalid identity %+v error=%v", source, err)
		}
	}
	for _, source := range []relayv2.LifecycleTrace{dropWithCause, wrongDisposition} {
		_, err := ProjectRelayLifecycle(clievent.CommandGet, source)
		var projection ProjectionError
		if !errors.As(err, &projection) || projection.Reason() != ProjectionInvalidStageFields {
			t.Fatalf("invalid %+v error=%v", source, err)
		}
	}
	unknown := sendAdmitted
	unknown.Stage = relayv2.LifecycleStage("future")
	if _, err := ProjectRelayLifecycle(clievent.CommandGet, unknown); ObserverLossReason(err) != clievent.ObserverLossUnknownEnum {
		t.Fatalf("unknown stage reason=%v", err)
	}
}

func TestLaneSettlementProjectionPreservesRouteAndSaturatingCounters(t *testing.T) {
	raw := make([]byte, protocolsession.IdentityBytes)
	raw[len(raw)-1] = 1
	session, err := protocolsession.ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	event, err := ProjectLaneSettlement(transfer.LaneSettlementSummary{
		ProtocolSessionID: session, Route: transfer.LaneRouteDirect, Lane: transfer.LaneIdentity{ID: 4, Epoch: 2},
		DeliveredBlocks: ^uint64(0), DeliveredBytes: ^uint64(0), FailedBlockAttempts: 3, ReassignedBlocks: 2, Incomplete: true,
	})
	if err != nil || event.Route() != clievent.LaneRouteDirect || event.DeliveredBlocks() != ^uint64(0) || !event.Incomplete() {
		t.Fatalf("lane settlement=%#v err=%v", event, err)
	}
}

func TestFilesystemFailureClassificationProjectionIsExhaustive(t *testing.T) {
	stages := []struct {
		source osfs.FilesystemOutputFailureStage
		want   clievent.FilesystemFailureStage
	}{
		{osfs.FilesystemOutputFailureDestinationBinding, clievent.FilesystemFailureDestinationBinding},
		{osfs.FilesystemOutputFailureInventoryPaging, clievent.FilesystemFailureInventoryPaging},
		{osfs.FilesystemOutputFailureActiveLookup, clievent.FilesystemFailureActiveLookup},
		{osfs.FilesystemOutputFailureOperationAcquisition, clievent.FilesystemFailureOperationAcquisition},
		{osfs.FilesystemOutputFailureOperationAdmission, clievent.FilesystemFailureOperationAdmission},
		{osfs.FilesystemOutputFailureCheckpointReconciliation, clievent.FilesystemFailureCheckpointReconciliation},
		{osfs.FilesystemOutputFailureNativeDurability, clievent.FilesystemFailureNativeDurability},
		{osfs.FilesystemOutputFailureAuthorityClose, clievent.FilesystemFailureAuthorityClose},
	}
	for _, test := range stages {
		event, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{
			Operation:    osfs.TraceRuntimeDecision,
			FailureStage: test.source,
			Failed:       true,
		})
		if err != nil {
			t.Fatalf("stage %v: %v", test.source, err)
		}
		stage, _, _, present := event.FailureClassification()
		if !present || stage != test.want {
			t.Fatalf("stage %v projected as %v, present=%t", test.source, stage, present)
		}
	}

	for name, source := range map[string]osfs.FilesystemOutputTrace{
		"unknown checkpoint decision": {
			Operation: osfs.TraceRuntimeDecision, CheckpointDecision: osfs.FilesystemCheckpointDecision(255),
		},
		"unknown stage": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureStage(255), Failed: true,
		},
		"unknown reconciliation step": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureNativeDurability,
			ReconciliationStep: osfs.FilesystemCheckpointReconciliationStep(255), Failed: true,
		},
		"unknown native class": {
			Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureNativeDurability,
			NativeErrorClass: osfs.FilesystemNativeErrorClass(255), Failed: true,
		},
	} {
		_, err := ProjectFilesystemOutput(source)
		var projection ProjectionError
		if !errors.As(err, &projection) || projection.Reason() != ProjectionUnknownEnum {
			t.Fatalf("%s error=%v", name, err)
		}
	}

	_, err := ProjectFilesystemOutput(osfs.FilesystemOutputTrace{
		Operation: osfs.TraceRuntimeDecision, FailureStage: osfs.FilesystemOutputFailureDestinationBinding,
		ReconciliationStep: osfs.FilesystemCheckpointRecordPromotion, Failed: true,
	})
	var projection ProjectionError
	if !errors.As(err, &projection) || projection.Reason() != ProjectionInvalidStageFields {
		t.Fatalf("invalid filesystem stage/step error=%v", err)
	}
}

func TestReceiverTerminationClosedClassMappingsAreExhaustive(t *testing.T) {
	for _, value := range []v2peer.ReceiverTerminalOwner{v2peer.ReceiverTerminalUnbound, v2peer.ReceiverTerminalLocal, v2peer.ReceiverTerminalRemote, v2peer.ReceiverTerminalRuntime} {
		if _, ok := projectReceiverTerminalOwner(value); !ok {
			t.Fatalf("owner %q rejected", value)
		}
	}
	for _, value := range []v2peer.ReceiverAttemptDisposition{v2peer.ReceiverDispositionFallbackAllowed, v2peer.ReceiverDispositionSessionUnavailable, v2peer.ReceiverDispositionSessionUnsafe} {
		if _, ok := projectReceiverDisposition(value); !ok {
			t.Fatalf("disposition %q rejected", value)
		}
	}
	provenances := []v2peer.ReceiverTerminalProvenance{
		v2peer.ReceiverProvenanceUnbound, v2peer.ReceiverProvenanceLocalExplicitStop, v2peer.ReceiverProvenanceLocalContextEnded,
		v2peer.ReceiverProvenanceLocalNegotiationFailure, v2peer.ReceiverProvenanceLocalNegotiationTimeout, v2peer.ReceiverProvenanceLocalAdmissionTimeout, v2peer.ReceiverProvenanceLocalOperationContract,
		v2peer.ReceiverProvenanceRemoteOperationRejected, v2peer.ReceiverProvenanceRemoteUnknownControl, v2peer.ReceiverProvenanceRemoteControlMalformed,
		v2peer.ReceiverProvenanceRemoteFailureMalformed, v2peer.ReceiverProvenanceRemoteFailureScopeViolation, v2peer.ReceiverProvenanceRuntimeStopping,
		v2peer.ReceiverProvenanceSignalingAdapterContract, v2peer.ReceiverProvenanceAuthenticatedSecondAnswer,
		v2peer.ReceiverProvenanceAuthenticatedFinalConflict, v2peer.ReceiverProvenanceAuthenticatedAnswerBindingMismatch,
		v2peer.ReceiverProvenanceAuthenticatedCandidateBindingMismatch, v2peer.ReceiverProvenanceAuthenticatedContinuationAuthority,
	}
	for _, value := range provenances {
		if _, ok := projectReceiverProvenance(value); !ok {
			t.Fatalf("provenance %q rejected", value)
		}
	}
	for _, value := range []v2peer.ReceiverBenignCause{v2peer.ReceiverBenignContextCanceled, v2peer.ReceiverBenignLocalOperationMissing, v2peer.ReceiverBenignRemoteOperationMissing} {
		if _, ok := projectReceiverBenign(value); !ok {
			t.Fatalf("benign %q rejected", value)
		}
	}
	classes := []v2peer.ReceiverCauseClass{v2peer.ReceiverCauseRuntimeClosed, v2peer.ReceiverCauseConfiguration, v2peer.ReceiverCauseOperationMissing, v2peer.ReceiverCauseNegotiationTimeout, v2peer.ReceiverCauseAdmissionTimeout, v2peer.ReceiverCauseCandidateLimit, v2peer.ReceiverCauseChannelAdmission, v2peer.ReceiverCauseEventCapacity, v2peer.ReceiverCauseNegotiation, v2peer.ReceiverCauseProtocol, v2peer.ReceiverCauseDeadline, v2peer.ReceiverCausePeerShutdown, v2peer.ReceiverCauseChannelDrain, v2peer.ReceiverCauseUnknown}
	for _, value := range classes {
		if _, ok := projectReceiverCauseClassification(value); !ok {
			t.Fatalf("cause %q rejected", value)
		}
	}
	for _, value := range []v2peer.PeerTeardownTransition{v2peer.PeerTeardownPeerShutdownInitiated, v2peer.PeerTeardownPeerShutdownReturned, v2peer.PeerTeardownChannelDrainStarted, v2peer.PeerTeardownChannelDrainJoined} {
		if _, ok := projectPeerTeardown(value); !ok {
			t.Fatalf("teardown %q rejected", value)
		}
	}
}

func TestWebRTCLifecycleProjectionEnforcesSourceFieldMatrix(t *testing.T) {
	valid := []wsrtc.LifecycleTrace{
		{ChannelID: 1, OperationID: 1, Operation: wsrtc.LifecycleOperationSendTerminal, Transition: wsrtc.LifecycleTransitionSendAccepted, Disposition: framechannel.SendAccepted, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalLocalPending, Cause: wsrtc.LifecycleCauseNone},
		{ChannelID: 1, OperationID: 2, Operation: wsrtc.LifecycleOperationSend, Transition: wsrtc.LifecycleTransitionSendAccepted, Disposition: framechannel.SendAccepted, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseTransport},
		{ChannelID: 1, OperationID: 3, Operation: wsrtc.LifecycleOperationSend, Transition: wsrtc.LifecycleTransitionSendRejected, Disposition: framechannel.SendRejected, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseCanceled},
		{ChannelID: 1, OperationID: 31, Operation: wsrtc.LifecycleOperationSendTerminal, Transition: wsrtc.LifecycleTransitionSendRejected, Disposition: framechannel.SendRejected, State: framechannel.Connecting, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNotOpen},
		{ChannelID: 1, OperationID: 32, Operation: wsrtc.LifecycleOperationSend, Transition: wsrtc.LifecycleTransitionSendRejected, Disposition: framechannel.SendRejected, State: framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNotOpen},
		{ChannelID: 1, OperationID: 4, Operation: wsrtc.LifecycleOperationSend, Transition: wsrtc.LifecycleTransitionSendRetired, Disposition: framechannel.SendRetired, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalLocalPending, Cause: wsrtc.LifecycleCauseNaturalRetirement},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionRemoteTerminalReserved, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalRemotePending, Cause: wsrtc.LifecycleCauseNone},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionTerminationPending, State: framechannel.Open, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseRemoteClosed},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionTerminationPending, State: framechannel.Connecting, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseTransport},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionClosedClean, State: framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNone},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionClosedFailed, State: framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseTransport},
		{ChannelID: 1, Operation: wsrtc.LifecycleOperationChannel, Transition: wsrtc.LifecycleTransitionTraceDropped, State: framechannel.Closed, Terminal: wsrtc.LifecycleTerminalNone, Cause: wsrtc.LifecycleCauseNone, Dropped: 5},
	}
	for _, source := range valid {
		if violation := wsrtc.ValidateLifecycleTrace(source); violation != wsrtc.LifecycleContractValid {
			t.Fatalf("source contract rejected %s: %v", source.Transition, violation)
		}
		if _, err := ProjectWebRTCLifecycle(clievent.CommandGet, source); err != nil {
			t.Fatalf("valid %s rejected: %v", source.Transition, err)
		}
	}
	for _, mutate := range []func(*wsrtc.LifecycleTrace){
		func(v *wsrtc.LifecycleTrace) { v.OperationID = 0 },
		func(v *wsrtc.LifecycleTrace) { v.State = framechannel.Closed },
		func(v *wsrtc.LifecycleTrace) { v.Dropped = 1 },
	} {
		source := valid[0]
		mutate(&source)
		_, err := ProjectWebRTCLifecycle(clievent.CommandGet, source)
		var projection ProjectionError
		if !errors.As(err, &projection) || projection.Reason() != ProjectionInvalidStageFields {
			t.Fatalf("invalid %+v error=%v", source, err)
		}
	}
}
