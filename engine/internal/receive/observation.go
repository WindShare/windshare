package receive

import (
	"errors"
	"sync"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

const protocolObservationCapacity observationstream.Capacity = 256
const (
	processTraceGetComponent         testrun.Component = "wind_get"
	processTraceReceiverDirectLane   testrun.Milestone = "receiver_direct_lane"
	processTraceReceiverRelayContent testrun.Milestone = "receiver_relay_content"
	processTraceReceiverJoinStopped  testrun.Milestone = "receiver_join_stopped"
)

type observedSource struct {
	complete func() uint64
	done     <-chan struct{}
	kind     ObservationSource
}
type observationState struct {
	control      task.Control
	detailed     bool
	metrics      *downloadmetrics.Metrics
	mu           sync.Mutex
	sources      []observedSource
	relays       map[v2.RelayIdentity]observedSource
	protocol     observationstream.Producer[sessionruntime.ProtocolObservation]
	failure      error
	failureCode  FailureCode
	cleanupError error
	losses       map[ObservationSource]uint64
}
type getObservation struct {
	*observationState
	session protocolsession.ProtocolSessionID
	paths   *receiverContentPaths
}

func newObservation(control task.Control, detailed bool, metrics *downloadmetrics.Metrics) getObservation {
	o := getObservation{observationState: &observationState{control: control, detailed: detailed, metrics: metrics}}
	if detailed {
		producer, consumer, _ := observationstream.New[sessionruntime.ProtocolObservation](protocolObservationCapacity)
		o.protocol = producer
		o.register(ObservationProtocol, func() uint64 { return producer.Complete().CapacityDropped }, drain(consumer, func(v sessionruntime.ProtocolObservation) { o.emit(ProtocolObserved{Value: v}) }))
	}
	return o
}
func (o getObservation) emit(e task.Event) bool {
	return o.observationState != nil && o.control.Emit != nil && o.control.Emit(e)
}
func drain[T any](values <-chan T, emit func(T)) <-chan struct{} {
	done := make(chan struct{})
	if values == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		for v := range values {
			emit(v)
		}
	}()
	return done
}
func (o getObservation) register(kind ObservationSource, complete func() uint64, done <-chan struct{}) {
	o.mu.Lock()
	o.sources = append(o.sources, observedSource{complete: complete, done: done, kind: kind})
	o.mu.Unlock()
}

// All source owners have stopped before this cut. Readers only forward into
// the task's bounded queue, so a detached client cannot delay producer joining.
func (o getObservation) completeGeneration() {
	o.mu.Lock()
	var settled, retained []observedSource
	for _, source := range o.sources {
		if source.kind == ObservationNative || source.kind == ObservationProtocol {
			retained = append(retained, source)
		} else {
			settled = append(settled, source)
		}
	}
	o.sources = retained
	for _, source := range o.relays {
		settled = append(settled, source)
	}
	o.relays = nil
	o.mu.Unlock()
	for _, source := range settled {
		loss := source.complete()
		<-source.done
		o.recordLoss(source.kind, loss)
	}
}
func (o getObservation) complete() {
	o.mu.Lock()
	sources := o.sources
	for _, source := range o.relays {
		sources = append(sources, source)
	}
	o.relays = nil
	o.sources = nil
	o.mu.Unlock()
	for _, source := range sources {
		loss := source.complete()
		<-source.done
		o.recordLoss(source.kind, loss)
	}
}
func (o getObservation) fail(code stepOutcome, cause error) stepOutcome {
	o.mu.Lock()
	o.failure = cause
	o.mu.Unlock()
	return code
}
func (o getObservation) failCode(code stepOutcome, failure FailureCode) stepOutcome {
	o.mu.Lock()
	o.failureCode = failure
	o.mu.Unlock()
	return code
}
func (o getObservation) warning(cause error) {
	if cause != nil {
		o.emit(Warning{Cause: cause})
	}
}
func (o getObservation) warningCode(code FailureCode)      { o.emit(Warning{Code: code}) }
func (o getObservation) relayConnected(v v2.RelayEndpoint) { o.emit(RelayConnected{Endpoint: v}) }
func (o getObservation) receiverRecovery(v relayset.ReceiverRecoveryObservation) {
	o.emit(RecoveryObserved{Value: v})
}
func (o getObservation) filesystemOutput(v osfs.FilesystemOutputTrace) {
	o.emit(FilesystemObserved{Value: v})
}
func (o getObservation) transferLifecycle(v transfer.TransferLifecycleTrace) {
	o.emit(TransferObserved{Value: v})
}
func (o getObservation) protocolObservations() observationstream.Producer[sessionruntime.ProtocolObservation] {
	return o.protocol
}
func (o getObservation) relayObservationCapacity() int {
	if o.detailed {
		return relayv2.DefaultLifecycleObservationCapacity
	}
	return 0
}
func (o getObservation) laneSettlementObservationCapacity() transfer.LaneSettlementObservationCapacity {
	if o.detailed {
		return transfer.DefaultLaneSettlementObservationCapacity
	}
	return 0
}
func (o getObservation) progress(operation receivecontract.OperationID, job transfer.TransferJobID, v transfer.ReceiveProgressSnapshot) {
	value := ProgressObserved{Operation: operation, Job: job, Value: v}
	if o.metrics != nil {
		value.Connectivity = o.metrics.Snapshot(false)
	}
	o.emit(value)
}
func (o getObservation) contentPath(path ContentPath) { o.emit(ContentPathObserved{Path: path}) }
func (o getObservation) fallback(code FailureCode)    { o.emit(FallbackObserved{Code: code}) }
func (o getObservation) laneAdopted(session protocolsession.ProtocolSessionID, lane sessionruntime.LaneIdentity) {
	o.emit(LaneAdopted{Session: session, Lane: lane})
}
func (o getObservation) registerNative(native *nativepeer.NativePeerConnectivity) {
	if !o.detailed {
		return
	}
	o.register(ObservationNative, func() uint64 { return native.CompleteObservations().CapacityDropped }, drain(native.Observations(), func(v nativepeer.Observation) { o.emit(NativeObserved{Value: v}) }))
}

type relayObservationSource interface {
	Endpoint() v2.RelayEndpoint
	LifecycleTrace() <-chan relayv2.LifecycleTrace
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

func (o getObservation) registerRelayConnection(c relayObservationSource) {
	if !o.detailed {
		return
	}
	source := observedSource{kind: ObservationRelay, complete: func() uint64 { return c.CompleteObservations().Loss.CapacityDropped }, done: drain(c.LifecycleTrace(), func(v relayv2.LifecycleTrace) { o.emit(RelayObserved{Value: v}) })}
	o.mu.Lock()
	if o.relays == nil {
		o.relays = make(map[v2.RelayIdentity]observedSource)
	}
	previous, exists := o.relays[c.Endpoint().Identity]
	o.relays[c.Endpoint().Identity] = source
	o.mu.Unlock()
	if exists {
		loss := previous.complete()
		<-previous.done
		o.recordLoss(ObservationRelay, loss)
	}
}
func (o getObservation) registerLaneSet(lanes *transfer.LaneSet) {
	if !o.detailed {
		return
	}
	o.register(ObservationLane, func() uint64 { return lanes.CompleteObservations().Loss.CapacityDropped }, drain(lanes.SettlementObservations(), func(v transfer.LaneSettlementSummary) { o.emit(LaneObserved{Value: v}) }))
}
func (o getObservation) registerWebRTC(channel *wsrtc.Channel) {
	if !o.detailed {
		return
	}
	o.register(ObservationWebRTC, func() uint64 { return channel.CompleteObservations().Loss.CapacityDropped }, drain(channel.LifecycleTrace(), func(v wsrtc.LifecycleTrace) { o.emit(WebRTCObserved{Value: v}) }))
}

type receiverObservationCompleter interface {
	ReceiverTerminationObservations() <-chan v2peer.ReceiverTerminationTrace
	PeerDiagnostics() <-chan v2peer.PeerDiagnosticObservation
	CompleteObservations() v2peer.ReceiverObservationCompletion
}

func (o getObservation) registerReceiverFactory(source receiverObservationCompleter, stop *receiverLocalStop) {
	if !o.detailed {
		return
	}
	terminal := drain(source.ReceiverTerminationObservations(), func(v v2peer.ReceiverTerminationTrace) { o.emit(PeerTerminated{Value: v, LocalStop: stop.snapshot()}) })
	diagnostic := drain(source.PeerDiagnostics(), func(v v2peer.PeerDiagnosticObservation) { o.emit(PeerDiagnosticObserved{Value: v}) })
	var completion v2peer.ReceiverObservationCompletion
	o.register(ObservationPeerTermination, func() uint64 {
		completion = source.CompleteObservations()
		return completion.Terminations.Loss.CapacityDropped
	}, terminal)
	o.register(ObservationPeerDiagnostic, func() uint64 { return completion.Diagnostics.Loss.CapacityDropped }, diagnostic)
}
func (a *runner) recordProcessTrace(component testrun.Component, name testrun.Milestone, outcome testrun.Outcome) {
	if a.control.Emit != nil {
		a.control.Emit(Milestone{Component: component, Name: name, Outcome: outcome})
	}
}

func (o getObservation) admission(decision receiverAdmissionDecision) {
	event := AdmissionObserved{Session: o.session, Trigger: decision.Trigger, TerminalOwner: decision.TerminalOwner, Cause: decision.Cause}
	if o.paths != nil {
		event.Operation, event.Job = o.paths.transferIdentity()
	}
	o.emit(event)
}

func (o getObservation) failureSnapshot() Failure {
	o.mu.Lock()
	defer o.mu.Unlock()
	return Failure{Code: o.failureCode, Cause: o.failure}
}
func (o getObservation) recordCleanup(cause error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cleanupError = errors.Join(o.cleanupError, cause)
}
func (o getObservation) cleanupFailure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cleanupError
}
func (o getObservation) recordLoss(source ObservationSource, count uint64) {
	if count == 0 {
		return
	}
	o.mu.Lock()
	if o.losses == nil {
		o.losses = make(map[ObservationSource]uint64)
	}
	prior := o.losses[source]
	if count > ^uint64(0)-prior {
		count = ^uint64(0)
	} else {
		count += prior
	}
	o.losses[source] = count
	o.mu.Unlock()
	o.emit(ObservationLoss{Source: source, Count: count})
}
func (o getObservation) lossSnapshot() []ObservationLoss {
	o.mu.Lock()
	defer o.mu.Unlock()
	var result []ObservationLoss
	for _, source := range []ObservationSource{ObservationRelay, ObservationWebRTC, ObservationLane, ObservationNative, ObservationProtocol, ObservationPeerTermination, ObservationPeerDiagnostic} {
		if count := o.losses[source]; count > 0 {
			result = append(result, ObservationLoss{Source: source, Count: count})
		}
	}
	return result
}
